#!/usr/bin/env bash
# Post-deploy verification, run on the GitHub Actions runner.
#
#   gha-node-verify.sh node    After one node's blue-green deploy: probe HEALTH_URL
#                              pinned to that node (curl --resolve) for its whole
#                              drain window, then confirm over ssh that the new slot
#                              serves APP_VERSION and the old slot has stopped.
#   gha-node-verify.sh public  After every node: probe HEALTH_URL through DNS, the
#                              way users reach it.
#
# Environment: HEALTH_URL and APP_VERSION; for `node` also NODE, NODE_SMOKE_IP,
# SLOT_SERVICE, NEW_PORT, OLD_PORT (empty on a node's first deploy),
# DRAIN_SECONDS and SHUTDOWN_GRACE_SECONDS from the deploy script's result line.
# Tuning: PROBE_MIN_SECONDS (210), PROBE_MARGIN_SECONDS (30),
# PROBE_INTERVAL_SECONDS (3), PUBLIC_PROBES (10), STOP_WAIT_EXTRA_SECONDS (60),
# STOP_POLL_SECONDS (10).
set -euo pipefail

fail() {
	echo "::error::$*"
	exit 1
}

require_number() {
	case "${2:-}" in
	'' | *[!0-9]*) fail "$1 must be a number, got '${2:-}'" ;;
	esac
}

parse_health_url() {
	case "$HEALTH_URL" in
	https://*) ;;
	*) fail "CLIRELAY_PUBLIC_HEALTH_URL must be an https:// URL, got ${HEALTH_URL}" ;;
	esac
	url_rest="${HEALTH_URL#https://}"
	url_authority="${url_rest%%/*}"
	url_host="${url_authority%%:*}"
	url_port=443
	case "$url_authority" in
	*:*) url_port="${url_authority##*:}" ;;
	esac
	require_number "the port in CLIRELAY_PUBLIC_HEALTH_URL" "$url_port"
}

# probe_window_seconds is how long a node is probed after its cutover: the
# drain the node reported plus a margin, and never less than PROBE_MIN_SECONDS.
probe_window_seconds() {
	probe_window=$((DRAIN_SECONDS + ${PROBE_MARGIN_SECONDS:-30}))
	if [ "$probe_window" -lt "${PROBE_MIN_SECONDS:-210}" ]; then
		probe_window="${PROBE_MIN_SECONDS:-210}"
	fi
	echo "$probe_window"
}

# probe sends one request to HEALTH_URL, adding any curl arguments given, and
# records probe_code and probe_version (the build named in x-cpa-version).
probe() {
	probe_out="$(curl -sS -o /dev/null -D - -w 'probe_status=%{http_code}\n' --max-time 10 "$@" "$HEALTH_URL" 2>/dev/null || true)"
	probe_code="$(printf '%s\n' "$probe_out" | sed -n 's/^probe_status=//p' | tail -n 1)"
	probe_version="$(printf '%s\n' "$probe_out" | tr -d '\r' | awk 'tolower($0) ~ /^x-cpa-version:/ { sub(/^[^:]*:[[:space:]]*/, ""); v = $0 } END { print v }')"
	case "$probe_code" in
	200 | 204) return 0 ;;
	*) return 1 ;;
	esac
}

# probe_twice retries once a second later, so a single packet lost between the
# runner and the node does not fail a rollout; an outage fails both attempts.
probe_twice() {
	probe "$@" && return 0
	sleep 1
	probe "$@"
}

probe_node_once() {
	probes=$((probes + 1))
	if probe_twice "${pin[@]}"; then
		if [ -n "$probe_version" ] && [ "$probe_version" != "$APP_VERSION" ]; then
			other_builds=$((other_builds + 1))
			echo "probe ${probes}: HTTP ${probe_code} from build ${probe_version}"
		fi
	else
		fails=$((fails + 1))
		echo "probe ${probes}: HTTP ${probe_code:-000}"
	fi
}

# REMOTE_STATE_SCRIPT runs on the node as the unprivileged deploy user: the
# state of both slot units, and the build the new slot reports on loopback,
# which bypasses nginx and so cannot be answered by the peer node.
read -r -d '' REMOTE_STATE_SCRIPT <<'EOF' || true
unit_state() { systemctl show -p ActiveState --value "$1" 2>/dev/null || echo unknown; }
new_state="$(unit_state "$NEW_UNIT")"
old_state=none
if [ -n "$OLD_UNIT" ]; then old_state="$(unit_state "$OLD_UNIT")"; fi
served="$(curl -sS -o /dev/null -D - --max-time 5 "http://127.0.0.1:${NEW_PORT}/healthz" 2>/dev/null | tr -d '\r' | awk 'tolower($0) ~ /^x-cpa-version:/ { sub(/^[^:]*:[[:space:]]*/, ""); v = $0 } END { print v }')"
printf 'new_state=%s old_state=%s served_version=%s\n' "${new_state:-unknown}" "${old_state:-unknown}" "${served:-none}"
EOF

read_remote_state() {
	remote_line="$(printf '%s\n' "$REMOTE_STATE_SCRIPT" |
		ssh deploy-target "NEW_UNIT=$(printf %q "${SLOT_SERVICE}-${NEW_PORT}") OLD_UNIT=$(printf %q "${OLD_PORT:+${SLOT_SERVICE}-${OLD_PORT}}") NEW_PORT=$(printf %q "$NEW_PORT") bash -s" 2>/dev/null |
		tail -n 1 || true)"
	new_state=unknown
	old_state=unknown
	served_version=none
	for field in $remote_line; do
		case "$field" in
		new_state=*) new_state="${field#new_state=}" ;;
		old_state=*) old_state="${field#old_state=}" ;;
		served_version=*) served_version="${field#served_version=}" ;;
		esac
	done
}

