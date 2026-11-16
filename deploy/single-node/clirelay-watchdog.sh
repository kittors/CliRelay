#!/usr/bin/env bash
# Hang watchdog for a single-node blue-green host (installed as
# /usr/local/bin/clirelay-watchdog.sh, run by clirelay-watchdog.service).
#
# A slot can hang without exiting (the host has frozen for minutes at a
# time), and Restart=always only covers a process that exits. This probes
# the slot that /opt/clirelay2/.active-port names and SIGKILLs it once it
# has stopped answering HTTP; Restart=always then starts it again in 3 s.
#
# Slow is not hung. /readyz pings PostgreSQL (and Redis) within 2 s and
# answers 503 when that fails, so a process that is only overloaded, or
# whose database is, answers 503 too. On 2026-11-16 a slot pinned by a
# quadratic request translation answered 503 "postgres" to four probes in a
# row; the watchdog took that for a hang and SIGKILLed it at 17:06, cutting
# every request in flight, and the clients' retries made the new process
# busier still. So the watchdog only counts what a hung process does:
# - any HTTP answer, whatever its status, means the process is alive. It
#   clears the count; a non-2xx one is logged with its reason;
# - when /readyz sends nothing back within WATCHDOG_PROBE_TIMEOUT, /healthz
#   is asked too (WATCHDOG_LIVENESS_TIMEOUT). It touches nothing but the HTTP
#   stack, so an answer there means slow, not hung;
# - a hang is a round in which neither answered: the probe timed out, or the
#   connection was accepted and then closed or reset without a reply. After
#   WATCHDOG_FAILS such rounds in a row the slot is SIGKILLed;
# - a refused connection means nothing listens on the port: a slot still
#   starting up, or one draining after a SIGTERM that did not come from
#   systemd (http.Server.Shutdown closes the listener first, then waits up to
#   CLIRELAY_SHUTDOWN_GRACE, 300 s, plus up to 90 s of shutdown work). That
#   is not a hang either. Refusals only count once this run of the slot has
#   answered at least once, and only kill after WATCHDOG_REFUSED_FAILS rounds
#   in a row, well past that drain: a process that closed its listener and
#   never exited. A run that has never answered is left to the operator,
#   since killing a slow cold start only starts it over.
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
#
# Before each SIGKILL it saves the slot's memory, CPU and process state under
# WATCHDOG_SNAPSHOT_DIR, so a kill can be checked against a false positive
# afterwards.
set -u

BASE_DIR=/opt/clirelay2
INTERVAL="${WATCHDOG_INTERVAL:-30}"
FAILS="${WATCHDOG_FAILS:-4}"
REFUSED_FAILS="${WATCHDOG_REFUSED_FAILS:-20}"
PROBE_TIMEOUT="${WATCHDOG_PROBE_TIMEOUT:-8}"
LIVENESS_TIMEOUT="${WATCHDOG_LIVENESS_TIMEOUT:-15}"
STARTUP_GRACE="${WATCHDOG_STARTUP_GRACE:-180}"
SNAPSHOT_DIR="${WATCHDOG_SNAPSHOT_DIR:-/var/log/clirelay-watchdog}"

hung=0         # rounds in a row in which the slot sent no HTTP answer at all
refused=0      # rounds in a row in which nothing listened on its port
last_port=""
seen_alive=""  # "<port>@<ActiveEnterTimestamp>" of the run last seen answering
quiet_run=""   # the never-answered run whose refusals were already logged

# probe PORT PATH TIMEOUT sends one GET to the slot. It sets probe_kind to
# answered (any HTTP status came back), refused (nothing listens on the
# port), silent (no reply within TIMEOUT seconds, or the connection closed or
# reset without one) or error (curl itself failed: not the slot's fault),
# probe_code to the HTTP status (000 without one) and probe_detail to what
# the log line says about it.
probe() {
	local out rc reason="" re='"reason":"([a-z_]+)"'
	# --noproxy: a proxy in the unit's environment must not answer for the slot
	out="$(curl -s --noproxy '*' -m "$3" -w '\n%{http_code}' "http://127.0.0.1:$1$2")"
	rc=$?
	probe_code="${out##*$'\n'}"
	case "$probe_code" in
	[1-5][0-9][0-9])
		probe_kind=answered
		if [[ $out =~ $re ]]; then
			reason="${BASH_REMATCH[1]}"
		fi
		probe_detail="$2 answered $probe_code${reason:+ ($reason)}"
		return 0
		;;
	esac
	probe_code=000
	case "$rc" in
	7) probe_kind=refused probe_detail="$2 refused the connection" ;;
	28) probe_kind=silent probe_detail="$2 sent nothing in ${3}s" ;;
	52 | 55 | 56) probe_kind=silent probe_detail="$2 closed the connection without an answer (curl exit $rc)" ;;
	*) probe_kind=error probe_detail="$2 could not be probed (curl exit $rc)" ;;
	esac
}

reset_counts() {
	hung=0
	refused=0
}

