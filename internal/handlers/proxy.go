package handlers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go-mcp-sse-proxy/internal/config"
	"go-mcp-sse-proxy/internal/models"
	"go-mcp-sse-proxy/internal/process"
	"go-mcp-sse-proxy/pkg/logger"
)

var (
	processManager *process.Manager
	log            = logger.Log
)

// SetupHandlers initializes the handlers with required dependencies
func SetupHandlers(pm *process.Manager) {
	processManager = pm
}

// MultiProxy routes SSE and message requests, spawning per-SSE instances
func MultiProxy(w http.ResponseWriter, r *http.Request) {
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
		log.Error("Invalid request path", "path", raw)
		http.NotFound(w, r)
		return
	}

	// decode command key
	prefix := parts[0]
	cmdKey, err := url.PathUnescape(prefix)
	if err != nil {
		log.Error("Failed to decode prefix", "prefix", prefix, "err", err)
		http.Error(w, "Bad prefix encoding", http.StatusBadRequest)
		return
	}

	cmdStr, allowed := config.AllowedCommands[cmdKey]
	if !allowed {
		log.Warn("Command not allowed", "cmd", cmdKey)
		http.Error(w, "Command not allowed", http.StatusForbidden)
		return
	}

	rest := "/"
	if len(parts) == 2 {
		rest += parts[1]
	}

	// SSE connect: spawn new instance
	if r.Method == http.MethodGet && rest == "/sse" {
		log.Info("New SSE connection",
			"cmd", cmdKey,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent())

		inst, err := spawnInstance(cmdKey, cmdStr, r)
		if err != nil {
			log.Error("Failed to spawn instance",
				"cmd", cmdKey,
				"err", err,
				"remote_addr", r.RemoteAddr)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		// Create a done channel to coordinate cleanup
		done := make(chan struct{})

		// Start cleanup goroutine that waits for SSE to actually end
		go func() {
			defer close(done)
			// proxy SSE and let supergateway handle session creation
			proxySSE(w, r, inst, rest)
			// Only cleanup after SSE connection actually ends
			processManager.TerminateInstance(cmdKey, inst)
		}()

		// Wait for cleanup to complete
		<-done
		return
	}

	// message POSTs
	if r.Method == http.MethodPost && rest == "/message" {
		sid := r.URL.Query().Get("sessionId")
		if sid == "" {
			log.Error("Missing sessionId in POST request",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path)
			http.Error(w, "Missing sessionId", http.StatusBadRequest)
			return
		}

		session := processManager.GetSession(sid)
		if session == nil {
			// Get instance
			inst := processManager.GetInstance(cmdKey)
			if inst == nil {
				log.Error("No gateway instance found",
					"cmd", cmdKey,
					"session_id", sid)
				http.Error(w, "No gateway instance", http.StatusBadGateway)
				return
			}

			// Create new session with supergateway's ID
			if err := processManager.CreateSession(sid, inst); err != nil {
				log.Error("Failed to create session",
					"session_id", sid,
					"err", err)
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			session = processManager.GetSession(sid)
		}

		// Update session last used time
		processManager.TouchSession(sid)

		handlePost(w, r, session.Instance, rest)
		return
	}

	// other requests proxy to existing instance
	inst := processManager.GetInstance(cmdKey)
	if inst == nil {
		log.Error("No gateway instance for request",
			"cmd", cmdKey,
			"method", r.Method,
			"path", r.URL.Path)
		http.Error(w, "No gateway instance", http.StatusBadGateway)
		return
	}
	proxyGeneral(w, r, inst, rest)
}

// spawnInstance starts a supergateway for each SSE connection
func spawnInstance(cmdKey, cmdStr string, r *http.Request) (*models.GatewayInstance, error) {
	log.Helper()
	// Create a context that inherits from the request context
	ctx, cancel := context.WithCancel(r.Context())

	// collect env vars
	envs := os.Environ()
	for name, vals := range r.Header {
		up := strings.ToUpper(name)
		if strings.HasPrefix(up, "ENV_") {
			envKey := up[len("ENV_"):]
			// Skip blacklisted environment variables silently
			if config.BlacklistedEnvVars[envKey] {
				continue
			}
			envs = append(envs, fmt.Sprintf("%s=%s", envKey, vals[0]))
		}
	}

	// Get a port from the port manager
	port, err := processManager.GetPort()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to acquire port: %w", err)
	}

	internalURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		processManager.ReleasePort(port)
		cancel()
		return nil, fmt.Errorf("failed to parse internal URL: %w", err)
	}

	// external baseUrl uses encoded cmdKey
	external := fmt.Sprintf("http://localhost:8000/%s", url.PathEscape(cmdKey))
	args := []string{
		"--stdio", cmdStr,
		"--port", strconv.Itoa(port),
		"--baseUrl", external,
		"--ssePath", "/sse",
		"--messagePath", "/message",
	}
	log.Info("Spawning instance",
		"cmd", cmdKey,
		"port", port,
		"url", external)

	cmd := exec.CommandContext(ctx, "supergateway", args...)
	cmd.Env = envs

	// Set up process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Create new process group
		Setpgid: true,
	}

	// Set up pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		processManager.ReleasePort(port)
		cancel()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		processManager.ReleasePort(port)
		cancel()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start output handler
	outputHandler := process.NewProcessOutputHandler(stdout, stderr)

	if err := cmd.Start(); err != nil {
		processManager.ReleasePort(port)
		cancel()
		outputHandler.Stop()
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	// Store process group ID for cleanup
	pgid := cmd.Process.Pid

	// Try to set process priority
	if err := process.SetPriority(pgid); err != nil {
		log.Warn("Failed to set process priority",
			"pid", pgid,
			"err", err)
	}

	// Create health check context with timeout
	healthCtx, healthCancel := context.WithTimeout(ctx, 30*time.Second)
	defer healthCancel()

	// Perform health check
	healthCheckURL := internalURL.String()
	if err := process.HealthCheck(healthCheckURL, healthCtx); err != nil {
		// Kill entire process group
		syscall.Kill(-pgid, syscall.SIGKILL)
		processManager.ReleasePort(port)
		cancel()
		outputHandler.Stop()
		return nil, fmt.Errorf("process health check failed: %w", err)
	}

	inst := models.NewGatewayInstance(cmd, internalURL, cancel)
	processManager.AddInstance(cmdKey, inst)

	// Start goroutine to clean up process resources when it exits
	go func() {
		defer func() {
			outputHandler.Stop()
			processManager.ReleasePort(port)
			processManager.RemoveInstance(cmdKey)

			// Ensure entire process group is terminated
			if err := syscall.Kill(-pgid, syscall.SIGTERM); err == nil {
				// Wait for graceful shutdown
				time.Sleep(5 * time.Second)
				// Force kill if still running
				syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}()

		outputHandler.Wait()
		if err := cmd.Wait(); err != nil {
			if ctx.Err() == context.Canceled {
				log.Info("Process cancelled", "cmd", cmdKey)
			} else {
				log.Error("Process exited with error",
					"cmd", cmdKey,
					"err", err,
					"pid", pgid)
			}
		} else {
			log.Info("Process exited successfully",
				"cmd", cmdKey,
				"pid", pgid)
		}
	}()

	return inst, nil
}

// proxySSE proxies SSE streams
func proxySSE(w http.ResponseWriter, r *http.Request, inst *models.GatewayInstance, rest string) {
	log.Helper()
	// Create a context without timeout for the SSE connection
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a done channel to handle connection close
	done := make(chan struct{})
	defer close(done)

	// Handle client disconnection
	go func() {
		<-r.Context().Done()
		log.Debug("Client connection closed",
			"remote_addr", r.RemoteAddr)
		cancel()
	}()

	sseURL := inst.InternalURL.String() + rest + "?" + r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		log.Error("Failed to create SSE request",
			"url", sseURL,
			"err", err)
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

	// Use a custom transport with optimized settings for SSE
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableKeepAlives:     false,
		MaxIdleConnsPerHost:   100,
	}

	// Enable TCP keep-alive
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(30 * time.Second)
			tc.SetNoDelay(true)
		}
		return conn, nil
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // No timeout for SSE connections
	}

	// Add retry logic for initial connection
	maxRetries := 3
	var resp *http.Response
	var connErr error

	for i := range maxRetries {
		resp, connErr = client.Do(req)
		if connErr == nil {
			break
		}
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
			log.Warn("Retrying SSE connection",
				"attempt", i+1,
				"max_attempts", maxRetries,
				"err", connErr)
		}
	}

	if connErr != nil {
		log.Error("SSE connection failed",
			"max_attempts", maxRetries,
			"err", connErr)
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
	heartbeat := time.NewTicker(15 * time.Second) // More frequent heartbeat
	defer heartbeat.Stop()

	// Create error channel for connection errors
	errCh := make(chan error, 1)

	// Start reader goroutine
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Reader goroutine panic",
					"panic", r)
				errCh <- fmt.Errorf("reader panic: %v", r)
			}
		}()

		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				select {
				case <-ctx.Done():
					return
				default:
					// Set a write deadline for each write operation
					if conn, ok := w.(interface{ SetWriteDeadline(time.Time) error }); ok {
						conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
					}

					if _, writeErr := w.Write(line); writeErr != nil {
						if !isConnectionError(writeErr) {
							log.Error("Failed to write SSE data",
								"err", writeErr)
							errCh <- writeErr
						}
						return
					}
					flusher.Flush()
				}
			}
			if err != nil {
				if !isConnectionError(err) {
					log.Error("Error reading SSE data",
						"err", err)
					errCh <- err
				}
				return
			}
		}
	}()

	// Main event loop
	for {
		select {
		case <-ctx.Done():
			log.Info("SSE connection cancelled")
			return
		case err := <-errCh:
			log.Error("SSE connection error",
				"err", err)
			return
		case <-heartbeat.C:
			select {
			case <-ctx.Done():
				return
			default:
				// Set a write deadline for heartbeat
				if conn, ok := w.(interface{ SetWriteDeadline(time.Time) error }); ok {
					conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				}

				if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
					if !isConnectionError(err) {
						log.Error("Failed to write heartbeat",
							"err", err)
					}
					return
				}
				flusher.Flush()
			}
		}
	}
}

