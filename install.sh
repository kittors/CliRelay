#!/usr/bin/env bash
#
# CliRelay one-click Docker installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/kittors/CliRelay/main/install.sh | bash
#
set -euo pipefail

C_RESET='\033[0m'; C_BOLD='\033[1m'; C_DIM='\033[2m'
C_RED='\033[0;31m'; C_GREEN='\033[0;32m'; C_YELLOW='\033[0;33m'
C_BLUE='\033[0;34m'; C_CYAN='\033[0;36m'; C_WHITE='\033[1;37m'
C_BG_BLUE='\033[44m'; C_BG_GREEN='\033[42m'; C_BG_RED='\033[41m'

SYM_OK="✓"; SYM_FAIL="✗"; SYM_ARROW="→"; SYM_DOT="·"; SYM_STAR="★"

DOCKER_IMAGE_DEFAULT="ghcr.io/kittors/clirelay:latest"
CONTAINER_NAME="clirelay"
DEFAULT_PORT=8317
TOTAL_STEPS=6
OS_NAME="$(uname -s)"

case "${OS_NAME}" in
    Darwin)
        DEFAULT_INSTALL_DIR="${HOME}/.clirelay"
        ;;
    *)
        DEFAULT_INSTALL_DIR="/opt/clirelay"
        ;;
esac

INSTALL_DIR="${CLIRELAY_DIR:-$DEFAULT_INSTALL_DIR}"

SCRIPT_LOCALE=""
CFG_PORT="${DEFAULT_PORT}"
CFG_SECRET=""
CFG_API_KEY=""
CFG_REMOTE="true"
TZ_VALUE="${TZ:-Asia/Shanghai}"
DOCKER_PLATFORM=""
DOCKER_ARCH=""
HELPER_PATH=""

can_prompt() {
    [[ -r /dev/tty ]]
}

prompt_input() {
    local prompt="$1"
    local default_value="${2:-}"
    local reply=""

    if ! can_prompt; then
        return 1
    fi

    printf "%b" "$prompt" >/dev/tty
    if ! IFS= read -r reply </dev/tty; then
        return 1
    fi

    if [[ -z "$reply" ]]; then
        reply="$default_value"
    fi

    printf '%s' "$reply"
}

require_interactive_config() {
    if can_prompt; then
        return
    fi
    if is_zh; then
        fail "当前运行方式没有可交互终端，无法完成安装配置。请直接执行 'bash install.sh'，或先下载脚本后在交互终端中运行。"
    else
        fail "No interactive terminal is available for configuration. Please run 'bash install.sh' directly, or download the script and run it from an interactive terminal."
    fi
}

normalize_locale() {
    local raw
    raw="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"
    case "$raw" in
        zh|zh-cn|zh_cn|cn|chinese) echo "zh" ;;
        en|en-us|en_us|en-gb|en_gb|english) echo "en" ;;
        *) echo "" ;;
    esac
}

detect_default_locale() {
    local candidate
    candidate="$(normalize_locale "${CLIRELAY_LOCALE:-}")"
    if [[ -n "$candidate" ]]; then
        echo "$candidate"
        return
    fi
    case "${LC_ALL:-${LANGUAGE:-${LANG:-}}}" in
        zh*|*zh_CN*|*zh_HK*|*zh_TW*|*ZH*) echo "zh" ;;
        *) echo "en" ;;
    esac
}

set_locale() {
    local normalized
    normalized="$(normalize_locale "${1:-}")"
    if [[ -n "$normalized" ]]; then
        SCRIPT_LOCALE="$normalized"
    fi
}

is_zh() {
    [[ "${SCRIPT_LOCALE}" == "zh" ]]
}

banner() {
    echo ""
    echo -e "${C_CYAN}${C_BOLD}"
    cat << 'EOF'
   ██████╗██╗     ██╗██████╗ ███████╗██╗      █████╗ ██╗   ██╗
  ██╔════╝██║     ██║██╔══██╗██╔════╝██║     ██╔══██╗╚██╗ ██╔╝
  ██║     ██║     ██║██████╔╝█████╗  ██║     ███████║ ╚████╔╝
  ██║     ██║     ██║██╔══██╗██╔══╝  ██║     ██╔══██║  ╚██╔╝
  ╚██████╗███████╗██║██║  ██║███████╗███████╗██║  ██║   ██║
   ╚═════╝╚══════╝╚═╝╚═╝  ╚═╝╚══════╝╚══════╝╚═╝  ╚═╝   ╚═╝
EOF
    echo -e "${C_RESET}"
    if is_zh; then
        echo -e "  ${C_DIM}AI Proxy Gateway ${SYM_DOT} Docker 一键部署${C_RESET}"
    else
        echo -e "  ${C_DIM}AI Proxy Gateway ${SYM_DOT} One-click Docker deployment${C_RESET}"
    fi
    echo -e "  ${C_DIM}────────────────────────────────────────────────${C_RESET}"
    echo ""
}

