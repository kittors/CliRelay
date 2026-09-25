#!/usr/bin/env bash
# Disposable rehearsal of the PostgreSQL HA migration on one Docker host.
#
# It reproduces the production topology in miniature on a private bridge
# network: a plain single-node PostgreSQL ("lab-old", playing today's
# primary), a stunnel pair carrying replication, a three-member etcd cluster
# and two Patroni members. Nothing is published on host ports, and every
# certificate comes from a throwaway lab CA, never the production CA.
#
# Usage (as root on a spare host):
#   lab.sh up            build everything and start the standby cluster
#   lab.sh down          remove all lab containers, volumes and the network
#   lab.sh psql <node>   psql into lab-old / lab-p1 / lab-p2
#   lab.sh ctl ...       patronictl against the lab cluster
set -euo pipefail

LAB_DIR="${LAB_DIR:-/opt/clirelay-cluster/lab}"
NET=clab
SUBNET=172.30.77.0/24
PG_IMAGE="${PG_IMAGE:-postgres:15.19-alpine3.24}"
HA_IMAGE="${HA_IMAGE:-clirelay/postgres-ha:15.19-patroni4.1.5}"
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.6.15}"
# Lab passwords are generated once and reused by later sub-commands
# (join/psql), otherwise a joining member would carry a different password.
secret() {
  local f="$LAB_DIR/secret-$1"
  if [ ! -s "$f" ]; then
    mkdir -p "$LAB_DIR"
    (umask 077; head -c 18 /dev/urandom | base64 | tr -d '/+=' >"$f")
  fi
  cat "$f"
}
SUPERPASS="$(secret super)"
REPLPASS="$(secret repl)"

here="$(cd "$(dirname "$0")" && pwd)"

certs() {
  local d="$LAB_DIR/tls"
  [ -f "$d/ca.crt" ] && return 0
  mkdir -p "$d"
  bash "$here/../tls/gen-cluster-certs.sh" "$d" \
    lab-etcd1=127.0.0.1 lab-etcd2=127.0.0.1 lab-etcd3=127.0.0.1 \
    lab-p1=127.0.0.1 lab-p2=127.0.0.1 lab-old=127.0.0.1 lab-stunnel-srv=127.0.0.1 lab-stunnel-cli=127.0.0.1 \
    >/dev/null
  # PostgreSQL (uid 70 in the alpine image) refuses a group/world-readable key.
  for n in lab-p1 lab-p2 lab-old; do chown -R 70:70 "$d/$n"; chmod 600 "$d/$n/node.key"; done
}

etcd_up() {
  local cluster="lab-etcd1=https://lab-etcd1:2380,lab-etcd2=https://lab-etcd2:2380,lab-etcd3=https://lab-etcd3:2380"
  for n in lab-etcd1 lab-etcd2 lab-etcd3; do
    docker run -d --name "$n" --hostname "$n" --network "$NET" \
      -v "$LAB_DIR/tls/$n:/tls:ro" -v "$n-data:/etcd-data" "$ETCD_IMAGE" \
      etcd --name "$n" --data-dir /etcd-data \
      --listen-client-urls https://0.0.0.0:2379 --advertise-client-urls "https://$n:2379" \
      --listen-peer-urls https://0.0.0.0:2380 --initial-advertise-peer-urls "https://$n:2380" \
      --initial-cluster "$cluster" --initial-cluster-state new --initial-cluster-token clirelay-lab \
      --client-cert-auth --trusted-ca-file /tls/ca.crt --cert-file /tls/node.crt --key-file /tls/node.key \
      --peer-client-cert-auth --peer-trusted-ca-file /tls/ca.crt --peer-cert-file /tls/node.crt --peer-key-file /tls/node.key \
      --heartbeat-interval 250 --election-timeout 2500 \
      --auto-compaction-mode periodic --auto-compaction-retention 1h --quota-backend-bytes 1073741824 \
      --log-level warn >/dev/null
  done
}

old_up() {
  docker run -d --name lab-old --hostname lab-old --network "$NET" \
    -e POSTGRES_USER=cliproxy -e POSTGRES_PASSWORD="$SUPERPASS" -e POSTGRES_DB=cliproxy \
    -v lab-old-data:/var/lib/postgresql/data "$PG_IMAGE" >/dev/null
  until docker exec lab-old pg_isready -U cliproxy -d cliproxy >/dev/null 2>&1; do sleep 1; done
  sleep 2
  docker exec lab-old pgbench -U cliproxy -i -s 20 cliproxy >/dev/null 2>&1
  docker exec -u postgres lab-old mkdir -p /var/lib/postgresql/data/tls
  docker cp "$LAB_DIR/tls/lab-old/." lab-old:/var/lib/postgresql/data/tls/
  docker exec lab-old sh -c 'chown -R postgres:postgres /var/lib/postgresql/data/tls && chmod 600 /var/lib/postgresql/data/tls/node.key'
}

