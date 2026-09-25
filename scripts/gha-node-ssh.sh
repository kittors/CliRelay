#!/usr/bin/env bash
# Prepares ssh on a GitHub Actions runner for one deploy node, as the host alias
# `deploy-target`. Native ssh only; the remote user is deploy, never root.
#
# The workflow maps the node's secrets into the environment. The node named
# "default" uses the original unsuffixed secrets, so a single-node repository
# keeps working without new configuration:
#
#   NODE              node name from the rollout list (letters, digits, underscore)
#   NODE_HOST         SERVER_HOST_<node>                (default: SERVER_HOST)
#   NODE_PORT         SERVER_PORT_<node>, optional, 22  (default: SERVER_PORT)
#   NODE_SSH_KEY      SSH_PRIVATE_KEY_<node>, else SSH_PRIVATE_KEY
#   NODE_KNOWN_HOSTS  DEPLOY_SSH_KNOWN_HOSTS_<node>, else DEPLOY_SSH_KNOWN_HOSTS
#   NODE_SMOKE_IP     optional public address to pin post-deploy probes to;
#                     defaults to NODE_HOST when that is an address
#
# Appends NODE_SMOKE_IP to $GITHUB_ENV for the steps that probe the node.
set -euo pipefail

fail() {
	echo "::error::$*" >&2
	exit 1
}

node="${NODE:-}"
case "$node" in
'' | *[!A-Za-z0-9_]*) fail "deploy node name '${node}' must be letters, digits and underscores: it is the suffix of the node's secret names" ;;
esac
secret_name() {
	if [ "$node" = default ]; then
		printf '%s' "$1"
	else
		printf '%s_%s' "$1" "$node"
	fi
}

host="${NODE_HOST:-}"
port="${NODE_PORT:-22}"
[ -n "$host" ] || fail "node ${node}: secret $(secret_name SERVER_HOST) is not set"
case "$host" in
*[!A-Za-z0-9.:-]*) fail "node ${node}: $(secret_name SERVER_HOST) must be a bare host name or address" ;;
esac
case "$port" in
'' | *[!0-9]*) fail "node ${node}: $(secret_name SERVER_PORT) must be a port number" ;;
esac
[ -n "${NODE_SSH_KEY:-}" ] || fail "node ${node}: neither $(secret_name SSH_PRIVATE_KEY) nor SSH_PRIVATE_KEY is set"
[ -n "${NODE_KNOWN_HOSTS:-}" ] || fail "node ${node}: neither $(secret_name DEPLOY_SSH_KNOWN_HOSTS) nor DEPLOY_SSH_KNOWN_HOSTS is set"

ssh_dir="${HOME}/.ssh"
mkdir -p "$ssh_dir"
chmod 700 "$ssh_dir"
printf '%s\n' "$NODE_SSH_KEY" >"${ssh_dir}/deploy_key"
chmod 600 "${ssh_dir}/deploy_key"
printf '%s\n' "$NODE_KNOWN_HOSTS" >"${ssh_dir}/known_hosts"
chmod 600 "${ssh_dir}/known_hosts"

# ssh looks a host on a non-standard port up as "[host]:port". An entry for the
# bare host only covers port 22, and StrictHostKeyChecking then fails with a
# message that does not say why, so check for the exact key ssh will use.
if [ "$port" = 22 ]; then
	known_key="$host"
else
	known_key="[${host}]:${port}"
fi
if ! ssh-keygen -F "$known_key" -f "${ssh_dir}/known_hosts" >/dev/null 2>&1; then
	fail "node ${node}: known_hosts has no entry for ${known_key}; regenerate $(secret_name DEPLOY_SSH_KNOWN_HOSTS) with: ssh-keyscan -p ${port} ${host}"
fi

{
	echo "Host deploy-target"
	echo "  HostName ${host}"
	echo "  Port ${port}"
	echo "  User deploy"
	echo "  IdentityFile ${ssh_dir}/deploy_key"
	echo "  IdentitiesOnly yes"
	echo "  UserKnownHostsFile ${ssh_dir}/known_hosts"
	echo "  StrictHostKeyChecking yes"
	echo "  BatchMode yes"
	echo "  ConnectTimeout 20"
	# A deploy session sits quiet through readiness and, without systemd-run,
	# a synchronous drain; keepalives stop a NAT from dropping it.
	echo "  ServerAliveInterval 15"
	echo "  ServerAliveCountMax 8"
} >"${ssh_dir}/config"
chmod 600 "${ssh_dir}/config"

is_ipv4() {
	[[ "$1" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]
}
is_ipv6() {
	[[ "$1" =~ ^[0-9A-Fa-f]*:[0-9A-Fa-f:]*$ ]]
}

# Post-deploy probes resolve the public host name to this address, so they
# reach this node rather than whichever node DNS picks.
smoke_ip="${NODE_SMOKE_IP:-}"
if [ -z "$smoke_ip" ]; then
	if is_ipv4 "$host" || is_ipv6 "$host"; then
		smoke_ip="$host"
	elif command -v getent >/dev/null 2>&1; then
		smoke_ip="$(getent ahostsv4 "$host" 2>/dev/null | awk 'NR == 1 { print $1 }')"
	fi
fi
if ! is_ipv4 "$smoke_ip" && ! is_ipv6 "$smoke_ip"; then
	fail "node ${node}: no public address to probe; set the secret $(secret_name SMOKE_IP) or the variable <prefix>_$(secret_name SMOKE_IP) (see docs/multi-instance-node-bootstrap_CN.md)"
fi
if [ -n "${GITHUB_ENV:-}" ]; then
	echo "NODE_SMOKE_IP=${smoke_ip}" >>"$GITHUB_ENV"
fi
echo "node ${node}: ssh deploy@${host}:${port}, probes pinned to ${smoke_ip}"