info()    { echo -e "  ${C_BLUE}${SYM_ARROW}${C_RESET} $*"; }
success() { echo -e "  ${C_GREEN}${SYM_OK}${C_RESET} $*"; }
warn()    { echo -e "  ${C_YELLOW}!${C_RESET} $*"; }
fail()    { echo -e "  ${C_RED}${SYM_FAIL}${C_RESET} $*"; exit 1; }
step()    { echo -e "\n${C_WHITE}${C_BOLD}  [$1/$TOTAL_STEPS] $2${C_RESET}"; }

progress_bar() {
    local current=$1 total=$2 width=40
    local filled=$((current * width / total))
    local empty=$((width - filled))
    local bar=""
    for ((i=0; i<filled; i++)); do bar+="█"; done
    for ((i=0; i<empty; i++)); do bar+="░"; done
    printf "\r  ${C_CYAN}[${bar}]${C_RESET} ${C_BOLD}%3d%%${C_RESET}" $((current * 100 / total))
}

spin_exec() {
    local msg="$1"; shift
    local spinchars='⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏'
    local tmplog
    tmplog="$(mktemp)"
    "$@" >"$tmplog" 2>&1 &
    local pid=$! i=0
    while kill -0 "$pid" 2>/dev/null; do
        printf "\r  ${C_CYAN}%s${C_RESET} %s" "${spinchars:i%${#spinchars}:1}" "$msg"
        i=$((i + 1))
        sleep 0.1
    done
    if wait "$pid"; then
        printf '\r\033[K'
        success "$msg"
    else
        printf '\r\033[K'
        fail "$msg ($(tail -3 "$tmplog"))"
    fi
    rm -f "$tmplog"
}

rand_hex() {
    openssl rand -hex "$1" 2>/dev/null || head -c $(( $1 * 2 )) /dev/urandom | xxd -p | head -c $(( $1 * 2 ))
}

compose_cmd() {
    docker compose --env-file "${INSTALL_DIR}/.env" -f "${INSTALL_DIR}/docker-compose.yml" "$@"
}

http_code() {
    local url="$1"
    if command -v curl >/dev/null 2>&1; then
        curl -s -o /dev/null -w "%{http_code}" --max-time 5 "$url" 2>/dev/null || echo "000"
        return
    fi
    if command -v wget >/dev/null 2>&1; then
        if wget -q -T 5 --spider "$url" >/dev/null 2>&1; then
            echo "200"
        else
            echo "000"
        fi
        return
    fi
    echo "000"
}

lang_tag() {
    if is_zh; then
        echo "zh_CN.UTF-8"
    else
        echo "en_US.UTF-8"
    fi
}

language_tag() {
    if is_zh; then
        echo "zh_CN:zh"
    else
        echo "en_US:en"
    fi
}

detect_arch() {
    local raw_arch
    raw_arch="$(uname -m)"
    case "$raw_arch" in
        x86_64|amd64)
            DOCKER_ARCH="amd64"
            DOCKER_PLATFORM="linux/amd64"
            ;;
        aarch64|arm64)
            DOCKER_ARCH="arm64"
            DOCKER_PLATFORM="linux/arm64"
            ;;
        *)
            if is_zh; then
                fail "暂不支持的 CPU 架构: ${raw_arch}。当前一键部署仅支持 amd64 和 arm64。"
            else
                fail "Unsupported CPU architecture: ${raw_arch}. One-click deployment currently supports amd64 and arm64 only."
            fi
            ;;
    esac
}

select_language() {
    if [[ -n "${SCRIPT_LOCALE}" ]]; then
        return
    fi
    set_locale "$(detect_default_locale)"
    if ! can_prompt; then
        return
    fi

    local current="${SCRIPT_LOCALE}"
    local default_choice="1"
    if [[ "$current" == "zh" ]]; then
        default_choice="2"
    fi

    echo ""
    echo -e "  ${C_BG_BLUE}${C_WHITE}${C_BOLD} Language / 语言 ${C_RESET}"
    echo ""
    echo "  1) English"
    echo "  2) 中文"
    choice="$(prompt_input "  $(echo -e "${C_CYAN}?${C_RESET}") Select language / 选择语言 [${default_choice}]: " "${default_choice}")" || choice="${default_choice}"
    case "${choice:-$default_choice}" in
        1|en|EN|english|English) set_locale "en" ;;
        2|zh|ZH|cn|CN|中文) set_locale "zh" ;;
        *) ;;
    esac
}

