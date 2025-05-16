package ratelimit

import (
	"sync"

	"golang.org/x/time/rate"
)

// RateLimiter manages rate limiting per IP address
type RateLimiter struct {
	visitors map[string]*rate.Limiter
	mu       sync.RWMutex
	rate     rate.Limit
	burst    int
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(r rate.Limit, b int) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*rate.Limiter),
		rate:     r,
		burst:    b,
	}
}

// GetVisitor retrieves or creates a limiter for a visitor
func (rl *RateLimiter) GetVisitor(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.visitors[ip]
	if !exists {
		limiter = rate.NewLimiter(rl.rate, rl.burst)
		rl.visitors[ip] = limiter
	}
	return limiter
}

// Allow checks if the request should be allowed for the given IP
func (rl *RateLimiter) Allow(ip string) bool {
	return rl.GetVisitor(ip).Allow()
}
