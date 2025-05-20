package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go-mcp-sse-proxy/internal/config"
	"go-mcp-sse-proxy/internal/handlers"
	"go-mcp-sse-proxy/internal/process"
	"go-mcp-sse-proxy/internal/ratelimit"
	"go-mcp-sse-proxy/pkg/logger"

	"github.com/charmbracelet/log"
	"golang.org/x/time/rate"
)

func main() {
	// Configure global logger
	configureLogger()

	// Initialize process manager and rate limiter
	processManager := process.NewManager(config.DefaultProcessConfig())
	rateLimiter := ratelimit.NewRateLimiter(rate.Limit(100.0/60.0), 10)

	// Setup handlers with dependencies
	handlers.SetupHandlers(processManager)

	// Create server with middleware chain
	server := setupServer(rateLimiter)

	// Setup signal handling for graceful shutdown
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	// Start server
	go func() {
		logger.Log.Info("Starting proxy",
			"address", server.Addr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			logger.Log.Error("HTTP server error",
				"err", err)
		}
	}()

	// Wait for shutdown signal
	<-shutdown
	logger.Log.Info("Shutdown signal received")

	// Graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Log.Error("HTTP server shutdown error",
			"err", err)
	}

	processManager.Shutdown()
	logger.Log.Info("Shutdown complete")
}

func configureLogger() {
	// Set log level from environment variable
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		switch strings.ToUpper(lvl) {
		case "DEBUG":
			logger.SetLogLevel(log.DebugLevel)
		case "INFO":
			logger.SetLogLevel(log.InfoLevel)
		case "WARN":
			logger.SetLogLevel(log.WarnLevel)
		case "ERROR":
			logger.SetLogLevel(log.ErrorLevel)
		}
	}
}

func setupServer(rateLimiter *ratelimit.RateLimiter) *http.Server {
	// Create handler chain with security middleware
	handler := handlers.RateLimitMiddleware(rateLimiter)(
		handlers.SecurityHeaders(
			http.HandlerFunc(handlers.MultiProxy),
		),
	)

	return &http.Server{
		Addr:         ":8000",
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}
