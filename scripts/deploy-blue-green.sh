#!/usr/bin/env bash
# SCRIPT_VERSION must stay in sync with deploy gate expectations.
SCRIPT_VERSION="${SCRIPT_VERSION:-2026.07.16}"
set -euo pipefail

SERVICE_NAME="${SERVICE_NAME:-clirelay2}"
BASE_DIR="${BASE_DIR:-/opt/clirelay2}"
TEMP_BIN="${TEMP_BIN:-${BASE_DIR}/cli-proxy-api-new}"
DOMAIN="${DOMAIN:-relay.07230805.xyz}"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-https://${DOMAIN}}"
PORT_A="${PORT_A:-8318}"
PORT_B="${PORT_B:-8319}"
DRAIN_SECONDS="${DRAIN_SECONDS:-35}"
HEALTH_TIMEOUT_SECONDS="${HEALTH_TIMEOUT_SECONDS:-90}"
SMOKE_TIMEOUT_SECONDS="${SMOKE_TIMEOUT_SECONDS:-30}"
MIN_AVAILABLE_MB="${MIN_AVAILABLE_MB:-512}"
NGINX_CONTAINER="${NGINX_CONTAINER:-nginx}"
SERVICE_CPU_QUOTA="${SERVICE_CPU_QUOTA:-170%}"
SERVICE_MEMORY_HIGH="${SERVICE_MEMORY_HIGH:-1400M}"
SERVICE_MEMORY_MAX="${SERVICE_MEMORY_MAX:-1600M}"
SERVICE_TASKS_MAX="${SERVICE_TASKS_MAX:-512}"
COMMIT_SHA="${COMMIT_SHA:?COMMIT_SHA is required}"
ACTIVE_PORT_FILE="${BASE_DIR}/.active-port"
CLEANUP_SCRIPT="${CLEANUP_SCRIPT:-${BASE_DIR}/scripts/cleanup-drained-slot.sh}"
RECONCILE_SCRIPT="${RECONCILE_SCRIPT:-${BASE_DIR}/scripts/reconcile-active-slot.sh}"
EXPECTED_SCRIPT_VERSION="${EXPECTED_SCRIPT_VERSION:-}"
if [ -n "$EXPECTED_SCRIPT_VERSION" ] && [ "$SCRIPT_VERSION" != "$EXPECTED_SCRIPT_VERSION" ]; then
	echo "deploy script version mismatch: have ${SCRIPT_VERSION}, want ${EXPECTED_SCRIPT_VERSION}" >&2
	exit 1
fi

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
[ -f "$CLEANUP_SCRIPT" ] || fail "drain cleanup script not found: $CLEANUP_SCRIPT"
[ -f "$RECONCILE_SCRIPT" ] || fail "active slot reconcile script not found: $RECONCILE_SCRIPT"
[ -f "$config_path" ] || fail "config file not found: $config_path"

read_config_scalar() {
	awk -v section="$1" -v key="$2" '
		$0 ~ "^[[:space:]]*" section ":[[:space:]]*$" {in_section=1; next}
		in_section && $0 ~ "^[^[:space:]#][^:]*:" {in_section=0}
		in_section && $0 ~ "^[[:space:]]*" key ":[[:space:]]*" {
			sub("^[[:space:]]*" key ":[[:space:]]*", "")
			gsub(/^[[:space:]"'\'']+|[[:space:]"'\'']+$/, "")
			print
			exit
		}
	' "$config_path" 2>/dev/null || true
}

read_env_scalar() {
	[ -f "$2" ] || return 0
	awk -F= -v key="$1" '
		$1 == key {
			value = substr($0, length(key) + 2)
			gsub(/^[[:space:]"'\'']+|[[:space:]"'\'']+$/, "", value)
			print value
			exit
		}
	' "$2" 2>/dev/null || true
}

env_path="${CLIRELAY_ENV_FILE:-${BASE_DIR}/.env}"
postgres_dsn="${CLIRELAY_POSTGRES_DSN:-$(read_env_scalar CLIRELAY_POSTGRES_DSN "$env_path")}"
postgres_dsn="${postgres_dsn:-$(read_config_scalar postgres dsn)}"
[ -n "$postgres_dsn" ] || fail "postgres.dsn or CLIRELAY_POSTGRES_DSN is required before deploying this runtime data stack"

