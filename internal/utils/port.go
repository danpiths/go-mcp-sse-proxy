package utils

import (
	"container/ring"
	"fmt"
	"net"
	"sync"
	"time"
)

// PortManager handles port allocation and tracking
type PortManager struct {
	mu              sync.Mutex
	usedPorts       map[int]bool
	minPort         int
	maxPort         int
	maxRetries      int
	releasedPorts   *ring.Ring    // Circular buffer of recently released ports
	maxReleased     int           // Maximum number of released ports to track
	lastCleanup     time.Time     // Last time stale ports were cleaned up
	cleanupInterval time.Duration // How often to clean up stale ports
}

// NewPortManager creates a new port manager with the specified port range
func NewPortManager(minPort, maxPort int) *PortManager {
	const (
		defaultMaxReleased     = 100 // Maximum number of released ports to track
		defaultMaxRetries      = 5
		defaultCleanupInterval = 5 * time.Minute
	)

	return &PortManager{
		usedPorts:       make(map[int]bool),
		minPort:         minPort,
		maxPort:         maxPort,
		maxRetries:      defaultMaxRetries,
		releasedPorts:   ring.New(defaultMaxReleased),
		maxReleased:     defaultMaxReleased,
		lastCleanup:     time.Now(),
		cleanupInterval: defaultCleanupInterval,
	}
}

// cleanupStale removes stale port entries and verifies port availability
func (pm *PortManager) cleanupStale() {
	now := time.Now()
	if now.Sub(pm.lastCleanup) < pm.cleanupInterval {
		return
	}
	pm.lastCleanup = now

	// Create a list of ports to check
	portsToCheck := make([]int, 0, len(pm.usedPorts))
	for port := range pm.usedPorts {
		portsToCheck = append(portsToCheck, port)
	}

	// Check each port's availability
	for _, port := range portsToCheck {
		if pm.isPortAvailable(port) {
			delete(pm.usedPorts, port)
		}
	}
}

// isPortAvailable checks if a port is actually available on the system
func (pm *PortManager) isPortAvailable(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// GetPort attempts to acquire a free port within the configured range
func (pm *PortManager) GetPort() (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Run cleanup if needed
	pm.cleanupStale()

	// First try to reuse a recently released port
	if pm.releasedPorts != nil {
		// Try each port in the ring buffer
		current := pm.releasedPorts
		for i := 0; i < pm.maxReleased; i++ {
			if port, ok := current.Value.(int); ok && port != 0 {
				// Verify the port is actually free
				if !pm.usedPorts[port] && pm.isPortAvailable(port) {
					pm.usedPorts[port] = true
					return port, nil
				}
			}
			current = current.Next()
		}
	}

	// Try to find a new port within range
	for attempt := 0; attempt < pm.maxRetries; attempt++ {
		// Try to get a port from the system
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()

		// Verify port is within our allowed range
		if port < pm.minPort || port > pm.maxPort {
			continue
		}

		// Double check port availability after closing
		if !pm.isPortAvailable(port) {
			continue
		}

		// Check if port is already tracked as in use
		if pm.usedPorts[port] {
			continue
		}

		pm.usedPorts[port] = true
		return port, nil
	}

	return 0, fmt.Errorf("failed to acquire free port after %d attempts", pm.maxRetries)
}

// ReleasePort marks a port as no longer in use
func (pm *PortManager) ReleasePort(port int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.usedPorts[port] {
		delete(pm.usedPorts, port)

		// Add to circular buffer of released ports
		if pm.releasedPorts != nil {
			pm.releasedPorts.Value = port
			pm.releasedPorts = pm.releasedPorts.Next()
		}
	}
}
