#!/usr/bin/env python3
import json
import os

import requests

BASE = os.environ.get("CLIRELAY_BASE_URL", "https://cliapi.029110.xyz").rstrip("/")
ADMIN_TOKEN = os.environ.get("CLIRELAY_ADMIN_TOKEN", "")


def main():
    if not ADMIN_TOKEN:
        raise SystemExit("CLIRELAY_ADMIN_TOKEN is required")

    auth_header = {"Authorization": f"Bearer {ADMIN_TOKEN}"}
    resp = requests.get(f"{BASE}/v0/management/auth-files", headers=auth_header, verify=False)
    resp.raise_for_status()
    data = resp.json()
    files = data if isinstance(data, list) else data.get("data", data.get("files", []))
    print(f"Total auth files: {len(files)}")
    print()
    for f in files:
        provider = f.get("provider")
        label = f.get("label")
        status = f.get("status")
        unavailable = f.get("unavailable", False)
        msg = f.get("status_message", "")
        if status == "disabled" or unavailable:
            print(f"{provider:12} | {label:40} | status={status:10} | unavailable={unavailable}")
    print()

    print("Checking recent logs...")
    log_resp = requests.get(
        f"{BASE}/v0/management/logs",
        params={"page": 1, "size": 30, "days": 0},
        headers=auth_header,
        verify=False,
    )
    log_resp.raise_for_status()
    logs = log_resp.json().get("logs", [])
    if logs:
        for l in logs[-15:]:
            print(f"{l.get('timestamp')} {l.get('level'):8} {l.get('message')[:150]}")


if __name__ == "__main__":
    main()
