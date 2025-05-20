package process

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go-mcp-sse-proxy/internal/config"
	"go-mcp-sse-proxy/internal/models"
	"go-mcp-sse-proxy/internal/utils"
)

// Manager handles lifecycle of gateway instances
type Manager struct {
	instancesMu   sync.RWMutex
	sessionsMu    sync.RWMutex
	instances     map[string]*models.GatewayInstance
	sessions      map[string]*models.SessionInfo
	maxLifetime   time.Duration
	cleanupTicker *time.Ticker
	portManager   *utils.PortManager

	// Configuration
	maxSessions      int           // Maximum concurrent sessions per instance
	maxTotalSessions int           // Maximum total sessions across all instances
	sessionTimeout   time.Duration // Time after which inactive sessions are cleaned up
	cleanupInterval  time.Duration // How often to run cleanup
	GracefulTimeout  time.Duration // Time to wait for graceful shutdown before force kill
}

// NewManager creates a new process manager
func NewManager(config config.ProcessConfig) *Manager {
	pm := &Manager{
		instances:        make(map[string]*models.GatewayInstance),
		sessions:         make(map[string]*models.SessionInfo),
		maxLifetime:      config.MaxLifetime,
		maxSessions:      config.MaxSessions,
		maxTotalSessions: config.MaxTotalSessions,
		sessionTimeout:   config.SessionTimeout,
		cleanupInterval:  config.CleanupInterval,
		cleanupTicker:    time.NewTicker(config.CleanupInterval),
		portManager:      utils.NewPortManager(10000, 65535), // Use non-privileged ports
		GracefulTimeout:  config.GracefulTimeout,
	}
	go pm.cleanupLoop()
	return pm
}

// cleanupLoop periodically checks for and terminates expired processes and sessions
func (pm *Manager) cleanupLoop() {
	for range pm.cleanupTicker.C {
		pm.cleanupExpiredProcesses()
		pm.cleanupExpiredSessions()
	}
}

// cleanupExpiredSessions removes sessions that have been inactive
func (pm *Manager) cleanupExpiredSessions() {
	pm.sessionsMu.Lock()
	defer pm.sessionsMu.Unlock()

	now := time.Now()

	for sid, session := range pm.sessions {
		// Only cleanup sessions that have been inactive for longer than the timeout
		// and don't have any active SSE connections
		if now.Sub(session.LastUsed) > pm.sessionTimeout &&
			session.Instance != nil &&
			session.Instance.Sessions.Load() <= 0 {
			log.Info("Cleaning up expired session: %s", sid)
			delete(pm.sessions, sid)
		}
	}
}

// cleanupExpiredProcesses terminates processes that have exceeded maxLifetime
func (pm *Manager) cleanupExpiredProcesses() {
	pm.instancesMu.Lock()
	defer pm.instancesMu.Unlock()

	now := time.Now()

	for key, inst := range pm.instances {
		if now.Sub(inst.StartTime) > pm.maxLifetime {
			log.Info("Terminating expired process for key: %s", key)
			pm.terminateInstance(key, inst)
		}
	}
}

// terminateInstance cleanly stops a gateway instance
func (pm *Manager) terminateInstance(key string, inst *models.GatewayInstance) {
	// Create a context with timeout for the cleanup process
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), pm.GracefulTimeout)
	defer cleanupCancel()

	// Cancel the instance context first
	if inst.Cancel != nil {
		inst.Cancel()
	}

	if inst.Cmd.Process != nil {
		// Create a channel to track process exit
		done := make(chan error, 1)

		// Start a goroutine to wait for the process
		go func() {
			done <- inst.Cmd.Wait()
		}()

		// First try graceful shutdown with SIGTERM
		if err := inst.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
			if !strings.Contains(err.Error(), "process already finished") {
				log.Error("Failed to send SIGTERM to process: %v", err)
			}
		}

		// Wait for process to exit or timeout
		select {
		case err := <-done:
			if err != nil && !strings.Contains(err.Error(), "signal: killed") {
				log.Error("Process exited with error: %v", err)
			} else {
				log.Info("Process terminated gracefully")
			}
		case <-cleanupCtx.Done():
			// Process didn't exit gracefully, force kill
			log.Warn("Process did not exit gracefully, forcing kill")
			if err := inst.Cmd.Process.Kill(); err != nil {
				if !strings.Contains(err.Error(), "process already finished") {
					log.Error("Failed to kill process: %v", err)
				}
			}
		}
	}

	// Release the port
	if inst.InternalURL != nil {
		if port, err := strconv.Atoi(inst.InternalURL.Port()); err == nil {
			pm.portManager.ReleasePort(port)
		}
	}

	// Clean up instance and sessions
	pm.cleanupInstanceAndSessions(key, inst)
}

