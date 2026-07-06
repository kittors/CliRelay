#!/bin/sh
set -eu

env_file="${CLIRELAY_ENV_FILE:-/clirelay-deploy/.env}"
project_dir="${CLIRELAY_PROJECT_DIR:-$(pwd)}"
config_file="${CLIRELAY_CONFIG_FILE:-/clirelay-deploy/config.yaml}"
config_example_file="${CLIRELAY_CONFIG_EXAMPLE_FILE:-/CLIProxyAPI/config.example.yaml}"

if [ ! -e "$config_file" ] && [ -f "$config_example_file" ]; then
	mkdir -p "$(dirname "$config_file")"
	cp "$config_example_file" "$config_file"
	chmod 600 "$config_file" 2>/dev/null || true
fi

mkdir -p "$(dirname "$env_file")"
if [ ! -e "$env_file" ]; then
	: >"$env_file"
	chmod 600 "$env_file" 2>/dev/null || true
fi

env_value() {
	awk -F= -v key="$1" '
		$0 !~ /^[[:space:]]*#/ && $1 == key {
			value = substr($0, index($0, "=") + 1)
			gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
			gsub(/^["'"'"']|["'"'"']$/, "", value)
			print value
			found = 1
		}
		END { if (!found) exit 1 }
	' "$env_file" 2>/dev/null || true
}

rand_hex() {
	od -An -N "${1:-16}" -tx1 /dev/urandom | tr -d ' \n'
}

append_env() {
	key="$1"
	value="$2"
	if [ -s "$env_file" ]; then
		last_byte="$(tail -c 1 "$env_file" | od -An -tx1 | tr -d ' \n')"
		if [ "$last_byte" != "0a" ]; then
			printf '\n' >>"$env_file"
		fi
	fi
	printf '%s=%s\n' "$key" "$value" >>"$env_file"
}

set_default() {
	key="$1"
	value="$2"
	if [ -z "$(env_value "$key")" ]; then
		append_env "$key" "$value"
	fi
}

updater_token="$(env_value CLIRELAY_UPDATER_TOKEN)"
postgres_password="$(env_value CLIRELAY_POSTGRES_PASSWORD)"

[ -n "$updater_token" ] || updater_token="$(rand_hex 16)"
[ -n "$postgres_password" ] || postgres_password="$(rand_hex 16)"

postgres_db="$(env_value CLIRELAY_POSTGRES_DB)"
postgres_user="$(env_value CLIRELAY_POSTGRES_USER)"
redis_db="$(env_value CLIRELAY_REDIS_DB)"

postgres_db="${postgres_db:-cliproxy}"
postgres_user="${postgres_user:-cliproxy}"
redis_db="${redis_db:-0}"

set_default CLI_PROXY_IMAGE "${CLI_PROXY_IMAGE:-ghcr.io/kittors/clirelay:latest}"
set_default CLIRELAY_PROJECT_DIR "$project_dir"
set_default CLIRELAY_TARGET_SERVICE "${CLIRELAY_TARGET_SERVICE:-cli-proxy-api}"
set_default CLIRELAY_COMPOSE_PROJECT_NAME "${CLIRELAY_COMPOSE_PROJECT_NAME:-clirelay}"
set_default CLIRELAY_UPDATER_URL "http://clirelay-updater:8320"
set_default CLIRELAY_UPDATER_TOKEN "$updater_token"
set_default CLIRELAY_POSTGRES_DB "$postgres_db"
set_default CLIRELAY_POSTGRES_USER "$postgres_user"
set_default CLIRELAY_POSTGRES_PASSWORD "$postgres_password"
set_default CLIRELAY_POSTGRES_DSN "postgres://${postgres_user}:${postgres_password}@postgres:5432/${postgres_db}?sslmode=disable"
set_default CLIRELAY_POSTGRES_DATA_PATH "${project_dir}/postgres-data"
set_default CLIRELAY_REDIS_ENABLE "true"
set_default CLIRELAY_REDIS_ADDR "redis:6379"
set_default CLIRELAY_REDIS_DB "$redis_db"
set_default CLIRELAY_REDIS_DATA_PATH "${project_dir}/redis-data"