stunnel_up() {
  docker build -q -t clirelay/stunnel-lab - >/dev/null <<'DOCKERFILE'
FROM alpine:3.22
RUN apk add --no-cache stunnel
ENTRYPOINT ["stunnel"]
DOCKERFILE
  cat >"$LAB_DIR/stunnel-srv.conf" <<CONF
foreground = yes
[pg]
accept = 0.0.0.0:25431
connect = lab-old:5432
cert = /tls/node.crt
key = /tls/node.key
CAfile = /tls/ca.crt
verifyChain = yes
requireCert = yes
CONF
  cat >"$LAB_DIR/stunnel-cli.conf" <<CONF
foreground = yes
[pg]
client = yes
accept = 0.0.0.0:25431
connect = lab-stunnel-srv:25431
cert = /tls/node.crt
key = /tls/node.key
CAfile = /tls/ca.crt
verifyChain = yes
checkHost = lab-stunnel-srv
CONF
  docker run -d --name lab-stunnel-srv --hostname lab-stunnel-srv --network "$NET" \
    -v "$LAB_DIR/tls/lab-stunnel-srv:/tls:ro" -v "$LAB_DIR/stunnel-srv.conf:/s.conf:ro" clirelay/stunnel-lab /s.conf >/dev/null
  docker run -d --name lab-stunnel-cli --hostname lab-stunnel-cli --network "$NET" \
    -v "$LAB_DIR/tls/lab-stunnel-cli:/tls:ro" -v "$LAB_DIR/stunnel-cli.conf:/s.conf:ro" clirelay/stunnel-lab /s.conf >/dev/null
}

node_env() {
  local name="$1" standby="$2"
  cat <<ENV
NODE_NAME=$name
NODE_ADDR=$name
PG_LISTEN=0.0.0.0
PG_PORT=5432
REST_PORT=8008
ETCD_HOSTS=lab-etcd1:2379,lab-etcd2:2379,lab-etcd3:2379
PEER_CIDRS=$SUBNET
LOCAL_CIDRS=127.0.0.1/32
PG_SUPERUSER=cliproxy
PG_SUPERUSER_PASSWORD=$SUPERPASS
PG_REPLICATION_PASSWORD=$REPLPASS
SHARED_BUFFERS=128MB
EFFECTIVE_CACHE_SIZE=512MB
TLS_DIR=/tls
ENV
  if [ "$standby" = yes ]; then
    printf 'STANDBY_HOST=lab-stunnel-cli\nSTANDBY_PORT=25431\nSTANDBY_SLOT=clirelay_standby\n'
  fi
}

patroni_up() {
  local name="$1" standby="$2"
  node_env "$name" "$standby" >"$LAB_DIR/$name.env"
  bash "$here/../postgres-ha/render-patroni.sh" "$LAB_DIR/$name.env" >"$LAB_DIR/$name.yml"
  chown 70:70 "$LAB_DIR/$name.yml"
  chmod 600 "$LAB_DIR/$name.yml"
  docker run -d --name "$name" --hostname "$name" --network "$NET" \
    -v "$LAB_DIR/tls/$name:/tls:ro" -v "$LAB_DIR/$name.yml:/etc/patroni/patroni.yml:ro" \
    -v "$name-data:/var/lib/postgresql/data" "$HA_IMAGE" >/dev/null
}

prepare_old_primary() {
  # Exactly the statements planned for the live primary: all online, only a
  # configuration reload, no restart.
  docker exec -i lab-old psql -v ON_ERROR_STOP=1 -U cliproxy -d cliproxy <<SQL
ALTER SYSTEM SET ssl = on;
ALTER SYSTEM SET ssl_cert_file = 'tls/node.crt';
ALTER SYSTEM SET ssl_key_file = 'tls/node.key';
ALTER SYSTEM SET ssl_ca_file = 'tls/ca.crt';
ALTER SYSTEM SET max_slot_wal_keep_size = '8GB';
CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '$REPLPASS';
SELECT pg_create_physical_replication_slot('clirelay_standby', true);
SQL
  local gw
  gw="$(docker network inspect "$NET" -f '{{(index .IPAM.Config 0).Subnet}}')"
  docker exec lab-old sh -c "echo 'hostssl replication replicator $gw scram-sha-256' >> /var/lib/postgresql/data/pg_hba.conf"
  docker exec lab-old psql -U cliproxy -d cliproxy -Atc "SELECT pg_reload_conf();" >/dev/null
  sleep 1
  docker exec lab-old psql -U cliproxy -d cliproxy -Atc "SHOW ssl;"
}

case "${1:-}" in
up)
  mkdir -p "$LAB_DIR"
  certs
  docker network create --subnet "$SUBNET" "$NET" >/dev/null
  etcd_up
  old_up
  prepare_old_primary
  stunnel_up
  sleep 3
  patroni_up lab-p1 yes
  echo "lab up; watch: $0 ctl list"
  ;;
join)
  patroni_up "${2:-lab-p2}" no
  ;;
down)
  docker rm -f lab-p1 lab-p2 lab-old lab-stunnel-srv lab-stunnel-cli lab-etcd1 lab-etcd2 lab-etcd3 >/dev/null 2>&1 || true
  docker volume rm lab-p1-data lab-p2-data lab-old-data lab-etcd1-data lab-etcd2-data lab-etcd3-data >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$LAB_DIR"
  ;;
psql)
  shift
  n="${1:-lab-p1}"
  shift || true
  docker exec -i "$n" psql -U cliproxy -d cliproxy -h /var/run/postgresql "$@" 2>/dev/null || docker exec -i "$n" psql -U cliproxy -d cliproxy "$@"
  ;;
ctl)
  shift
  n="$(docker ps --format '{{.Names}}' | grep -m1 -E '^lab-p[12]$')"
  docker exec -i "$n" patronictl -c /etc/patroni/patroni.yml "$@"
  ;;
*)
  sed -n '2,16p' "$0"
  exit 2
  ;;
esac
