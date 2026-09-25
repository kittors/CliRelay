#!/usr/bin/env bash
# Issue the private CA and per-node certificates that authenticate every
# cross-node connection in a multi-instance CliRelay cluster (etcd peers and
# clients, Patroni REST, PostgreSQL replication and remote application
# connections, the nginx spill-over port).
#
# Nodes talk over the public internet: some VPS providers drop all inbound UDP,
# so an overlay network such as WireGuard is not an option everywhere. Mutual
# TLS against this CA is the access control instead of firewall rules. Keep
# ca.key off the servers; only the CA certificate and each node's own key pair
# are copied to a node.
#
# Usage:
#   gen-cluster-certs.sh <out-dir> <node-name>=<ip>[,<ip>...] [...]
# Example:
#   gen-cluster-certs.sh ~/secure/clirelay-cluster n43=43.255.122.4 n2=198.51.100.20 relay=103.231.58.53
#
# Re-running keeps an existing CA and skips nodes whose certificate exists, so
# adding a node later only issues the new one.
set -euo pipefail

OPENSSL="${OPENSSL:-openssl}"
CA_DAYS="${CA_DAYS:-3650}"
NODE_DAYS="${NODE_DAYS:-1825}"

if [ "$#" -lt 2 ]; then
  sed -n '2,20p' "$0"
  exit 2
fi

out="$1"
shift
umask 077
mkdir -p "$out"

if [ ! -f "$out/ca.key" ]; then
  "$OPENSSL" ecparam -name prime256v1 -genkey -noout -out "$out/ca.key"
  "$OPENSSL" req -x509 -new -key "$out/ca.key" -sha256 -days "$CA_DAYS" \
    -subj "/O=CliRelay/CN=CliRelay Cluster CA" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -out "$out/ca.crt"
  echo "created CA $out/ca.crt"
fi

for spec in "$@"; do
  name="${spec%%=*}"
  ips="${spec#*=}"
  if [ -z "$name" ] || [ "$name" = "$spec" ] || [ -z "$ips" ]; then
    echo "invalid node spec: $spec (want name=ip[,ip])" >&2
    exit 2
  fi
  dir="$out/$name"
  if [ -f "$dir/node.crt" ]; then
    echo "skip $name: $dir/node.crt exists"
    continue
  fi
  mkdir -p "$dir"
  san="DNS:$name,DNS:localhost,IP:127.0.0.1"
  IFS=',' read -r -a ip_list <<<"$ips"
  for ip in "${ip_list[@]}"; do
    san="$san,IP:$ip"
  done
  "$OPENSSL" ecparam -name prime256v1 -genkey -noout -out "$dir/node.key"
  "$OPENSSL" req -new -key "$dir/node.key" -subj "/O=CliRelay/CN=$name" -out "$dir/node.csr"
  ext="$(mktemp)"
  cat >"$ext" <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=$san
EXT
  "$OPENSSL" x509 -req -in "$dir/node.csr" -CA "$out/ca.crt" -CAkey "$out/ca.key" \
    -CAcreateserial -sha256 -days "$NODE_DAYS" -extfile "$ext" -out "$dir/node.crt"
  rm -f "$ext" "$dir/node.csr"
  cp "$out/ca.crt" "$dir/ca.crt"
  echo "issued $name ($san)"
done
