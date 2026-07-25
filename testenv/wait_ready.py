#!/usr/bin/env python3
"""Wait until the locally published slskdarr health endpoint responds."""

from __future__ import annotations

import argparse
from pathlib import Path
import time
from urllib.error import HTTPError, URLError
from urllib.request import urlopen

from render_config import load_env


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--env", type=Path, required=True)
    parser.add_argument("--timeout", type=int, default=120)
    args = parser.parse_args()

    env = load_env(args.env)
    port = env.get("SLSKDARR_PORT", "9090")
    url = f"http://127.0.0.1:{port}/healthz"
    deadline = time.monotonic() + args.timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            with urlopen(url, timeout=3) as response:
                if response.status == 200:
                    print(f"slskdarr is healthy at {url}")
                    return 0
        except (HTTPError, OSError, URLError) as exc:
            last_error = exc
        time.sleep(2)
    print(f"slskdarr did not become healthy at {url}: {last_error}")
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
