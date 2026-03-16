package config

import (
	"flag"
	"io"
	"os"
	"testing"
)

func TestParseConfigDefaultsLogLevelToInfo(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")

	cfg, err := parseConfigForTest(t)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("expected info log level by default, got %q", cfg.LogLevel)
	}
}

func TestParseConfigVerboseFlagSetsDebugLogLevel(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")

	cfg, err := parseConfigForTest(t, "--verbose")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected debug log level from --verbose, got %q", cfg.LogLevel)
	}
}

func TestParseConfigRelayVerboseEnvSetsDebugLogLevel(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("RELAY_VERBOSE", "true")

	cfg, err := parseConfigForTest(t)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected debug log level from RELAY_VERBOSE, got %q", cfg.LogLevel)
	}
}

func TestParseConfigRelayLogLevelOverridesRelayVerboseEnv(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("RELAY_VERBOSE", "true")
	t.Setenv("RELAY_LOG_LEVEL", "error")

	cfg, err := parseConfigForTest(t)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.LogLevel != "error" {
		t.Fatalf("expected error log level from RELAY_LOG_LEVEL, got %q", cfg.LogLevel)
	}
}

func TestParseConfigVerboseFlagOverridesExplicitLogLevel(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")

	cfg, err := parseConfigForTest(t, "--log-level=warn", "--verbose")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected debug log level when --verbose is set, got %q", cfg.LogLevel)
	}
}

func TestParseConfigRejectsInvalidLogLevel(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")

	if _, err := parseConfigForTest(t, "--log-level=noisy"); err == nil {
		t.Fatal("expected invalid log level error")
	}
}

func parseConfigForTest(t *testing.T, args ...string) (Config, error) {
	t.Helper()

	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	fs := flag.NewFlagSet(oldArgs[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = append([]string{oldArgs[0]}, args...)

	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	})

	return Parse()
}
