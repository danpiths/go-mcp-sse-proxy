package config

import "time"

// TimeoutConfig holds timeout configuration for various operations
type TimeoutConfig struct {
	SSETimeout        time.Duration // Timeout for SSE connections
	RequestTimeout    time.Duration // Timeout for regular HTTP requests
	ShutdownTimeout   time.Duration // Timeout for graceful shutdown
	HealthCheckPeriod time.Duration // Period between health checks
}

// ProcessConfig holds configuration for the process manager
type ProcessConfig struct {
	MaxLifetime      time.Duration
	MaxSessions      int
	MaxTotalSessions int
	SessionTimeout   time.Duration
	CleanupInterval  time.Duration
	GracefulTimeout  time.Duration // Time to wait for graceful shutdown before force kill
}

// DefaultTimeoutConfig returns sensible default timeout configuration
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		SSETimeout:        1 * time.Hour,
		RequestTimeout:    60 * time.Second,
		ShutdownTimeout:   60 * time.Second,
		HealthCheckPeriod: 30 * time.Second,
	}
}

// DefaultProcessConfig returns sensible default process configuration
func DefaultProcessConfig() ProcessConfig {
	return ProcessConfig{
		MaxLifetime:      1 * time.Hour,
		MaxSessions:      100,  // Max 100 sessions per instance
		MaxTotalSessions: 1000, // Max 1000 total sessions
		SessionTimeout:   30 * time.Minute,
		CleanupInterval:  1 * time.Minute,
		GracefulTimeout:  10 * time.Second,
	}
}

// AllowedCommands maps unescaped command strings to their actual invocations
var AllowedCommands = map[string]string{
	"npx -y @upstash/context7-mcp@latest":                                                 "npx -y @upstash/context7-mcp@latest",
	"npx -y @maximai/mcp-server@latest":                                                   "npx -y @maximai/mcp-server@latest",
	"docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server": "docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server",
}

// BlacklistedEnvVars contains environment variables that should never be overwritten
var BlacklistedEnvVars = map[string]bool{
	"PATH":              true,
	"HOME":              true,
	"USER":              true,
	"SHELL":             true,
	"PWD":               true,
	"TMPDIR":            true,
	"TEMP":              true,
	"TMP":               true,
	"HOSTNAME":          true,
	"LANG":              true,
	"LC_ALL":            true,
	"SUDO_USER":         true,
	"SUDO_COMMAND":      true,
	"SSH_AUTH_SOCK":     true,
	"SSH_AGENT_PID":     true,
	"AWS_ACCESS_KEY":    true,
	"AWS_SECRET_KEY":    true,
	"AWS_SESSION_TOKEN": true,
	"API_KEY":           true,
	"SECRET_KEY":        true,
	"PRIVATE_KEY":       true,
	"PASSWORD":          true,
	"TOKEN":             true,
}
