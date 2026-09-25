#!/usr/bin/env bash
# Blue-green deploy for one CliRelay node: start the staged binary on the idle
# slot, move nginx onto it once it is healthy, and drain the old slot later.
#
# Runs as root. The deploy user reaches it only through the fixed sudo
# entrypoint /usr/local/sbin/clirelay-gha-deploy (scripts/clirelay-gha-deploy),
# which hands over a scrubbed environment. Node setup is described in
# docs/multi-instance-node-bootstrap_CN.md.
#
#   deploy-blue-green.sh                   deploy ${BASE_DIR}/cli-proxy-api-new
#   deploy-blue-green.sh --check           validate this node's setup; changes nothing
#   deploy-blue-green.sh --print-settings  print the effective settings and exit
#
# SCRIPT_VERSION must stay in sync with deploy gate expectations.
SCRIPT_VERSION='2026.09.25.1'
set -euo pipefail

mode=deploy
case "$#:${1:-}" in
0:) ;;
1:--check) mode=check ;;
1:--print-settings) mode=print-settings ;;
*)
	echo "usage: deploy-blue-green.sh [--check|--print-settings]" >&2
	exit 2
	;;
esac

fail() {
	echo "$*" >&2
	exit 1
}

# --- Settings -----------------------------------------------------------------
#
# This script runs as root and the deploy user reaches it through sudo, so the
# caller's environment is untrusted input. It used to read BASE_DIR,
# RECONCILE_SCRIPT, CLEANUP_SCRIPT and the rest as ${VAR:-default}; under the
# SETENV sudo rule that let the deploy user point root at a script of its own.
# Every setting is now fixed here or read from DEPLOY_ENV_FILE, which only root
# can write. The caller supplies nothing beyond the tunables validated in
# read_caller_tunables.
DEPLOY_ENV_FILE=/etc/clirelay2/deploy.env
SERVICE_NAME=clirelay2
BASE_DIR=/opt/clirelay2
PORT_A=8318
PORT_B=8319
TEMP_BIN="${BASE_DIR}/cli-proxy-api-new"
ACTIVE_PORT_FILE="${BASE_DIR}/.active-port"
CLEANUP_SCRIPT="${BASE_DIR}/scripts/cleanup-drained-slot.sh"
RECONCILE_SCRIPT="${BASE_DIR}/scripts/reconcile-active-slot.sh"
APP_ENV_FILE="${BASE_DIR}/.env"
NGINX_BODY_SIZE_CONF=/etc/nginx/conf.d/90-clirelay-body-size.conf
# The directories the drain guard in cleanup-drained-slot.sh scans by default.
NGINX_DRAIN_GUARD_DIRS='/etc/nginx/conf.d /etc/nginx/sites-enabled'

# Per-node settings. The defaults describe the original single-node host; the
# keys in DEPLOY_ENV_KEYS may be overridden in DEPLOY_ENV_FILE.
DOMAIN=relay.07230805.xyz
# Address the post-cutover smoke pins DOMAIN to, so it runs the node's full
# public path (443 -> nginx stream -> TLS vhost -> slot). Empty means entering
# nginx's TLS listener at SMOKE_LOCAL_TLS_ADDR directly.
NODE_PUBLIC_IP=
SMOKE_LOCAL_TLS_ADDR=127.0.0.1:8444
NGINX_CONF=
NGINX_SEARCH_DIRS='/etc/nginx/conf.d /etc/nginx/sites-enabled /etc/nginx/sites-available'
NGINX_CONTAINER=nginx
# Account the slot units run as. Empty copies User=/Group= from the base unit.
SLOT_USER=
SLOT_GROUP=
# How long the retired slot keeps serving after nginx has stopped sending it
# new traffic. It only needs to outlast requests already in flight, and this
# proxy fronts LLMs where a single streamed answer runs for minutes — 35s cut
# those off. Draining is scheduled asynchronously, so a longer window costs
# one idle process, not deploy time.
DRAIN_SECONDS=180
# Upper bound the retiring process may spend finishing in-flight requests
# after SIGTERM. Passed to the unit as TimeoutStopSec and to the binary as
# CLIRELAY_SHUTDOWN_GRACE so systemd cannot kill it mid-stream.
SHUTDOWN_GRACE_SECONDS=300
HEALTH_TIMEOUT_SECONDS=90
SMOKE_TIMEOUT_SECONDS=30
MIN_AVAILABLE_MB=512
# The Go runtime does not read cgroup limits, so on its own it grows the heap
# until GOGC says to collect — which is well past MemoryHigh. Crossing that
# watermark hands the cgroup to the kernel, which throttles every allocation
# and runs synchronous direct reclaim; with no swap on this host the heap is
# anonymous and unreclaimable, so the process burns CPU scanning pages it can
# never free and stops answering instead of simply running a GC cycle. Handing
# the runtime a ceiling below MemoryHigh keeps collection in Go's hands.
GO_MEM_LIMIT_PERCENT=85

# DEPLOY_ENV_KEYS are the settings DEPLOY_ENV_FILE may set. Anything else there
# is refused, since a misspelt key would otherwise be ignored in silence.
DEPLOY_ENV_KEYS='DOMAIN NODE_PUBLIC_IP SMOKE_LOCAL_TLS_ADDR NGINX_CONF NGINX_SEARCH_DIRS NGINX_CONTAINER SLOT_USER SLOT_GROUP DRAIN_SECONDS SHUTDOWN_GRACE_SECONDS HEALTH_TIMEOUT_SECONDS SMOKE_TIMEOUT_SECONDS MIN_AVAILABLE_MB GO_MEM_LIMIT_PERCENT'

