package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"container/ring"

	"go-mcp-sse-proxy/logger"

	"golang.org/x/time/rate"
)

// RateLimiter manages rate limiting per IP address
type RateLimiter struct {
	visitors map[string]*rate.Limiter
	mu       sync.RWMutex
	rate     rate.Limit
	burst    int
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(r rate.Limit, b int) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*rate.Limiter),
		rate:     r,
		burst:    b,
	}
}

// GetVisitor retrieves or creates a limiter for a visitor
func (rl *RateLimiter) GetVisitor(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.visitors[ip]
	if !exists {
		limiter = rate.NewLimiter(rl.rate, rl.burst)
		rl.visitors[ip] = limiter
	}
	return limiter
}

// securityHeaders adds security headers to the response
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HSTS header (only in production)
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		// Security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' http: https:; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline';")

		// CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*") // You might want to restrict this in production
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, ENV_*")
		w.Header().Set("Access-Control-Max-Age", "3600")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware applies rate limiting per IP
func rateLimitMiddleware(rl *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get IP from X-Forwarded-For header, or fallback to RemoteAddr
			ip := r.Header.Get("X-Forwarded-For")
			if ip == "" {
				ip = r.RemoteAddr
			}

			// Clean IP address if it contains port
			if strings.Contains(ip, ":") {
				ip = strings.Split(ip, ":")[0]
			}

			limiter := rl.GetVisitor(ip)
			if !limiter.Allow() {
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// SessionInfo tracks session metadata
type SessionInfo struct {
	Instance  *GatewayInstance
	LastUsed  time.Time
	CreatedAt time.Time
}

// GatewayInstance holds a supergateway subprocess bound to an internal URL.
type GatewayInstance struct {
	Cmd         *exec.Cmd
	InternalURL *url.URL
	StartTime   time.Time
	Cancel      context.CancelFunc
	Sessions    atomic.Int32 // Number of active sessions
}

// PortManager handles port allocation and tracking
type PortManager struct {
	mu              sync.Mutex
	usedPorts       map[int]bool
	minPort         int
	maxPort         int
	maxRetries      int
	releasedPorts   *ring.Ring    // Circular buffer of recently released ports
	maxReleased     int           // Maximum number of released ports to track
	lastCleanup     time.Time     // Last time stale ports were cleaned up
	cleanupInterval time.Duration // How often to clean up stale ports
}

// NewPortManager creates a new port manager with the specified port range
func NewPortManager(minPort, maxPort int) *PortManager {
	const (
		defaultMaxReleased     = 100 // Maximum number of released ports to track
		defaultMaxRetries      = 5
		defaultCleanupInterval = 5 * time.Minute
	)

	return &PortManager{
		usedPorts:       make(map[int]bool),
		minPort:         minPort,
		maxPort:         maxPort,
		maxRetries:      defaultMaxRetries,
		releasedPorts:   ring.New(defaultMaxReleased),
		maxReleased:     defaultMaxReleased,
		lastCleanup:     time.Now(),
		cleanupInterval: defaultCleanupInterval,
	}
}

// cleanupStale removes stale port entries and verifies port availability
func (pm *PortManager) cleanupStale() {
	now := time.Now()
	if now.Sub(pm.lastCleanup) < pm.cleanupInterval {
		return
	}
	pm.lastCleanup = now

	// Create a list of ports to check
	portsToCheck := make([]int, 0, len(pm.usedPorts))
	for port := range pm.usedPorts {
		portsToCheck = append(portsToCheck, port)
	}

	// Check each port's availability
	for _, port := range portsToCheck {
		if pm.isPortAvailable(port) {
			delete(pm.usedPorts, port)
		}
	}
}

// isPortAvailable checks if a port is actually available on the system
func (pm *PortManager) isPortAvailable(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// GetPort attempts to acquire a free port within the configured range
func (pm *PortManager) GetPort() (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Run cleanup if needed
	pm.cleanupStale()

	// First try to reuse a recently released port
	if pm.releasedPorts != nil {
		// Try each port in the ring buffer
		current := pm.releasedPorts
		for i := 0; i < pm.maxReleased; i++ {
			if port, ok := current.Value.(int); ok && port != 0 {
				// Verify the port is actually free
				if !pm.usedPorts[port] && pm.isPortAvailable(port) {
					pm.usedPorts[port] = true
					return port, nil
				}
			}
			current = current.Next()
		}
	}

	// Try to find a new port within range
	for attempt := 0; attempt < pm.maxRetries; attempt++ {
		// Try to get a port from the system
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()

		// Verify port is within our allowed range
		if port < pm.minPort || port > pm.maxPort {
			continue
		}

		// Double check port availability after closing
		if !pm.isPortAvailable(port) {
			continue
		}

		// Check if port is already tracked as in use
		if pm.usedPorts[port] {
			continue
		}

		pm.usedPorts[port] = true
		return port, nil
	}

	return 0, fmt.Errorf("failed to acquire free port after %d attempts", pm.maxRetries)
}

// ReleasePort marks a port as no longer in use
func (pm *PortManager) ReleasePort(port int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.usedPorts[port] {
		delete(pm.usedPorts, port)

		// Add to circular buffer of released ports
		if pm.releasedPorts != nil {
			pm.releasedPorts.Value = port
			pm.releasedPorts = pm.releasedPorts.Next()
		}
	}
}

// ProcessManager handles lifecycle of gateway instances
type ProcessManager struct {
	instancesMu   sync.RWMutex
	sessionsMu    sync.RWMutex
	instances     map[string]*GatewayInstance
	sessions      map[string]*SessionInfo
	maxLifetime   time.Duration
	cleanupTicker *time.Ticker
	portManager   *PortManager

	// Configuration
	maxSessions      int           // Maximum concurrent sessions per instance
	maxTotalSessions int           // Maximum total sessions across all instances
	sessionTimeout   time.Duration // Time after which inactive sessions are cleaned up
	cleanupInterval  time.Duration // How often to run cleanup
	GracefulTimeout  time.Duration // Time to wait for graceful shutdown before force kill
}

// ProcessManagerConfig holds configuration for the process manager
type ProcessManagerConfig struct {
	MaxLifetime      time.Duration
	MaxSessions      int
	MaxTotalSessions int
	SessionTimeout   time.Duration
	CleanupInterval  time.Duration
	GracefulTimeout  time.Duration // Time to wait for graceful shutdown before force kill
}

// TimeoutConfig holds timeout configuration for various operations
type TimeoutConfig struct {
	SSETimeout        time.Duration // Timeout for SSE connections
	RequestTimeout    time.Duration // Timeout for regular HTTP requests
	ShutdownTimeout   time.Duration // Timeout for graceful shutdown
	HealthCheckPeriod time.Duration // Period between health checks
}

// DefaultConfig returns sensible default configuration
func DefaultConfig() ProcessManagerConfig {
	return ProcessManagerConfig{
		MaxLifetime:      1 * time.Hour,
		MaxSessions:      100,  // Max 100 sessions per instance
		MaxTotalSessions: 1000, // Max 1000 total sessions
		SessionTimeout:   30 * time.Minute,
		CleanupInterval:  1 * time.Minute,
		GracefulTimeout:  5 * time.Second, // Give processes 5 seconds to shutdown gracefully
	}
}

// DefaultTimeoutConfig returns sensible default timeout configuration
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		SSETimeout:        24 * time.Hour,   // Increased from 1 hour
		RequestTimeout:    60 * time.Second, // Increased from 30 seconds
		ShutdownTimeout:   60 * time.Second, // Increased from 30 seconds
		HealthCheckPeriod: 30 * time.Second,
	}
}

// NewProcessManager creates a new process manager
func NewProcessManager(config ProcessManagerConfig) *ProcessManager {
	pm := &ProcessManager{
		instances:        make(map[string]*GatewayInstance),
		sessions:         make(map[string]*SessionInfo),
		maxLifetime:      config.MaxLifetime,
		maxSessions:      config.MaxSessions,
		maxTotalSessions: config.MaxTotalSessions,
		sessionTimeout:   config.SessionTimeout,
		cleanupInterval:  config.CleanupInterval,
		cleanupTicker:    time.NewTicker(config.CleanupInterval),
		portManager:      NewPortManager(10000, 65535), // Use non-privileged ports
		GracefulTimeout:  config.GracefulTimeout,
	}
	go pm.cleanupLoop()
	return pm
}

// cleanupLoop periodically checks for and terminates expired processes and sessions
func (pm *ProcessManager) cleanupLoop() {
	for range pm.cleanupTicker.C {
		pm.cleanupExpiredProcesses()
		pm.cleanupExpiredSessions()
	}
}

// cleanupExpiredSessions removes sessions that have been inactive
func (pm *ProcessManager) cleanupExpiredSessions() {
	now := time.Now()
	pm.sessionsMu.Lock()
	defer pm.sessionsMu.Unlock()

	for sid, session := range pm.sessions {
		if now.Sub(session.LastUsed) > pm.sessionTimeout {
			log.Info("Cleaning up expired session: %s", sid)
			if session.Instance != nil {
				session.Instance.Sessions.Add(-1)
			}
			delete(pm.sessions, sid)
		}
	}
}

// cleanupExpiredProcesses terminates processes that have exceeded maxLifetime
func (pm *ProcessManager) cleanupExpiredProcesses() {
	now := time.Now()
	pm.instancesMu.Lock()
	defer pm.instancesMu.Unlock()

	for key, inst := range pm.instances {
		if now.Sub(inst.StartTime) > pm.maxLifetime {
			log.Info("Terminating expired process for key: %s", key)
			pm.terminateInstance(key, inst)
		}
	}
}

// terminateInstance cleanly stops a gateway instance
func (pm *ProcessManager) terminateInstance(key string, inst *GatewayInstance) {
	// Create a context with timeout for the cleanup process
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), pm.GracefulTimeout)
	defer cleanupCancel()

	// Cancel the instance context first
	if inst.Cancel != nil {
		inst.Cancel()
	}

	if inst.Cmd.Process != nil {
		// Create a channel to track process exit
		done := make(chan error, 1)

		// Start a goroutine to wait for the process
		go func() {
			done <- inst.Cmd.Wait()
		}()

		// First try graceful shutdown with SIGTERM
		if err := inst.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
			if !strings.Contains(err.Error(), "process already finished") {
				log.Error("Failed to send SIGTERM to process: %v", err)
			}
		}

		// Wait for process to exit or timeout
		select {
		case err := <-done:
			if err != nil && !strings.Contains(err.Error(), "signal: killed") {
				log.Error("Process exited with error: %v", err)
			} else {
				log.Info("Process terminated gracefully")
			}
		case <-cleanupCtx.Done():
			// Process didn't exit gracefully, force kill
			log.Warn("Process did not exit gracefully, forcing kill")
			if err := inst.Cmd.Process.Kill(); err != nil {
				if !strings.Contains(err.Error(), "process already finished") {
					log.Error("Failed to kill process: %v", err)
				}
			}
		}
	}

	// Release the port
	if inst.InternalURL != nil {
		if port, err := strconv.Atoi(inst.InternalURL.Port()); err == nil {
			pm.portManager.ReleasePort(port)
		}
	}

	// Clean up instance and sessions
	pm.cleanupInstanceAndSessions(key, inst)
}

