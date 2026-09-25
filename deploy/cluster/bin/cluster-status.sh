#!/usr/bin/env bash
# One-screen health view of a CliRelay cluster, run as root on any member.
#
# Shows: etcd members, the Patroni topology (roles, lag, timeline), the local
# CliRelay slot and its readiness, and — when run on the arbiter — the base
# backups and the shared Redis. Every section degrades to a short "n/a" line
# when the component is not installed on this host.
set -uo pipefail

section() { printf '\n== %s\n' "$1"; }
have_container() { docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$1"; }

section "etcd"
if have_container clirelay-etcd; then
  docker exec clirelay-etcd etcdctl --endpoints=https://127.0.0.1:2379 \
    --cacert=/tls/ca.crt --cert=/tls/node.crt --key=/tls/node.key \
    endpoint status --cluster -w table 2>&1 | sed 's/^/  /'
else
  echo "  n/a (no clirelay-etcd container here)"
fi

section "PostgreSQL (Patroni)"
if have_container clirelay-patroni; then
  docker exec clirelay-patroni patronictl -c /etc/patroni/patroni.yml list 2>&1 | sed 's/^/  /'
  docker exec clirelay-patroni patronictl -c /etc/patroni/patroni.yml show-config 2>/dev/null \
    | grep -E '^(synchronous_mode|failsafe_mode|ttl|loop_wait)' | sed 's/^/  /'
elif have_container clirelay-etcd; then
  docker exec clirelay-etcd etcdctl --endpoints=https://127.0.0.1:2379 \
    --cacert=/tls/ca.crt --cert=/tls/node.crt --key=/tls/node.key \
    get --prefix /clirelay/clirelay/leader --print-value-only 2>/dev/null | sed 's/^/  leader: /'
else
  echo "  n/a"
fi

section "CliRelay"
if [ -f /opt/clirelay2/.active-port ]; then
  port="$(cat /opt/clirelay2/.active-port)"
  state="$(systemctl is-active "clirelay2-$port" 2>/dev/null)"
  code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:$port/readyz")"
  node="$(curl -s -D - -o /dev/null -m 5 "http://127.0.0.1:$port/healthz" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-clirelay-node"{print $2}')"
  echo "  active slot $port: $state, /readyz $code${node:+, node $node}"
else
  echo "  n/a (no /opt/clirelay2 here)"
fi

section "Backups"
if [ -d /opt/clirelay-cluster/backups ]; then
  ls -1t /opt/clirelay-cluster/backups/base-*.tar.gz 2>/dev/null | head -3 | while read -r f; do
    printf '  %s  %s\n' "$(du -h "$f" | cut -f1)" "$(basename "$f")"
  done
  systemctl list-timers clirelay-pg-basebackup.timer --no-pager 2>/dev/null | sed -n 2p | sed 's/^/  next: /'
else
  echo "  n/a"
fi

section "Shared Redis"
if have_container clirelay-cluster-redis; then
  docker exec clirelay-cluster-redis sh -c 'redis-cli --tls --cacert /tls/ca.crt --cert /tls/node.crt --key /tls/node.key -p 6380 -a "$(cat /proc/1/cmdline | tr "\0" "\n" | grep -A1 -- --requirepass | tail -1)" --no-auth-warning INFO memory' 2>/dev/null \
    | grep -E '^used_memory_human|^maxmemory_human' | sed 's/^/  /'
else
  echo "  n/a"
fi
