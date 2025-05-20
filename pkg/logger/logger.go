package logger

import (
	"os"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
)

var (
	Log *log.Logger
)

func init() {
	// Create a new logger with rich options
	Log = log.NewWithOptions(os.Stderr, log.Options{
		ReportCaller:    true,
		ReportTimestamp: true,
		TimeFormat:      time.RFC3339,
		Prefix:          "🔄 SSE-Proxy",
	})

	// Set up custom styles for different log levels
	styles := log.DefaultStyles()

	// Error style - rich red with bold text and padding
	styles.Levels[log.ErrorLevel] = lipgloss.NewStyle().
		SetString("ERROR").
		Padding(0, 1).
		Bold(true).
		Background(lipgloss.Color("196")).
		Foreground(lipgloss.Color("15"))

	// Warning style - yellow with black text
	styles.Levels[log.WarnLevel] = lipgloss.NewStyle().
		SetString("WARN").
		Padding(0, 1).
		Background(lipgloss.Color("214")).
		Foreground(lipgloss.Color("0"))

	// Info style - blue with white text
	styles.Levels[log.InfoLevel] = lipgloss.NewStyle().
		SetString("INFO").
		Padding(0, 1).
		Background(lipgloss.Color("39")).
		Foreground(lipgloss.Color("15"))

	// Debug style - gray with white text
	styles.Levels[log.DebugLevel] = lipgloss.NewStyle().
		SetString("DEBUG").
		Padding(0, 1).
		Background(lipgloss.Color("242")).
		Foreground(lipgloss.Color("15"))

	// Custom styles for specific keys
	styles.Keys["err"] = lipgloss.NewStyle().
		Foreground(lipgloss.Color("196")).
		Bold(true)
	styles.Values["err"] = lipgloss.NewStyle().
		Foreground(lipgloss.Color("196")).
		Italic(true)

	styles.Keys["port"] = lipgloss.NewStyle().
		Foreground(lipgloss.Color("39")).
		Bold(true)
	styles.Values["port"] = lipgloss.NewStyle().
		Foreground(lipgloss.Color("39")).
		Underline(true)

	styles.Keys["cmd"] = lipgloss.NewStyle().
		Foreground(lipgloss.Color("213")).
		Bold(true)
	styles.Values["cmd"] = lipgloss.NewStyle().
		Foreground(lipgloss.Color("213"))

	styles.Keys["url"] = lipgloss.NewStyle().
		Foreground(lipgloss.Color("83")).
		Bold(true)
	styles.Values["url"] = lipgloss.NewStyle().
		Foreground(lipgloss.Color("83")).
		Underline(true)

	Log.SetStyles(styles)
}

// GetLogLevel returns the current log level
func GetLogLevel() log.Level {
	return Log.GetLevel()
}

// SetLogLevel sets the log level
func SetLogLevel(level log.Level) {
	Log.SetLevel(level)
}

// Helper marks the calling function as a helper
func Helper() {
	Log.Helper()
}
