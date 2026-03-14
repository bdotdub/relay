# Telegram Codex Relay

Go service that relays messages between a Telegram bot and the Codex app server.

## What It Does

- receives Telegram bot messages through long polling
- sends each plain-text message to the Codex app server as a new turn
- sends the Codex reply back to Telegram
- keeps one Codex thread per Telegram chat
- saves the chat-to-thread mapping in `.relay-state.json`

By default the relay starts `codex app-server` itself over `stdio`. It can also connect to an already-running app server over WebSocket.

## Conversation Model

The relay keeps context per Telegram chat.

- first message from a chat: create a new Codex thread
- later messages from the same chat: reuse that same thread
- `/new` or `/reset`: discard the saved mapping for that chat and start a fresh thread
- if a saved thread cannot be resumed: fall back to a new thread automatically

In practice this means the relay keeps a session going for each chat until you reset it.

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
- `RELAY_STATE_PATH`: optional path for the chat/thread mapping file
- `CODEX_CWD`: working directory for Codex threads

Codex process settings:

- `CODEX_START_APP_SERVER`: defaults to `true`
- `CODEX_APP_SERVER_COMMAND`: defaults to `codex app-server`
- `CODEX_APP_SERVER_WS_URL`: required only when not starting the app server locally

Optional Codex turn settings:

- `CODEX_MODEL`
- `CODEX_PERSONALITY`
- `CODEX_SANDBOX`
- `CODEX_APPROVAL_POLICY`
- `CODEX_SERVICE_TIER`
- `CODEX_BASE_INSTRUCTIONS`
- `CODEX_DEVELOPER_INSTRUCTIONS`
- `CODEX_CONFIG_JSON`
- `CODEX_EPHEMERAL_THREADS`

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
- `/status`: show the current transport, thread id, and working directory
- `/new`: start a new Codex thread for this Telegram chat
- `/reset`: same as `/new`

Any other plain-text message is forwarded to Codex.

## Current Limitations

- only plain-text Telegram messages are supported
- updates are processed sequentially
- replies are sent after the Codex turn completes, not streamed incrementally to Telegram
- thread state is local file state, not a database

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
