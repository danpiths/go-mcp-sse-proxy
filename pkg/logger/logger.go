package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// LogLevel represents different logging levels
type LogLevel int

const (
	LogDebug LogLevel = iota
	LogInfo
	LogWarn
	LogError
)

func (l LogLevel) String() string {
	switch l {
	case LogDebug:
		return "DEBUG"
	case LogInfo:
		return "INFO"
	case LogWarn:
		return "WARN"
	case LogError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// LogEntry represents a structured log entry
type LogEntry struct {
	Timestamp     string                 `json:"timestamp"`
	Level         string                 `json:"level"`
	Message       string                 `json:"message"`
	CorrelationID string                 `json:"correlation_id,omitempty"`
	File          string                 `json:"file,omitempty"`
	Line          int                    `json:"line,omitempty"`
	Fields        map[string]interface{} `json:"fields,omitempty"`
}

// LoggerConfig holds configuration for the logger
type LoggerConfig struct {
	Level      LogLevel
	TimeFormat string
	// File rotation settings
	Filename   string
	MaxSize    int  // megabytes
	MaxBackups int  // number of backups
	MaxAge     int  // days
	Compress   bool // compress old files
	// Output settings
	UseConsole bool
	UseFile    bool
}

var defaultConfig = LoggerConfig{
	Level:      LogInfo,
	TimeFormat: time.RFC3339,
	MaxSize:    100,   // 100MB
	MaxBackups: 5,     // 5 backups
	MaxAge:     30,    // 30 days
	Compress:   true,  // compress old files
	UseConsole: true,  // output to console by default
	UseFile:    false, // don't output to file by default
}

// Logger provides structured logging with rotation
type Logger struct {
	config *LoggerConfig
	file   *os.File
	fields map[string]interface{}
}

// NewLogger creates a new logger with the specified configuration
func NewLogger(config *LoggerConfig) *Logger {
	if config == nil {
		config = &defaultConfig
	}

	logger := &Logger{
		config: config,
		fields: make(map[string]interface{}),
	}

	if config.UseFile {
		// Create logs directory if it doesn't exist
		if err := os.MkdirAll(filepath.Dir(config.Filename), 0755); err != nil {
			fmt.Printf("Error creating log directory: %v\n", err)
		}

		// Open log file
		file, err := os.OpenFile(config.Filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Printf("Error opening log file: %v\n", err)
		} else {
			logger.file = file
		}
	}

	return logger
}

// WithFields creates a new logger with the additional fields
func (l *Logger) WithFields(fields map[string]interface{}) *Logger {
	newLogger := &Logger{
		config: l.config,
		file:   l.file,
		fields: make(map[string]interface{}, len(l.fields)+len(fields)),
	}

	// Copy existing fields
	for k, v := range l.fields {
		newLogger.fields[k] = v
	}

	// Add new fields
	for k, v := range fields {
		newLogger.fields[k] = v
	}

	return newLogger
}

// WithCorrelationID creates a new logger with the specified correlation ID
func (l *Logger) WithCorrelationID(correlationID string) *Logger {
	if correlationID == "" {
		correlationID = uuid.New().String()
	}
	return l.WithFields(map[string]interface{}{
		"correlation_id": correlationID,
	})
}

// GetLogLevel returns the current log level
func (l *Logger) GetLogLevel() LogLevel {
	return l.config.Level
}

// log writes a log message with the given level
func (l *Logger) log(level LogLevel, format string, args ...interface{}) {
	if level < l.config.Level {
		return
	}

	// Format the message
	timestamp := time.Now().Format(l.config.TimeFormat)
	levelStr := ""
	switch level {
	case LogDebug:
		levelStr = "DEBUG"
	case LogInfo:
		levelStr = "INFO"
	case LogWarn:
		levelStr = "WARN"
	case LogError:
		levelStr = "ERROR"
	}

	message := fmt.Sprintf(format, args...)
	logLine := fmt.Sprintf("%s [%s] %s\n", timestamp, levelStr, message)

	// Write to console if enabled
	if l.config.UseConsole {
		fmt.Print(logLine)
	}

	// Write to file if enabled and file is open
	if l.config.UseFile && l.file != nil {
		if _, err := l.file.WriteString(logLine); err != nil {
			fmt.Printf("Error writing to log file: %v\n", err)
		}
	}
}

// Debug logs a debug message
func (l *Logger) Debug(format string, args ...interface{}) {
	l.log(LogDebug, format, args...)
}

// Info logs an info message
func (l *Logger) Info(format string, args ...interface{}) {
	l.log(LogInfo, format, args...)
}

// Warn logs a warning message
func (l *Logger) Warn(format string, args ...interface{}) {
	l.log(LogWarn, format, args...)
}

// Error logs an error message
func (l *Logger) Error(format string, args ...interface{}) {
	l.log(LogError, format, args...)
}

// Close closes the logger and its file
func (l *Logger) Close() {
	if l.file != nil {
		l.file.Close()
	}
}