# write_snapshot UNIT PID WHY saves what the slot looked like just before the
# SIGKILL and prints the file's path (nothing when it could not be written).
write_snapshot() {
	local cg="/sys/fs/cgroup/system.slice/$1.service" snap f
	mkdir -p "$SNAPSHOT_DIR" 2>/dev/null || return 0
	snap="$SNAPSHOT_DIR/hang-$(date +%Y%m%d_%H%M%S)-${1#clirelay2-}.txt"
	{
		echo "time $(date '+%Y-%m-%dT%H:%M:%S%z') unit $1 pid $2"
		echo "why $3"
		systemctl show "$1" -p MemoryCurrent -p MemoryHigh -p MemoryMax -p CPUQuotaPerSecUSec
		for f in memory.pressure memory.events cpu.pressure cpu.stat; do
			echo "--- $f"
			cat "$cg/$f"
		done
		echo "--- host"
		free -m
		cat /proc/loadavg /proc/pressure/cpu /proc/pressure/memory
		echo "--- process"
		grep -E '^(State|Threads|VmRSS)' "/proc/$2/status"
		cat "/proc/$2/stack"
		ss -s | head -2
	} >"$snap" 2>&1
	if [ -s "$snap" ]; then
		echo "$snap"
	fi
}

# kill_slot UNIT WHY snapshots the slot and SIGKILLs it; Restart=always
# brings it back.
kill_slot() {
	local pid snap
	pid="$(systemctl show -p MainPID --value "$1")"
	snap="$(write_snapshot "$1" "$pid" "$2")"
	echo "$1 looks hung (pid $pid): $2; SIGKILL, Restart=always brings it back${snap:+ (state saved to $snap)}"
	systemctl kill -s KILL "$1"
	reset_counts
}

# watchdog_tick runs one probe round; the counters and seen_alive carry over.
watchdog_tick() {
	local port unit entered run detail
	port="$(tr -d '[:space:]' <"$BASE_DIR/.active-port" 2>/dev/null || true)"
	case "$port" in
	8318 | 8319) ;;
	*)
		reset_counts
		return 0
		;;
	esac
	unit="clirelay2-$port"
	if [ "$port" != "$last_port" ]; then
		[ -z "$last_port" ] || echo "active slot changed $last_port -> $port; now watching $unit"
		reset_counts
		last_port="$port"
	fi
	if [ "$(systemctl show -p ActiveState --value "$unit")" != active ] ||
		[ "$(systemctl show -p SubState --value "$unit")" != running ]; then
		# crashed, stopping or stopped: Restart=always, the unit's TimeoutStopSec
		# (a stop systemd sent drains as deactivating) or the operator owns it
		reset_counts
		return 0
	fi
	entered="$(systemctl show --timestamp=unix -p ActiveEnterTimestamp --value "$unit")"
	entered="${entered#@}"
	case "$entered" in '' | *[!0-9]*) entered=0 ;; esac
	if [ $(($(date +%s) - entered)) -lt "$STARTUP_GRACE" ]; then
		reset_counts
		return 0
	fi
	run="$port@$entered"

	probe "$port" /readyz "$PROBE_TIMEOUT"
	if [ "$probe_kind" = answered ]; then
		seen_alive="$run"
		case "$probe_code" in
		2??) ;;
		*) echo "$unit: $probe_detail; alive, not counted as a hang" ;;
		esac
		reset_counts
		return 0
	fi
	detail="$probe_detail"
	if [ "$probe_kind" = silent ]; then
		# /readyz waits on the database; /healthz only needs the HTTP stack
		probe "$port" /healthz "$LIVENESS_TIMEOUT"
		detail="$detail, $probe_detail"
		if [ "$probe_kind" = answered ]; then
			seen_alive="$run"
			echo "$unit: $detail; slow but alive, not counted as a hang"
			reset_counts
			return 0
		fi
	fi
	case "$probe_kind" in
	refused)
		hung=0
		if [ "$seen_alive" != "$run" ]; then
			# never answered since it started: a slow cold start, not a hang
			[ "$quiet_run" = "$run" ] ||
				echo "$unit: $detail; it has not answered since it started, left alone"
			quiet_run="$run"
			refused=0
			return 0
		fi
		refused=$((refused + 1))
		echo "$unit: $detail; nothing listens on $port ($refused/$REFUSED_FAILS)"
		if [ "$refused" -ge "$REFUSED_FAILS" ]; then
			kill_slot "$unit" "nothing listened on $port for $refused rounds in a row"
		fi
		;;
	silent)
		refused=0
		hung=$((hung + 1))
		echo "$unit: $detail; no HTTP answer ($hung/$FAILS)"
		if [ "$hung" -ge "$FAILS" ]; then
			kill_slot "$unit" "no HTTP answer for $hung rounds in a row"
		fi
		;;
	*)
		echo "$unit: $detail; not counted"
		;;
	esac
	return 0
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	while true; do
		watchdog_tick
		sleep "$INTERVAL"
	done
fi
