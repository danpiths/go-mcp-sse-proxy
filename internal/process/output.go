package process

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"go-mcp-sse-proxy/pkg/logger"
)

// processOutputHandler manages the output streams of a process
type processOutputHandler struct {
	stdout    *bufio.Reader
	stderr    *bufio.Reader
	done      chan struct{}
	ctx       context.Context    // Context for cancellation
	cancel    context.CancelFunc // Cancel function for cleanup
	maxBuffer int                // Maximum buffer size for output channels
	wg        sync.WaitGroup     // WaitGroup for tracking goroutines

	// Buffer for accumulating JSON lines
	jsonBuffer   strings.Builder
	inJSONObject bool
	bracketCount int
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

func (h *processOutputHandler) processLine(line string) {
	// Check if line starts a new JSON object
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") {
		h.inJSONObject = true
		h.jsonBuffer.Reset()
		h.bracketCount = 0
	}

	if h.inJSONObject {
		h.jsonBuffer.WriteString(trimmed)

		// Count brackets
		h.bracketCount += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")

		// If brackets are balanced, we have a complete JSON object
		if h.bracketCount == 0 {
			h.inJSONObject = false
			jsonStr := h.jsonBuffer.String()

			// Try to parse and pretty print the JSON
			var jsonData map[string]interface{}
			if err := json.Unmarshal([]byte(jsonStr), &jsonData); err == nil {
				// Extract meaningful information from the JSON
				if method, ok := jsonData["method"].(string); ok {
					logger.Log.Debug("Process JSON message",
						"method", method,
						"data", jsonData)
				} else if errObj, ok := jsonData["error"].(map[string]interface{}); ok {
					logger.Log.Error("Process error",
						"code", errObj["code"],
						"message", errObj["message"])
				} else {
					logger.Log.Debug("Process output",
						"json", jsonData)
				}
			} else {
				// If not valid JSON, log the raw string
				logger.Log.Debug("Process output",
					"raw", jsonStr)
			}
		}
	} else {
		// Handle non-JSON lines
		if strings.Contains(trimmed, "SSE ↔ Child") {
			logger.Log.Debug("SSE connection event",
				"event", trimmed)
		} else if strings.Contains(trimmed, "POST to SSE transport") {
			logger.Log.Debug("SSE transport event",
				"event", trimmed)
		} else if len(trimmed) > 0 {
			logger.Log.Debug("Process output",
				"message", trimmed)
		}
	}
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
			h.processLine(line)
		case line, ok := <-stderrCh:
			if !ok {
				continue
			}
			h.processLine(line)
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
			h.processLine(line)
		case line, ok := <-stderrCh:
			if !ok {
				return
			}
			h.processLine(line)
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
					logger.Log.Error("Error reading from stream",
						"stream", name,
						"err", err)
				}
				return
			}

			// Try to write with timeout and backpressure handling
			select {
			case <-h.ctx.Done():
				return
			case ch <- line:
				// Successfully wrote to channel
			case <-time.After(writeTimeout):
				// Channel is full, log warning and continue
				logger.Log.Warn("Buffer full, dropping line",
					"stream", name)
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

// NewProcessOutputHandler creates a new process output handler
func NewProcessOutputHandler(stdout, stderr io.Reader) *processOutputHandler {
	return newProcessOutputHandler(stdout, stderr)
}

// SetPriority attempts to set process priority (nice value) on supported platforms
func SetPriority(pid int) error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10)
}

// HealthCheck attempts to connect to the spawned process
func HealthCheck(url string, ctx context.Context) error {
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