check_dependencies() {
    local missing=()
    if ! command -v bash >/dev/null 2>&1; then missing+=("bash"); fi
    if ! command -v sed >/dev/null 2>&1; then missing+=("sed"); fi
    if ! command -v awk >/dev/null 2>&1; then missing+=("awk"); fi
    if ! command -v grep >/dev/null 2>&1; then missing+=("grep"); fi
    if [[ ${#missing[@]} -gt 0 ]]; then
        if is_zh; then
            fail "缺少基础命令: ${missing[*]}"
        else
            fail "Missing required base commands: ${missing[*]}"
        fi
    fi
}

check_docker() {
    if ! command -v docker >/dev/null 2>&1; then
        if [[ "${OS_NAME}" == "Darwin" ]]; then
            if is_zh; then
                fail "macOS 不支持通过本脚本自动安装 Docker。请先安装并启动 Docker Desktop、OrbStack 或 Colima，然后重新运行安装脚本。"
            else
                fail "Automatic Docker installation is not supported on macOS. Please install and start Docker Desktop, OrbStack, or Colima first, then rerun the installer."
            fi
        fi

        if is_zh; then
            warn "Docker 未安装，开始自动安装..."
        else
            warn "Docker is not installed. Installing automatically..."
        fi
        if command -v curl >/dev/null 2>&1; then
            curl -fsSL https://get.docker.com | sh
        elif command -v wget >/dev/null 2>&1; then
            wget -qO- https://get.docker.com | sh
        else
            if is_zh; then
                fail "请先安装 curl 或 wget"
            else
                fail "Please install curl or wget first."
            fi
        fi
        if command -v systemctl >/dev/null 2>&1; then
            systemctl enable docker 2>/dev/null || true
            systemctl start docker 2>/dev/null || true
        fi
    fi

    if ! docker info >/dev/null 2>&1; then
        if [[ "${OS_NAME}" == "Darwin" ]]; then
            if is_zh; then
                fail "检测到 Docker 命令，但 Docker 引擎未启动。请先启动 Docker Desktop、OrbStack 或 Colima。"
            else
                fail "Docker CLI is installed, but the Docker engine is not running. Please start Docker Desktop, OrbStack, or Colima first."
            fi
        fi
        if is_zh; then
            fail "Docker 已安装，但 Docker daemon 未启动。请先启动 Docker 服务后重试。"
        else
            fail "Docker is installed, but the Docker daemon is not running. Please start Docker and try again."
        fi
    fi

    if ! docker compose version >/dev/null 2>&1; then
        if [[ "${OS_NAME}" == "Darwin" ]]; then
            if is_zh; then
                fail "当前 Docker 环境缺少 docker compose。请升级 Docker Desktop/OrbStack，或使用 Homebrew 安装 docker-compose 后重试。"
            else
                fail "Your Docker environment does not provide docker compose. Upgrade Docker Desktop/OrbStack or install docker-compose with Homebrew, then retry."
            fi
        fi
        if is_zh; then
            warn "Docker Compose 插件未安装，开始自动安装..."
        else
            warn "Docker Compose plugin is missing. Installing automatically..."
        fi
        local plugin_dir
        plugin_dir="${DOCKER_CONFIG:-${HOME}/.docker}/cli-plugins"
        mkdir -p "$plugin_dir"
        local plugin_arch
        case "$DOCKER_ARCH" in
            amd64) plugin_arch="x86_64" ;;
            arm64) plugin_arch="aarch64" ;;
            *)
                plugin_arch="$DOCKER_ARCH"
                ;;
        esac
        local plugin_url="https://github.com/docker/compose/releases/latest/download/docker-compose-linux-${plugin_arch}"
        if command -v curl >/dev/null 2>&1; then
            curl -fsSL "$plugin_url" -o "${plugin_dir}/docker-compose"
        else
            wget -qO "${plugin_dir}/docker-compose" "$plugin_url"
        fi
        chmod +x "${plugin_dir}/docker-compose"
    fi

    if is_zh; then
        success "Docker 环境已就绪 ($(docker --version | awk '{print $3}' | tr -d ','), ${DOCKER_PLATFORM})"
    else
        success "Docker is ready ($(docker --version | awk '{print $3}' | tr -d ','), ${DOCKER_PLATFORM})"
    fi
}

extract_port() {
    grep -E '^port:' "${INSTALL_DIR}/config.yaml" 2>/dev/null | awk '{print $2}' | head -1
}

extract_secret() {
    grep -E 'secret-key:' "${INSTALL_DIR}/config.yaml" 2>/dev/null | head -1 | sed -E 's/.*secret-key:[[:space:]]*"?(.*)"?/\1/' | tr -d '"'
}

extract_api_key() {
    grep -E '^ *- "sk-' "${INSTALL_DIR}/config.yaml" 2>/dev/null | head -1 | sed -E 's/.*"(.*)".*/\1/'
}

