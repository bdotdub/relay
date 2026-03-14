from __future__ import annotations

import asyncio
import logging

from relay_bot.app import RelayApp
from relay_bot.config import parse_args


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    settings = parse_args()
    asyncio.run(RelayApp(settings).run())


if __name__ == "__main__":
    main()
