#!/usr/bin/env bash
# Hang watchdog for a single-node blue-green host (installed as
# /usr/local/bin/clirelay-watchdog.sh, run by clirelay-watchdog.service).
#
# A slot can hang without exiting (the host has frozen for minutes at a
# time), and Restart=always only covers a process that exits. This probes
# the slot that /opt/clirelay2/.active-port names and SIGKILLs it after
# WATCHDOG_FAILS failed /readyz probes in a row; Restart=always then starts
# it again in 3 s.
#
# It must never touch any other slot, nor start anything:
# - the idle slot is stopped on purpose by the drain timer, and a draining
#   slot has already closed its listener (http.Server.Shutdown), so either
#   one looks hung to a probe. The first version of this watchdog had port
#   8319 hard-coded: on 2026-10-27 it SIGKILLed a draining 8319, cutting its
#   shutdown grace, then started it again as an orphan running the retired
#   build next to the new one;
# - "systemctl start" would revive a slot the deploy retired; a SIGKILL is
#   enough for a hung active slot, since its unit has Restart=always;
# - a slot that (re)started less than WATCHDOG_STARTUP_GRACE seconds ago is
#   left alone, since a cold start can take most of a minute to pass /readyz.
set -u

BASE_DIR=/opt/clirelay2
INTERVAL="${WATCHDOG_INTERVAL:-30}"
FAILS="${WATCHDOG_FAILS:-2}"
PROBE_TIMEOUT="${WATCHDOG_PROBE_TIMEOUT:-8}"
STARTUP_GRACE="${WATCHDOG_STARTUP_GRACE:-180}"

fail=0
last_port=""

# watchdog_tick runs one probe round; fail and last_port carry over.
watchdog_tick() {
	local port unit entered code
	port="$(tr -d '[:space:]' <"$BASE_DIR/.active-port" 2>/dev/null || true)"
	case "$port" in
	8318 | 8319) ;;
	*)
		fail=0
		return 0
		;;
	esac
	unit="clirelay2-$port"
	if [ "$port" != "$last_port" ]; then
		[ -z "$last_port" ] || echo "active slot changed $last_port -> $port; now watching $unit"
		fail=0
		last_port="$port"
	fi
	if [ "$(systemctl show -p ActiveState --value "$unit")" != active ] ||
		[ "$(systemctl show -p SubState --value "$unit")" != running ]; then
		# crashed or stopped: Restart=always (or the operator) owns that case
		fail=0
		return 0
	fi
	entered="$(systemctl show --timestamp=unix -p ActiveEnterTimestamp --value "$unit")"
	entered="${entered#@}"
	case "$entered" in '' | *[!0-9]*) entered=0 ;; esac
	if [ $(($(date +%s) - entered)) -lt "$STARTUP_GRACE" ]; then
		fail=0
		return 0
	fi
	code="$(curl -s -m "$PROBE_TIMEOUT" -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/readyz" || true)"
	case "$code" in
	2??) fail=0 ;;
	*)
		fail=$((fail + 1))
		echo "readyz on $port answered '${code:-nothing}' ($fail/$FAILS)"
		;;
	esac
	if [ "$fail" -ge "$FAILS" ]; then
		echo "$unit looks hung (pid $(systemctl show -p MainPID --value "$unit")); SIGKILL, Restart=always brings it back"
		systemctl kill -s KILL "$unit"
		fail=0
	fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	while true; do
		watchdog_tick
		sleep "$INTERVAL"
	done
fi
