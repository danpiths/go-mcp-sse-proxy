package models

import (
	"context"
	"net/url"
	"os/exec"
	"sync/atomic"
	"time"
)

// GatewayInstance holds a supergateway subprocess bound to an internal URL
type GatewayInstance struct {
	Cmd         *exec.Cmd
	InternalURL *url.URL
	StartTime   time.Time
	Cancel      context.CancelFunc
	Sessions    atomic.Int32 // Number of active sessions
}

// NewGatewayInstance creates a new gateway instance
func NewGatewayInstance(cmd *exec.Cmd, internalURL *url.URL, cancel context.CancelFunc) *GatewayInstance {
	inst := &GatewayInstance{
		Cmd:         cmd,
		InternalURL: internalURL,
		StartTime:   time.Now(),
		Cancel:      cancel,
	}
	inst.Sessions.Store(0)
	return inst
}
