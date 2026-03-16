package logx

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

func SetLevel(name string) error {
	level, err := ParseLevel(name)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
	return nil
}

func ParseLevel(name string) (slog.Level, error) {
	switch normalized, err := NormalizeLevel(name); {
	case err != nil:
		return slog.LevelInfo, err
	case normalized == "debug":
		return slog.LevelDebug, nil
	case normalized == "info":
		return slog.LevelInfo, nil
	case normalized == "warn":
		return slog.LevelWarn, nil
	case normalized == "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unsupported log level %q", name)
	}
}

func NormalizeLevel(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return "info", nil
	case "debug":
		return "debug", nil
	case "warn", "warning":
		return "warn", nil
	case "error":
		return "error", nil
	default:
		return "", fmt.Errorf("unsupported log level %q (expected debug, info, warn, or error)", name)
	}
}

func Debug(msg string, attrs ...any) {
	slog.Debug(msg, attrs...)
}

func Info(msg string, attrs ...any) {
	slog.Info(msg, attrs...)
}

func Warn(msg string, attrs ...any) {
	slog.Warn(msg, attrs...)
}

func Error(msg string, attrs ...any) {
	slog.Error(msg, attrs...)
}
