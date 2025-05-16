package models

import "time"

// SessionInfo tracks session metadata
type SessionInfo struct {
	Instance  *GatewayInstance
	LastUsed  time.Time
	CreatedAt time.Time
}

// NewSessionInfo creates a new session info instance
func NewSessionInfo(instance *GatewayInstance) *SessionInfo {
	now := time.Now()
	return &SessionInfo{
		Instance:  instance,
		LastUsed:  now,
		CreatedAt: now,
	}
}

// Touch updates the last used time of the session
func (s *SessionInfo) Touch() {
	s.LastUsed = time.Now()
}