redis_enable="${CLIRELAY_REDIS_ENABLE:-$(read_env_scalar CLIRELAY_REDIS_ENABLE "$env_path")}"
redis_enable="${redis_enable:-$(read_config_scalar redis enable)}"
case "${redis_enable,,}" in
	true|yes|1)
		redis_addr="${CLIRELAY_REDIS_ADDR:-$(read_env_scalar CLIRELAY_REDIS_ADDR "$env_path")}"
		redis_addr="${redis_addr:-$(read_config_scalar redis addr)}"
		[ -n "$redis_addr" ] || fail "redis.addr or CLIRELAY_REDIS_ADDR is required when redis is enabled"
		;;
esac

config_port="$(awk '/^port:[[:space:]]*[0-9]+/ {print $2; exit}' "$config_path" 2>/dev/null || true)"
active_port="$("$RECONCILE_SCRIPT")"
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

# Validate staged binary before replacing any slot binary (failed deploys must not clobber next_bin).
if ! grep -a -q "$COMMIT_SHA" "$TEMP_BIN"; then
	fail "uploaded binary does not contain expected commit SHA"
fi
install -m 0755 "$TEMP_BIN" "$next_bin"
rm -f "$TEMP_BIN"

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
	[ -f "$env_path" ] && echo "EnvironmentFile=${env_path}"
	[ -n "$environment" ] && echo "Environment=${environment}"
	# Keep the canonical config path; only override the listen port for this deploy slot.
	echo "Environment=CLIRELAY_PORT=${next_port} PORT=${next_port}"
	echo "ExecStart=${next_bin} -config ${config_path}"
	echo "Restart=always"
	echo "RestartSec=3"
	echo "KillSignal=SIGTERM"
	echo "TimeoutStopSec=90"
	[ -n "$SERVICE_CPU_QUOTA" ] && echo "CPUQuota=${SERVICE_CPU_QUOTA}"
	[ -n "$SERVICE_MEMORY_HIGH" ] && echo "MemoryHigh=${SERVICE_MEMORY_HIGH}"
	[ -n "$SERVICE_MEMORY_MAX" ] && echo "MemoryMax=${SERVICE_MEMORY_MAX}"
	[ -n "$SERVICE_TASKS_MAX" ] && echo "TasksMax=${SERVICE_TASKS_MAX}"
	echo "OOMPolicy=stop"
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

# Prefer readiness; fall back to liveness only if /readyz is absent (old binary during rollout).
ready_url="http://127.0.0.1:${next_port}/readyz"
health_url="http://127.0.0.1:${next_port}/healthz"
probe_url="$ready_url"
for _ in $(seq 1 "$HEALTH_TIMEOUT_SECONDS"); do
	if http_ok "$ready_url"; then
		probe_url="$ready_url"
		break
	fi
	# 404 means old binary without /readyz; accept /healthz for one release window.
	if command -v curl >/dev/null 2>&1; then
		code="$(curl -s -o /dev/null -w '%{http_code}' "$ready_url" || true)"
		if [ "$code" = "404" ] && http_ok "$health_url"; then
			probe_url="$health_url"
			break
		fi
	elif http_ok "$health_url"; then
		probe_url="$health_url"
		break
	fi
	sleep 1
done
if ! http_ok "$probe_url"; then
	systemctl status "$next_unit" --no-pager -l >&2 || true
	journalctl -u "$next_unit" --no-pager -n 80 >&2 || true
	fail "new slot failed readiness check after ${HEALTH_TIMEOUT_SECONDS}s: $ready_url (fallback $health_url)"
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

switch_nginx_port() {
	from_port="$1"
	to_port="$2"
	if [ "$nginx_mode" = "container" ]; then
		tmp_conf="$(mktemp)"
		docker cp "${NGINX_CONTAINER}:${nginx_conf}" "$tmp_conf"
		if ! grep -Eq ":${from_port}\\b" "$tmp_conf"; then
			rm -f "$tmp_conf"
			return 1
		fi
		perl -0pi -e "s/:${from_port}\\b/:${to_port}/g" "$tmp_conf"
		ensure_container_body_size_conf
		docker cp "$tmp_conf" "${NGINX_CONTAINER}:${nginx_conf}"
		rm -f "$tmp_conf"
		docker exec "$NGINX_CONTAINER" nginx -t
		docker exec "$NGINX_CONTAINER" nginx -s reload
	else
		[ -f "$nginx_conf" ] || return 1
		ensure_host_body_size_conf
		if ! grep -Eq ":${from_port}\\b" "$nginx_conf"; then
			return 1
		fi
		perl -0pi -e "s/:${from_port}\\b/:${to_port}/g" "$nginx_conf"
		nginx -t
		nginx -s reload || systemctl reload nginx
	fi
}