# setting_is_valid checks a DEPLOY_ENV_FILE value against the shape its setting
# may take. The values reach sed patterns, curl arguments and the unit file.
setting_is_valid() {
	case "$1" in
	DOMAIN) pattern='^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$' ;;
	NODE_PUBLIC_IP) pattern='^([0-9]{1,3}(\.[0-9]{1,3}){3}|[0-9A-Fa-f]*:[0-9A-Fa-f:]*)?$' ;;
	SMOKE_LOCAL_TLS_ADDR) pattern='^([0-9]{1,3}(\.[0-9]{1,3}){3}|\[[0-9A-Fa-f:]+\]|localhost):[0-9]{1,5}$' ;;
	NGINX_CONF) pattern='^(/[A-Za-z0-9._@+-]+)*$' ;;
	NGINX_SEARCH_DIRS) pattern='^/[A-Za-z0-9._@+/-]*( +/[A-Za-z0-9._@+/-]*)*$' ;;
	NGINX_CONTAINER) pattern='^[A-Za-z0-9][A-Za-z0-9_.-]*$' ;;
	SLOT_USER | SLOT_GROUP) pattern='^([a-z_][a-z0-9_-]{0,31})?$' ;;
	DRAIN_SECONDS | SHUTDOWN_GRACE_SECONDS | HEALTH_TIMEOUT_SECONDS | SMOKE_TIMEOUT_SECONDS) pattern='^[1-9][0-9]{0,5}$' ;;
	MIN_AVAILABLE_MB) pattern='^[0-9]{1,6}$' ;;
	GO_MEM_LIMIT_PERCENT) pattern='^([1-9]|[1-9][0-9]|100)$' ;;
	*) return 1 ;;
	esac
	[[ "$2" =~ $pattern ]]
}

# load_deploy_env reads KEY=value lines from DEPLOY_ENV_FILE. The file is parsed,
# never sourced: a value stays data even when it looks like a command.
load_deploy_env() {
	env_line_no=0
	while IFS= read -r env_line || [ -n "$env_line" ]; do
		env_line_no=$((env_line_no + 1))
		env_line="${env_line%$'\r'}"
		env_line="${env_line#"${env_line%%[![:space:]]*}"}"
		env_line="${env_line%"${env_line##*[![:space:]]}"}"
		case "$env_line" in
		'' | '#'*) continue ;;
		*=*) ;;
		*) fail "${1}:${env_line_no}: expected KEY=value" ;;
		esac
		env_key="${env_line%%=*}"
		env_value="${env_line#*=}"
		case "$env_value" in
		\"*\")
			env_value="${env_value#\"}"
			env_value="${env_value%\"}"
			;;
		\'*\')
			env_value="${env_value#\'}"
			env_value="${env_value%\'}"
			;;
		esac
		case " ${DEPLOY_ENV_KEYS} " in
		*" ${env_key} "*) ;;
		*) fail "${1}:${env_line_no}: unknown setting ${env_key}" ;;
		esac
		setting_is_valid "$env_key" "$env_value" || fail "${1}:${env_line_no}: invalid ${env_key}: ${env_value}"
		printf -v "$env_key" '%s' "$env_value"
	done <"$1"
}

# tunable_is_valid checks a value the caller passed in. The slot unit is built
# from these, and a stray newline could smuggle an ExecStartPre=+ line that
# runs as root into it, so each must match the narrow shape its directive
# accepts. scripts/clirelay-gha-deploy applies the same table before it gets
# here; TestDeployTunableValidationMatchesEntrypoint keeps the two equal.
tunable_is_valid() {
	case "$1" in
	COMMIT_SHA) pattern='^([0-9a-f]{40}|[0-9a-f]{64})?$' ;;
	EXPECTED_SCRIPT_VERSION) pattern='^[0-9A-Za-z._-]{0,64}$' ;;
	SERVICE_CPU_QUOTA) pattern='^([0-9]{1,5}%)?$' ;;
	SERVICE_MEMORY_HIGH | SERVICE_MEMORY_MAX) pattern='^([0-9]{1,15}[KMGT]?|infinity)?$' ;;
	SERVICE_TASKS_MAX) pattern='^([0-9]{1,9}|infinity)?$' ;;
	SERVICE_GO_MEM_LIMIT) pattern='^([0-9]{1,15}(B|KiB|MiB|GiB|TiB)?|off)?$' ;;
	*) return 1 ;;
	esac
	[[ "$2" =~ $pattern ]]
}

# read_caller_tunables takes the only values this script accepts from its
# environment: the ones the workflow passes through the sudo env_keep whitelist.
read_caller_tunables() {
	COMMIT_SHA="${COMMIT_SHA:-}"
	EXPECTED_SCRIPT_VERSION="${EXPECTED_SCRIPT_VERSION:-}"
	SERVICE_CPU_QUOTA="${SERVICE_CPU_QUOTA:-170%}"
	SERVICE_MEMORY_HIGH="${SERVICE_MEMORY_HIGH:-1400M}"
	SERVICE_MEMORY_MAX="${SERVICE_MEMORY_MAX:-1600M}"
	SERVICE_TASKS_MAX="${SERVICE_TASKS_MAX:-512}"
	SERVICE_GO_MEM_LIMIT="${SERVICE_GO_MEM_LIMIT:-}"
	for tunable in COMMIT_SHA EXPECTED_SCRIPT_VERSION SERVICE_CPU_QUOTA SERVICE_MEMORY_HIGH SERVICE_MEMORY_MAX SERVICE_TASKS_MAX SERVICE_GO_MEM_LIMIT; do
		tunable_is_valid "$tunable" "${!tunable}" || fail "refusing ${tunable}=$(printf '%q' "${!tunable}")"
	done
}

# canonical_path resolves symlinks, so one file reached two ways counts once.
canonical_path() {
	readlink -f -- "$1" 2>/dev/null || printf '%s\n' "$1"
}

