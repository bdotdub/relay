# Telegram Codex Relay

Small Python service that relays Telegram bot messages into the Codex app server and sends Codex replies back to Telegram.

Default behavior:

- starts `codex app-server` as a local subprocess over `stdio`
- creates one Codex thread per Telegram chat
- persists the chat-to-thread mapping in `.relay-state.json`

Optional behavior:

- connect to an already-running Codex app server over WebSocket instead of starting one

## Why the Telegram Bot API

For a Telegram bot, the Bot API is the practical transport. This relay uses long polling with `deleteWebhook`, `getUpdates`, and `sendMessage`.

## Requirements

- Python 3.11+
- a Telegram bot token from BotFather
- Codex CLI installed and authenticated

## Install

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -e .
```

## Configure

```bash
export TELEGRAM_BOT_TOKEN="123456:abc..."
export TELEGRAM_ALLOWED_CHAT_IDS="123456789"
```

Optional Codex settings:

```bash
export CODEX_CWD="/Users/benny/Development/python/relay"
export CODEX_MODEL="gpt-5-codex"
export CODEX_APPROVAL_POLICY="never"
export CODEX_SANDBOX="workspace-write"
```

## Run

Default mode, which auto-starts the Codex app server:

```bash
telegram-codex-relay
```

Equivalent explicit form:

```bash
telegram-codex-relay --start-app-server --codex-app-server-command "codex app-server"
```

External app-server mode:

```bash
telegram-codex-relay \
  --no-start-app-server \
  --codex-app-server-ws-url "ws://127.0.0.1:8765"
```

## Bot Commands

- `/help` shows the supported commands
- `/status` shows the current relay status for that chat
- `/new` or `/reset` starts a fresh Codex thread for that chat

Any other plain text message is forwarded to Codex.

## Notes

- Only plain text messages are handled right now.
- Each Telegram chat maps to a single Codex thread.
- If the Codex app server restarts, the relay will try to resume saved thread ids.
- Telegram messages are chunked before sending to stay below Telegram's message size limit.

## Upstream References

- Codex app server: https://github.com/openai/codex/tree/main/codex-rs/app-server
- Telegram Bot API: https://core.telegram.org/bots/api
