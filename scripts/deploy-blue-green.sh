#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="${SERVICE_NAME:-clirelay2}"
BASE_DIR="${BASE_DIR:-/opt/clirelay2}"
TEMP_BIN="${TEMP_BIN:-${BASE_DIR}/cli-proxy-api-new}"
DOMAIN="${DOMAIN:-relay.07230805.xyz}"
PORT_A="${PORT_A:-8318}"
PORT_B="${PORT_B:-8319}"
DRAIN_SECONDS="${DRAIN_SECONDS:-35}"
HEALTH_TIMEOUT_SECONDS="${HEALTH_TIMEOUT_SECONDS:-90}"
MIN_AVAILABLE_MB="${MIN_AVAILABLE_MB:-512}"
NGINX_CONTAINER="${NGINX_CONTAINER:-nginx}"
COMMIT_SHA="${COMMIT_SHA:?COMMIT_SHA is required}"
ACTIVE_PORT_FILE="${BASE_DIR}/.active-port"

fail() {
	echo "$*" >&2
	exit 1
}

read_service_property() {
	systemctl show -p "$1" --value "$SERVICE_NAME" 2>/dev/null || true
}

service_exec="$(read_service_property ExecStart)"
service_bin="$(printf '%s\n' "$service_exec" | sed -nE 's/.*path=([^ ;]+).*/\1/p' | head -n1)"
if [ -z "$service_bin" ]; then
	if [ -x "${BASE_DIR}/clirelay2" ]; then
		service_bin="${BASE_DIR}/clirelay2"
	else
		service_bin="${BASE_DIR}/cli-proxy-api"
	fi
fi
service_dir="$(dirname "$service_bin")"
config_path="$(printf '%s\n' "$service_exec" | sed -nE 's/.* -config[= ]([^ ;]+).*/\1/p' | head -n1)"
config_path="${config_path:-${service_dir}/config.yaml}"

[ -f "$TEMP_BIN" ] || fail "uploaded temp binary not found: $TEMP_BIN"
[ -f "$config_path" ] || fail "config file not found: $config_path"

config_port="$(awk '/^port:[[:space:]]*[0-9]+/ {print $2; exit}' "$config_path" 2>/dev/null || true)"
active_port="$(cat "$ACTIVE_PORT_FILE" 2>/dev/null || true)"
active_port="${active_port:-${config_port:-$PORT_A}}"
# Alternate between two local ports so nginx can cut over only after the new slot is healthy.
case "$active_port" in
	"$PORT_A") next_port="$PORT_B" ;;
	*) next_port="$PORT_A" ;;
esac

next_unit="${SERVICE_NAME}-${next_port}"
next_bin="${BASE_DIR}/${next_unit}"
cutover_done=0
# If anything fails before nginx is switched, stop the candidate slot and keep the old service live.
cleanup_failed_deploy() {
	status=$?
	if [ "$status" -ne 0 ] && [ "$cutover_done" -ne 1 ]; then
		systemctl disable --now "$next_unit" >/dev/null 2>&1 || true
	fi
	exit "$status"
}
trap cleanup_failed_deploy EXIT

available_mb="$(awk '/MemAvailable:/ {print int($2 / 1024); exit}' /proc/meminfo 2>/dev/null || true)"
if [ -n "$available_mb" ] && [ "$available_mb" -lt "$MIN_AVAILABLE_MB" ]; then
	fail "not enough free memory for blue-green deploy: ${available_mb}MB available, need ${MIN_AVAILABLE_MB}MB"
fi

install -m 0755 "$TEMP_BIN" "$next_bin"
rm -f "$TEMP_BIN"

if ! grep -a -q "$COMMIT_SHA" "$next_bin"; then
	fail "uploaded binary does not contain expected commit SHA"
fi

working_dir="$(read_service_property WorkingDirectory)"
working_dir="${working_dir:-$service_dir}"
environment="$(read_service_property Environment)"
user="$(read_service_property User)"
group="$(read_service_property Group)"

unit_file="/etc/systemd/system/${next_unit}.service"
{
	echo "[Unit]"
	echo "Description=CliRelay blue-green slot ${next_port}"
	echo "After=network.target"
	echo
	echo "[Service]"
	echo "Type=simple"
	echo "WorkingDirectory=${working_dir}"
	[ -n "$user" ] && echo "User=${user}"
	[ -n "$group" ] && echo "Group=${group}"
	[ -n "$environment" ] && echo "Environment=${environment}"
	# Keep the canonical config path; only override the listen port for this deploy slot.
	echo "Environment=CLIRELAY_PORT=${next_port} PORT=${next_port}"
	echo "ExecStart=${next_bin} -config ${config_path}"
	echo "Restart=always"
	echo "RestartSec=3"
	echo "KillSignal=SIGTERM"
	echo "TimeoutStopSec=45"
	echo
	echo "[Install]"
	echo "WantedBy=multi-user.target"
} > "$unit_file"

systemctl daemon-reload
systemctl enable --now "$next_unit"

http_ok() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsS "$1" >/dev/null 2>&1
	else
		wget -q -O /dev/null "$1" >/dev/null 2>&1
	fi
}

health_url="http://127.0.0.1:${next_port}/healthz"
for _ in $(seq 1 "$HEALTH_TIMEOUT_SECONDS"); do
	if http_ok "$health_url"; then
		break
	fi
	sleep 1