load_existing_state() {
    if [[ -f "${INSTALL_DIR}/.env" ]]; then
        # shellcheck disable=SC1090
        set -a && . "${INSTALL_DIR}/.env" && set +a
        if [[ -n "${CLIRELAY_LOCALE:-}" ]]; then
            set_locale "${CLIRELAY_LOCALE}"
        fi
        if [[ -n "${CLI_PROXY_PLATFORM:-}" ]]; then
            DOCKER_PLATFORM="${CLI_PROXY_PLATFORM}"
        fi
    fi

    local existing_port existing_secret existing_api_key
    existing_port="$(extract_port || true)"
    existing_secret="$(extract_secret || true)"
    existing_api_key="$(extract_api_key || true)"

    if [[ -n "$existing_port" ]]; then CFG_PORT="$existing_port"; fi
    if [[ -n "$existing_secret" ]]; then CFG_SECRET="$existing_secret"; fi
    if [[ -n "$existing_api_key" ]]; then CFG_API_KEY="$existing_api_key"; fi
}

prompt_config() {
    require_interactive_config
    echo ""
    if is_zh; then
        echo -e "  ${C_BG_BLUE}${C_WHITE}${C_BOLD} 服务配置 ${C_RESET}"
    else
        echo -e "  ${C_BG_BLUE}${C_WHITE}${C_BOLD} Service Configuration ${C_RESET}"
    fi
    echo ""

    local port_prompt secret_prompt key_prompt remote_prompt
    if is_zh; then
        port_prompt="服务端口"
        secret_prompt="管理面板密钥 (留空自动生成)"
        key_prompt="客户端 API Key (留空自动生成)"
        remote_prompt="允许远程管理? [Y/n]"
    else
        port_prompt="Service port"
        secret_prompt="Management secret (leave blank to auto-generate)"
        key_prompt="Client API key (leave blank to auto-generate)"
        remote_prompt="Allow remote management? [Y/n]"
    fi

    CFG_PORT="$(prompt_input "  $(echo -e "${C_CYAN}?${C_RESET}") ${port_prompt} [${DEFAULT_PORT}]: " "${DEFAULT_PORT}")" || CFG_PORT="${DEFAULT_PORT}"
    CFG_PORT="${CFG_PORT:-$DEFAULT_PORT}"

    CFG_SECRET="$(prompt_input "  $(echo -e "${C_CYAN}?${C_RESET}") ${secret_prompt}: " "")" || CFG_SECRET=""
    if [[ -z "$CFG_SECRET" ]]; then
        CFG_SECRET="$(rand_hex 16)"
        if is_zh; then
            echo -e "  ${C_DIM}  已生成: ${CFG_SECRET}${C_RESET}"
        else
            echo -e "  ${C_DIM}  Generated: ${CFG_SECRET}${C_RESET}"
        fi
    fi

    CFG_API_KEY="$(prompt_input "  $(echo -e "${C_CYAN}?${C_RESET}") ${key_prompt}: " "")" || CFG_API_KEY=""
    if [[ -z "$CFG_API_KEY" ]]; then
        CFG_API_KEY="sk-$(rand_hex 16)"
        if is_zh; then
            echo -e "  ${C_DIM}  已生成: ${CFG_API_KEY}${C_RESET}"
        else
            echo -e "  ${C_DIM}  Generated: ${CFG_API_KEY}${C_RESET}"
        fi
    fi

    yn="$(prompt_input "  $(echo -e "${C_CYAN}?${C_RESET}") ${remote_prompt} " "")" || yn=""
    case "$yn" in
        [Nn]*) CFG_REMOTE="false" ;;
        *) CFG_REMOTE="true" ;;
    esac
}

write_config() {
    cat >"${INSTALL_DIR}/config.yaml" <<YAML
# CliRelay configuration file - generated by install.sh
host: ""
port: ${CFG_PORT}

redis:
  enable: false

remote-management:
  allow-remote: ${CFG_REMOTE}
  secret-key: "${CFG_SECRET}"
  disable-control-panel: false

auto-update:
  enabled: true
  channel: main
  repository: https://github.com/kittors/CliRelay
  docker-image: ghcr.io/kittors/clirelay
  updater-url: http://clirelay-updater:8320

auth-dir: "/root/.cli-proxy-api"

api-keys:
  - "${CFG_API_KEY}"

debug: false
logging-to-file: true
logs-max-total-size-mb: 100
usage-statistics-enabled: true
request-retry: 3
max-retry-interval: 30
routing:
  strategy: "round-robin"
YAML
}

write_env() {
    cat >"${INSTALL_DIR}/.env" <<EOF
CLI_PROXY_IMAGE=${CLI_PROXY_IMAGE:-$DOCKER_IMAGE_DEFAULT}
CLI_PROXY_PLATFORM=${DOCKER_PLATFORM}
CLIRELAY_CONTAINER_NAME=${CONTAINER_NAME}
CLIRELAY_INSTALL_DIR=${INSTALL_DIR}
CLIRELAY_LOCALE=${SCRIPT_LOCALE}
CLIRELAY_LANG=$(lang_tag)
CLIRELAY_LANGUAGE=$(language_tag)
CLIRELAY_PORT=${CFG_PORT}
CLIRELAY_UPDATE_CHANNEL=main
CLIRELAY_UPDATER_URL=http://clirelay-updater:8320
CLIRELAY_UPDATER_TOKEN=$(rand_hex 16)
CLIRELAY_TARGET_SERVICE=clirelay
CLIRELAY_COMPOSE_PROJECT_NAME=$(basename "${INSTALL_DIR}")
CLI_PROXY_CONFIG_PATH=${INSTALL_DIR}/config.yaml
CLI_PROXY_AUTH_PATH=${INSTALL_DIR}/auths
AUTH_PATH=/root/.cli-proxy-api
CLI_PROXY_LOG_PATH=${INSTALL_DIR}/logs
CLI_PROXY_DATA_PATH=${INSTALL_DIR}/data
TZ=${TZ_VALUE}
EOF
}