// isConnectionError returns true if the error is a normal connection closure or timeout
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "i/o timeout") ||
		err == io.EOF
}

// proxyGeneral proxies non-POST, non-SSE requests
func proxyGeneral(w http.ResponseWriter, r *http.Request, inst *models.GatewayInstance, rest string) {
	log.Helper()
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
func handlePost(w http.ResponseWriter, r *http.Request, inst *models.GatewayInstance, rest string) {
	log.Helper()
	body, _ := io.ReadAll(r.Body)

	if log.GetLevel() <= logger.Log.GetLevel() {
		log.Debug("POST request details",
			"path", r.URL.Path,
			"target", fmt.Sprintf("%s%s?%s",
				inst.InternalURL.String(),
				rest,
				r.URL.RawQuery),
			"body", string(body))
	} else {
		log.Info("Received POST request",
			"path", r.URL.Path)
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

	// Create a new request context that doesn't affect the SSE connection
	postCtx, postCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer postCancel()

	req, _ := http.NewRequestWithContext(postCtx, http.MethodPost, target, bytes.NewReader(body))
	req.Header = r.Header.Clone()

	// Add retry logic
	maxRetries := 3
	var resp *http.Response
	var err error

	for i := range maxRetries {
		resp, err = client.Do(req)
		if err == nil {
			break
		}
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
			log.Warn("Retrying POST request",
				"attempt", i+1,
				"max_attempts", maxRetries,
				"err", err)
		}
	}

	if err != nil {
		log.Error("Post proxy failed",
			"max_attempts", maxRetries,
			"err", err)
		http.Error(w, "Post proxy failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if log.GetLevel() <= logger.Log.GetLevel() {
		log.Debug("Response details",
			"status", resp.StatusCode,
			"body", string(respBody))
	} else {
		log.Info("Response received",
			"status", resp.StatusCode)
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}
