from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

from relay_bot.config import Settings
from relay_bot.transport import (
    JsonRpcError,
    JsonRpcTransport,
    StdioJsonRpcTransport,
    WebSocketJsonRpcTransport,
)


LOGGER = logging.getLogger(__name__)
DeltaHandler = Callable[[str], Awaitable[None] | None]


@dataclass
class TurnResult:
    thread_id: str
    turn_id: str
    text: str
    error_message: str | None = None


@dataclass
class _TurnSubscription:
    thread_id: str
    turn_id: str | None = None
    queue: asyncio.Queue[dict[str, object]] = field(default_factory=asyncio.Queue)


class CodexAppClient:
    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._transport: JsonRpcTransport | None = None
        self._dispatcher_task: asyncio.Task[None] | None = None
        self._active_turns_by_thread: dict[str, _TurnSubscription] = {}
        self._thread_locks: dict[str, asyncio.Lock] = {}
        self._loaded_threads: set[str] = set()

    async def start(self) -> None:
        if self._settings.codex_start_app_server:
            transport = StdioJsonRpcTransport(self._settings.codex_app_server_command)
            await transport.start()
        else:
            assert self._settings.codex_app_server_ws_url is not None
            transport = WebSocketJsonRpcTransport(self._settings.codex_app_server_ws_url)
            await transport.start()
        self._transport = transport
        await self._transport.request(
            "initialize",
            {
                "clientInfo": {
                    "name": "telegram-codex-relay",
                    "version": "0.1.0",
                }
            },
        )
        await self._transport.notify("initialized")
        self._dispatcher_task = asyncio.create_task(self._dispatch_notifications())

    async def close(self) -> None:
        if self._dispatcher_task is not None:
            self._dispatcher_task.cancel()
        if self._transport is not None:
            await self._transport.close()

    async def new_thread(self) -> str:
        response = await self._request("thread/start", self._new_thread_params())
        thread_id = str(response["thread"]["id"])
        self._loaded_threads.add(thread_id)
        return thread_id

    async def ensure_thread(self, thread_id: str | None) -> str:
        if not thread_id:
            return await self.new_thread()
        if thread_id in self._loaded_threads:
            return thread_id
        try:
            await self._request(
                "thread/resume",
                {
                    **self._resume_thread_params(),
                    "threadId": thread_id,
                },
            )
            self._loaded_threads.add(thread_id)
            return thread_id
        except JsonRpcError:
            LOGGER.warning("Failed to resume thread %s. Starting a new thread.", thread_id)
            return await self.new_thread()

    async def run_turn(
        self,
        thread_id: str,
        user_text: str,
        *,
        on_delta: DeltaHandler | None = None,
    ) -> TurnResult:
        lock = self._thread_locks.setdefault(thread_id, asyncio.Lock())
        async with lock:
            subscription = _TurnSubscription(thread_id=thread_id)
            self._active_turns_by_thread[thread_id] = subscription
            try:
                response = await self._request(
                    "turn/start",
                    {
                        "threadId": thread_id,
                        "input": [{"type": "text", "text": user_text}],
                    },
                )
                subscription.turn_id = str(response["turn"]["id"])
                result = await self._collect_turn_result(subscription, on_delta=on_delta)
                return result
            finally:
                self._active_turns_by_thread.pop(thread_id, None)

    async def _collect_turn_result(
        self,
        subscription: _TurnSubscription,
        *,
        on_delta: DeltaHandler | None,
    ) -> TurnResult:
        delta_parts: list[str] = []
        completed_messages: list[tuple[str | None, str]] = []
        error_message: str | None = None
        while True:
            message = await subscription.queue.get()
            method = message.get("method")
            params = message.get("params")
            if not isinstance(params, dict):
                continue

            if method == "item/agentMessage/delta":
                delta = str(params.get("delta", ""))
                if delta:
                    delta_parts.append(delta)
                    if on_delta is not None:
                        maybe_coro = on_delta("".join(delta_parts))
                        if maybe_coro is not None:
                            await maybe_coro
                continue

            if method == "item/completed":
                item = params.get("item")
                if isinstance(item, dict) and item.get("type") == "agentMessage":
                    completed_messages.append(
                        (
                            item.get("phase") if isinstance(item.get("phase"), str) else None,
                            str(item.get("text", "")),
                        )
                    )
                continue

            if method == "error":
                error = params.get("error")
                if isinstance(error, dict):
                    error_message = str(error.get("message", "Codex turn failed."))
                else:
                    error_message = "Codex turn failed."
                continue

            if method == "turn/completed":
                turn = params.get("turn")
                if isinstance(turn, dict) and turn.get("status") == "failed" and not error_message:
                    error = turn.get("error")
                    if isinstance(error, dict):
                        error_message = str(error.get("message", "Codex turn failed."))
                    else:
                        error_message = "Codex turn failed."
                break

        final_messages = [text for phase, text in completed_messages if phase == "final_answer"]
        text = "\n\n".join(part.strip() for part in final_messages if part.strip())
        if not text:
            text = "\n\n".join(part.strip() for _, part in completed_messages if part.strip())
        if not text:
            text = "".join(delta_parts).strip()
        return TurnResult(
            thread_id=subscription.thread_id,
            turn_id=subscription.turn_id or "",
            text=text,
            error_message=error_message,
        )

    async def _dispatch_notifications(self) -> None:
        assert self._transport is not None
        while True:
            message = await self._transport.notifications.get()
            params = message.get("params")
            if not isinstance(params, dict):
                continue
            thread_id = params.get("threadId")
            if not isinstance(thread_id, str):
                continue
            subscription = self._active_turns_by_thread.get(thread_id)
            if subscription is None:
                continue
            turn_id = params.get("turnId")
            if subscription.turn_id is not None and turn_id not in {None, subscription.turn_id}:
                continue
            await subscription.queue.put(message)

    async def _request(self, method: str, params: dict[str, Any]) -> dict[str, object]:
        assert self._transport is not None
        return await self._transport.request(method, params)

    def _base_thread_params(self) -> dict[str, Any]:
        params: dict[str, Any] = {
            "cwd": str(self._settings.codex_cwd),
        }
        if self._settings.codex_approval_policy is not None:
            params["approvalPolicy"] = self._settings.codex_approval_policy
        if self._settings.codex_sandbox is not None:
            params["sandbox"] = self._settings.codex_sandbox
        if self._settings.codex_model is not None:
            params["model"] = self._settings.codex_model
        if self._settings.codex_personality is not None:
            params["personality"] = self._settings.codex_personality
        if self._settings.codex_service_tier is not None:
            params["serviceTier"] = self._settings.codex_service_tier
        if self._settings.codex_base_instructions is not None:
            params["baseInstructions"] = self._settings.codex_base_instructions
        if self._settings.codex_developer_instructions is not None:
            params["developerInstructions"] = self._settings.codex_developer_instructions
        if self._settings.codex_config is not None:
            params["config"] = self._settings.codex_config
        return params

    def _new_thread_params(self) -> dict[str, Any]:
        params = self._base_thread_params()
        if self._settings.codex_ephemeral_threads is not None:
            params["ephemeral"] = self._settings.codex_ephemeral_threads
        return params

    def _resume_thread_params(self) -> dict[str, Any]:
        return self._base_thread_params()
