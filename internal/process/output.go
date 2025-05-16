package process

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"syscall"
	"time"

	"go-mcp-sse-proxy/pkg/logger"
)

var log *logger.Logger

func init() {
	// Initialize with default configuration
	log = logger.NewLogger(&logger.LoggerConfig{
		Level:      logger.LogDebug,
		UseConsole: true,
	})
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
	logger    *logger.Logger     // Logger instance
}

const (
	defaultMaxBuffer = 1000                   // Maximum number of lines in buffer
	maxLineLength    = 4096                   // Maximum length of a single line
	writeTimeout     = 100 * time.Millisecond // Timeout for writing to channels
	drainTimeout     = 5 * time.Second        // Timeout for draining channels during shutdown
)

func newProcessOutputHandler(stdout, stderr io.Reader, l *logger.Logger) *processOutputHandler {
	ctx, cancel := context.WithCancel(context.Background())
	handler := &processOutputHandler{
		stdout:    bufio.NewReaderSize(stdout, maxLineLength),
		stderr:    bufio.NewReaderSize(stderr, maxLineLength),
		done:      make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		maxBuffer: defaultMaxBuffer,
		logger:    l,
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
			h.logger.Debug("[Process stdout] %s", line)
		case line, ok := <-stderrCh:
			if !ok {
				continue
			}
			h.logger.Debug("[Process stderr] %s", line)
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
			h.logger.Debug("[Process stdout] (drain) %s", line)
		case line, ok := <-stderrCh:
			if !ok {
				return
			}
			h.logger.Debug("[Process stderr] (drain) %s", line)
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
					h.logger.Error("Error reading from %s: %v", name, err)
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
				h.logger.Warn("Buffer full for %s, dropping line", name)
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
	return newProcessOutputHandler(stdout, stderr, log)
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
