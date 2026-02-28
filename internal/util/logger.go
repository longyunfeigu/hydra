package util

import (
	"fmt"
	"os"
	"strings"
)

type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelNames = map[string]LogLevel{
	"debug": LevelDebug,
	"info":  LevelInfo,
	"warn":  LevelWarn,
	"error": LevelError,
}

var logger *Logger

func init() {
	logger = NewLogger()
}

type Logger struct {
	level LogLevel
}

func NewLogger() *Logger {
	l := &Logger{level: LevelInfo}
	if envLevel := os.Getenv("HYDRA_LOG_LEVEL"); envLevel != "" {
		if level, ok := levelNames[strings.ToLower(envLevel)]; ok {
			l.level = level
		}
	}
	return l
}

func (l *Logger) shouldLog(level LogLevel) bool {
	return level >= l.level
}

func (l *Logger) Debug(args ...interface{}) {
	if l.shouldLog(LevelDebug) {
		fmt.Fprintln(os.Stderr, append([]interface{}{"[DEBUG]"}, args...)...)
	}
}

func (l *Logger) Debugf(format string, args ...interface{}) {
	if l.shouldLog(LevelDebug) {
		fmt.Fprintf(os.Stderr, "[DEBUG] "+format+"\n", args...)
	}
}

func (l *Logger) Info(args ...interface{}) {
	if l.shouldLog(LevelInfo) {
		fmt.Fprintln(os.Stderr, append([]interface{}{"[INFO]"}, args...)...)
	}
}

func (l *Logger) Infof(format string, args ...interface{}) {
	if l.shouldLog(LevelInfo) {
		fmt.Fprintf(os.Stderr, "[INFO] "+format+"\n", args...)
	}
}

func (l *Logger) Warn(args ...interface{}) {
	if l.shouldLog(LevelWarn) {
		fmt.Fprintln(os.Stderr, append([]interface{}{"[WARN]"}, args...)...)
	}
}

func (l *Logger) Warnf(format string, args ...interface{}) {
	if l.shouldLog(LevelWarn) {
		fmt.Fprintf(os.Stderr, "[WARN] "+format+"\n", args...)
	}
}

func (l *Logger) Error(args ...interface{}) {
	if l.shouldLog(LevelError) {
		fmt.Fprintln(os.Stderr, append([]interface{}{"[ERROR]"}, args...)...)
	}
}

func (l *Logger) Errorf(format string, args ...interface{}) {
	if l.shouldLog(LevelError) {
		fmt.Fprintf(os.Stderr, "[ERROR] "+format+"\n", args...)
	}
}

func (l *Logger) SetLevel(level LogLevel) {
	l.level = level
}

// Package-level convenience functions
func Debug(args ...interface{})                 { logger.Debug(args...) }
func Debugf(format string, args ...interface{}) { logger.Debugf(format, args...) }
func Info(args ...interface{})                  { logger.Info(args...) }
func Infof(format string, args ...interface{})  { logger.Infof(format, args...) }
func Warn(args ...interface{})                  { logger.Warn(args...) }
func Warnf(format string, args ...interface{})  { logger.Warnf(format, args...) }
func Error(args ...interface{})                 { logger.Error(args...) }
func Errorf(format string, args ...interface{}) { logger.Errorf(format, args...) }
func SetLevel(level LogLevel)                   { logger.SetLevel(level) }