// cleanupInstanceAndSessions removes the instance and its associated sessions
func (pm *Manager) cleanupInstanceAndSessions(key string, inst *models.GatewayInstance) {
	const lockTimeout = 5 * time.Second

	// First acquire instancesMu
	if !utils.TryLock(&pm.instancesMu, lockTimeout) {
		log.Error("Failed to acquire instance lock for cleanup")
		return
	}
	// Only delete if it's still the same instance
	if current := pm.instances[key]; current == inst {
		delete(pm.instances, key)
	}
	pm.instancesMu.Unlock()

	// Then acquire sessionsMu
	if !utils.TryLock(&pm.sessionsMu, lockTimeout) {
		log.Error("Failed to acquire session lock for cleanup")
		return
	}
	defer pm.sessionsMu.Unlock()

	// Only clean up sessions that are specifically tied to this instance
	for sid, session := range pm.sessions {
		if session.Instance == inst {
			// Check if the session is still active
			if session.Instance.Sessions.Load() > 0 {
				session.Instance.Sessions.Add(-1)
			}
			delete(pm.sessions, sid)
		}
	}
}

// Shutdown cleanly stops all managed processes
func (pm *Manager) Shutdown() {
	pm.cleanupTicker.Stop()
	pm.instancesMu.Lock()
	defer pm.instancesMu.Unlock()

	for key, inst := range pm.instances {
		log.Info("Shutting down instance: %s", key)
		pm.terminateInstance(key, inst)
	}
}

// CreateSession creates a new session for an instance
func (pm *Manager) CreateSession(sessionID string, inst *models.GatewayInstance) error {
	pm.sessionsMu.Lock()
	defer pm.sessionsMu.Unlock()

	// Check if session already exists
	if _, exists := pm.sessions[sessionID]; exists {
		return fmt.Errorf("session already exists")
	}

	// Check total session limit
	if len(pm.sessions) >= pm.maxTotalSessions {
		return fmt.Errorf("maximum total sessions reached")
	}

	// Check per-instance session limit
	if inst.Sessions.Load() >= int32(pm.maxSessions) {
		return fmt.Errorf("maximum sessions per instance reached")
	}

	// Create new session
	pm.sessions[sessionID] = models.NewSessionInfo(inst)
	inst.Sessions.Add(1)

	return nil
}

// TouchSession updates the last used time for a session
func (pm *Manager) TouchSession(sessionID string) bool {
	const lockTimeout = 5 * time.Second

	if !utils.TryLock(&pm.sessionsMu, lockTimeout) {
		log.Error("Failed to acquire session lock for touch")
		return false
	}
	defer pm.sessionsMu.Unlock()

	if session, exists := pm.sessions[sessionID]; exists {
		session.Touch()
		return true
	}
	return false
}

// GetInstance returns an instance by key
func (pm *Manager) GetInstance(key string) *models.GatewayInstance {
	pm.instancesMu.RLock()
	defer pm.instancesMu.RUnlock()
	return pm.instances[key]
}

// GetSession returns a session by ID
func (pm *Manager) GetSession(sessionID string) *models.SessionInfo {
	pm.sessionsMu.RLock()
	defer pm.sessionsMu.RUnlock()
	return pm.sessions[sessionID]
}

// AddInstance adds a new instance
func (pm *Manager) AddInstance(key string, instance *models.GatewayInstance) {
	pm.instancesMu.Lock()
	defer pm.instancesMu.Unlock()
	pm.instances[key] = instance
}

// GetPort gets a port from the port manager
func (pm *Manager) GetPort() (int, error) {
	return pm.portManager.GetPort()
}

// ReleasePort releases a port back to the pool
func (pm *Manager) ReleasePort(port int) {
	pm.portManager.ReleasePort(port)
}

// RemoveInstance removes an instance from the manager
func (pm *Manager) RemoveInstance(key string) {
	pm.instancesMu.Lock()
	defer pm.instancesMu.Unlock()
	delete(pm.instances, key)
}

// TerminateInstance cleanly stops a gateway instance
func (pm *Manager) TerminateInstance(key string, inst *models.GatewayInstance) {
	pm.terminateInstance(key, inst)
}
