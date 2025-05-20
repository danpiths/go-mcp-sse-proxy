package handlers

import (
	"net/http"
	"os"
	"strings"

	"go-mcp-sse-proxy/internal/ratelimit"
)

// AuthMiddleware ensures all requests have valid bearer token
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health checks and OPTIONS requests
		if r.URL.Path == "/health" || r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}

		// Get the Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization header required", http.StatusUnauthorized)
			return
		}

		// Check if it's a Bearer token
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			http.Error(w, "Invalid authorization format", http.StatusUnauthorized)
			return
		}

		// Get the secret from environment
		secret := os.Getenv("MAXIM_SECRET")
		if secret == "" {
			http.Error(w, "Server configuration error", http.StatusInternalServerError)
			return
		}

		// Validate the token
		if parts[1] != secret {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders adds security-related headers to all responses
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health check endpoint
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// HSTS header (only in production)
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		// Security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Content Security Policy - Allow EventSource connections
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' *")

		// CORS headers - More permissive for SSE but still secure
		if origin := r.Header.Get("Origin"); origin != "" {
			// Allow the specific origin that made the request
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			// Fallback for non-browser clients
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}

		// Allow methods needed for SSE and message posting
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		// Allow headers commonly used with SSE and required for your API
		w.Header().Set("Access-Control-Allow-Headers", strings.Join([]string{
			"Content-Type",
			"Authorization",
			"Cache-Control",
			"Last-Event-ID",
			"ENV_*",
			"X-Requested-With",
		}, ", "))

		// Increase max age for better performance
		w.Header().Set("Access-Control-Max-Age", "86400") // 24 hours

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			// Ensure the response includes the allowed headers
			w.Header().Set("Access-Control-Expose-Headers", "Content-Type, Last-Event-ID")
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// RateLimitMiddleware applies rate limiting to all requests
func RateLimitMiddleware(limiter *ratelimit.RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip rate limiting for health checks and OPTIONS requests
			if r.URL.Path == "/health" || r.Method == "OPTIONS" {
				next.ServeHTTP(w, r)
				return
			}

			// Get IP from X-Forwarded-For header, or fallback to RemoteAddr
			ip := r.Header.Get("X-Forwarded-For")
			if ip == "" {
				ip = r.RemoteAddr
			}

			// Clean IP address if it contains port
			if strings.Contains(ip, ":") {
				ip = strings.Split(ip, ":")[0]
			}

			if !limiter.Allow(ip) {
				http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