# require_root_controlled refuses a file root is about to trust unless only root
# can change it: the file and every directory above it must be owned by root
# and not writable by group or others. Symlinks are resolved first, since a
# root-owned link into a writable directory would otherwise pass.
require_root_controlled() {
	trusted_path="$(canonical_path "$1")"
	while :; do
		owner_mode="$(stat -c '%u %a' "$trusted_path" 2>/dev/null || stat -f '%u %Lp' "$trusted_path" 2>/dev/null || true)"
		trusted_owner="${owner_mode%% *}"
		trusted_mode="${owner_mode##* }"
		case "$trusted_mode" in
		'' | *[!0-7]*) fail "cannot read the owner and mode of ${trusted_path}" ;;
		esac
		[ "$trusted_owner" = 0 ] || fail "${trusted_path} must be owned by root (it is uid ${trusted_owner}) before root trusts ${1}"
		[ $((8#$trusted_mode & 8#022)) -eq 0 ] || fail "${trusted_path} must not be writable by group or others (mode ${trusted_mode}) before root trusts ${1}"
		[ "$trusted_path" != / ] || break
		trusted_path="$(dirname "$trusted_path")"
	done
}

print_settings() {
	for setting in SCRIPT_VERSION DEPLOY_ENV_FILE deploy_env_state SERVICE_NAME BASE_DIR PORT_A PORT_B TEMP_BIN ACTIVE_PORT_FILE CLEANUP_SCRIPT RECONCILE_SCRIPT APP_ENV_FILE DOMAIN PUBLIC_BASE_URL NODE_PUBLIC_IP SMOKE_LOCAL_TLS_ADDR NGINX_CONF NGINX_SEARCH_DIRS NGINX_CONTAINER SLOT_USER SLOT_GROUP DRAIN_SECONDS SHUTDOWN_GRACE_SECONDS HEALTH_TIMEOUT_SECONDS SMOKE_TIMEOUT_SECONDS MIN_AVAILABLE_MB GO_MEM_LIMIT_PERCENT SERVICE_CPU_QUOTA SERVICE_MEMORY_HIGH SERVICE_MEMORY_MAX SERVICE_TASKS_MAX SERVICE_GO_MEM_LIMIT; do
		printf '%s=%s\n' "$setting" "${!setting}"
	done
}

read_caller_tunables
deploy_env_state=absent
if [ -e "$DEPLOY_ENV_FILE" ]; then
	require_root_controlled "$DEPLOY_ENV_FILE"
	load_deploy_env "$DEPLOY_ENV_FILE"
	deploy_env_state=loaded
fi
PUBLIC_BASE_URL="https://${DOMAIN}"

if [ "$mode" = print-settings ]; then
	print_settings
	exit 0
fi

if [ -n "$EXPECTED_SCRIPT_VERSION" ] && [ "$SCRIPT_VERSION" != "$EXPECTED_SCRIPT_VERSION" ]; then
	echo "deploy script version mismatch: have ${SCRIPT_VERSION}, want ${EXPECTED_SCRIPT_VERSION}" >&2
	exit 1
fi
if [ "$mode" = deploy ] && [ -z "$COMMIT_SHA" ]; then
	fail "COMMIT_SHA is required"
fi

# Convert a systemd byte quantity (1400M, 2G, plain bytes) to bytes. Anything
# else — "infinity", a percentage, a malformed value — yields nothing so the
# caller can fall back to leaving GOMEMLIMIT unset.
systemd_bytes() {
	case "$1" in
	'' | *[!0-9KMGkmg]* | [!0-9]*) return 0 ;;
	esac
	_num="${1%[KMGkmg]}"
	case "$_num" in
	'' | *[!0-9]*) return 0 ;;
	esac
	case "$1" in
	*[Kk]) echo $((_num * 1024)) ;;
	*[Mm]) echo $((_num * 1024 * 1024)) ;;
	*[Gg]) echo $((_num * 1024 * 1024 * 1024)) ;;
	*) echo "$_num" ;;
	esac
}

if [ -z "$SERVICE_GO_MEM_LIMIT" ]; then
	high_bytes="$(systemd_bytes "$SERVICE_MEMORY_HIGH")"
	if [ -n "$high_bytes" ] && [ "$high_bytes" -gt 0 ]; then
		derived="$((high_bytes * GO_MEM_LIMIT_PERCENT / 100))"
		# A limit this small can only come from a misread MemoryHigh (systemd
		# treats a suffixless number as bytes). Passing it on would pin the
		# runtime in back-to-back GC, which is worse than not setting it.
		if [ "$derived" -ge $((64 * 1024 * 1024)) ]; then
			SERVICE_GO_MEM_LIMIT="$derived"
		fi
	fi
fi

# Refuse to start when a tool the cutover depends on is missing. The previous
# version simply carried on without perl, silently skipping the config rewrite
# while reporting success, so a missing binary was indistinguishable from a
# working deploy. Failing here costs one red build; not failing cost an outage.
for required_cmd in sed grep systemctl nginx curl; do
	command -v "$required_cmd" >/dev/null 2>&1 || fail "required command not found on deploy host: ${required_cmd}"
done

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

if [ "$mode" = deploy ]; then
	[ -f "$TEMP_BIN" ] || fail "uploaded temp binary not found: $TEMP_BIN"
fi
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

# The data stack settings come from the files the slot will read, not from this
# script's environment, which no longer carries anything from the caller.
env_path="$APP_ENV_FILE"
postgres_dsn="$(read_env_scalar CLIRELAY_POSTGRES_DSN "$env_path")"
postgres_dsn="${postgres_dsn:-$(read_config_scalar postgres dsn)}"
[ -n "$postgres_dsn" ] || fail "postgres.dsn or CLIRELAY_POSTGRES_DSN is required before deploying this runtime data stack"

redis_enable="$(read_env_scalar CLIRELAY_REDIS_ENABLE "$env_path")"
redis_enable="${redis_enable:-$(read_config_scalar redis enable)}"
case "$(printf '%s' "$redis_enable" | tr '[:upper:]' '[:lower:]')" in
true | yes | 1)
	redis_addr="$(read_env_scalar CLIRELAY_REDIS_ADDR "$env_path")"
	redis_addr="${redis_addr:-$(read_config_scalar redis addr)}"
	[ -n "$redis_addr" ] || fail "redis.addr or CLIRELAY_REDIS_ADDR is required when redis is enabled"
	;;
esac