// cleanupInstanceAndSessions removes the instance and its associated sessions
func (pm *ProcessManager) cleanupInstanceAndSessions(key string, inst *GatewayInstance) {
	const lockTimeout = 5 * time.Second

	// First acquire instancesMu
	if !tryLock(&pm.instancesMu, lockTimeout) {
		log.Error("Failed to acquire instance lock for cleanup")
		return
	}
	// Only delete if it's still the same instance
	if current := pm.instances[key]; current == inst {
		delete(pm.instances, key)
	}
	pm.instancesMu.Unlock()

	// Then acquire sessionsMu
	if !tryLock(&pm.sessionsMu, lockTimeout) {
		log.Error("Failed to acquire session lock for cleanup")
		return
	}
	defer pm.sessionsMu.Unlock()

	// Only clean up sessions that are specifically tied to this instance
	for sid, session := range pm.sessions {
		if session.Instance == inst {
			// Check if the session is still active
			if session.Instance.Sessions.Load() > 0 {
				session.Instance.Sessions.Add(-1)
			}
			delete(pm.sessions, sid)
		}
	}
}

// Shutdown cleanly stops all managed processes
func (pm *ProcessManager) Shutdown() {
	pm.cleanupTicker.Stop()
	pm.instancesMu.Lock()
	defer pm.instancesMu.Unlock()

	for key, inst := range pm.instances {
		log.Info("Shutting down instance: %s", key)
		pm.terminateInstance(key, inst)
	}
}

