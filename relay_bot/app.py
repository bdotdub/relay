from __future__ import annotations

import asyncio
import json
import logging
from pathlib import Path

from relay_bot.codex import CodexAppClient
from relay_bot.config import Settings
from relay_bot.telegram import TelegramBotClient, chunk_message
from relay_bot.transport import JsonRpcError, TransportClosedError


LOGGER = logging.getLogger(__name__)


class RelayApp:
    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._telegram = TelegramBotClient(settings.telegram_bot_token)
        self._codex = CodexAppClient(settings)
        self._thread_ids_by_chat: dict[str, str] = {}
        self._chat_locks: dict[int, asyncio.Lock] = {}

    async def run(self) -> None:
        self._load_state()
        await self._codex.start()
        await self._telegram.delete_webhook(drop_pending_updates=False)

        offset: int | None = None
        try:
            while True:
                updates = await self._telegram.get_updates(
                    offset=offset,
                    timeout_seconds=self._settings.telegram_poll_timeout_seconds,
                    allowed_updates=["message"],
                )
                for update in updates:
                    update_id = update.get("update_id")
                    if isinstance(update_id, int):
                        offset = update_id + 1
                    await self._handle_update(update)
        finally:
            await self._telegram.close()
            await self._codex.close()

    async def _handle_update(self, update: dict[str, object]) -> None:
        message = update.get("message")
        if not isinstance(message, dict):
            return
        chat = message.get("chat")
        if not isinstance(chat, dict):
            return
        chat_id = chat.get("id")
        if not isinstance(chat_id, int):
            return
        if not self._is_chat_allowed(chat_id):
            LOGGER.warning("Ignoring message from unauthorized chat %s", chat_id)
            return

        text = message.get("text")
        if not isinstance(text, str):
            await self._telegram.send_message(
                chat_id,
                "Only plain text messages are supported right now.",
                reply_to_message_id=self._message_id(message),
            )
            return

        if text.startswith("/"):
            await self._handle_command(chat_id, message, text)
            return

        lock = self._chat_locks.setdefault(chat_id, asyncio.Lock())
        async with lock:
            await self._relay_message(chat_id, message, text)

    async def _handle_command(
        self,
        chat_id: int,
        message: dict[str, object],
        text: str,
    ) -> None:
        command = text.split()[0].split("@")[0]
        if command in {"/new", "/reset"}:
            thread_id = await self._codex.new_thread()
            self._thread_ids_by_chat[str(chat_id)] = thread_id
            self._save_state()
            await self._telegram.send_message(
                chat_id,
                f"Started a new Codex thread.\nthread_id={thread_id}",
                reply_to_message_id=self._message_id(message),
            )
            return

        if command == "/status":
            current_thread_id = self._thread_ids_by_chat.get(str(chat_id))
            mode = "stdio subprocess" if self._settings.codex_start_app_server else "websocket"
            status_text = (
                f"Transport: {mode}\n"
                f"Thread: {current_thread_id or '(not started yet)'}\n"
                f"CWD: {self._settings.codex_cwd}"
            )
            await self._telegram.send_message(
                chat_id,
                status_text,
                reply_to_message_id=self._message_id(message),
            )
            return

        if command == "/help":
            help_text = (
                "Send any text message to relay it to Codex.\n"
                "/new or /reset starts a fresh Codex thread.\n"
                "/status shows the current thread mapping."
            )
            await self._telegram.send_message(
                chat_id,
                help_text,
                reply_to_message_id=self._message_id(message),
            )
            return

        await self._telegram.send_message(
            chat_id,
            "Unknown command. Use /help for the supported commands.",
            reply_to_message_id=self._message_id(message),
        )

    async def _relay_message(
        self,
        chat_id: int,
        message: dict[str, object],
        text: str,
    ) -> None:
        current_thread_id = self._thread_ids_by_chat.get(str(chat_id))
        thread_id = await self._codex.ensure_thread(current_thread_id)
        if thread_id != current_thread_id:
            self._thread_ids_by_chat[str(chat_id)] = thread_id
            self._save_state()

        try:
            result = await self._codex.run_turn(thread_id, text)
        except (JsonRpcError, TransportClosedError) as exc:
            await self._telegram.send_message(
                chat_id,
                f"Codex relay error: {exc}",
                reply_to_message_id=self._message_id(message),
            )
            return

        reply_text = result.text
        if result.error_message:
            prefix = f"Codex reported an error: {result.error_message}"
            reply_text = f"{prefix}\n\n{reply_text}" if reply_text else prefix
        if not reply_text:
            reply_text = "Codex completed the turn without returning assistant text."

        chunks = chunk_message(reply_text, self._settings.telegram_message_chunk_size)
        for index, chunk in enumerate(chunks):
            await self._telegram.send_message(
                chat_id,
                chunk,
                reply_to_message_id=self._message_id(message) if index == 0 else None,
            )

    def _is_chat_allowed(self, chat_id: int) -> bool:
        allowed_chat_ids = self._settings.telegram_allowed_chat_ids
        return allowed_chat_ids is None or chat_id in allowed_chat_ids

    def _load_state(self) -> None:
        self._thread_ids_by_chat = _load_json_mapping(self._settings.state_path)

    def _save_state(self) -> None:
        payload = json.dumps(self._thread_ids_by_chat, indent=2, sort_keys=True)
        self._settings.state_path.parent.mkdir(parents=True, exist_ok=True)
        self._settings.state_path.write_text(payload + "\n", encoding="utf-8")

    @staticmethod
    def _message_id(message: dict[str, object]) -> int | None:
        message_id = message.get("message_id")
        return message_id if isinstance(message_id, int) else None


def _load_json_mapping(path: Path) -> dict[str, str]:
    if not path.exists():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        LOGGER.warning("State file %s is not valid JSON. Ignoring it.", path)
        return {}
    if not isinstance(data, dict):
        LOGGER.warning("State file %s does not contain a JSON object. Ignoring it.", path)
        return {}
    mapping: dict[str, str] = {}
    for key, value in data.items():
        if isinstance(key, str) and isinstance(value, str):
            mapping[key] = value
    return mapping