# resolve_slot_identity picks the account the slot runs as. The slot unit used
# to copy User=/Group= from the base unit and nothing else, so on a node
# without a base unit it silently ran the relay as root. Such a node must now
# name the account in DEPLOY_ENV_FILE. A base unit that exists without User=
# keeps its historical meaning rather than breaking a running host.
resolve_slot_identity() {
	slot_user="$SLOT_USER"
	slot_group="$SLOT_GROUP"
	if [ -z "$slot_user" ]; then
		if [ "$(read_service_property LoadState)" != "loaded" ]; then
			fail "base unit ${SERVICE_NAME}.service not found, so there is no User= to copy and the slot would run as root; install the base unit or set SLOT_USER in ${DEPLOY_ENV_FILE} (docs/multi-instance-node-bootstrap_CN.md)"
		fi
		slot_user="$(read_service_property User)"
		if [ -z "$slot_group" ]; then
			slot_group="$(read_service_property Group)"
		fi
		if [ -z "$slot_user" ]; then
			echo "warning: ${SERVICE_NAME}.service sets no User=, so the slot runs as root as before; set SLOT_USER in ${DEPLOY_ENV_FILE} to change that" >&2
		fi
	fi
	if [ -n "$slot_user" ] && ! id -u "$slot_user" >/dev/null 2>&1; then
		fail "slot user ${slot_user} does not exist on this host"
	fi
}

resolve_slot_identity

# --- nginx --------------------------------------------------------------------

# Backups of the live vhost sit in the same directory and contain the same
# server_name, so an under-filtered match hands cutover a file nginx never
# reads. The rewrite then "succeeds" against a backup, traffic never moves, and
# drain stops the slot nginx is still serving -- the same shape of outage the
# missing-perl bug caused. The deploy host carries both `.bak.<timestamp>` and
# `.bak-before-<tag>` shapes; the previous dot-anchored filter only matched the
# first of those and let every `bak-before` file through.
# Single source of truth: both lookups and the drain guard use this pattern.
NGINX_BACKUP_PATTERN='\.bak($|[.-])|/[^/]*bak-before'

# Cutover rewrites the vhost that proxies DOMAIN to a slot, so only such a file
# qualifies. The domain is routinely named in more than one live file -- the
# host keeps its port-80 redirect in one and the TLS vhost that proxies to the
# slot in another -- and grep -R lists them in directory order, which nothing
# controls. Taking the first match handed cutover the redirect block, which has
# no slot port to rewrite, so every deploy failed at cutover. This is the same
# slot-port test the drain guard in cleanup-drained-slot.sh applies. Every
# qualifying file is printed; the caller refuses more than one.
#
# The multi-node layout keeps its upstream block and the listener for the peer
# node's overflow in the vhost file too, so it is still exactly one file.
find_host_nginx_conf() {
	if [ -n "${NGINX_CONF:-}" ]; then
		echo "$NGINX_CONF"
		return
	fi
	# shellcheck disable=SC2086 # NGINX_SEARCH_DIRS is an intentional word list.
	grep -Rsl "$DOMAIN" $NGINX_SEARCH_DIRS 2>/dev/null | grep -Ev "$NGINX_BACKUP_PATTERN" |
		xargs -r grep -lE "127\.0\.0\.1:(${PORT_A}|${PORT_B})" 2>/dev/null || true
}

find_container_nginx_conf() {
	if ! command -v docker >/dev/null 2>&1; then
		return
	fi
	if ! docker inspect "$NGINX_CONTAINER" >/dev/null 2>&1; then
		return
	fi
	docker exec "$NGINX_CONTAINER" sh -c "grep -Rsl '$DOMAIN' ${NGINX_SEARCH_DIRS} 2>/dev/null | grep -Ev '${NGINX_BACKUP_PATTERN}' | xargs -r grep -lE '127\\.0\\.0\\.1:(${PORT_A}|${PORT_B})' 2>/dev/null" || true
}

# nginx_conf_text prints the cutover config as nginx reads it, from the host or
# from inside the container.
nginx_conf_text() {
	if [ "$nginx_mode" = "container" ]; then
		docker exec "$NGINX_CONTAINER" cat "$nginx_conf" 2>/dev/null
	else
		cat "$nginx_conf" 2>/dev/null
	fi
}

# slot_refs reads nginx config text on stdin and prints "<line>:<port>" for
# every location that proxies to a blue-green slot. The multi-node layout names
# the slot twice -- the upstream server that takes user traffic, and the
# listener that takes the peer node's overflow -- and each is a location that
# has to move. Comments are skipped so a note about an old port is not taken
# for a route.
slot_refs() {
	sed -E 's/#.*$//' | grep -noE "127\.0\.0\.1:(${PORT_A}|${PORT_B})([^0-9]|\$)" |
		sed -E 's/^([0-9]+):127\.0\.0\.1:([0-9]+).*$/\1:\2/' || true
}

# nginx_slot_ports prints the slot port of every location in the live config.
nginx_slot_ports() {
	{ nginx_conf_text | slot_refs | cut -d: -f2; } || true
}

# nginx_slot_port reports the blue-green slot nginx currently proxies to, or an
# empty string when it points at neither -- or at both, because a cutover is
# only real once every location has moved.
nginx_slot_port() {
	slot_ports="$(nginx_slot_ports | sort -u)"
	case "$slot_ports" in
	*$'\n'*) ;;
	*) printf '%s' "$slot_ports" ;;
	esac
}

# nginx_routes_to succeeds while any location still sends traffic to the port.
nginx_routes_to() {
	routed_ports="$(nginx_slot_ports)"
	case $'\n'"${routed_ports}"$'\n' in
	*$'\n'"$1"$'\n'*) return 0 ;;
	esac
	return 1
}

# slot_ref_lines prints the line numbers in a config file that route to a slot.
slot_ref_lines() {
	[ -f "$1" ] || return 0
	slot_refs <"$1" | awk -F: -v port="$2" '$2 == port { print $1 }'
}

