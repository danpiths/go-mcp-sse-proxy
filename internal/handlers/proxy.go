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
	log            *logger.Logger
)

// SetupHandlers initializes the handlers with required dependencies
func SetupHandlers(pm *process.Manager, l *logger.Logger) {
	processManager = pm
	log = l
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

	cmdStr, allowed := config.AllowedCommands[cmdKey]
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
		processManager.TerminateInstance(cmdKey, inst)
		return
	}

	// message POSTs
	if r.Method == http.MethodPost && rest == "/message" {
		sid := r.URL.Query().Get("sessionId")
		if sid == "" {
			http.Error(w, "Missing sessionId", http.StatusBadRequest)
			return
		}

		session := processManager.GetSession(sid)
		if session == nil {
			// Get instance
			inst := processManager.GetInstance(cmdKey)
			if inst == nil {
				http.Error(w, "No gateway instance", http.StatusBadGateway)
				return
			}

			// Create new session with supergateway's ID
			if err := processManager.CreateSession(sid, inst); err != nil {
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
		http.Error(w, "No gateway instance", http.StatusBadGateway)
		return
	}
	proxyGeneral(w, r, inst, rest)
}

// spawnInstance starts a supergateway for each SSE connection
func spawnInstance(cmdKey, cmdStr string, r *http.Request) (*models.GatewayInstance, error) {
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
		log.Warn("Failed to set process priority: %v", err)
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
func proxySSE(w http.ResponseWriter, r *http.Request, inst *models.GatewayInstance, rest string) {
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
func proxyGeneral(w http.ResponseWriter, r *http.Request, inst *models.GatewayInstance, rest string) {
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
