#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="${SERVICE_NAME:-clirelay2}"
BASE_DIR="${BASE_DIR:-/opt/clirelay2}"
PORT_A="${PORT_A:-8318}"
PORT_B="${PORT_B:-8319}"
ACTIVE_PORT_FILE="${ACTIVE_PORT_FILE:-${BASE_DIR}/.active-port}"

old_port="${1:?old port is required}"
expected_active_port="${2:?expected active port is required}"

case "${old_port}:${expected_active_port}" in
	"${PORT_A}:${PORT_B}"|"${PORT_B}:${PORT_A}") ;;
	*)
		echo "refusing invalid drain transition ${old_port} -> ${expected_active_port}" >&2
		exit 1
		;;
esac

current_active_port="$(cat "$ACTIVE_PORT_FILE" 2>/dev/null || true)"
if [ "$current_active_port" != "$expected_active_port" ]; then
	echo "Skip draining ${old_port}: active port moved from ${expected_active_port} to ${current_active_port:-unknown}."
	exit 0
fi

if ! systemctl is-active --quiet "${SERVICE_NAME}-${expected_active_port}"; then
	echo "Refusing to drain ${old_port}: expected active slot ${expected_active_port} is not running."
	exit 1
fi

# Last line of defence: nginx decides where traffic actually goes, so never stop
# a slot it still proxies to. .active-port and systemd can both agree while the
# cutover silently failed to rewrite the vhost, and draining then takes the
# serving slot down. Backup vhosts are excluded because nginx never reads them.
NGINX_SEARCH_DIRS="${NGINX_SEARCH_DIRS:-/etc/nginx/conf.d /etc/nginx/sites-enabled}"
NGINX_BACKUP_PATTERN="${NGINX_BACKUP_PATTERN:-\.bak($|[.-])|/[^/]*bak-before}"
# shellcheck disable=SC2086 # NGINX_SEARCH_DIRS is an intentional word list.
live_nginx_ports="$(grep -Rsl "127.0.0.1:" $NGINX_SEARCH_DIRS 2>/dev/null \
	| grep -Ev "$NGINX_BACKUP_PATTERN" \
	| xargs -r grep -hoE "127\.0\.0\.1:(${PORT_A}|${PORT_B})" 2>/dev/null | sort -u || true)"
if printf '%s\n' "$live_nginx_ports" | grep -q "127.0.0.1:${old_port}"; then
	echo "Refusing to drain ${old_port}: live nginx still proxies to it (${live_nginx_ports})." >&2
	exit 1
fi

for old_unit in "$SERVICE_NAME" "${SERVICE_NAME}-${old_port}"; do
	if [ "$old_unit" != "${SERVICE_NAME}-${expected_active_port}" ]; then
		systemctl disable --now "$old_unit" 2>/dev/null || systemctl stop "$old_unit" 2>/dev/null || true
	fi
done

active_bin="${BASE_DIR}/${SERVICE_NAME}-${expected_active_port}"
find "$BASE_DIR" -maxdepth 1 -type f -name "${SERVICE_NAME}-*" ! -name "$(basename "$active_bin")" -mtime +7 -delete 2>/dev/null || true
echo "Drain complete: stopped ${SERVICE_NAME}-${old_port}; active port is ${expected_active_port}."