// CreateSession creates a new session for an instance
func (pm *ProcessManager) CreateSession(sessionID string, inst *GatewayInstance) error {
	pm.sessionsMu.Lock()
	defer pm.sessionsMu.Unlock()

	// Check if session already exists
	if _, exists := pm.sessions[sessionID]; exists {
		return errors.New("session already exists")
	}

	// Check total session limit
	if len(pm.sessions) >= pm.maxTotalSessions {
		return errors.New("maximum total sessions reached")
	}

	// Check per-instance session limit
	if inst.Sessions.Load() >= int32(pm.maxSessions) {
		return errors.New("maximum sessions per instance reached")
	}

	// Create new session
	pm.sessions[sessionID] = &SessionInfo{
		Instance:  inst,
		LastUsed:  time.Now(),
		CreatedAt: time.Now(),
	}
	inst.Sessions.Add(1)

	return nil
}

// TouchSession updates the last used time for a session
func (pm *ProcessManager) TouchSession(sessionID string) bool {
	const lockTimeout = 5 * time.Second

	if !tryLock(&pm.sessionsMu, lockTimeout) {
		log.Error("Failed to acquire session lock for touch")
		return false
	}
	defer pm.sessionsMu.Unlock()

	if session, exists := pm.sessions[sessionID]; exists {
		session.LastUsed = time.Now()
		return true
	}
	return false
}