# require_one_slot_in_conf refuses a config whose locations disagree. Deploys
# move every location together, so a split can only come from a hand edit, and
# guessing which half is live is how the slot-drift outages started.
require_one_slot_in_conf() {
	conf_ports="$(nginx_slot_ports | sort -u | tr '\n' ' ')"
	conf_ports="${conf_ports% }"
	case "$conf_ports" in
	'') fail "${nginx_conf} proxies to neither slot (127.0.0.1:${PORT_A} or 127.0.0.1:${PORT_B}); set NGINX_CONF in ${DEPLOY_ENV_FILE} to the file that proxies ${DOMAIN}" ;;
	*' '*) fail "${nginx_conf} routes to both slots (${conf_ports}); every upstream server and proxy_pass must name the same slot before a deploy can move them (line:port $(nginx_conf_text | slot_refs | tr '\n' ' '))" ;;
	esac
}

# other_live_slot_files lists live nginx files besides the cutover file that
# name a slot. The drain guard in cleanup-drained-slot.sh refuses to stop a
# slot any live file names, so such a file pins the old slot for good: cutover
# moves only the cutover file, the drain is refused, and the next deploy starts
# with both slots running. Splitting the upstream layout across two files --
# the peer listener in a file of its own, say -- ends exactly there. Same
# directories and backup filter as the drain guard.
other_live_slot_files() {
	cutover_real="$(canonical_path "$nginx_conf")"
	# shellcheck disable=SC2086 # NGINX_DRAIN_GUARD_DIRS is an intentional word list.
	{ grep -RlsE "127\.0\.0\.1:(${PORT_A}|${PORT_B})([^0-9]|\$)" $NGINX_DRAIN_GUARD_DIRS 2>/dev/null || true; } |
		{ grep -Ev "$NGINX_BACKUP_PATTERN" || true; } |
		while IFS= read -r live_file; do
			[ "$(canonical_path "$live_file")" = "$cutover_real" ] || printf '%s\n' "$live_file"
		done
}

ensure_host_body_size_conf() {
	[ -d "${NGINX_BODY_SIZE_CONF%/conf.d/*}" ] || return 0
	mkdir -p "$(dirname "$NGINX_BODY_SIZE_CONF")"
	cat >"$NGINX_BODY_SIZE_CONF" <<'EOF'
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

# rewrite_slot_port moves every location in a config file from one slot port to
# the other and proves each one took.
#
# sed, not perl: this host has no perl, and the previous implementation could
# not tell. The failed perl call left the file untouched, then `nginx -t` and
# `reload` both succeeded on the unchanged config, so the function returned 0
# and the deploy reported a cutover that never happened. Every deploy advanced
# .active-port while nginx kept pointing at the old slot — that is where the
# drift came from, and the drift is what later let a failed deploy stop the
# slot that was serving production.
#
# The edit is a plain `sed -E` to a temporary file, copied back over the
# original: GNU and BSD sed disagree on in-place editing, and copying back
# keeps the file's owner, mode and any symlink pointing at it.
#
# The verification is the point: never infer that an edit happened from the
# exit status of the command that was supposed to make it.
rewrite_slot_port() {
	rewrite_file="$1"
	rewrite_from="$2"
	rewrite_to="$3"
	# Record every location naming the old slot before the edit, so each one
	# can be checked after it. Nothing to rewrite is a failure, not a success:
	# reporting success for a transition the file cannot express is exactly
	# the bug this function was written to eliminate.
	rewrite_lines="$(slot_ref_lines "$rewrite_file" "$rewrite_from")"
	[ -n "$rewrite_lines" ] || return 1
	rewrite_tmp="$(mktemp)" || return 1
	if ! sed -E "s/:${rewrite_from}([^0-9]|\$)/:${rewrite_to}\\1/g" "$rewrite_file" >"$rewrite_tmp" ||
		! cat "$rewrite_tmp" >"$rewrite_file"; then
		rm -f "$rewrite_tmp"
		return 1
	fi
	rm -f "$rewrite_tmp"
	verify_slot_locations "$rewrite_file" "$rewrite_from" "$rewrite_to" "$rewrite_lines"
}

# verify_slot_locations checks, location by location, that every recorded line
# now names the new slot, and that the old port is gone from the whole file.
# Plain substring checks, so the verification cannot depend on a regex dialect.
verify_slot_locations() {
	verify_file="$1"
	verify_from="$2"
	verify_to="$3"
	for verify_line in $4; do
		verify_text="$(sed -n "${verify_line}p" "$verify_file")"
		case "$verify_text" in
		*"127.0.0.1:${verify_to}"*)
			echo "  ${verify_file}:${verify_line} now routes to ${verify_to}:$(printf '%s' "$verify_text" | sed -E 's/^[[:space:]]*/ /')" >&2
			;;
		*)
			echo "${verify_file}:${verify_line} did not move from ${verify_from} to ${verify_to}: ${verify_text}" >&2
			return 1
			;;
		esac
	done
	if grep -q ":${verify_from}" "$verify_file"; then
		echo "${verify_file} still names port ${verify_from} after the rewrite" >&2
		return 1
	fi
}

reload_nginx() {
	if [ "$nginx_mode" = "container" ]; then
		docker exec "$NGINX_CONTAINER" nginx -t || return 1
		docker exec "$NGINX_CONTAINER" nginx -s reload || return 1
	else
		nginx -t || return 1
		nginx -s reload || systemctl reload nginx || return 1
	fi
}

switch_nginx_port() {
	from_port="$1"
	to_port="$2"
	if [ "$nginx_mode" = "container" ]; then
		tmp_conf="$(mktemp)"
		docker cp "${NGINX_CONTAINER}:${nginx_conf}" "$tmp_conf"
		if ! rewrite_slot_port "$tmp_conf" "$from_port" "$to_port"; then
			rm -f "$tmp_conf"
			return 1
		fi
		ensure_container_body_size_conf
		docker cp "$tmp_conf" "${NGINX_CONTAINER}:${nginx_conf}"
		rm -f "$tmp_conf"
	else
		[ -f "$nginx_conf" ] || return 1
		ensure_host_body_size_conf
		rewrite_slot_port "$nginx_conf" "$from_port" "$to_port" || return 1
	fi
	reload_nginx || return 1
	# Final proof: every location nginx now routes to must be the requested slot.
	[ "$(nginx_slot_port)" = "$to_port" ]
}

