package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go-mcp-sse-proxy/internal/config"
	"go-mcp-sse-proxy/internal/handlers"
	"go-mcp-sse-proxy/internal/process"
	"go-mcp-sse-proxy/internal/ratelimit"
	"go-mcp-sse-proxy/pkg/logger"

	"golang.org/x/time/rate"
)

func main() {
	// Initialize logger with configuration
	log := initLogger()
	defer log.Close()

	// Initialize process manager and rate limiter
	processManager := process.NewManager(config.DefaultProcessConfig())
	rateLimiter := ratelimit.NewRateLimiter(rate.Limit(100.0/60.0), 10)

	// Setup handlers with dependencies
	handlers.SetupHandlers(processManager, log)

	// Create server with middleware chain
	server := setupServer(rateLimiter)

	// Setup signal handling for graceful shutdown
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	// Start server
	go func() {
		log.Info("Starting proxy on %s...", server.Addr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Error("HTTP server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-shutdown
	log.Info("Shutdown signal received")

	// Graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error: %v", err)
	}

	processManager.Shutdown()
	log.Info("Shutdown complete")
}

func initLogger() *logger.Logger {
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

	return logger.NewLogger(logConfig)
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