var (
	// allowedCommands: unescaped -> actual command invocation
	allowedCommands = map[string]string{
		"npx -y @upstash/context7-mcp@latest": "npx -y @upstash/context7-mcp@latest",
		"npx -y @maximai/mcp-server@latest":   "npx -y @maximai/mcp-server@latest",
	}

	// blacklistedEnvVars contains environment variables that should never be overwritten
	blacklistedEnvVars = map[string]bool{
		"PATH":              true,
		"HOME":              true,
		"USER":              true,
		"SHELL":             true,
		"PWD":               true,
		"TMPDIR":            true,
		"TEMP":              true,
		"TMP":               true,
		"HOSTNAME":          true,
		"LANG":              true,
		"LC_ALL":            true,
		"SUDO_USER":         true,
		"SUDO_COMMAND":      true,
		"SSH_AUTH_SOCK":     true,
		"SSH_AGENT_PID":     true,
		"AWS_ACCESS_KEY":    true,
		"AWS_SECRET_KEY":    true,
		"AWS_SESSION_TOKEN": true,
		"API_KEY":           true,
		"SECRET_KEY":        true,
		"PRIVATE_KEY":       true,
		"PASSWORD":          true,
		"TOKEN":             true,
	}

	processManager *ProcessManager
	log            *logger.Logger
	rateLimiter    *RateLimiter
	timeoutConfig  TimeoutConfig
)

