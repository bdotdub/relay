# Telegram Codex Relay

Relay messages between Telegram and a Codex app server.

<img width="1536" height="1024" alt="relay" src="https://github.com/user-attachments/assets/b025aeca-650c-491b-b155-80f22caf7b0b" />

## Quick Start

- Create a [Telegram bot](https://core.telegram.org/bots/tutorial)
- Download (or build) the `relay` binary: https://github.com/bdotdub/relay/actions/workflows/build-binaries.yml
- In a repo, run: relay --telegram-bot-token <TOKEN>

---

## What this does

- Polls Telegram updates in private 1:1 chats.
- For each plain-text message, sends it into an existing Codex thread for that chat.
- Forwards Codex answers back to the same Telegram chat.
- Keeps one conversation thread per chat and supports steering while a turn is active.
- Stores chat → thread state, a rolling per-chat continuity snapshot, and per-chat verbose, YOLO, fast-mode, and model settings in `.relay-state.json` by default.
- Injects Telegram-specific developer instructions so replies use Telegram MarkdownV2 and avoid unnecessary filesystem paths.
- Registers supported slash commands with Telegram via `setMyCommands` at startup.

If `codex app-server` is already running, it can use that server over WebSocket.
Otherwise it launches Codex locally over stdio by default.

## Conversation behavior

- First message in a chat: create a new Codex thread.
- Subsequent messages in same chat: reuse the chat thread.
- New message arrives mid-turn: treated as `turn/steer` (inline follow-up).
- `/new` or `/reset`: clear saved thread mapping and start a fresh thread.
- If a saved thread cannot be resumed, the relay automatically starts a new thread.
- When a saved thread cannot be resumed, the relay injects the last few persisted chat messages into the first turn on the replacement thread.

## Telegram commands

The following commands are registered automatically and available in Telegram’s command UI:

- `/help`: show supported commands
- `/status`: show transport, mode, model, reasoning effort, thread id, working directory, token usage
- `/new`: start a fresh thread for this chat
- `/reset`: same as `/new`
- `/verbose` / `/verbose on|off|status`: toggle or inspect visible intermediate output
- `/yolo` / `/yolo on|off|status`: toggle YOLO execution mode and start a fresh thread when changed
- `/fast` / `/fast on|off|status`: toggle fast mode and start a fresh thread when changed
- `/model`: show current chat model
- `/model <name>`: set chat model override and start a fresh thread
- `/model default`: clear model override and use default
- `/reasoning`: show current chat reasoning effort and supported values
- `/reasoning <level>`: set chat reasoning effort override and start a fresh thread
- `/reasoning default`: clear reasoning effort override and use the model default
- `/reload`: replace the running relay process with the current binary; active turns are interrupted

Any other plain-text message is forwarded directly to Codex.

## Prerequisites

- Go 1.25.8+
- Telegram bot token from BotFather
- Authenticated `codex` CLI (if running Codex locally)

## Configuration

Core relay settings:

- `TELEGRAM_BOT_TOKEN` (required): Telegram bot token
- `TELEGRAM_ALLOWED_CHAT_IDS` (required): comma-separated allowed private chat IDs
- `RELAY_LOG_LEVEL` (optional): default `info`
- `RELAY_VERBOSE` (optional): deprecated compatibility for `RELAY_LOG_LEVEL=debug`
- `RELAY_STATE_PATH` (optional): path to relay state file (default `.relay-state.json`)
- `CODEX_CWD` (optional): working directory for Codex threads

Codex process settings:

- `CODEX_START_APP_SERVER`: default `true`
- `CODEX_APP_SERVER_COMMAND`: default `codex app-server`
- `CODEX_APP_SERVER_WS_URL`: required when not starting the app server

Optional Codex turn settings:

- `CODEX_MODEL` (default `gpt-5.4`)
- `CODEX_PERSONALITY`
- `CODEX_SANDBOX`
- `CODEX_APPROVAL_POLICY`
- `CODEX_SERVICE_TIER` (default `fast`)
- `CODEX_BASE_INSTRUCTIONS`
- `CODEX_DEVELOPER_INSTRUCTIONS`
- `CODEX_CONFIG_JSON`
- `CODEX_EPHEMERAL_THREADS`

Permission profile settings:

- `CODEX_NETWORK_ENABLED`: `true` or `false`
- `CODEX_FS_READ`: comma-separated read paths
- `CODEX_FS_WRITE`: comma-separated write paths

Run `go run . --help` to see all matching CLI flags.

## Running modes

Default:

```bash
go run .
```

Explicit local app-server mode:

```bash
go run . \
  --start-app-server \
  --codex-app-server-command "codex app-server"
```

Use an existing app server:

```bash
go run . \
  --no-start-app-server \
  --codex-app-server-ws-url "ws://127.0.0.1:8765"
```

## Known limitations

- Only plain-text messages in private chats are supported
- Global Telegram updates, per-chat workflow state
- One active Codex turn per chat
- Verbose output is delivered as separate Telegram messages while a turn is running
- `/model <name>` depends on the configured Codex server accepting that model string
- Replies are delivered when a turn completes (no streaming)
- Thread, continuity snapshot, YOLO, fast-mode, and model state are local-file state, not shared across instances

## Verification

```bash
go test ./...
go build ./...
```

If default cache dirs are restricted:

```bash
env GOCACHE=$PWD/.gocache GOTMPDIR=$PWD/.gotmp GOMODCACHE=$PWD/.gomodcache \
go test ./...
env GOCACHE=$PWD/.gocache GOTMPDIR=$PWD/.gotmp GOMODCACHE=$PWD/.gomodcache \
go build ./...
```

## References

- Codex app server: https://github.com/openai/codex/tree/main/codex-rs/app-server
- Telegram Bot API: https://core.telegram.org/bots/api
