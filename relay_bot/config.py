from __future__ import annotations

import argparse
import json
import os
import shlex
from dataclasses import dataclass
from pathlib import Path


def _env_bool(name: str, default: bool) -> bool:
    value = os.getenv(name)
    if value is None:
        return default
    return value.strip().lower() in {"1", "true", "yes", "on"}


def _parse_allowed_chat_ids(raw: str | None) -> frozenset[int] | None:
    if not raw:
        return None
    values = []
    for part in raw.split(","):
        part = part.strip()
        if not part:
            continue
        values.append(int(part))
    return frozenset(values)


def _parse_json(raw: str | None) -> dict[str, object] | None:
    if not raw:
        return None
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise ValueError("CODEX_CONFIG_JSON must decode to a JSON object.")
    return value


@dataclass(frozen=True)
class Settings:
    telegram_bot_token: str
    telegram_allowed_chat_ids: frozenset[int] | None
    telegram_poll_timeout_seconds: int
    telegram_message_chunk_size: int
    state_path: Path
    codex_cwd: Path
    codex_start_app_server: bool
    codex_app_server_command: tuple[str, ...]
    codex_app_server_ws_url: str | None
    codex_model: str | None
    codex_personality: str | None
    codex_sandbox: str | None
    codex_approval_policy: str | None
    codex_service_tier: str | None
    codex_base_instructions: str | None
    codex_developer_instructions: str | None
    codex_config: dict[str, object] | None
    codex_ephemeral_threads: bool | None


def parse_args(argv: list[str] | None = None) -> Settings:
    parser = argparse.ArgumentParser(
        description="Relay Telegram bot messages to the Codex app server."
    )
    parser.add_argument(
        "--telegram-bot-token",
        default=os.getenv("TELEGRAM_BOT_TOKEN"),
        help="Telegram bot token. Defaults to TELEGRAM_BOT_TOKEN.",
    )
    parser.add_argument(
        "--telegram-allowed-chat-ids",
        default=os.getenv("TELEGRAM_ALLOWED_CHAT_IDS"),
        help="Comma-separated list of chat ids allowed to talk to the bot.",
    )
    parser.add_argument(
        "--telegram-poll-timeout-seconds",
        type=int,
        default=int(os.getenv("TELEGRAM_POLL_TIMEOUT_SECONDS", "30")),
        help="Long-poll timeout passed to getUpdates.",
    )
    parser.add_argument(
        "--telegram-message-chunk-size",
        type=int,
        default=int(os.getenv("TELEGRAM_MESSAGE_CHUNK_SIZE", "3900")),
        help="Maximum size for each outgoing Telegram message chunk.",
    )
    parser.add_argument(
        "--state-path",
        default=os.getenv("RELAY_STATE_PATH", ".relay-state.json"),
        help="Path to the chat/thread mapping file.",
    )
    parser.add_argument(
        "--codex-cwd",
        default=os.getenv("CODEX_CWD", os.getcwd()),
        help="Working directory used for new Codex threads.",
    )
    start_app_server_default = _env_bool("CODEX_START_APP_SERVER", True)
    parser.add_argument(
        "--start-app-server",
        dest="codex_start_app_server",
        action="store_true",
        default=start_app_server_default,
        help="Start a Codex app-server subprocess. Default: enabled.",
    )
    parser.add_argument(
        "--no-start-app-server",
        dest="codex_start_app_server",
        action="store_false",
        help="Do not start a subprocess. Requires --codex-app-server-ws-url.",
    )
    parser.add_argument(
        "--codex-app-server-command",
        default=os.getenv("CODEX_APP_SERVER_COMMAND", "codex app-server"),
        help="Command used when auto-starting the Codex app server.",
    )
    parser.add_argument(
        "--codex-app-server-ws-url",
        default=os.getenv("CODEX_APP_SERVER_WS_URL"),
        help="WebSocket URL for an already-running Codex app server.",
    )
    parser.add_argument(
        "--codex-model",
        default=os.getenv("CODEX_MODEL"),
        help="Optional Codex model override.",
    )
    parser.add_argument(
        "--codex-personality",
        default=os.getenv("CODEX_PERSONALITY", "pragmatic"),
        help="Optional Codex personality override.",
    )
    parser.add_argument(
        "--codex-sandbox",
        default=os.getenv("CODEX_SANDBOX", "workspace-write"),
        help="Sandbox mode for thread/start and thread/resume.",
    )
    parser.add_argument(
        "--codex-approval-policy",
        default=os.getenv("CODEX_APPROVAL_POLICY", "never"),
        help="Approval policy for the relay's Codex turns.",
    )
    parser.add_argument(
        "--codex-service-tier",
        default=os.getenv("CODEX_SERVICE_TIER"),
        help="Optional service tier override.",
    )
    parser.add_argument(
        "--codex-base-instructions",
        default=os.getenv("CODEX_BASE_INSTRUCTIONS"),
        help="Optional base instructions for new threads.",
    )
    parser.add_argument(
        "--codex-developer-instructions",
        default=os.getenv("CODEX_DEVELOPER_INSTRUCTIONS"),
        help="Optional developer instructions for new threads.",
    )
    parser.add_argument(
        "--codex-config-json",
        default=os.getenv("CODEX_CONFIG_JSON"),
        help="Optional JSON object passed through as the thread config.",
    )
    parser.add_argument(
        "--codex-ephemeral-threads",
        action="store_true",
        default=_env_bool("CODEX_EPHEMERAL_THREADS", False),
        help="Create ephemeral threads instead of persisted ones.",
    )
    parser.add_argument(
        "--no-codex-ephemeral-threads",
        dest="codex_ephemeral_threads",
        action="store_false",
        help="Persist threads on disk in the Codex app server.",
    )
    args = parser.parse_args(argv)

    if not args.telegram_bot_token:
        parser.error("--telegram-bot-token or TELEGRAM_BOT_TOKEN is required.")

    if not args.codex_start_app_server and not args.codex_app_server_ws_url:
        parser.error(
            "--no-start-app-server requires --codex-app-server-ws-url "
            "or CODEX_APP_SERVER_WS_URL."
        )

    try:
        codex_config = _parse_json(args.codex_config_json)
    except ValueError as exc:
        parser.error(str(exc))

    return Settings(
        telegram_bot_token=args.telegram_bot_token,
        telegram_allowed_chat_ids=_parse_allowed_chat_ids(
            args.telegram_allowed_chat_ids
        ),
        telegram_poll_timeout_seconds=args.telegram_poll_timeout_seconds,
        telegram_message_chunk_size=args.telegram_message_chunk_size,
        state_path=Path(args.state_path).expanduser().resolve(),
        codex_cwd=Path(args.codex_cwd).expanduser().resolve(),
        codex_start_app_server=args.codex_start_app_server,
        codex_app_server_command=tuple(
            shlex.split(args.codex_app_server_command.strip())
        ),
        codex_app_server_ws_url=args.codex_app_server_ws_url,
        codex_model=args.codex_model,
        codex_personality=args.codex_personality,
        codex_sandbox=args.codex_sandbox,
        codex_approval_policy=args.codex_approval_policy,
        codex_service_tier=args.codex_service_tier,
        codex_base_instructions=args.codex_base_instructions,
        codex_developer_instructions=args.codex_developer_instructions,
        codex_config=codex_config,
        codex_ephemeral_threads=args.codex_ephemeral_threads,
    )