# Nginx is the third state source, and the only one that decides where traffic
# actually goes. It is resolved here, before any slot arithmetic, because both
# the active-slot decision and the failure path depend on knowing which slot
# nginx currently routes to.
nginx_mode="host"
nginx_conf="$(find_host_nginx_conf)"
if [ -z "$nginx_conf" ]; then
	nginx_conf="$(find_container_nginx_conf)"
	nginx_mode="container"
fi
[ -n "$nginx_conf" ] || fail "nginx config proxying ${DOMAIN} to port ${PORT_A} or ${PORT_B} not found on host or docker container ${NGINX_CONTAINER}; set NGINX_CONF/NGINX_CONTAINER in ${DEPLOY_ENV_FILE}"
# Rewriting only one of several vhosts that route the domain to a slot would
# leave the rest on the old slot. Refuse rather than guess which one is live.
case "$nginx_conf" in
*$'\n'*) fail "several nginx configs proxy ${DOMAIN} to a slot; set NGINX_CONF in ${DEPLOY_ENV_FILE} to the one to cut over: $(printf '%s' "$nginx_conf" | tr '\n' ' ')" ;;
esac
require_one_slot_in_conf
if [ "$nginx_mode" = "host" ]; then
	stray_slot_files="$(other_live_slot_files)"
	if [ -n "$stray_slot_files" ]; then
		fail "live nginx files besides ${nginx_conf} also proxy to a slot: $(printf '%s' "$stray_slot_files" | tr '\n' ' ')-- cutover moves only ${nginx_conf}, so the drain guard would then refuse to stop the old slot. Keep every slot reference (upstream server, peer listener) in ${nginx_conf}, or retire the others with a .bak suffix."
	fi
fi

# --- Post-cutover smoke route -------------------------------------------------

# The smoke goes to this node's own nginx. A request to PUBLIC_BASE_URL follows
# DNS, which with several nodes behind the name usually lands elsewhere: it
# could pass while this node is broken, or fail while it is fine. With
# NODE_PUBLIC_IP the name resolves to this node's public address and the
# request takes the full path users take (443, nginx stream by SNI, the TLS
# vhost, the slot); without it the request enters the TLS listener directly.
# Either way it keeps the real host name, so SNI and certificate checks apply.
set_smoke_route() {
	if [ -n "$NODE_PUBLIC_IP" ]; then
		case "$NODE_PUBLIC_IP" in
		*:*) smoke_addr="[${NODE_PUBLIC_IP}]" ;;
		*) smoke_addr="$NODE_PUBLIC_IP" ;;
		esac
		smoke_route=(--resolve "${DOMAIN}:443:${smoke_addr}")
	else
		smoke_route=(--connect-to "${DOMAIN}:443:${SMOKE_LOCAL_TLS_ADDR}")
	fi
	smoke_route_desc="${smoke_route[0]#--}=${smoke_route[1]}"
}

# header_version reads response headers on stdin and prints the build named in
# x-cpa-version, which CliRelay sets on every response.
header_version() {
	tr -d '\r' | awk 'tolower($0) ~ /^x-cpa-version:/ { sub(/^[^:]*:[[:space:]]*/, ""); version = $0 } END { print version }'
}

# node_smoke sends one request for a path through this node, and records the
# status and the build that answered in smoke_status and smoke_version.
node_smoke() {
	smoke_out="$(curl -sS -o /dev/null -D - -w 'smoke_status=%{http_code}\n' --max-time 5 "${smoke_route[@]}" "https://${DOMAIN}$1" 2>/dev/null || true)"
	smoke_status="$(printf '%s\n' "$smoke_out" | sed -n 's/^smoke_status=//p' | tail -n 1)"
	smoke_version="$(printf '%s\n' "$smoke_out" | header_version)"
	case "$smoke_status" in
	2??) return 0 ;;
	*) return 1 ;;
	esac
}

set_smoke_route

slot_is_running() {
	systemctl is-active --quiet "${SERVICE_NAME}-$1"
}

if [ "$mode" = check ]; then
	if [ "$nginx_mode" = "container" ]; then
		nginx_test_out="$(docker exec "$NGINX_CONTAINER" nginx -t 2>&1)" || fail "nginx -t fails in container ${NGINX_CONTAINER}: ${nginx_test_out}"
	else
		nginx_test_out="$(nginx -t 2>&1)" || fail "nginx -t fails on this host: ${nginx_test_out}"
	fi
	recorded_port=""
	if [ -f "$ACTIVE_PORT_FILE" ]; then
		recorded_port="$(tr -d '[:space:]' <"$ACTIVE_PORT_FILE")"
	fi
	running_slots=""
	for check_port in "$PORT_A" "$PORT_B"; do
		if slot_is_running "$check_port"; then
			running_slots="${running_slots:+${running_slots},}${check_port}"
		fi
	done
	first_deploy_hint=no
	if [ ! -e "$ACTIVE_PORT_FILE" ] && [ -z "$running_slots" ]; then
		first_deploy_hint=yes
	fi
	printf 'CLIRELAY_DEPLOY_CHECK ok script_version=%s domain=%s nginx=%s:%s routed_port=%s recorded_port=%s running_slots=%s first_deploy=%s slot_user=%s smoke=%s deploy_env=%s\n' \
		"$SCRIPT_VERSION" "$DOMAIN" "$nginx_mode" "$nginx_conf" "$(nginx_slot_port)" "${recorded_port:-none}" \
		"${running_slots:-none}" "$first_deploy_hint" "${slot_user:-root}" "$smoke_route_desc" "$deploy_env_state"
	exit 0
fi

# --- Slots --------------------------------------------------------------------