func main() {
	// Initialize timeout configuration
	timeoutConfig = DefaultTimeoutConfig()

	// Initialize logger with configuration
	logConfig := &logger.LoggerConfig{
		Level:      logger.LogDebug,
		TimeFormat: time.RFC3339,
		Filename:   filepath.Join("logs", "proxy.log"),
		MaxSize:    100, // 100MB
		MaxBackups: 5,   // 5 backups
		MaxAge:     30,  // 30 days
		Compress:   true,
		UseConsole: true,
		UseFile:    true,
	}

	// Allow setting log level via environment variable
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		switch strings.ToUpper(lvl) {
		case "DEBUG":
			logConfig.Level = logger.LogDebug
		case "INFO":
			logConfig.Level = logger.LogInfo
		case "WARN":
			logConfig.Level = logger.LogWarn
		case "ERROR":
			logConfig.Level = logger.LogError
		}
	}

	log = logger.NewLogger(logConfig)

	// Create process manager with default configuration
	processManager = NewProcessManager(DefaultConfig())

	// Initialize rate limiter (100 requests per minute per IP, burst of 10)
	rateLimiter = NewRateLimiter(rate.Limit(100.0/60.0), 10)

	// Setup signal handling for graceful shutdown
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Create handler chain with security middleware
	handler := rateLimitMiddleware(rateLimiter)(
		securityHeaders(
			http.HandlerFunc(multiProxy),
		),
	)

	server := &http.Server{
		Addr:    ":8000",
		Handler: handler,
		// Add timeouts for additional security
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Info("Starting proxy on %s...", server.Addr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Error("HTTP server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-signalChan
	log.Info("Shutdown signal received")

	// Create shutdown context with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Shutdown HTTP server
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error: %v", err)
	}

	// Cleanup all processes
	processManager.Shutdown()
	log.Info("Shutdown complete")
}

// multiProxy routes SSE and message requests, spawning per-SSE instances
func multiProxy(w http.ResponseWriter, r *http.Request) {
	const lockTimeout = 5 * time.Second

	// get raw path without query
	raw := r.URL.RawPath
	if raw == "" {
		raw = r.RequestURI
		if i := strings.Index(raw, "?"); i != -1 {
			raw = raw[:i]
		}
	}
	raw = strings.TrimPrefix(raw, "/")
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) < 1 {
		http.NotFound(w, r)
		return
	}

	// decode command key
	prefix := parts[0]
	cmdKey, err := url.PathUnescape(prefix)
	if err != nil {
		http.Error(w, "Bad prefix encoding", http.StatusBadRequest)
		return
	}

	cmdStr, allowed := allowedCommands[cmdKey]
	if !allowed {
		http.Error(w, "Command not allowed", http.StatusForbidden)
		return
	}

	rest := "/"
	if len(parts) == 2 {
		rest += parts[1]
	}

	// SSE connect: spawn new instance
	if r.Method == http.MethodGet && rest == "/sse" {
		inst, err := spawnInstance(cmdKey, cmdStr, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		// proxy SSE and let supergateway handle session creation
		proxySSE(w, r, inst, rest)

		// cleanup on disconnect
		processManager.terminateInstance(cmdKey, inst)
		return
	}

	// message POSTs
	if r.Method == http.MethodPost && rest == "/message" {
		sid := r.URL.Query().Get("sessionId")
		if sid == "" {
			http.Error(w, "Missing sessionId", http.StatusBadRequest)
			return
		}

		// First check if session exists with read lock
		if !tryRLock(&processManager.sessionsMu, lockTimeout) {
			http.Error(w, "Failed to acquire session lock", http.StatusServiceUnavailable)
			return
		}
		session, exists := processManager.sessions[sid]
		processManager.sessionsMu.RUnlock()

		if !exists {
			// Get instance with read lock
			if !tryRLock(&processManager.instancesMu, lockTimeout) {
				http.Error(w, "Failed to acquire instance lock", http.StatusServiceUnavailable)
				return
			}
			inst := processManager.instances[cmdKey]
			processManager.instancesMu.RUnlock()

			if inst == nil {
				http.Error(w, "No gateway instance", http.StatusBadGateway)
				return
			}

			// Create new session with write lock
			if !tryLock(&processManager.sessionsMu, lockTimeout) {
				http.Error(w, "Failed to acquire session write lock", http.StatusServiceUnavailable)
				return
			}
			// Double check after acquiring write lock
			session, exists = processManager.sessions[sid]
			if !exists {
				// Create session with supergateway's ID
				session = &SessionInfo{
					Instance:  inst,
					LastUsed:  time.Now(),
					CreatedAt: time.Now(),
				}
				processManager.sessions[sid] = session
				inst.Sessions.Add(1)
			}
			processManager.sessionsMu.Unlock()
		}

		// Update session last used time
		processManager.TouchSession(sid)

		handlePost(w, r, session.Instance, rest)
		return
	}

	// other requests proxy to existing instance
	if !tryRLock(&processManager.instancesMu, lockTimeout) {
		http.Error(w, "Failed to acquire instance lock", http.StatusServiceUnavailable)
		return
	}
	inst := processManager.instances[cmdKey]
	processManager.instancesMu.RUnlock()

	if inst == nil {
		http.Error(w, "No gateway instance", http.StatusBadGateway)
		return
	}
	proxyGeneral(w, r, inst, rest)
}

// healthCheck attempts to connect to the spawned process
func healthCheck(url string, ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check failed: %w", ctx.Err())
		case <-ticker.C:
			resp, err := http.Get(url)
			if err == nil {
				resp.Body.Close()
				return nil // Any HTTP response means process is up
			}
		}
	}
}

