from __future__ import annotations

import asyncio
import json
import logging
from abc import ABC, abstractmethod
from collections.abc import Mapping
from dataclasses import dataclass


LOGGER = logging.getLogger(__name__)


class TransportClosedError(RuntimeError):
    """Raised when the JSON-RPC transport closes unexpectedly."""


class JsonRpcError(RuntimeError):
    """Raised when the remote JSON-RPC peer returns an error."""

    def __init__(self, payload: Mapping[str, object]) -> None:
        super().__init__(str(payload.get("message", "Unknown JSON-RPC error")))
        self.payload = dict(payload)


@dataclass
class _PendingRequest:
    future: asyncio.Future[dict[str, object]]


class JsonRpcTransport(ABC):
    def __init__(self) -> None:
        self.notifications: asyncio.Queue[dict[str, object]] = asyncio.Queue()
        self._pending: dict[int, _PendingRequest] = {}
        self._request_id = 0
        self._closed = False

    async def request(
        self, method: str, params: Mapping[str, object] | None = None
    ) -> dict[str, object]:
        self._ensure_open()
        self._request_id += 1
        request_id = self._request_id
        future: asyncio.Future[dict[str, object]] = asyncio.get_running_loop().create_future()
        self._pending[request_id] = _PendingRequest(future=future)
        payload: dict[str, object] = {
            "jsonrpc": "2.0",
            "id": request_id,
            "method": method,
        }
        if params is not None:
            payload["params"] = dict(params)
        await self._send(payload)
        return await future

    async def notify(
        self, method: str, params: Mapping[str, object] | None = None
    ) -> None:
        self._ensure_open()
        payload: dict[str, object] = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            payload["params"] = dict(params)
        await self._send(payload)

    async def _handle_message(self, message: dict[str, object]) -> None:
        if "id" in message:
            request_id = int(message["id"])
            pending = self._pending.pop(request_id, None)
            if pending is None:
                LOGGER.warning("Discarding response for unknown request id %s", request_id)
                return
            if "error" in message:
                error_payload = message.get("error")
                if isinstance(error_payload, Mapping):
                    pending.future.set_exception(JsonRpcError(error_payload))
                else:
                    pending.future.set_exception(
                        JsonRpcError({"message": "Malformed JSON-RPC error payload"})
                    )
                return
            result = message.get("result")
            if isinstance(result, Mapping):
                pending.future.set_result(dict(result))
                return
            pending.future.set_result({})
            return

        if "method" in message:
            await self.notifications.put(message)

    def _fail_pending(self, exc: Exception) -> None:
        for pending in self._pending.values():
            if not pending.future.done():
                pending.future.set_exception(exc)
        self._pending.clear()

    def _ensure_open(self) -> None:
        if self._closed:
            raise TransportClosedError("The JSON-RPC transport is closed.")

    @abstractmethod
    async def _send(self, payload: Mapping[str, object]) -> None:
        raise NotImplementedError

    @abstractmethod
    async def close(self) -> None:
        raise NotImplementedError


class StdioJsonRpcTransport(JsonRpcTransport):
    def __init__(self, command: tuple[str, ...]) -> None:
        super().__init__()
        self._command = command
        self._process: asyncio.subprocess.Process | None = None
        self._stdout_task: asyncio.Task[None] | None = None
        self._stderr_task: asyncio.Task[None] | None = None

    async def start(self) -> None:
        self._process = await asyncio.create_subprocess_exec(
            *self._command,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        self._stdout_task = asyncio.create_task(self._read_stdout())
        self._stderr_task = asyncio.create_task(self._read_stderr())

    async def _read_stdout(self) -> None:
        assert self._process is not None
        assert self._process.stdout is not None
        try:
            while not self._process.stdout.at_eof():
                raw_line = await self._process.stdout.readline()
                if not raw_line:
                    break
                line = raw_line.decode("utf-8").strip()
                if not line:
                    continue
                message = json.loads(line)
                if isinstance(message, dict):
                    await self._handle_message(message)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            self._fail_pending(exc)
            raise
        finally:
            self._closed = True
            self._fail_pending(TransportClosedError("Codex app-server stdout closed."))

    async def _read_stderr(self) -> None:
        assert self._process is not None
        assert self._process.stderr is not None
        while not self._process.stderr.at_eof():
            raw_line = await self._process.stderr.readline()
            if not raw_line:
                break
            LOGGER.info("codex-app-server: %s", raw_line.decode("utf-8").rstrip())

    async def _send(self, payload: Mapping[str, object]) -> None:
        assert self._process is not None
        assert self._process.stdin is not None
        encoded = (json.dumps(payload, separators=(",", ":")) + "\n").encode("utf-8")
        self._process.stdin.write(encoded)
        await self._process.stdin.drain()

    async def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        if self._stdout_task is not None:
            self._stdout_task.cancel()
        if self._stderr_task is not None:
            self._stderr_task.cancel()
        if self._process is not None:
            if self._process.stdin is not None:
                self._process.stdin.close()
            if self._process.returncode is None:
                self._process.terminate()
                try:
                    await asyncio.wait_for(self._process.wait(), timeout=5)
                except asyncio.TimeoutError:
                    self._process.kill()
                    await self._process.wait()


class WebSocketJsonRpcTransport(JsonRpcTransport):
    def __init__(self, url: str) -> None:
        super().__init__()
        self._url = url
        self._connection = None
        self._reader_task: asyncio.Task[None] | None = None

    async def start(self) -> None:
        import websockets

        self._connection = await websockets.connect(self._url)
        self._reader_task = asyncio.create_task(self._read_messages())

    async def _read_messages(self) -> None:
        assert self._connection is not None
        try:
            async for raw_message in self._connection:
                if isinstance(raw_message, bytes):
                    text = raw_message.decode("utf-8")
                else:
                    text = raw_message
                message = json.loads(text)
                if isinstance(message, dict):
                    await self._handle_message(message)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            self._fail_pending(exc)
            raise
        finally:
            self._closed = True
            self._fail_pending(TransportClosedError("Codex app-server WebSocket closed."))

    async def _send(self, payload: Mapping[str, object]) -> None:
        assert self._connection is not None
        await self._connection.send(json.dumps(payload, separators=(",", ":")))

    async def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        if self._reader_task is not None:
            self._reader_task.cancel()
        if self._connection is not None:
            await self._connection.close()