# resolve_slots decides which slot serves now (active_port, empty on a first
# deploy), which one this deploy fills (next_port), and which port nginx moves
# away from at cutover (cutover_from).
resolve_slots() {
	first_deploy=0
	if [ ! -e "$ACTIVE_PORT_FILE" ] && ! slot_is_running "$PORT_A" && ! slot_is_running "$PORT_B"; then
		# A node that has never run a slot. The reconcile script treats "no
		# slot running" as an outage and refuses, which is right on a live
		# node and made a new node impossible to deploy at all.
		first_deploy=1
		active_port=""
		next_port="$PORT_A"
		cutover_from="$(nginx_slot_port)"
		echo "first deploy on this node: no slot is running and ${ACTIVE_PORT_FILE} does not exist; starting ${SERVICE_NAME}-${next_port}" >&2
		return 0
	fi

	active_port="$(env SERVICE_NAME="$SERVICE_NAME" BASE_DIR="$BASE_DIR" PORT_A="$PORT_A" PORT_B="$PORT_B" ACTIVE_PORT_FILE="$ACTIVE_PORT_FILE" bash "$RECONCILE_SCRIPT")"
	case "$active_port" in
	"$PORT_A" | "$PORT_B") ;;
	*) fail "reconcile returned no usable active slot: ${active_port:-<empty>}" ;;
	esac

	# Reconcile against nginx, which is the only source that decides where
	# traffic actually goes.
	routed_port="$(nginx_slot_port)"
	if [ -n "$routed_port" ] && [ "$routed_port" != "$active_port" ]; then
		if slot_is_running "$routed_port"; then
			# nginx points at a live slot the record disagrees with. nginx
			# wins: it is serving the traffic. Correcting the record here turns
			# what used to be a guaranteed cutover failure into an ordinary
			# deploy.
			echo "active slot drift: recorded ${active_port}, nginx routes to live ${routed_port}; trusting nginx" >&2
			active_port="$routed_port"
			printf '%s\n' "$active_port" >"$ACTIVE_PORT_FILE"
		elif slot_is_running "$active_port"; then
			# nginx points at a slot that is not running while a healthy one
			# sits beside it. That is an outage in progress: every request
			# 502s. Repair it before deploying anything, so the site comes back
			# immediately and the deploy starts from a consistent state.
			echo "nginx routes to ${routed_port} which is not running; repointing at live slot ${active_port}" >&2
			if switch_nginx_port "$routed_port" "$active_port"; then
				echo "nginx repaired: now routing to ${active_port}" >&2
			else
				fail "nginx routes to dead slot ${routed_port} and repointing at ${active_port} failed"
			fi
		else
			fail "nginx routes to ${routed_port} and neither slot is running"
		fi
	fi

	# Alternate between two local ports so nginx can cut over only after the
	# new slot is healthy.
	case "$active_port" in
	"$PORT_A") next_port="$PORT_B" ;;
	*) next_port="$PORT_A" ;;
	esac
	cutover_from="$active_port"
}

resolve_slots

next_unit="${SERVICE_NAME}-${next_port}"
next_bin="${BASE_DIR}/${next_unit}"
cutover_done=0
# If anything fails before nginx is switched, stop the candidate slot and keep the old service live.
cleanup_failed_deploy() {
	status=$?
	if [ "$status" -ne 0 ] && [ "$cutover_done" -ne 1 ]; then
		# Never stop the slot nginx is routing to. The candidate is normally the
		# idle slot, but if anything moved nginx onto it, stopping it here turns
		# a failed deploy into a 502. A stray slot left running is cheap; taking
		# production down to tidy it up is not. A first deploy is the exception:
		# its candidate never passed a health check, so it serves nothing, and
		# stopping it is what lets nginx fall over to a peer node's backup.
		if [ "$first_deploy" -ne 1 ] && nginx_routes_to "$next_port"; then
			echo "refusing to stop ${next_unit}: nginx is routing traffic to port ${next_port}" >&2
		else
			systemctl disable --now "$next_unit" >/dev/null 2>&1 || true
		fi
	fi
	exit "$status"
}
trap cleanup_failed_deploy EXIT

available_mb="$(awk '/MemAvailable:/ {print int($2 / 1024); exit}' /proc/meminfo 2>/dev/null || true)"
if [ -n "$available_mb" ] && [ "$available_mb" -lt "$MIN_AVAILABLE_MB" ]; then
	fail "not enough free memory for blue-green deploy: ${available_mb}MB available, need ${MIN_AVAILABLE_MB}MB"
fi

# Validate staged binary before replacing any slot binary (failed deploys must not clobber next_bin).
if ! grep -a -F -q "$COMMIT_SHA" "$TEMP_BIN"; then
	fail "uploaded binary does not contain expected commit SHA"
fi

# The idle slot is normally stopped by the previous drain. If that drain was
# refused or has not run yet, the slot still runs the previous binary, and
# `systemctl enable --now` alone would leave it that way: readiness would pass
# on the old process and nginx would cut over to a build this deploy never
# started. nginx does not route to it (checked above), so restarting is safe.
candidate_was_running=0
if slot_is_running "$next_port"; then
	candidate_was_running=1
	echo "${next_unit} is still running a previous build; restarting it on the new binary" >&2
fi

install -m 0755 "$TEMP_BIN" "$next_bin"
rm -f "$TEMP_BIN"

working_dir="$(read_service_property WorkingDirectory)"
working_dir="${working_dir:-$service_dir}"
environment="$(read_service_property Environment)"