if [ "$nginx_mode" = "container" ]; then
	backup="${nginx_conf}.bak.$(date +%Y%m%d_%H%M%S)"
	docker exec "$NGINX_CONTAINER" cp "$nginx_conf" "$backup"
else
	[ -f "$nginx_conf" ] || fail "nginx config not found: $nginx_conf"
	backup="${nginx_conf}.bak.$(date +%Y%m%d_%H%M%S)"
	cp "$nginx_conf" "$backup"
fi

if ! switch_nginx_port "$active_port" "$next_port"; then
	if [ "$nginx_mode" = "container" ]; then
		docker exec "$NGINX_CONTAINER" cp "$backup" "$nginx_conf" || true
		docker exec "$NGINX_CONTAINER" nginx -t || true
	else
		cp "$backup" "$nginx_conf" || true
		nginx -t || true
	fi
	fail "nginx cutover failed; restored backup and kept active port ${active_port}"
fi

# External smoke before abandoning old slot. Failure rolls nginx back to active_port.
smoke_ok=0
public_ready="${PUBLIC_BASE_URL%/}/readyz"
public_health="${PUBLIC_BASE_URL%/}/healthz"
for _ in $(seq 1 "$SMOKE_TIMEOUT_SECONDS"); do
	if http_ok "$public_ready" || http_ok "$public_health"; then
		smoke_ok=1
		break
	fi
	sleep 1
done
if [ "$smoke_ok" -ne 1 ]; then
	echo "external smoke failed after cutover; rolling nginx back to ${active_port}" >&2
	if ! switch_nginx_port "$next_port" "$active_port"; then
		if [ "$nginx_mode" = "container" ]; then
			docker exec "$NGINX_CONTAINER" cp "$backup" "$nginx_conf" || true
			docker exec "$NGINX_CONTAINER" nginx -s reload || true
		else
			cp "$backup" "$nginx_conf" || true
			nginx -s reload || systemctl reload nginx || true
		fi
	fi
	systemctl disable --now "$next_unit" >/dev/null 2>&1 || true
	fail "external HTTPS smoke failed for ${PUBLIC_BASE_URL}; traffic restored to ${active_port}"
fi

echo "$next_port" > "$ACTIVE_PORT_FILE"
cutover_done=1

cleanup_unit="${SERVICE_NAME}-drain-${active_port}-$(date +%s)"
if systemd-run \
	--unit="$cleanup_unit" \
	--collect \
	--on-active="${DRAIN_SECONDS}s" \
	env \
		SERVICE_NAME="$SERVICE_NAME" \
		BASE_DIR="$BASE_DIR" \
		PORT_A="$PORT_A" \
		PORT_B="$PORT_B" \
		ACTIVE_PORT_FILE="$ACTIVE_PORT_FILE" \
		bash "$CLEANUP_SCRIPT" "$active_port" "$next_port"; then
	echo "Deploy complete: ${next_unit} (${next_port}) is serving ${COMMIT_SHA}; ${active_port} will drain for ${DRAIN_SECONDS}s in ${cleanup_unit}."
else
	echo "Failed to schedule ${cleanup_unit}; draining ${active_port} synchronously." >&2
	sleep "$DRAIN_SECONDS"
	SERVICE_NAME="$SERVICE_NAME" \
		BASE_DIR="$BASE_DIR" \
		PORT_A="$PORT_A" \
		PORT_B="$PORT_B" \
		ACTIVE_PORT_FILE="$ACTIVE_PORT_FILE" \
		bash "$CLEANUP_SCRIPT" "$active_port" "$next_port"
	echo "Deploy complete after synchronous drain: ${next_unit} (${next_port}) is serving ${COMMIT_SHA}."
fi
