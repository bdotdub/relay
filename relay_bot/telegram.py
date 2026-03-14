from __future__ import annotations

from collections.abc import Sequence


class TelegramApiError(RuntimeError):
    """Raised when the Telegram Bot API returns an error."""


class TelegramBotClient:
    def __init__(self, token: str) -> None:
        import httpx

        self._client = httpx.AsyncClient(
            base_url=f"https://api.telegram.org/bot{token}/",
            timeout=httpx.Timeout(60.0, connect=10.0),
        )

    async def close(self) -> None:
        await self._client.aclose()

    async def delete_webhook(self, drop_pending_updates: bool = False) -> None:
        await self._call(
            "deleteWebhook",
            {"drop_pending_updates": drop_pending_updates},
        )

    async def get_updates(
        self,
        *,
        offset: int | None,
        timeout_seconds: int,
        allowed_updates: Sequence[str] | None = None,
    ) -> list[dict[str, object]]:
        payload: dict[str, object] = {
            "timeout": timeout_seconds,
        }
        if offset is not None:
            payload["offset"] = offset
        if allowed_updates is not None:
            payload["allowed_updates"] = list(allowed_updates)
        result = await self._call("getUpdates", payload)
        if isinstance(result, list):
            return [item for item in result if isinstance(item, dict)]
        return []

    async def send_message(
        self,
        chat_id: int,
        text: str,
        *,
        reply_to_message_id: int | None = None,
    ) -> dict[str, object]:
        payload: dict[str, object] = {
            "chat_id": chat_id,
            "text": text,
            "disable_web_page_preview": True,
        }
        if reply_to_message_id is not None:
            payload["reply_to_message_id"] = reply_to_message_id
        result = await self._call("sendMessage", payload)
        if isinstance(result, dict):
            return result
        return {}

    async def _call(self, method: str, payload: dict[str, object]) -> object:
        response = await self._client.post(method, json=payload)
        response.raise_for_status()
        body = response.json()
        if not body.get("ok"):
            raise TelegramApiError(body.get("description", "Telegram API request failed."))
        return body.get("result")


def chunk_message(text: str, limit: int) -> list[str]:
    stripped = text.strip()
    if not stripped:
        return ["(empty response)"]
    if len(stripped) <= limit:
        return [stripped]

    chunks: list[str] = []
    current = ""
    for paragraph in stripped.split("\n\n"):
        candidate = paragraph.strip()
        if not candidate:
            continue
        candidate_with_spacing = candidate if not current else f"{current}\n\n{candidate}"
        if len(candidate_with_spacing) <= limit:
            current = candidate_with_spacing
            continue
        if current:
            chunks.append(current)
            current = ""
        if len(candidate) <= limit:
            current = candidate
            continue
        for line in candidate.splitlines():
            line = line.strip()
            if not line:
                continue
            joined = line if not current else f"{current}\n{line}"
            if len(joined) <= limit:
                current = joined
                continue
            if current:
                chunks.append(current)
                current = ""
            while len(line) > limit:
                chunks.append(line[:limit])
                line = line[limit:]
            current = line
    if current:
        chunks.append(current)
    return chunks