write_compose() {
    cat >"${INSTALL_DIR}/docker-compose.yml" <<'YAML'
services:
  clirelay:
    image: ${CLI_PROXY_IMAGE}
    platform: ${CLI_PROXY_PLATFORM}
    container_name: ${CLIRELAY_CONTAINER_NAME}
    pull_policy: always
    ports:
      - "${CLIRELAY_PORT}:${CLIRELAY_PORT}"
    volumes:
      - ${CLI_PROXY_CONFIG_PATH}:/CLIProxyAPI/config.yaml
      - ${CLI_PROXY_AUTH_PATH}:${AUTH_PATH}
      - ${CLI_PROXY_LOG_PATH}:/CLIProxyAPI/logs
      - ${CLI_PROXY_DATA_PATH}:/CLIProxyAPI/data
    environment:
      TZ: ${TZ}
      CLIRELAY_LOCALE: ${CLIRELAY_LOCALE}
      CLIRELAY_UPDATE_CHANNEL: ${CLIRELAY_UPDATE_CHANNEL}
      CLIRELAY_UPDATER_URL: ${CLIRELAY_UPDATER_URL}
      CLIRELAY_UPDATER_TOKEN: ${CLIRELAY_UPDATER_TOKEN}
      CLIRELAY_TARGET_SERVICE: ${CLIRELAY_TARGET_SERVICE}
      AUTH_PATH: ${AUTH_PATH}
      LANG: ${CLIRELAY_LANG}
      LANGUAGE: ${CLIRELAY_LANGUAGE}
      LC_ALL: ${CLIRELAY_LANG}
      PORT: ${CLIRELAY_PORT}
    healthcheck:
      test: ["CMD-SHELL", "wget -q -O- http://127.0.0.1:${CLIRELAY_PORT}/ >/dev/null 2>&1 || exit 1"]
      interval: 30s
      timeout: 5s
      retries: 5
      start_period: 20s
    restart: unless-stopped

  clirelay-updater:
    image: ${CLI_PROXY_IMAGE}
    platform: ${CLI_PROXY_PLATFORM}
    command: ["./clirelay-updater"]
    environment:
      CLIRELAY_UPDATER_TOKEN: ${CLIRELAY_UPDATER_TOKEN}
      CLIRELAY_COMPOSE_FILE: ${CLIRELAY_INSTALL_DIR}/docker-compose.yml
      CLIRELAY_ENV_FILE: ${CLIRELAY_INSTALL_DIR}/.env
      CLIRELAY_COMPOSE_PROJECT_NAME: ${CLIRELAY_COMPOSE_PROJECT_NAME}
      CLIRELAY_TARGET_SERVICE: ${CLIRELAY_TARGET_SERVICE}
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./docker-compose.yml:${CLIRELAY_INSTALL_DIR}/docker-compose.yml:ro
      - ./.env:${CLIRELAY_INSTALL_DIR}/.env
    restart: unless-stopped
YAML
}

install_helper_command() {
    local target
    if [[ -d "/usr/local/bin" && -w "/usr/local/bin" ]]; then
        target="/usr/local/bin/clirelay"
    elif [[ ! -e "/usr/local/bin" ]] && mkdir -p /usr/local/bin 2>/dev/null; then
        target="/usr/local/bin/clirelay"
    else
        target="${HOME}/.local/bin/clirelay"
        mkdir -p "$(dirname "$target")"
    fi

    cat >"$target" <<EOF
#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR}"
ENV_FILE="\${INSTALL_DIR}/.env"
COMPOSE_FILE="\${INSTALL_DIR}/docker-compose.yml"
CONFIG_FILE="\${INSTALL_DIR}/config.yaml"

if [[ ! -f "\${COMPOSE_FILE}" ]] || [[ ! -f "\${ENV_FILE}" ]]; then
  echo "CliRelay is not installed at \${INSTALL_DIR}." >&2
  exit 1
fi

normalize_locale() {
  local raw
  raw="\$(printf '%s' "\${1:-}" | tr '[:upper:]' '[:lower:]')"
  case "\$raw" in
    zh|zh-cn|zh_cn|cn|chinese) echo "zh" ;;
    en|en-us|en_us|en-gb|en_gb|english) echo "en" ;;
    *) echo "" ;;
  esac
}

