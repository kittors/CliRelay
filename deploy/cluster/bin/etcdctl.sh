#!/bin/sh
# etcdctl inside the local member's container, authenticated with the node
# certificate. Example: etcdctl.sh endpoint health --cluster
exec docker exec clirelay-etcd etcdctl \
  --endpoints=https://127.0.0.1:2379 \
  --cacert=/tls/ca.crt --cert=/tls/node.crt --key=/tls/node.key "$@"
