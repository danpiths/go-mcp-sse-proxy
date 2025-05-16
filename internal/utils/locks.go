package utils

import (
	"sync"
	"time"
)

// TryLock attempts to acquire a lock with timeout
func TryLock(mu sync.Locker, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		mu.Lock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// TryRLock attempts to acquire a read lock with timeout
func TryRLock(mu *sync.RWMutex, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		mu.RLock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