load_env() {
  # shellcheck disable=SC1090
  set -a && . "\${ENV_FILE}" && set +a
  CLIRELAY_LOCALE="\$(normalize_locale "\${CLIRELAY_LOCALE:-en}")"
  if [[ -z "\${CLIRELAY_LOCALE}" ]]; then
    CLIRELAY_LOCALE="en"
  fi
}

is_zh() {
  [[ "\${CLIRELAY_LOCALE}" == "zh" ]]
}

say() {
  local zh_text="\$1"
  local en_text="\$2"
  if is_zh; then
    printf '%s\n' "\$zh_text"
  else
    printf '%s\n' "\$en_text"
  fi
}

compose() {
  docker compose --env-file "\${ENV_FILE}" -f "\${COMPOSE_FILE}" "\$@"
}

extract_port() {
  grep -E '^port:' "\${CONFIG_FILE}" 2>/dev/null | awk '{print \$2}' | head -1
}

extract_secret() {
  grep -E 'secret-key:' "\${CONFIG_FILE}" 2>/dev/null | head -1 | sed -E 's/.*secret-key:[[:space:]]*"?(.*)"?/\1/' | tr -d '"'
}

http_code() {
  local url="\$1"
  if command -v curl >/dev/null 2>&1; then
    curl -s -o /dev/null -w "%{http_code}" --max-time 5 "\$url" 2>/dev/null || echo "000"
    return
  fi
  if command -v wget >/dev/null 2>&1; then
    if wget -q -T 5 --spider "\$url" >/dev/null 2>&1; then
      echo "200"
    else
      echo "000"
    fi
    return
  fi
  echo "000"
}

status_cmd() {
  load_env
  local port secret root_code panel_code latest
  port="\$(extract_port)"
  port="\${port:-\${CLIRELAY_PORT:-8317}}"
  secret="\$(extract_secret)"
  root_code="\$(http_code "http://127.0.0.1:\${port}/")"
  panel_code="\$(http_code "http://127.0.0.1:\${port}/manage")"

  say "CliRelay 部署状态" "CliRelay deployment status"
  echo "  Install Dir : \${INSTALL_DIR}"
  echo "  Image       : \${CLI_PROXY_IMAGE}"
  echo "  Platform    : \${CLI_PROXY_PLATFORM}"
  echo "  Locale      : \${CLIRELAY_LOCALE}"
  echo "  Port        : \${port}"
  echo "  Container   : \${CLIRELAY_CONTAINER_NAME}"
  echo "  API /       : HTTP \${root_code}"
  echo "  Panel /manage: HTTP \${panel_code}"

  if docker inspect "\${CLIRELAY_CONTAINER_NAME}" >/dev/null 2>&1; then
    echo "  Runtime     : \$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "\${CLIRELAY_CONTAINER_NAME}")"
    echo "  Started At  : \$(docker inspect -f '{{.State.StartedAt}}' "\${CLIRELAY_CONTAINER_NAME}")"
  else
    say "  Runtime     : 容器不存在" "  Runtime     : container not found"
  fi

  if [[ -n "\$secret" ]] && command -v curl >/dev/null 2>&1; then
    latest="\$(curl -fsSL -H "Authorization: Bearer \$secret" "http://127.0.0.1:\${port}/v0/management/latest-version" 2>/dev/null | sed -n 's/.*"latest-version":"\([^"]*\)".*/\1/p' | head -1 || true)"
    if [[ -n "\$latest" ]]; then
      echo "  Latest      : \${latest}"
    fi
  fi

  echo ""
  compose ps || true
}

update_cmd() {
  load_env
  say "正在拉取最新镜像并更新服务..." "Pulling the latest image and updating the service..."
  compose pull
  compose up -d --remove-orphans
  status_cmd
}

restart_cmd() {
  load_env
  say "正在重启服务..." "Restarting service..."
  compose restart
  status_cmd
}

logs_cmd() {
  load_env
  compose logs -f --tail=200
}

tui_cmd() {
  load_env
  local secret
  secret="\$(extract_secret)"
  if [[ -z "\$secret" ]]; then
    say "无法启动 TUI：未找到 management secret。" "Unable to start TUI: management secret not found."
    exit 1
  fi
  exec docker exec -e CLIRELAY_LOCALE="\${CLIRELAY_LOCALE}" -it "\${CLIRELAY_CONTAINER_NAME}" /CLIProxyAPI/CLIProxyAPI -config /CLIProxyAPI/config.yaml -password "\${secret}" -tui
}

help_cmd() {
  load_env
  say "用法: clirelay <status|update|restart|logs|tui>" "Usage: clirelay <status|update|restart|logs|tui>"
}