unit_file="/etc/systemd/system/${next_unit}.service"
{
	echo "[Unit]"
	echo "Description=CliRelay blue-green slot ${next_port}"
	echo "After=network.target"
	echo
	echo "[Service]"
	echo "Type=simple"
	echo "WorkingDirectory=${working_dir}"
	[ -n "$slot_user" ] && echo "User=${slot_user}"
	[ -n "$slot_group" ] && echo "Group=${slot_group}"
	[ -f "$env_path" ] && echo "EnvironmentFile=${env_path}"
	[ -n "$environment" ] && echo "Environment=${environment}"
	# Keep the canonical config path; only override the listen port for this deploy slot.
	echo "Environment=CLIRELAY_PORT=${next_port} PORT=${next_port}"
	echo "ExecStart=${next_bin} -config ${config_path}"
	echo "Restart=always"
	echo "RestartSec=3"
	echo "KillSignal=SIGTERM"
	echo "TimeoutStopSec=${SHUTDOWN_GRACE_SECONDS}"
	echo "Environment=CLIRELAY_SHUTDOWN_GRACE=${SHUTDOWN_GRACE_SECONDS}s"
	# Must stay below MemoryHigh: it is what keeps the runtime collecting on its
	# own instead of letting the kernel throttle the cgroup into reclaim.
	[ -n "$SERVICE_GO_MEM_LIMIT" ] && echo "Environment=GOMEMLIMIT=${SERVICE_GO_MEM_LIMIT}"
	[ -n "$SERVICE_CPU_QUOTA" ] && echo "CPUQuota=${SERVICE_CPU_QUOTA}"
	[ -n "$SERVICE_MEMORY_HIGH" ] && echo "MemoryHigh=${SERVICE_MEMORY_HIGH}"
	[ -n "$SERVICE_MEMORY_MAX" ] && echo "MemoryMax=${SERVICE_MEMORY_MAX}"
	[ -n "$SERVICE_TASKS_MAX" ] && echo "TasksMax=${SERVICE_TASKS_MAX}"
	echo "OOMPolicy=stop"
	echo
	echo "[Install]"
	echo "WantedBy=multi-user.target"
} >"$unit_file"

systemctl daemon-reload
systemctl enable --now "$next_unit"
if [ "$candidate_was_running" -eq 1 ]; then
	systemctl restart "$next_unit"
fi

http_ok() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsS --max-time 10 "$1" >/dev/null 2>&1
	else
		wget -q -T 10 -O /dev/null "$1" >/dev/null 2>&1
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
		code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$ready_url" || true)"
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

# The build the new slot reports. The smoke below compares against it, so an
# answer from any other process is not taken for the new slot.
expected_version="$({ curl -sS -o /dev/null -D - --max-time 5 "$health_url" 2>/dev/null || true; } | header_version)"

if [ "$nginx_mode" = "container" ]; then
	backup="${nginx_conf}.bak.$(date +%Y%m%d_%H%M%S)"
	docker exec "$NGINX_CONTAINER" cp "$nginx_conf" "$backup"
else
	[ -f "$nginx_conf" ] || fail "nginx config not found: $nginx_conf"
	backup="${nginx_conf}.bak.$(date +%Y%m%d_%H%M%S)"
	cp "$nginx_conf" "$backup"
fi

restore_nginx_backup() {
	if [ "$nginx_mode" = "container" ]; then
		docker exec "$NGINX_CONTAINER" cp "$backup" "$nginx_conf" || true
		docker exec "$NGINX_CONTAINER" nginx -t || true
		docker exec "$NGINX_CONTAINER" nginx -s reload || true
	else
		cp "$backup" "$nginx_conf" || true
		nginx -t || true
		nginx -s reload || systemctl reload nginx || true
	fi
}

if [ "$cutover_from" = "$next_port" ]; then
	# Only on a first deploy, when the node's config already names the new
	# slot. Reload anyway so nginx forgets failures it recorded while the slot
	# was down.
	if ! reload_nginx; then
		restore_nginx_backup
		fail "nginx reload failed on first deploy; restored backup"
	fi
elif ! switch_nginx_port "$cutover_from" "$next_port"; then
	restore_nginx_backup
	fail "nginx cutover failed; restored backup and kept nginx on ${cutover_from}"
fi

# External smoke through this node before abandoning the old slot. Failure
# rolls nginx back to the port it routed to before the cutover.
smoke_ok=0
smoke_seen=""
smoke_deadline=$(($(date +%s) + SMOKE_TIMEOUT_SECONDS))
while :; do
	for smoke_path in /readyz /healthz; do
		node_smoke "$smoke_path" || continue
		# In the multi-node layout nginx answers from the peer node's backup
		# server when the local slot fails, so a 2xx alone does not prove the
		# new slot is serving. Its build does whenever the header comes
		# through and the builds differ.
		if [ -z "$expected_version" ] || [ -z "$smoke_version" ] || [ "$smoke_version" = "$expected_version" ]; then
			smoke_ok=1
			break
		fi
		smoke_seen="answered by build ${smoke_version}, not ${expected_version}"
	done
	if [ "$smoke_ok" -eq 1 ] || [ "$(date +%s)" -ge "$smoke_deadline" ]; then
		break
	fi
	sleep 1
done
if [ "$smoke_ok" -ne 1 ]; then
	echo "external smoke failed after cutover (${smoke_seen:-no healthy answer via ${smoke_route_desc}}); rolling nginx back to ${cutover_from}" >&2
	if [ "$cutover_from" = "$next_port" ] || ! switch_nginx_port "$next_port" "$cutover_from"; then
		restore_nginx_backup
	fi
	systemctl disable --now "$next_unit" >/dev/null 2>&1 || true
	fail "external HTTPS smoke failed for ${PUBLIC_BASE_URL} via ${smoke_route_desc}; traffic restored to ${cutover_from}"
fi

echo "$next_port" >"$ACTIVE_PORT_FILE"
cutover_done=1

old_port="$active_port"
if [ "$first_deploy" -eq 1 ]; then
	echo "Deploy complete: first slot ${next_unit} (${next_port}) is serving ${COMMIT_SHA}; nothing to drain."
else
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
fi

# One machine-readable line for the workflow, which keeps probing this node
# until the old slot named here has really stopped.
printf 'CLIRELAY_DEPLOY_RESULT service=%s new_port=%s old_port=%s drain_seconds=%s shutdown_grace_seconds=%s version=%s\n' \
	"$SERVICE_NAME" "$next_port" "$old_port" "$DRAIN_SECONDS" "$SHUTDOWN_GRACE_SECONDS" \
	"$(printf '%s' "$expected_version" | tr -cd 'A-Za-z0-9._+:-')"
