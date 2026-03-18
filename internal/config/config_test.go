package config

import (
	"testing"
)

func TestParseConfigDefaultsLogLevelToInfo(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", "123")

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
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", "123")

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
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", "123")
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
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", "123")
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
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", "123")

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
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", "123")

	if _, err := parseConfigForTest(t, "--log-level=noisy"); err == nil {
		t.Fatal("expected invalid log level error")
	}
}

func TestParseConfigRequiresAllowedChatIDs(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")

	if _, err := parseConfigForTest(t); err == nil {
		t.Fatal("expected missing allowlist error")
	}
}

func TestParseConfigDefaultsModelToGPT54(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", "123")

	cfg, err := parseConfigForTest(t)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.CodexModel != "gpt-5.4" {
		t.Fatalf("expected default model gpt-5.4, got %q", cfg.CodexModel)
	}
}

func parseConfigForTest(t *testing.T, args ...string) (Config, error) {
	t.Helper()
	return Parse(args)
}