case "\${1:-status}" in
  status) status_cmd ;;
  update) update_cmd ;;
  restart) restart_cmd ;;
  logs) logs_cmd ;;
  tui) tui_cmd ;;
  help|--help|-h) help_cmd ;;
  *)
    help_cmd
    exit 1
    ;;
esac
EOF

    chmod +x "$target"
    HELPER_PATH="$target"
}

wait_for_ready() {
    local ready=false
    local i
    for i in $(seq 1 30); do
        progress_bar "$i" 30
        if [[ "$(http_code "http://127.0.0.1:${CFG_PORT}/")" == "200" ]]; then
            ready=true
            break
        fi
        sleep 1
    done
    echo ""
    [[ "$ready" == "true" ]]
}

show_result() {
    local public_ip root_code panel_code
    if command -v curl >/dev/null 2>&1; then
        public_ip="$(curl -s --max-time 5 https://api.ipify.org 2>/dev/null || true)"
    elif command -v wget >/dev/null 2>&1; then
        public_ip="$(wget -qO- --timeout=5 https://api.ipify.org 2>/dev/null || true)"
    else
        public_ip=""
    fi
    public_ip="${public_ip:-YOUR_SERVER_IP}"
    root_code="$(http_code "http://127.0.0.1:${CFG_PORT}/")"
    panel_code="$(http_code "http://127.0.0.1:${CFG_PORT}/manage")"

    echo ""
    if is_zh; then
        echo -e "  ${C_BG_GREEN}${C_WHITE}${C_BOLD} 部署完成 ${C_RESET}"
        echo ""
        echo -e "  ${C_GREEN}${SYM_STAR}${C_RESET} ${C_BOLD}CliRelay 已成功部署${C_RESET}"
    else
        echo -e "  ${C_BG_GREEN}${C_WHITE}${C_BOLD} Deployment Complete ${C_RESET}"
        echo ""
        echo -e "  ${C_GREEN}${SYM_STAR}${C_RESET} ${C_BOLD}CliRelay has been deployed successfully${C_RESET}"
    fi
    echo ""
    echo "  Install Dir : ${INSTALL_DIR}"
    echo "  Image       : ${CLI_PROXY_IMAGE:-$DOCKER_IMAGE_DEFAULT}"
    echo "  Platform    : ${DOCKER_PLATFORM}"
    echo "  Locale      : ${SCRIPT_LOCALE}"
    echo "  API         : http://${public_ip}:${CFG_PORT}/v1/chat/completions"
    echo "  Panel       : http://${public_ip}:${CFG_PORT}/manage"
    echo "  API /       : HTTP ${root_code}"
    echo "  Panel /manage: HTTP ${panel_code}"
    echo "  Helper      : ${HELPER_PATH}"
    echo ""
    if is_zh; then
        echo -e "  ${C_DIM}常用命令:${C_RESET}"
        echo -e "    ${C_YELLOW}clirelay status${C_RESET}   查看部署状态"
        echo -e "    ${C_YELLOW}clirelay update${C_RESET}   拉取最新镜像并更新"
        echo -e "    ${C_YELLOW}clirelay restart${C_RESET}  重启服务"
        echo -e "    ${C_YELLOW}clirelay logs${C_RESET}     查看实时日志"
        echo -e "    ${C_YELLOW}clirelay tui${C_RESET}      进入容器内 TUI（语言随部署配置）"
    else
        echo -e "  ${C_DIM}Common commands:${C_RESET}"
        echo -e "    ${C_YELLOW}clirelay status${C_RESET}   Show deployment status"
        echo -e "    ${C_YELLOW}clirelay update${C_RESET}   Pull the latest image and update"
        echo -e "    ${C_YELLOW}clirelay restart${C_RESET}  Restart the service"
        echo -e "    ${C_YELLOW}clirelay logs${C_RESET}     Tail service logs"
        echo -e "    ${C_YELLOW}clirelay tui${C_RESET}      Open the container TUI in the selected language"
    fi
    echo ""
}

uninstall() {
    select_language
    banner
    if is_zh; then
        echo -e "  ${C_BG_RED}${C_WHITE}${C_BOLD} 卸载 CliRelay ${C_RESET}"
        yn="$(prompt_input "  $(echo -e "${C_RED}?${C_RESET}") 确认卸载? 配置和数据将被删除 [y/N]: " "N")" || yn="N"
    else
        echo -e "  ${C_BG_RED}${C_WHITE}${C_BOLD} Uninstall CliRelay ${C_RESET}"
        yn="$(prompt_input "  $(echo -e "${C_RED}?${C_RESET}") Confirm uninstall? Configuration and data will be deleted [y/N]: " "N")" || yn="N"
    fi
    case "$yn" in
        [Yy]*)
            if [[ -f "${INSTALL_DIR}/docker-compose.yml" ]] && command -v docker >/dev/null 2>&1; then
                compose_cmd down --remove-orphans 2>/dev/null || true
            fi
            rm -rf "${INSTALL_DIR}"
            rm -f "/usr/local/bin/clirelay" "${HOME}/.local/bin/clirelay" 2>/dev/null || true
            if is_zh; then
                success "CliRelay 已完全卸载"
            else
                success "CliRelay has been removed completely"
            fi
            ;;
        *)
            if is_zh; then
                info "取消卸载"
            else
                info "Uninstall cancelled"
            fi
            ;;
    esac
}