// processOutputHandler manages the output streams of a process
type processOutputHandler struct {
	stdout    *bufio.Reader
	stderr    *bufio.Reader
	done      chan struct{}
	ctx       context.Context    // Context for cancellation
	cancel    context.CancelFunc // Cancel function for cleanup
	maxBuffer int                // Maximum buffer size for output channels
	wg        sync.WaitGroup     // WaitGroup for tracking goroutines
}

const (
	defaultMaxBuffer = 1000                   // Maximum number of lines in buffer
	maxLineLength    = 4096                   // Maximum length of a single line
	writeTimeout     = 100 * time.Millisecond // Timeout for writing to channels
	drainTimeout     = 5 * time.Second        // Timeout for draining channels during shutdown
)

func newProcessOutputHandler(stdout, stderr io.Reader) *processOutputHandler {
	ctx, cancel := context.WithCancel(context.Background())
	handler := &processOutputHandler{
		stdout:    bufio.NewReaderSize(stdout, maxLineLength),
		stderr:    bufio.NewReaderSize(stderr, maxLineLength),
		done:      make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		maxBuffer: defaultMaxBuffer,
	}
	go handler.handle()
	return handler
}

func (h *processOutputHandler) handle() {
	defer func() {
		h.cancel() // Ensure context is cancelled
		close(h.done)
	}()

	// Create buffered channels with dynamic size management
	stdoutCh := make(chan string, h.maxBuffer)
	stderrCh := make(chan string, h.maxBuffer)

	// Start goroutines to read from stdout and stderr
	h.wg.Add(2)
	go h.readOutput(h.stdout, stdoutCh, "stdout")
	go h.readOutput(h.stderr, stderrCh, "stderr")

	// Create a rate limiter for processing output
	limiter := rate.NewLimiter(rate.Every(100*time.Millisecond), 10)

	// Create cleanup context with timeout
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cleanupCancel()

	for {
		select {
		case <-h.ctx.Done():
			// Drain remaining messages before shutting down
			h.drainChannels(cleanupCtx, stdoutCh, stderrCh)
			h.wg.Wait() // Wait for reader goroutines to finish
			return
		case line, ok := <-stdoutCh:
			if !ok {
				continue
			}
			if err := limiter.Wait(h.ctx); err != nil {
				log.Error("Rate limit error: %v", err)
				continue
			}
			log.Debug("[Process stdout] %s", line)
		case line, ok := <-stderrCh:
			if !ok {
				continue
			}
			if err := limiter.Wait(h.ctx); err != nil {
				log.Error("Rate limit error: %v", err)
				continue
			}
			log.Debug("[Process stderr] %s", line)
		}
	}
}

func (h *processOutputHandler) drainChannels(ctx context.Context, stdoutCh, stderrCh chan string) {
	// Drain remaining messages with timeout
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-stdoutCh:
			if !ok {
				return
			}
			log.Debug("[Process stdout] (drain) %s", line)
		case line, ok := <-stderrCh:
			if !ok {
				return
			}
			log.Debug("[Process stderr] (drain) %s", line)
		default:
			// No more messages to drain
			return
		}
	}
}

func (h *processOutputHandler) readOutput(reader *bufio.Reader, ch chan<- string, name string) {
	defer func() {
		close(ch)
		h.wg.Done()
	}()

	for {
		select {
		case <-h.ctx.Done():
			return
		default:
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					log.Error("Error reading from %s: %v", name, err)
				}
				return
			}

			// Truncate if line is too long
			if len(line) > maxLineLength {
				line = line[:maxLineLength] + "... [truncated]"
			}

			// Try to write with timeout and backpressure handling
			select {
			case <-h.ctx.Done():
				return
			case ch <- strings.TrimSpace(line):
				// Successfully wrote to channel
			case <-time.After(writeTimeout):
				// Channel is full, log warning and continue
				log.Warn("Buffer full for %s, dropping line", name)
				// Implement exponential backoff
				time.Sleep(writeTimeout)
			}
		}
	}
}

func (h *processOutputHandler) Wait() {
	<-h.done
}

func (h *processOutputHandler) Stop() {
	h.cancel()
	<-h.done
}

// setPriority attempts to set process priority (nice value) on supported platforms
func setPriority(pid int) error {
	// On Unix-like systems, we can use setpriority
	return syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10)
}

