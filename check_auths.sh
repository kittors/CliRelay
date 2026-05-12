#!/bin/bash
set -euo pipefail

BASE="${CLIRELAY_BASE_URL:-https://cliapi.029110.xyz}"
ADMIN_TOKEN="${CLIRELAY_ADMIN_TOKEN:-}"
SERVER_SSH_TARGET="${CLIRELAY_SERVER_SSH_TARGET:-}"

if [ -z "$ADMIN_TOKEN" ]; then
  echo "CLIRELAY_ADMIN_TOKEN is required" >&2
  exit 1
fi

AUTH_HEADER="Authorization: Bearer ${ADMIN_TOKEN}"

echo "=== Check public root ==="
curl -k -sS -i "$BASE/" | head -10

echo -e "\n=== List auth files summary ==="
curl -k -sS "$BASE/v0/management/auth-files" -H "$AUTH_HEADER" > /tmp/auths.json
python3 -c "
import json
with open('/tmp/auths.json') as f:
    data = json.load(f)
files = data if isinstance(data, list) else data.get('data', data.get('files', []))
print(f'Total: {len(files)}')
by_status = {}
for f in files:
    s = f.get('status','unknown')
    unavail = f.get('unavailable',False)
    key = f'status={s}, unavailable={unavail}'
    by_status.setdefault(key, []).append(f)
for k, fs in by_status.items():
    print(f'- {k}: {len(fs)}')
    if k != 'status=active, unavailable=False':
        for f in fs[:3]:
            print(f'  * {f.get(\"provider\")}: {f.get(\"label\")}')
" 2>/dev/null || echo "Could not parse auths"

echo -e "\n=== Check server containers ==="
if [ -z "$SERVER_SSH_TARGET" ]; then
  echo "CLIRELAY_SERVER_SSH_TARGET not set, skip server container check"
  exit 0
fi

ssh -o ConnectTimeout=5 -o StrictHostKeyChecking=no "$SERVER_SSH_TARGET" "
cd /opt/clirelay
podman ps -a --format 'table {{.Names}}\t{{.Status}}'
echo -e '\n--- cli-proxy-api logs (last 20 lines) ---'
podman logs --tail=20 cli-proxy-api
echo -e '\n--- Check compose status ---'
podman compose ps 2>&1 || docker compose ps 2>&1
" 2>&1