main() {
    check_dependencies
    select_language
    detect_arch
    banner

    if is_zh; then
        info "系统: $(uname -s)/$(uname -m)"
    else
        info "System: $(uname -s)/$(uname -m)"
    fi

    local is_update=false
    if [[ -d "${INSTALL_DIR}" ]] && [[ -f "${INSTALL_DIR}/docker-compose.yml" ]]; then
        load_existing_state
        if [[ -z "${SCRIPT_LOCALE}" ]]; then
            set_locale "$(detect_default_locale)"
        fi
        banner
        if is_zh; then
            warn "检测到已有安装: ${INSTALL_DIR}"
            choice="$(prompt_input "  $(echo -e "${C_CYAN}?${C_RESET}") 选择操作 [1=更新 2=重装 3=取消]: " "1")" || choice="1"
        else
            warn "Existing installation detected at ${INSTALL_DIR}"
            choice="$(prompt_input "  $(echo -e "${C_CYAN}?${C_RESET}") Choose action [1=update 2=reinstall 3=cancel]: " "1")" || choice="1"
        fi
        case "$choice" in
            1) is_update=true ;;
            3)
                if is_zh; then
                    info "已取消"
                else
                    info "Cancelled"
                fi
                exit 0
                ;;
            *) is_update=false ;;
        esac
    fi

    step 1 "$(is_zh && echo "检查架构与基础命令" || echo "Checking architecture and base commands")"
    if is_zh; then
        success "已识别部署平台: ${DOCKER_PLATFORM}"
    else
        success "Detected deployment platform: ${DOCKER_PLATFORM}"
    fi

    step 2 "$(is_zh && echo "检查 Docker 环境" || echo "Checking Docker environment")"
    check_docker

    step 3 "$(is_zh && echo "准备配置与部署元数据" || echo "Preparing configuration and deployment metadata")"
    mkdir -p "${INSTALL_DIR}" "${INSTALL_DIR}/auths" "${INSTALL_DIR}/logs" "${INSTALL_DIR}/data"
    if [[ "$is_update" == "true" ]]; then
        if is_zh; then
            success "保留现有配置: ${INSTALL_DIR}/config.yaml"
        else
            success "Keeping existing configuration: ${INSTALL_DIR}/config.yaml"
        fi
    else
        prompt_config
        write_config
        if is_zh; then
            success "已写入配置文件"
        else
            success "Configuration file written"
        fi
    fi
    write_env
    write_compose
    install_helper_command

    step 4 "$(is_zh && echo "拉取 Docker 镜像" || echo "Pulling Docker image")"
    spin_exec "${CLI_PROXY_IMAGE:-$DOCKER_IMAGE_DEFAULT}" docker pull "${CLI_PROXY_IMAGE:-$DOCKER_IMAGE_DEFAULT}"

    step 5 "$(is_zh && echo "启动或更新服务" || echo "Starting or updating service")"
    if [[ "$is_update" == "true" ]]; then
        compose_cmd down --remove-orphans 2>/dev/null || true
    fi
    compose_cmd up -d --remove-orphans
    if is_zh; then
        info "等待服务就绪..."
    else
        info "Waiting for the service to become ready..."
    fi
    if wait_for_ready; then
        if is_zh; then
            success "服务已启动并就绪"
        else
            success "Service is up and ready"
        fi
    else
        if is_zh; then
            warn "服务未在预期时间内完成启动，可稍后执行 clirelay status / clirelay logs 检查。"
        else
            warn "Service did not become ready in time. Run clirelay status or clirelay logs to inspect it."
        fi
    fi

    step 6 "$(is_zh && echo "验证部署结果" || echo "Verifying deployment result")"
    show_result
}

case "${1:-}" in
    --uninstall|uninstall) uninstall ;;
    --help|-h|help)
        set_locale "$(detect_default_locale)"
        banner
        if is_zh; then
            echo "  用法:"
            echo "    bash install.sh              安装或更新"
            echo "    bash install.sh uninstall    卸载"
            echo ""
            echo "  一键安装:"
            echo "    curl -fsSL https://raw.githubusercontent.com/kittors/CliRelay/main/install.sh | bash"
        else
            echo "  Usage:"
            echo "    bash install.sh              Install or update"
            echo "    bash install.sh uninstall    Uninstall"
            echo ""
            echo "  One-click install:"
            echo "    curl -fsSL https://raw.githubusercontent.com/kittors/CliRelay/main/install.sh | bash"
        fi
        ;;
    *)
        set_locale "$(detect_default_locale)"
        main
        ;;
esac
