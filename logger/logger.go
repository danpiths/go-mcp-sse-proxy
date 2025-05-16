package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/natefinch/lumberjack.v2"
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

// Logger provides structured logging with rotation
type Logger struct {
	level      LogLevel
	output     io.Writer
	mu         sync.Mutex
	fields     map[string]interface{}
	timeFormat string
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

// NewLogger creates a new logger with the specified configuration
func NewLogger(config *LoggerConfig) *Logger {
	if config == nil {
		config = &defaultConfig
	}

	var writers []io.Writer

	// Add console writer if enabled
	if config.UseConsole {
		writers = append(writers, os.Stdout)
	}

	// Add file writer if enabled
	if config.UseFile && config.Filename != "" {
		// Create directory if it doesn't exist
		if err := os.MkdirAll(filepath.Dir(config.Filename), 0755); err != nil {
			fmt.Printf("Error creating log directory: %v\n", err)
		}

		// Configure log rotation
		fileWriter := &lumberjack.Logger{
			Filename:   config.Filename,
			MaxSize:    config.MaxSize,
			MaxBackups: config.MaxBackups,
			MaxAge:     config.MaxAge,
			Compress:   config.Compress,
		}
		writers = append(writers, fileWriter)
	}

	// Create multi-writer if multiple outputs
	var output io.Writer
	if len(writers) > 1 {
		output = io.MultiWriter(writers...)
	} else if len(writers) == 1 {
		output = writers[0]
	} else {
		output = os.Stdout // fallback to stdout
	}

	return &Logger{
		level:      config.Level,
		output:     output,
		fields:     make(map[string]interface{}),
		timeFormat: config.TimeFormat,
	}
}

// WithFields creates a new logger with the additional fields
func (l *Logger) WithFields(fields map[string]interface{}) *Logger {
	newLogger := &Logger{
		level:      l.level,
		output:     l.output,
		timeFormat: l.timeFormat,
		fields:     make(map[string]interface{}, len(l.fields)+len(fields)),
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

// SetLogLevel sets the logger's level
func (l *Logger) SetLogLevel(level LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// GetLogLevel returns the current log level
func (l *Logger) GetLogLevel() LogLevel {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

func (l *Logger) log(level LogLevel, format string, v ...interface{}) {
	if level < l.level {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Get caller information
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		file = "unknown"
		line = 0
	}

	// Create log entry
	entry := LogEntry{
		Timestamp: time.Now().Format(l.timeFormat),
		Level:     level.String(),
		Message:   fmt.Sprintf(format, v...),
		File:      filepath.Base(file),
		Line:      line,
		Fields:    l.fields,
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling log entry: %v\n", err)
		return
	}

	// Write log entry
	l.output.Write(append(jsonData, '\n'))
}

func (l *Logger) Debug(format string, v ...interface{}) {
	l.log(LogDebug, format, v...)
}

func (l *Logger) Info(format string, v ...interface{}) {
	l.log(LogInfo, format, v...)
}

func (l *Logger) Warn(format string, v ...interface{}) {
	l.log(LogWarn, format, v...)
}

func (l *Logger) Error(format string, v ...interface{}) {
	l.log(LogError, format, v...)
}
