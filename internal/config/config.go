package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bdotdub/relay/internal/logx"
)

type Config struct {
	LogLevel                   string
	TelegramBotToken           string
	TelegramAllowedChatIDs     map[int64]struct{}
	TelegramPollTimeoutSeconds int
	TelegramMessageChunkSize   int
	StatePath                  string
	CodexCWD                   string
	CodexStartAppServer        bool
	CodexAppServerCommand      []string
	CodexAppServerWSURL        string
	CodexModel                 string
	CodexPersonality           string
	CodexSandbox               string
	CodexApprovalPolicy        string
	CodexServiceTier           string
	CodexBaseInstructions      string
	CodexDeveloperInstructions string
	CodexConfig                map[string]any
	CodexEphemeralThreads      bool
	// Codex permission profile (optional)
	CodexNetworkEnabled string   // "true"/"false"/""; when set, sends permission_profile.network.enabled
	CodexFsReadPaths    []string // absolute paths the agent may read
	CodexFsWritePaths   []string // absolute paths the agent may write
}

func Parse(args []string) (Config, error) {
	return parse(args, io.Discard)
}

func parse(args []string, output io.Writer) (Config, error) {
	startDefault := envBool("CODEX_START_APP_SERVER", true)
	ephemeralDefault := envBool("CODEX_EPHEMERAL_THREADS", false)
	defaultLogLevel := "info"
	if strings.TrimSpace(os.Getenv("RELAY_LOG_LEVEL")) == "" && envBool("RELAY_VERBOSE", false) {
		defaultLogLevel = "debug"
	}

	var cfg Config
	var allowedChatIDs string
	var codexConfigJSON string
	var codexAppServerCommand string
	var codexFsRead, codexFsWrite string
	var noStartAppServer bool
	var noCodexEphemeralThreads bool
	var verbose bool

	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(output)

	fs.StringVar(&cfg.LogLevel, "log-level", envString("RELAY_LOG_LEVEL", defaultLogLevel), "Set internal log level: debug, info, warn, or error")
	fs.BoolVar(&verbose, "verbose", false, "Deprecated alias for --log-level=debug")
	fs.BoolVar(&verbose, "v", false, "Deprecated alias for --log-level=debug")
	fs.StringVar(&cfg.TelegramBotToken, "telegram-bot-token", envString("TELEGRAM_BOT_TOKEN", ""), "Telegram bot token")
	fs.StringVar(&allowedChatIDs, "telegram-allowed-chat-ids", envString("TELEGRAM_ALLOWED_CHAT_IDS", ""), "Comma-separated list of allowed Telegram chat IDs")
	fs.IntVar(&cfg.TelegramPollTimeoutSeconds, "telegram-poll-timeout-seconds", envInt("TELEGRAM_POLL_TIMEOUT_SECONDS", 30), "Telegram getUpdates timeout in seconds")
	fs.IntVar(&cfg.TelegramMessageChunkSize, "telegram-message-chunk-size", envInt("TELEGRAM_MESSAGE_CHUNK_SIZE", 3900), "Maximum Telegram message chunk size")
	fs.StringVar(&cfg.StatePath, "state-path", envString("RELAY_STATE_PATH", ".relay-state.json"), "Path to the chat/thread state file")
	fs.StringVar(&cfg.CodexCWD, "codex-cwd", envString("CODEX_CWD", "."), "Working directory for Codex threads")
	fs.BoolVar(&cfg.CodexStartAppServer, "start-app-server", startDefault, "Start a local Codex app-server subprocess")
	fs.BoolVar(&noStartAppServer, "no-start-app-server", false, "Do not start a local Codex app-server subprocess")
	fs.StringVar(&codexAppServerCommand, "codex-app-server-command", envString("CODEX_APP_SERVER_COMMAND", "codex app-server"), "Command used to start the Codex app server")
	fs.StringVar(&cfg.CodexAppServerWSURL, "codex-app-server-ws-url", envString("CODEX_APP_SERVER_WS_URL", ""), "WebSocket URL for an already-running Codex app server")
	fs.StringVar(&cfg.CodexModel, "codex-model", envString("CODEX_MODEL", "gpt-5.3-codex-spark"), "Codex model override")
	fs.StringVar(&cfg.CodexPersonality, "codex-personality", envString("CODEX_PERSONALITY", "pragmatic"), "Optional Codex personality override")
	fs.StringVar(&cfg.CodexSandbox, "codex-sandbox", envString("CODEX_SANDBOX", "workspace-write"), "Codex sandbox mode")
	fs.StringVar(&cfg.CodexApprovalPolicy, "codex-approval-policy", envString("CODEX_APPROVAL_POLICY", "never"), "Codex approval policy")
	fs.StringVar(&cfg.CodexServiceTier, "codex-service-tier", envString("CODEX_SERVICE_TIER", ""), "Optional Codex service tier override")
	fs.StringVar(&cfg.CodexBaseInstructions, "codex-base-instructions", envString("CODEX_BASE_INSTRUCTIONS", ""), "Optional Codex base instructions")
	fs.StringVar(&cfg.CodexDeveloperInstructions, "codex-developer-instructions", envString("CODEX_DEVELOPER_INSTRUCTIONS", ""), "Optional Codex developer instructions")
	fs.StringVar(&codexConfigJSON, "codex-config-json", envString("CODEX_CONFIG_JSON", ""), "Optional JSON object passed as Codex thread config")
	fs.BoolVar(&cfg.CodexEphemeralThreads, "codex-ephemeral-threads", ephemeralDefault, "Create ephemeral Codex threads")
	fs.BoolVar(&noCodexEphemeralThreads, "no-codex-ephemeral-threads", false, "Persist Codex threads")
	// Codex permission profile
	fs.StringVar(&cfg.CodexNetworkEnabled, "codex-network-enabled", envString("CODEX_NETWORK_ENABLED", ""), "Set to true/false to allow or deny network access (Codex permission profile)")
	fs.StringVar(&codexFsRead, "codex-fs-read", envString("CODEX_FS_READ", ""), "Comma-separated paths the agent may read (Codex permission profile)")
	fs.StringVar(&codexFsWrite, "codex-fs-write", envString("CODEX_FS_WRITE", ""), "Comma-separated paths the agent may write (Codex permission profile)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if verbose {
		cfg.LogLevel = "debug"
	}
	var err error
	cfg.LogLevel, err = logx.NormalizeLevel(cfg.LogLevel)
	if err != nil {
		return Config{}, err
	}
	cfg.CodexFsReadPaths = parsePathList(codexFsRead)
	cfg.CodexFsWritePaths = parsePathList(codexFsWrite)

	if noStartAppServer {
		cfg.CodexStartAppServer = false
	}
	if noCodexEphemeralThreads {
		cfg.CodexEphemeralThreads = false
	}

	if strings.TrimSpace(cfg.TelegramBotToken) == "" {
		return Config{}, errors.New("--telegram-bot-token or TELEGRAM_BOT_TOKEN is required")
	}
	if !cfg.CodexStartAppServer && strings.TrimSpace(cfg.CodexAppServerWSURL) == "" {
		return Config{}, errors.New("--no-start-app-server requires --codex-app-server-ws-url")
	}
	if cfg.TelegramMessageChunkSize <= 0 {
		return Config{}, errors.New("--telegram-message-chunk-size must be positive")
	}
	if cfg.TelegramPollTimeoutSeconds <= 0 {
		return Config{}, errors.New("--telegram-poll-timeout-seconds must be positive")
	}

	cfg.TelegramAllowedChatIDs, err = parseAllowedChatIDs(allowedChatIDs)
	if err != nil {
		return Config{}, err
	}
	cfg.CodexAppServerCommand, err = splitCommand(codexAppServerCommand)
	if err != nil {
		return Config{}, err
	}
	if len(cfg.CodexAppServerCommand) == 0 {
		return Config{}, errors.New("CODEX_APP_SERVER_COMMAND cannot be empty")
	}
	cfg.CodexConfig, err = parseJSONObject(codexConfigJSON)
	if err != nil {
		return Config{}, err
	}
	cfg.StatePath = cleanPath(cfg.StatePath)
	cfg.CodexCWD = cleanPath(cfg.CodexCWD)

	return cfg, nil
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return number
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func parsePathList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if abs, err := filepath.Abs(part); err == nil {
			part = abs
		}
		out = append(out, part)
	}
	return out
}

func parseAllowedChatIDs(raw string) (map[int64]struct{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	values := make(map[int64]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse TELEGRAM_ALLOWED_CHAT_IDS entry %q: %w", part, err)
		}
		values[id] = struct{}{}
	}
	return values, nil
}

func parseJSONObject(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("failed to parse CODEX_CONFIG_JSON: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("CODEX_CONFIG_JSON must decode to a JSON object")
	}
	return object, nil
}

func cleanPath(path string) string {
	if path == "" {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
