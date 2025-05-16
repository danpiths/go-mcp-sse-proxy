package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GatewayInstance holds a running supergateway process
// and the URL where it's serving.
type GatewayInstance struct {
	Cmd     *exec.Cmd
	BaseURL *url.URL
}

var (
	// allowedCommands: unescaped -> actual invocation
	allowedCommands = map[string]string{
		"npx -y @upstash/context7-mcp@latest": "npx -y @upstash/context7-mcp@latest",
		"npx -y @maximai/mcp-server@latest":   "npx -y @maximai/mcp-server@latest",
	}
	mu        sync.Mutex
	instances = make(map[string]*GatewayInstance)
)

func main() {
	http.HandleFunc("/", multiProxy)
	addr := ":8000"
	log.Printf("Starting server on %s...", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// multiProxy routes both prefixed commands and unprefixed endpoints
// to the corresponding Supergateway instance, with special handling
// for POST to log and forward message bodies.
func multiProxy(w http.ResponseWriter, r *http.Request) {
	// Determine raw path (excluding query)
	rawPath := r.URL.RawPath
	if rawPath == "" {
		rawPath = r.RequestURI
		if i := strings.Index(rawPath, "?"); i != -1 {
			rawPath = rawPath[:i]
		}
	}
	rawPath = strings.TrimPrefix(rawPath, "/")

	// Find or spawn the gateway instance
	inst, rest := instanceForPath(r, rawPath)
	if inst == nil {
		http.Error(w, "Command not allowed or no gateway found", http.StatusForbidden)
		return
	}

	// If this is a POST (message) request, handle manually for logging and query forwarding
	if r.Method == http.MethodPost {
		handlePost(w, r, inst, rest)
		return
	}

	// Otherwise, proxy everything else (e.g., SSE GETs)
	proxy := httputil.NewSingleHostReverseProxy(inst.BaseURL)
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		req.URL.Path = rest
		req.Host = inst.BaseURL.Host
		req.URL.RawQuery = r.URL.RawQuery
	}
	proxy.FlushInterval = 100 * time.Millisecond
	proxy.ServeHTTP(w, r)
}

// instanceForPath returns the appropriate GatewayInstance and the downstream path
func instanceForPath(r *http.Request, rawPath string) (*GatewayInstance, string) {
	// Try prefix mode: /{cmd}/rest
	parts := strings.SplitN(rawPath, "/", 2)
	if len(parts) >= 1 {
		if keyUnescaped, err := url.PathUnescape(parts[0]); err == nil {
			if cmdStr, ok := allowedCommands[keyUnescaped]; ok {
				rest := "/"
				if len(parts) == 2 {
					rest += parts[1]
				}
				return getOrCreateInstance(keyUnescaped, cmdStr, r), rest
			}
		}
	}
	// Fallback: if only one instance exists, use it
	mu.Lock()
	count := len(instances)
	var inst *GatewayInstance
	if count == 1 {
		for _, one := range instances {
			inst = one
			break
		}
	}
	mu.Unlock()
	if inst != nil {
		return inst, "/" + rawPath
	}
	return nil, ""
}

// handlePost logs request and response bodies for POST proxying,
// and ensures original query parameters (e.g., sessionId) are forwarded
func handlePost(w http.ResponseWriter, r *http.Request, inst *GatewayInstance, rest string) {
	// Read body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	log.Printf("Proxying POST to %s%s?%s; Body: %s", inst.BaseURL, rest, r.URL.RawQuery, string(bodyBytes))

	// Build target URL with original query string
	target := inst.BaseURL.String() + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	// Create downstream request
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, "Failed to create proxy request", http.StatusInternalServerError)
		return
	}
	// Copy headers (including Content-Type, etc.)
	req.Header = r.Header.Clone()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Failed to connect to internal server", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("Upstream POST response %d; Body: %s", resp.StatusCode, string(respBody))

	// Mirror headers
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// getOrCreateInstance spawns a Supergateway subprocess once per key
func getOrCreateInstance(key, cmdStr string, r *http.Request) *GatewayInstance {
	mu.Lock()
	if inst, exists := instances[key]; exists {
		mu.Unlock()
		return inst
	}
	mu.Unlock()

	// Collect ENV_ headers
	envs := os.Environ()
	for name, vals := range r.Header {
		up := strings.ToUpper(name)
		if strings.HasPrefix(up, "ENV_") {
			k := up[len("ENV_"):]
			envs = append(envs, fmt.Sprintf("%s=%s", k, vals[0]))
		}
	}

	// Pick free port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("Port allocation failed: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	u, _ := url.Parse(base)

	// Start Supergateway
	args := []string{"-y", "supergateway", "--stdio", cmdStr, "--port", strconv.Itoa(port), "--baseUrl", base, "--ssePath", "/sse", "--messagePath", "/message"}
	log.Printf("Launching supergateway for '%s' on port %d", key, port)
	cmd := exec.CommandContext(context.Background(), "npx", args...)
	cmd.Env = envs
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("Subprocess start error: %v", err)
	}

	// Give time to bind
	time.Sleep(3 * time.Second)

	inst := &GatewayInstance{Cmd: cmd, BaseURL: u}
	mu.Lock()
	instances[key] = inst
	mu.Unlock()
	return inst
}
