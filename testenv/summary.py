#!/usr/bin/env python3
"""Print where the running lab listens and which accounts it uses.

Passwords and tokens are deliberately reported by variable name only: lab
output regularly ends up pasted into issues and pull requests.
"""

from __future__ import annotations

import argparse
from pathlib import Path
import sys

from render_config import load_env


def value(env: dict[str, str], key: str, default: str) -> str:
    return env.get(key, "").strip() or default


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--env", type=Path, required=True)
    args = parser.parse_args()

    try:
        env = load_env(args.env)
    except (OSError, ValueError) as exc:
        print(f"configuration error: {exc}", file=sys.stderr)
        return 2

    backend = value(env, "SLSKDARR_BACKEND", "soulseek")
    slskd_listen = value(env, "SLSKD_LISTEN_PORT", "50300")
    native_listen = value(env, "SLSKDARR_LISTEN_PORT", "50301")

    services = [
        (
            "slskdarr",
            f"http://127.0.0.1:{value(env, 'SLSKDARR_PORT', '9090')}",
            "Basic auth: any username, password = $SLSKDARR_OBSERV_TOKEN",
        ),
        (
            "Lidarr",
            f"http://127.0.0.1:{value(env, 'LIDARR_PORT', '8686')}",
            "no authentication in the lab",
        ),
        (
            "slskd",
            f"http://127.0.0.1:{value(env, 'SLSKD_WEB_PORT', '5030')}",
            f"{value(env, 'SLSKD_WEB_USERNAME', 'slskd')} / $SLSKD_WEB_PASSWORD",
        ),
        (
            "Postgres",
            f"127.0.0.1:{value(env, 'POSTGRES_PORT', '15432')}",
            "slskdarr / slskdarr-test, database slskdarr",
        ),
    ]
    accounts = [
        ("slskd", value(env, "SLSKD_SOULSEEK_USERNAME", "<unset>"), slskd_listen),
        ("slskdarr", value(env, "SLSKDARR_SOULSEEK_USERNAME", "<unset>"), native_listen),
    ]

    print("\nPR lab is ready.\n")
    print("Services")
    name_width = max(len(name) for name, _, _ in services)
    address_width = max(len(address) for _, address, _ in services)
    for name, address, note in services:
        print(f"  {name:<{name_width}}   {address:<{address_width}}   {note}")

    print("\nSoulseek accounts (Soulseek allows one login per account, so these must differ)")
    account_width = max(len(account) for _, account, _ in accounts)
    for name, account, listen_port in accounts:
        print(f"  {name:<{name_width}}   {account:<{account_width}}   listening on 0.0.0.0:{listen_port}")

    print(
        f"""
  Both clients log in, but SLSKDARR_BACKEND={backend} drives the pipeline jobs.
  Ports {slskd_listen} and {native_listen} are published on all interfaces because Soulseek
  peers must connect inbound. Web and database ports are bound to loopback.

Credentials stay in testenv/.env and are not printed here:
  eval "$(grep '^SLSKDARR_OBSERV_TOKEN=' testenv/.env)"

Next
  testenv/lab.sh logs slskdarr   follow the pipeline
  testenv/lab.sh info            print this again
"""
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
