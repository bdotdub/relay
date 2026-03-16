# Telegram Codex Relay

Go service that relays messages between a Telegram bot and the Codex app server.

## What It Does

- receives Telegram bot messages through long polling
- sends each plain-text message to the Codex app server as a new turn
- sends the Codex reply back to Telegram
- keeps one Codex thread per Telegram chat
- saves the chat-to-thread mapping in `.relay-state.json`
- appends a Telegram-specific developer instruction so replies target Telegram MarkdownV2 rendering and avoid unnecessary local filesystem paths

By default the relay starts `codex app-server` itself over `stdio`. It can also connect to an already-running app server over WebSocket.

## Conversation Model

The relay keeps context per Telegram chat.

- first message from a chat: create a new Codex thread
- later messages from the same chat: reuse that same thread
- if a second message arrives for the same chat while a turn is still running: send it as `turn/steer` into the active turn
- `/new` or `/reset`: discard the saved mapping for that chat and start a fresh thread
- if a saved thread cannot be resumed: fall back to a new thread automatically

In practice this means the relay keeps a session going for each chat until you reset it, and quick follow-up messages can steer an in-flight answer instead of waiting for a brand new turn.

## Requirements

- Go 1.25+
- a Telegram bot token from BotFather
- Codex CLI installed and authenticated

## Build

```bash
go build ./...
```

## Minimal Setup

```bash
export TELEGRAM_BOT_TOKEN="123456:abc..."
export TELEGRAM_ALLOWED_CHAT_IDS="123456789"
go run .
```

That starts the relay and, by default, also starts `codex app-server`.

## Configuration

Core settings:

- `TELEGRAM_BOT_TOKEN`: required
- `TELEGRAM_ALLOWED_CHAT_IDS`: optional comma-separated allowlist
- `RELAY_LOG_LEVEL`: optional internal log level; defaults to `info`
- `RELAY_VERBOSE`: deprecated compatibility toggle for `RELAY_LOG_LEVEL=debug`
- `RELAY_STATE_PATH`: optional path for the chat/thread mapping file
- `CODEX_CWD`: working directory for Codex threads

Codex process settings:

- `CODEX_START_APP_SERVER`: defaults to `true`
- `CODEX_APP_SERVER_COMMAND`: defaults to `codex app-server`
- `CODEX_APP_SERVER_WS_URL`: required only when not starting the app server locally

Optional Codex turn settings:

- `CODEX_MODEL`: defaults to `gpt-5.3-codex-spark`
- `CODEX_PERSONALITY`
- `CODEX_SANDBOX`
- `CODEX_APPROVAL_POLICY`
- `CODEX_SERVICE_TIER`
- `CODEX_BASE_INSTRUCTIONS`
- `CODEX_DEVELOPER_INSTRUCTIONS`
- `CODEX_CONFIG_JSON`
- `CODEX_EPHEMERAL_THREADS`

Codex permission profile (optional; merged into thread config):

- `CODEX_NETWORK_ENABLED`: set to `true` or `false` to allow or deny network access
- `CODEX_FS_READ`: comma-separated absolute paths the agent may read
- `CODEX_FS_WRITE`: comma-separated absolute paths the agent may write

All of these also have matching CLI flags. Run `go run . --help` for the full list.

## Running

Default mode:

```bash
go run .
```

Explicit local app-server mode:

```bash
go run . \
  --start-app-server \
  --codex-app-server-command "codex app-server"
```

External app-server mode:

```bash
go run . \
  --no-start-app-server \
  --codex-app-server-ws-url "ws://127.0.0.1:8765"
```

## Telegram Commands

- `/help`: show supported commands
- `/status`: show the current transport, execution mode, model, thread id, working directory, and last token usage
- `/new`: start a new Codex thread for this Telegram chat
- `/reset`: same as `/new`
- `/verbose`: toggle visible intermediate output for this chat
- `/verbose on|off|status`: explicit verbose control
- `/yolo`: toggle YOLO execution mode for this chat and start a fresh thread
- `/yolo on|off|status`: explicit YOLO control
- `/model`: show the current model for this chat
- `/model <name>`: set a per-chat model override and start a fresh thread
- `/model default`: clear the per-chat override and use the configured default model

Any other plain-text message is forwarded to Codex.

## Current Limitations

- only plain-text Telegram messages are supported
- Telegram update intake is global, but work is processed per chat
- one active Codex turn per chat; extra messages during that turn are treated as steering input
- verbose mode sends visible intermediate output as separate Telegram messages while the turn is running
- YOLO mode runs that chat with `approval=never` and `sandbox=danger-full-access`
- model selection is optimistic; `/model <name>` forwards that string to Codex and relies on the server to accept it
- the bot shows Telegram `typing` activity while Codex is generating a reply
- replies are sent after the Codex turn completes, not streamed incrementally to Telegram
- thread state, per-chat YOLO mode, and per-chat model overrides are local file state, not a database

## Verification

```bash
go test ./...
go build ./...
```

If your environment restricts the default Go cache paths, set local cache directories before running those commands:

```bash
env GOCACHE=$PWD/.gocache GOTMPDIR=$PWD/.gotmp GOMODCACHE=$PWD/.gomodcache go test ./...
env GOCACHE=$PWD/.gocache GOTMPDIR=$PWD/.gotmp GOMODCACHE=$PWD/.gomodcache go build ./...
```

## References

- Codex app server: https://github.com/openai/codex/tree/main/codex-rs/app-server
- Telegram Bot API: https://core.telegram.org/bots/api
