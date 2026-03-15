package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type config struct {
	verbose                    bool
	telegramBotToken           string
	telegramAllowedChatIDs     map[int64]struct{}
	telegramPollTimeoutSeconds int
	telegramMessageChunkSize   int
	statePath                  string
	codexCWD                   string
	codexStartAppServer        bool
	codexAppServerCommand      []string
	codexAppServerWSURL        string
	codexModel                 string
	codexPersonality           string
	codexSandbox               string
	codexApprovalPolicy        string
	codexServiceTier           string
	codexBaseInstructions      string
	codexDeveloperInstructions string
	codexConfig                map[string]any
	codexEphemeralThreads      bool
	// Codex permission profile (optional)
	codexNetworkEnabled string   // "true"/"false"/""; when set, sends permission_profile.network.enabled
	codexFsReadPaths    []string // absolute paths the agent may read
	codexFsWritePaths   []string // absolute paths the agent may write
}

func parseConfig() (config, error) {
	startDefault := envBool("CODEX_START_APP_SERVER", true)
	ephemeralDefault := envBool("CODEX_EPHEMERAL_THREADS", false)

	var cfg config
	var allowedChatIDs string
	var codexConfigJSON string
	var codexAppServerCommand string
	var codexFsRead, codexFsWrite string
	var noStartAppServer bool
	var noCodexEphemeralThreads bool

	flag.BoolVar(&cfg.verbose, "verbose", envBool("RELAY_VERBOSE", false), "Enable verbose introspection logging")
	flag.BoolVar(&cfg.verbose, "v", envBool("RELAY_VERBOSE", false), "Enable verbose introspection logging")
	flag.StringVar(&cfg.telegramBotToken, "telegram-bot-token", envString("TELEGRAM_BOT_TOKEN", ""), "Telegram bot token")
	flag.StringVar(&allowedChatIDs, "telegram-allowed-chat-ids", envString("TELEGRAM_ALLOWED_CHAT_IDS", ""), "Comma-separated list of allowed Telegram chat IDs")
	flag.IntVar(&cfg.telegramPollTimeoutSeconds, "telegram-poll-timeout-seconds", envInt("TELEGRAM_POLL_TIMEOUT_SECONDS", 30), "Telegram getUpdates timeout in seconds")
	flag.IntVar(&cfg.telegramMessageChunkSize, "telegram-message-chunk-size", envInt("TELEGRAM_MESSAGE_CHUNK_SIZE", 3900), "Maximum Telegram message chunk size")
	flag.StringVar(&cfg.statePath, "state-path", envString("RELAY_STATE_PATH", ".relay-state.json"), "Path to the chat/thread state file")
	flag.StringVar(&cfg.codexCWD, "codex-cwd", envString("CODEX_CWD", "."), "Working directory for Codex threads")
	flag.BoolVar(&cfg.codexStartAppServer, "start-app-server", startDefault, "Start a local Codex app-server subprocess")
	flag.BoolVar(&noStartAppServer, "no-start-app-server", false, "Do not start a local Codex app-server subprocess")
	flag.StringVar(&codexAppServerCommand, "codex-app-server-command", envString("CODEX_APP_SERVER_COMMAND", "codex app-server"), "Command used to start the Codex app server")
	flag.StringVar(&cfg.codexAppServerWSURL, "codex-app-server-ws-url", envString("CODEX_APP_SERVER_WS_URL", ""), "WebSocket URL for an already-running Codex app server")
	flag.StringVar(&cfg.codexModel, "codex-model", envString("CODEX_MODEL", "spark"), "Codex model override")
	flag.StringVar(&cfg.codexPersonality, "codex-personality", envString("CODEX_PERSONALITY", "pragmatic"), "Optional Codex personality override")
	flag.StringVar(&cfg.codexSandbox, "codex-sandbox", envString("CODEX_SANDBOX", "workspace-write"), "Codex sandbox mode")
	flag.StringVar(&cfg.codexApprovalPolicy, "codex-approval-policy", envString("CODEX_APPROVAL_POLICY", "never"), "Codex approval policy")
	flag.StringVar(&cfg.codexServiceTier, "codex-service-tier", envString("CODEX_SERVICE_TIER", ""), "Optional Codex service tier override")
	flag.StringVar(&cfg.codexBaseInstructions, "codex-base-instructions", envString("CODEX_BASE_INSTRUCTIONS", ""), "Optional Codex base instructions")
	flag.StringVar(&cfg.codexDeveloperInstructions, "codex-developer-instructions", envString("CODEX_DEVELOPER_INSTRUCTIONS", ""), "Optional Codex developer instructions")
	flag.StringVar(&codexConfigJSON, "codex-config-json", envString("CODEX_CONFIG_JSON", ""), "Optional JSON object passed as Codex thread config")
	flag.BoolVar(&cfg.codexEphemeralThreads, "codex-ephemeral-threads", ephemeralDefault, "Create ephemeral Codex threads")
	flag.BoolVar(&noCodexEphemeralThreads, "no-codex-ephemeral-threads", false, "Persist Codex threads")
	// Codex permission profile
	flag.StringVar(&cfg.codexNetworkEnabled, "codex-network-enabled", envString("CODEX_NETWORK_ENABLED", ""), "Set to true/false to allow or deny network access (Codex permission profile)")
	flag.StringVar(&codexFsRead, "codex-fs-read", envString("CODEX_FS_READ", ""), "Comma-separated paths the agent may read (Codex permission profile)")
	flag.StringVar(&codexFsWrite, "codex-fs-write", envString("CODEX_FS_WRITE", ""), "Comma-separated paths the agent may write (Codex permission profile)")
	flag.Parse()

	cfg.codexFsReadPaths = parsePathList(codexFsRead)
	cfg.codexFsWritePaths = parsePathList(codexFsWrite)

	if noStartAppServer {
		cfg.codexStartAppServer = false
	}
	if noCodexEphemeralThreads {
		cfg.codexEphemeralThreads = false
	}

	if strings.TrimSpace(cfg.telegramBotToken) == "" {
		return config{}, errors.New("--telegram-bot-token or TELEGRAM_BOT_TOKEN is required")
	}
	if !cfg.codexStartAppServer && strings.TrimSpace(cfg.codexAppServerWSURL) == "" {
		return config{}, errors.New("--no-start-app-server requires --codex-app-server-ws-url")
	}
	if cfg.telegramMessageChunkSize <= 0 {
		return config{}, errors.New("--telegram-message-chunk-size must be positive")
	}
	if cfg.telegramPollTimeoutSeconds <= 0 {
		return config{}, errors.New("--telegram-poll-timeout-seconds must be positive")
	}

	var err error
	cfg.telegramAllowedChatIDs, err = parseAllowedChatIDs(allowedChatIDs)
	if err != nil {
		return config{}, err
	}
	cfg.codexAppServerCommand, err = splitCommand(codexAppServerCommand)
	if err != nil {
		return config{}, err
	}
	if len(cfg.codexAppServerCommand) == 0 {
		return config{}, errors.New("CODEX_APP_SERVER_COMMAND cannot be empty")
	}
	cfg.codexConfig, err = parseJSONObject(codexConfigJSON)
	if err != nil {
		return config{}, err
	}
	cfg.statePath = cleanPath(cfg.statePath)
	cfg.codexCWD = cleanPath(cfg.codexCWD)

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