// spawnInstance starts a supergateway for each SSE connection
func spawnInstance(cmdKey, cmdStr string, r *http.Request) (*GatewayInstance, error) {
	// Create a context that inherits from the request context
	ctx, cancel := context.WithCancel(r.Context())

	// collect env vars
	envs := os.Environ()
	for name, vals := range r.Header {
		up := strings.ToUpper(name)
		if strings.HasPrefix(up, "ENV_") {
			envKey := up[len("ENV_"):]
			// Skip blacklisted environment variables silently
			if blacklistedEnvVars[envKey] {
				continue
			}
			envs = append(envs, fmt.Sprintf("%s=%s", envKey, vals[0]))
		}
	}

	// Get a port from the port manager
	port, err := processManager.portManager.GetPort()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to acquire port: %w", err)
	}

	internalURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		processManager.portManager.ReleasePort(port)
		cancel()
		return nil, fmt.Errorf("failed to parse internal URL: %w", err)
	}

	// external baseUrl uses encoded cmdKey
	external := fmt.Sprintf("http://localhost:8000/%s", url.PathEscape(cmdKey))
	args := []string{
		"-y", "supergateway",
		"--stdio", cmdStr,
		"--port", strconv.Itoa(port),
		"--baseUrl", external,
		"--ssePath", "/sse",
		"--messagePath", "/message",
	}
	log.Info("Spawning [%s] on %d base %s", cmdKey, port, external)

	cmd := exec.CommandContext(ctx, "npx", args...)
	cmd.Env = envs

	// Set up process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Create new process group
		Setpgid: true,
	}

	// Set up pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		processManager.portManager.ReleasePort(port)
		cancel()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		processManager.portManager.ReleasePort(port)
		cancel()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start output handler
	outputHandler := newProcessOutputHandler(stdout, stderr)

	if err := cmd.Start(); err != nil {
		processManager.portManager.ReleasePort(port)
		cancel()
		outputHandler.Stop()
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	// Store process group ID for cleanup
	pgid := cmd.Process.Pid

	// Try to set process priority
	if err := setPriority(pgid); err != nil {
		log.Warn("Failed to set process priority: %v", err)
	}

	// Create health check context with timeout
	healthCtx, healthCancel := context.WithTimeout(ctx, timeoutConfig.HealthCheckPeriod)
	defer healthCancel()

	// Perform health check
	healthCheckURL := internalURL.String()
	if err := healthCheck(healthCheckURL, healthCtx); err != nil {
		// Kill entire process group
		syscall.Kill(-pgid, syscall.SIGKILL)
		processManager.portManager.ReleasePort(port)
		cancel()
		outputHandler.Stop()
		return nil, fmt.Errorf("process health check failed: %w", err)
	}

	inst := &GatewayInstance{
		Cmd:         cmd,
		InternalURL: internalURL,
		StartTime:   time.Now(),
		Cancel:      cancel,
	}
	inst.Sessions.Store(0) // Initialize atomic counter

	processManager.instancesMu.Lock()
	processManager.instances[cmdKey] = inst
	processManager.instancesMu.Unlock()

	// Start goroutine to clean up process resources when it exits
	go func() {
		defer func() {
			outputHandler.Stop()
			processManager.portManager.ReleasePort(port)
			processManager.instancesMu.Lock()
			delete(processManager.instances, cmdKey)
			processManager.instancesMu.Unlock()

			// Ensure entire process group is terminated
			if err := syscall.Kill(-pgid, syscall.SIGTERM); err == nil {
				// Wait for graceful shutdown
				time.Sleep(processManager.GracefulTimeout)
				// Force kill if still running
				syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}()

		outputHandler.Wait()
		if err := cmd.Wait(); err != nil {
			if ctx.Err() == context.Canceled {
				log.Info("Process [%s] was cancelled", cmdKey)
			} else {
				log.Error("Process [%s] exited with error: %v", cmdKey, err)
			}
		} else {
			log.Info("Process [%s] exited successfully", cmdKey)
		}
	}()

	return inst, nil
}

// proxySSE proxies SSE streams
func proxySSE(w http.ResponseWriter, r *http.Request, inst *GatewayInstance, rest string) {
	// Create a context with timeout for the SSE connection
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Create a done channel to handle connection close
	done := make(chan struct{})
	defer close(done)

	// Handle client disconnection
	go func() {
		select {
		case <-r.Context().Done():
			log.Debug("Client connection closed")
			select {
			case <-done:
				return
			default:
				cancel()
			}
		case <-done:
			return
		}
	}()

	sseURL := inst.InternalURL.String() + rest + "?" + r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		log.Error("Failed to create SSE request: %v", err)
		http.Error(w, "Failed to create SSE request", http.StatusInternalServerError)
		return
	}

	// Copy original headers
	req.Header = r.Header.Clone()

	// Set up response writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Keep-Alive", "timeout=86400") // 24 hours

	// Verify flusher support
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Error("Streaming not supported")
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Use a custom transport with longer timeouts
	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       10 * time.Minute,
			DisableKeepAlives:     false,
			MaxIdleConnsPerHost:   100,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		Timeout: 0, // No timeout for SSE connections
	}

	// Add retry logic for initial connection
	maxRetries := 3
	var resp *http.Response
	var connErr error

	for i := 0; i < maxRetries; i++ {
		resp, connErr = client.Do(req)
		if connErr == nil {
			break
		}
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
			log.Warn("Retrying SSE connection after error: %v (attempt %d/%d)", connErr, i+1, maxRetries)
		}
	}

	if connErr != nil {
		log.Error("SSE connection failed after %d attempts: %v", maxRetries, connErr)
		http.Error(w, "SSE connection failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy remaining headers from upstream
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	reader := bufio.NewReaderSize(resp.Body, 4096)
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				log.Info("SSE connection cancelled")
			} else {
				log.Info("SSE connection timeout: %v", ctx.Err())
			}
			return
		default:
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				if _, writeErr := w.Write(line); writeErr != nil {
					if !strings.Contains(writeErr.Error(), "broken pipe") && !strings.Contains(writeErr.Error(), "connection reset by peer") {
						log.Error("Failed to write SSE data: %v", writeErr)
					}
					return
				}
				flusher.Flush()
			}
			if err != nil {
				if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
					log.Error("Error reading SSE data: %v", err)
				}
				return
			}
		}
	}
}