done
if ! http_ok "$health_url"; then
	systemctl status "$next_unit" --no-pager -l >&2 || true
	journalctl -u "$next_unit" --no-pager -n 80 >&2 || true
	fail "new slot failed health check after ${HEALTH_TIMEOUT_SECONDS}s: $health_url"
fi

ensure_host_body_size_conf() {
	[ -d /etc/nginx ] || return 0
	body_size_conf="/etc/nginx/conf.d/90-clirelay-body-size.conf"
	mkdir -p "$(dirname "$body_size_conf")"
	cat > "$body_size_conf" <<'EOF'
# Managed by CliRelay GitHub Actions deploy workflow
client_max_body_size 2000m;
EOF
}

ensure_container_body_size_conf() {
	docker exec -i "$NGINX_CONTAINER" sh -c 'cat > /etc/nginx/conf.d/90-clirelay-body-size.conf' <<'EOF'
# Managed by CliRelay GitHub Actions deploy workflow
client_max_body_size 2000m;
EOF
}

find_host_nginx_conf() {
	if [ -n "${NGINX_CONF:-}" ]; then
		echo "$NGINX_CONF"
		return
	fi
	grep -Rsl "$DOMAIN" /etc/nginx/conf.d /etc/nginx/sites-enabled /etc/nginx/sites-available 2>/dev/null | grep -v '\.bak\.' | head -n1 || true
}

find_container_nginx_conf() {
	if ! command -v docker >/dev/null 2>&1; then
		return
	fi
	if ! docker inspect "$NGINX_CONTAINER" >/dev/null 2>&1; then
		return
	fi
	docker exec "$NGINX_CONTAINER" sh -c "grep -Rsl '$DOMAIN' /etc/nginx/conf.d /etc/nginx/sites-enabled /etc/nginx/sites-available 2>/dev/null | grep -v '\\.bak\\.' | head -n1" || true
}

nginx_mode="host"
nginx_conf="$(find_host_nginx_conf)"
if [ -z "$nginx_conf" ]; then
	nginx_conf="$(find_container_nginx_conf)"
	nginx_mode="container"
fi
[ -n "$nginx_conf" ] || fail "nginx config for ${DOMAIN} not found on host or docker container ${NGINX_CONTAINER}; set NGINX_CONF/NGINX_CONTAINER"

if [ "$nginx_mode" = "container" ]; then
	tmp_conf="$(mktemp)"
	backup="${nginx_conf}.bak.$(date +%Y%m%d_%H%M%S)"
	docker cp "${NGINX_CONTAINER}:${nginx_conf}" "$tmp_conf"
	docker exec "$NGINX_CONTAINER" cp "$nginx_conf" "$backup"
	if ! grep -Eq ":${active_port}\\b" "$tmp_conf"; then
		fail "nginx config ${NGINX_CONTAINER}:${nginx_conf} does not reference active port ${active_port}"
	fi
	# Replace only the active backend port, leaving the existing nginx layout untouched.
	perl -0pi -e "s/:${active_port}\\b/:${next_port}/g" "$tmp_conf"
	ensure_container_body_size_conf
	docker cp "$tmp_conf" "${NGINX_CONTAINER}:${nginx_conf}"
	rm -f "$tmp_conf"
	if ! docker exec "$NGINX_CONTAINER" nginx -t; then
		docker exec "$NGINX_CONTAINER" cp "$backup" "$nginx_conf"
		docker exec "$NGINX_CONTAINER" nginx -t || true
		fail "nginx config test failed; reverted ${NGINX_CONTAINER}:${nginx_conf}"
	fi
	docker exec "$NGINX_CONTAINER" nginx -s reload
else
	[ -f "$nginx_conf" ] || fail "nginx config not found: $nginx_conf"
	ensure_host_body_size_conf
	backup="${nginx_conf}.bak.$(date +%Y%m%d_%H%M%S)"
	cp "$nginx_conf" "$backup"
	if ! grep -Eq ":${active_port}\\b" "$nginx_conf"; then
		fail "nginx config $nginx_conf does not reference active port ${active_port}"
	fi
	# Replace only the active backend port, leaving the existing nginx layout untouched.
	perl -0pi -e "s/:${active_port}\\b/:${next_port}/g" "$nginx_conf"
	if ! nginx -t; then
		cp "$backup" "$nginx_conf"
		nginx -t || true
		fail "nginx config test failed; reverted $nginx_conf"
	fi
	nginx -s reload || systemctl reload nginx
fi

echo "$next_port" > "$ACTIVE_PORT_FILE"
cutover_done=1
echo "CliRelay is serving on ${next_unit} (${next_port}); draining ${active_port} for ${DRAIN_SECONDS}s"
sleep "$DRAIN_SECONDS"

for old_unit in "$SERVICE_NAME" "${SERVICE_NAME}-${active_port}"; do
	if [ "$old_unit" != "$next_unit" ]; then
		systemctl disable --now "$old_unit" 2>/dev/null || systemctl stop "$old_unit" 2>/dev/null || true
	fi
done

find "$BASE_DIR" -maxdepth 1 -type f -name "${SERVICE_NAME}-*" ! -name "$(basename "$next_bin")" -mtime +7 -delete 2>/dev/null || true
echo "Deploy complete: ${next_unit}"