old_slot_stopped() {
	case "$old_state" in
	inactive | failed | none) return 0 ;;
	esac
	return 1
}

verify_node() {
	: "${NODE:?NODE is required}"
	: "${SLOT_SERVICE:?SLOT_SERVICE is required}"
	[ -n "${NODE_SMOKE_IP:-}" ] || fail "NODE_SMOKE_IP is not set; gha-node-ssh.sh resolves it"
	require_number NEW_PORT "${NEW_PORT:-}"
	[ -z "${OLD_PORT:-}" ] || require_number OLD_PORT "$OLD_PORT"
	require_number DRAIN_SECONDS "${DRAIN_SECONDS:-}"
	require_number SHUTDOWN_GRACE_SECONDS "${SHUTDOWN_GRACE_SECONDS:-}"

	case "$NODE_SMOKE_IP" in
	*:*) pin_addr="[${NODE_SMOKE_IP}]" ;;
	*) pin_addr="$NODE_SMOKE_IP" ;;
	esac
	# Pin the public name to this node. Through DNS the probe could land on a
	# node that is fine while this one is not, and the other way round.
	pin=(--resolve "${url_host}:${url_port}:${pin_addr}")
	window="$(probe_window_seconds)"
	echo "Probing ${HEALTH_URL} on node ${NODE} via ${NODE_SMOKE_IP} for ${window}s (drain ${DRAIN_SECONDS}s plus margin)"

	probes=0
	fails=0
	other_builds=0
	started="$(date +%s)"
	while :; do
		probe_node_once
		[ $(($(date +%s) - started)) -lt "$window" ] || break
		sleep "${PROBE_INTERVAL_SECONDS:-3}"
	done

	# The drain sends the old slot SIGTERM, after which it may keep finishing
	# streams for up to its shutdown grace. The next node waits until it has
	# really exited, and the probes continue meanwhile.
	stop_deadline=$(($(date +%s) + SHUTDOWN_GRACE_SECONDS + ${STOP_WAIT_EXTRA_SECONDS:-60}))
	while :; do
		read_remote_state
		if old_slot_stopped || [ "$(date +%s)" -ge "$stop_deadline" ]; then
			break
		fi
		echo "old slot ${SLOT_SERVICE}-${OLD_PORT} is ${old_state}; waiting for it to stop"
		probe_node_once
		sleep "${STOP_POLL_SECONDS:-10}"
	done

	problems=0
	if [ "$fails" -gt 0 ]; then
		echo "::error::node ${NODE}: ${fails}/${probes} probes through ${NODE_SMOKE_IP} failed"
		problems=$((problems + 1))
	fi
	if [ "$new_state" != active ]; then
		echo "::error::node ${NODE}: new slot ${SLOT_SERVICE}-${NEW_PORT} is ${new_state}"
		problems=$((problems + 1))
	fi
	if [ "$served_version" != "$APP_VERSION" ]; then
		echo "::error::node ${NODE}: ${SLOT_SERVICE}-${NEW_PORT} reports build ${served_version}, expected ${APP_VERSION}"
		problems=$((problems + 1))
	fi
	if ! old_slot_stopped; then
		echo "::error::node ${NODE}: old slot ${SLOT_SERVICE}-${OLD_PORT} is still ${old_state} after the drain window; the drain did not run or was refused (journalctl -u '${SLOT_SERVICE}-drain-*' on the node)"
		problems=$((problems + 1))
	fi
	if [ "$other_builds" -gt 0 ]; then
		# Not fatal: under load nginx sends overflow to the peer node's backup
		# server, which may still run the previous build. The slot itself was
		# checked directly above.
		echo "::warning::node ${NODE}: ${other_builds}/${probes} probes were answered by another build"
	fi
	[ "$problems" -eq 0 ] || return 1
	echo "node ${NODE}: ${probes}/${probes} probes healthy over ${window}s; ${SLOT_SERVICE}-${NEW_PORT} serves ${APP_VERSION}; old slot ${OLD_PORT:-none} ${old_state}"
}

verify_public() {
	total="${PUBLIC_PROBES:-10}"
	require_number PUBLIC_PROBES "$total"
	fails=0
	other_builds=0
	for i in $(seq 1 "$total"); do
		if probe_twice; then
			if [ -n "$probe_version" ] && [ "$probe_version" != "$APP_VERSION" ]; then
				other_builds=$((other_builds + 1))
				echo "probe ${i}/${total}: HTTP ${probe_code} from build ${probe_version}"
			fi
		else
			fails=$((fails + 1))
			echo "probe ${i}/${total}: HTTP ${probe_code:-000}"
		fi
		sleep "${PROBE_INTERVAL_SECONDS:-3}"
	done
	if [ "$fails" -gt 0 ]; then
		echo "::error::Post-deploy health probes failed ${fails}/${total} on ${HEALTH_URL}"
		return 1
	fi
	if [ "$other_builds" -gt 0 ]; then
		# Every node in the rollout serves APP_VERSION by now, so another build
		# means DNS sends users to a node this rollout does not list.
		echo "::error::${other_builds}/${total} public probes were answered by a build other than ${APP_VERSION}; a node behind ${url_host} is missing from the rollout list"
		return 1
	fi
	echo "Public endpoint: ${total}/${total} probes healthy on ${APP_VERSION}"
}

: "${HEALTH_URL:?HEALTH_URL is required}"
: "${APP_VERSION:?APP_VERSION is required}"
parse_health_url
case "${1:-}" in
node) verify_node ;;
public) verify_public ;;
*)
	echo "usage: gha-node-verify.sh node|public" >&2
	exit 2
	;;
esac
