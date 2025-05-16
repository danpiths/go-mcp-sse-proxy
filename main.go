package main

import (
	"bufio"
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

// GatewayInstance holds a supergateway subprocess bound to an internal URL.
type GatewayInstance struct {
	Cmd         *exec.Cmd
	InternalURL *url.URL
}

var (
	// allowedCommands: unescaped -> actual command invocation
	allowedCommands = map[string]string{
		"npx -y @upstash/context7-mcp@latest": "npx -y @upstash/context7-mcp@latest",
		"npx -y @maximai/mcp-server@latest":   "npx -y @maximai/mcp-server@latest",
	}

	// prefixMap tracks active instance per command key
	prefixMap = make(map[string]*GatewayInstance)
	// sessionMap tracks session to instance mapping
	sessionMap = make(map[string]*GatewayInstance)
	mu         sync.Mutex
)

func main() {
	http.HandleFunc("/", multiProxy)
	addr := ":8000"
	log.Printf("Starting proxy on %s...", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// multiProxy routes SSE and message requests, spawning per-SSE instances
func multiProxy(w http.ResponseWriter, r *http.Request) {
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

	// decode command key and rest of path
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
		inst := spawnInstance(cmdKey, cmdStr, r)
		mu.Lock()
		prefixMap[cmdKey] = inst
		mu.Unlock()

		// proxy SSE
		proxySSE(w, r, inst, rest)

		// cleanup on disconnect
		inst.Cmd.Process.Kill()
		mu.Lock()
		delete(prefixMap, cmdKey)
		for sid, mp := range sessionMap {
			if mp == inst {
				delete(sessionMap, sid)
			}
		}
		mu.Unlock()
		return
	}

	// message POSTs
	if r.Method == http.MethodPost && rest == "/message" {
		sid := r.URL.Query().Get("sessionId")
		mu.Lock()
		inst := sessionMap[sid]
		mu.Unlock()
		if inst == nil {
			// first POST: map session to prefix instance
			mu.Lock()
			inst = prefixMap[cmdKey]
			sessionMap[sid] = inst
			mu.Unlock()
		}
		if inst == nil {
			http.Error(w, "No active session", http.StatusForbidden)
			return
		}
		handlePost(w, r, inst, rest)
		return
	}

	// other requests proxy to existing instance
	mu.Lock()
	inst := prefixMap[cmdKey]
	mu.Unlock()
	if inst == nil {
		http.Error(w, "No gateway instance", http.StatusBadGateway)
		return
	}
	proxyGeneral(w, r, inst, rest)
}

// spawnInstance starts a supergateway for each SSE connection
func spawnInstance(cmdKey, cmdStr string, r *http.Request) *GatewayInstance {
	// collect env vars
	envs := os.Environ()
	for name, vals := range r.Header {
		up := strings.ToUpper(name)
		if strings.HasPrefix(up, "ENV_") {
			envKey := up[len("ENV_"):]
			envs = append(envs, fmt.Sprintf("%s=%s", envKey, vals[0]))
		}
	}

	// pick free port
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	internalURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
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
	log.Printf("Spawning [%s] on %d base %s", cmdKey, port, external)
	cmd := exec.CommandContext(context.Background(), "npx", args...)
	cmd.Env = envs
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Start()
	// allow startup
	time.Sleep(2 * time.Second)
	return &GatewayInstance{Cmd: cmd, InternalURL: internalURL}
}

// proxySSE proxies SSE streams
func proxySSE(w http.ResponseWriter, r *http.Request, inst *GatewayInstance, rest string) {
	sseURL := inst.InternalURL.String() + rest + "?" + r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, sseURL, nil)
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "SSE proxy failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	reader := bufio.NewReader(resp.Body)
	flusher, _ := w.(http.Flusher)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			w.Write(line)
			flusher.Flush()
		}
		if err != nil {
			break
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
	log.Printf("POST %s -> %s%s?%s body: %s", r.URL.Path, inst.InternalURL, rest, r.URL.RawQuery, string(body))
	target := inst.InternalURL.String() + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Post proxy failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("POST response %d body: %s", resp.StatusCode, string(respBody))
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}