// proxyGeneral proxies non-POST, non-SSE requests
func proxyGeneral(w http.ResponseWriter, r *http.Request, inst *GatewayInstance, rest string) {
	proxy := httputil.NewSingleHostReverseProxy(inst.InternalURL)
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		req.URL.Path = rest
		req.Host = inst.InternalURL.Host
		req.URL.RawQuery = r.URL.RawQuery
	}
	proxy.FlushInterval = 100 * time.Millisecond
	proxy.ServeHTTP(w, r)
}

// handlePost proxies /message POSTs
func handlePost(w http.ResponseWriter, r *http.Request, inst *GatewayInstance, rest string) {
	body, _ := io.ReadAll(r.Body)

	if log.GetLogLevel() <= logger.LogDebug {
		log.Debug("POST %s -> %s%s?%s",
			r.URL.Path,
			inst.InternalURL.String(),
			rest,
			r.URL.RawQuery)
		log.Debug("Request body: %s", string(body))
	} else {
		log.Info("Received POST request to %s", r.URL.Path)
	}

	target := inst.InternalURL.String() + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	// Create custom client with timeouts
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives:     false,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   100,
		},
	}

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	req.Header = r.Header.Clone()

	// Add retry logic
	maxRetries := 3
	var resp *http.Response
	var err error

	for i := 0; i < maxRetries; i++ {
		resp, err = client.Do(req)
		if err == nil {
			break
		}
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
			log.Warn("Retrying POST request after error: %v (attempt %d/%d)", err, i+1, maxRetries)
		}
	}

	if err != nil {
		log.Error("Post proxy failed after %d attempts: %v", maxRetries, err)
		http.Error(w, "Post proxy failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if log.GetLogLevel() <= logger.LogDebug {
		log.Debug("Response status: %d, body: %s", resp.StatusCode, string(respBody))
	} else {
		log.Info("Response status: %d", resp.StatusCode)
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// tryLock attempts to acquire a lock with timeout
func tryLock(mu sync.Locker, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		mu.Lock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// tryRLock attempts to acquire a read lock with timeout
func tryRLock(mu *sync.RWMutex, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		mu.RLock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
