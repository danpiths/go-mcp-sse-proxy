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

// GatewayInstance represents a Supergateway subprocess
// bound to an internal port and proxied under a specific URL prefix.
type GatewayInstance struct {
	Cmd         *exec.Cmd
	InternalURL *url.URL
}

var (
	// allowedCommands maps unescaped stdio command strings to their invocation
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
	log.Printf("Starting proxy on %s...", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// multiProxy handles all incoming requests by routing them
// to the corresponding GatewayInstance based on URL prefix.
func multiProxy(w http.ResponseWriter, r *http.Request) {
	// Preserve raw path (without query)
	raw := r.URL.RawPath
	if raw == "" {
		raw = r.RequestURI
		if i := strings.Index(raw, "?"); i != -1 {
			raw = raw[:i]
		}
	}
	raw = strings.TrimPrefix(raw, "/")

	inst, rest := instanceForPath(r, raw)
	if inst == nil {
		http.Error(w, "Command not allowed or no gateway found", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodPost {
		handlePost(w, r, inst, rest)
		return
	}

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

// instanceForPath determines which instance to use based on URL prefix,
// spinning up a new one if needed, and returns the rest-of-path.
func instanceForPath(r *http.Request, raw string) (*GatewayInstance, string) {
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) >= 1 {
		// Decode the command key
		if key, err := url.PathUnescape(parts[0]); err == nil {
			if cmdStr, ok := allowedCommands[key]; ok {
				rest := "/"
				if len(parts) == 2 {
					rest += parts[1]
				}
				inst := getOrCreateInstance(key, cmdStr, r)
				return inst, rest
			}
		}
	}
	return nil, ""
}

// handlePost proxies POST requests (e.g., /message), preserving query params and logging bodies.
func handlePost(w http.ResponseWriter, r *http.Request, inst *GatewayInstance, rest string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	log.Printf("POST %s%s?%s -> %s%s; Body: %s", r.Host, r.URL.Path, r.URL.RawQuery, inst.InternalURL, rest, string(body))

	target := inst.InternalURL.String() + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "Failed to create downstream request", http.StatusInternalServerError)
		return
	}
	req.Header = r.Header.Clone()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Downstream POST failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("Response %d from downstream POST; Body: %s", resp.StatusCode, string(respBody))

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// getOrCreateInstance starts a new Supergateway subprocess for a given key if not present.
// Now accepts the HTTP request to extract ENV_ headers for each instance separately.
func getOrCreateInstance(key, cmdStr string, r *http.Request) *GatewayInstance {
	mu.Lock()
	if inst, exists := instances[key]; exists {
		mu.Unlock()
		return inst
	}
	mu.Unlock()

	// Collect ENV_ headers from this request
	envs := os.Environ()
	for name, vals := range r.Header {
		up := strings.ToUpper(name)
		if strings.HasPrefix(up, "ENV_") {
			k := up[len("ENV_"):]
			envs = append(envs, fmt.Sprintf("%s=%s", k, vals[0]))
			log.Printf("Setting env %s=%s for instance %s", k, vals[0], key)
		}
	}

	// Allocate an internal port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("Port allocation failed: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	internal := fmt.Sprintf("http://127.0.0.1:%d", port)
	internalURL, _ := url.Parse(internal)

	// External baseUrl so client always uses the prefix path
	externalBase := fmt.Sprintf("http://localhost:8000/%s", url.PathEscape(key))

	args := []string{
		"-y", "supergateway",
		"--stdio", cmdStr,
		"--port", strconv.Itoa(port),
		"--baseUrl", externalBase,
		"--ssePath", "/sse",
		"--messagePath", "/message",
	}
	log.Printf("Launching supergateway '%s' on port %d with baseUrl %s", key, port, externalBase)
	cmd := exec.CommandContext(context.Background(), "npx", args...)
	cmd.Env = envs
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start supergateway: %v", err)
	}

	// Allow binding
	time.Sleep(3 * time.Second)

	inst := &GatewayInstance{Cmd: cmd, InternalURL: internalURL}
	mu.Lock()
	instances[key] = inst
	mu.Unlock()
	return inst
}
