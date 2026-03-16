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

func Debugf(format string, args ...any) {
	slog.Debug(fmt.Sprintf(format, args...))
}

func Infof(format string, args ...any) {
	slog.Info(fmt.Sprintf(format, args...))
}

func Warnf(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...))
}

func Errorf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
}

func KVSummary(values ...any) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values)/2)
	for index := 0; index+1 < len(values); index += 2 {
		parts = append(parts, fmt.Sprintf("%v=%v", values[index], values[index+1]))
	}
	return strings.Join(parts, " ")
}
