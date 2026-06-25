#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   sudo ./install_agent.sh https://gateway.example.com
# or:
#   sudo GATEWAY_URL=https://gateway.example.com ./install_agent.sh
#
# Optional:
#   INFRAHUB_KEY may be set when the gateway requires an api-key header to
#   authorize release metadata/download access. The key is only sent as a curl
#   header and is never written to disk or systemd.
#   RUNTIME=basic downloads, verifies, installs, and execs the agent directly.
#   The default RUNTIME=systemd installs and starts a systemd service.

SERVICE_NAME="hyperstack-agent"
SERVICE_USER="hyperstack-agent"
SERVICE_GROUP="hyperstack-agent"
AGENT_BIN_PATH="/usr/local/bin/hyperstack-agent"

die() {
  echo "ERROR: $*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

validate_gateway_url() {
  case "$1" in
    https://*) ;;
    http://localhost:*|http://127.0.0.1:*|http://\[::1\]:*) ;;
    http://*)
      if [ "${ALLOW_INSECURE_HTTP:-}" = "1" ]; then
        echo "WARNING: allowing insecure HTTP URL for local/test use: $1" >&2
      else
        die "GATEWAY_URL must use https, except localhost test URLs"
      fi
      ;;
    *) die "GATEWAY_URL must use https, except localhost test URLs" ;;
  esac
  case "$1" in
    *[[:space:]\"\'\\]*|*';'*|*'|'*|*'&'*|*'$'*|*'`'*)
      die "GATEWAY_URL contains unsafe characters"
      ;;
  esac
}

header_value() {
  local header_name=$1
  local header_file=$2
  awk -v key="$header_name" '
    BEGIN { key = tolower(key) }
    {
      line = $0
      sub(/\r$/, "", line)
      pos = index(line, ":")
      if (pos > 0 && tolower(substr(line, 1, pos - 1)) == key) {
        value = substr(line, pos + 1)
        sub(/^[ \t]+/, "", value)
      }
    }
    END { print value }
  ' "$header_file"
}

resolve_download_url() {
  local gateway_url=$1
  local location=$2

  if [ -z "$location" ]; then
    printf '%s/download' "$gateway_url"
    return
  fi

  case "$location" in
    https://*|http://*) printf '%s' "$location" ;;
    /*) printf '%s%s' "$gateway_url" "$location" ;;
    *) die "download redirect Location is not an absolute URL or absolute path" ;;
  esac
}

install_systemd_service() {
  getent group "$SERVICE_GROUP" >/dev/null 2>&1 || groupadd --system "$SERVICE_GROUP"
  id -u "$SERVICE_USER" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --gid "$SERVICE_GROUP" "$SERVICE_USER"

  cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<SERVICE
[Unit]
Description=Hyperstack Monitoring Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
ExecStart=${AGENT_BIN_PATH}
Restart=always
RestartSec=5s
NoNewPrivileges=yes
CapabilityBoundingSet=
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/var/lib/${SERVICE_NAME}
ProtectControlGroups=yes
ProtectKernelLogs=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
ProcSubset=pid
SystemCallFilter=@system-service
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CPUQuota=10%
MemoryMax=300M
Environment=HYPERSTACK_URL=${GATEWAY_URL}
Environment=HYPERSTACK_INTERVAL=15s
Environment=HYPERSTACK_ENABLE_NODE=true
Environment=HYPERSTACK_ENABLE_GPU=true
Environment=HYPERSTACK_HEALTH_ADDR=127.0.0.1:9100

[Install]
WantedBy=multi-user.target
SERVICE

  mkdir -p "/var/lib/${SERVICE_NAME}"
  chown "$SERVICE_USER:$SERVICE_GROUP" "/var/lib/${SERVICE_NAME}"
  systemctl daemon-reload
  systemctl enable --now "$SERVICE_NAME"
}

GATEWAY_URL="${1:-${GATEWAY_URL:-}}"
[ -n "$GATEWAY_URL" ] || die "GATEWAY_URL is required (arg1 or env var)"
GATEWAY_URL="${GATEWAY_URL%/}"
validate_gateway_url "$GATEWAY_URL"
RUNTIME="${RUNTIME:-systemd}"
case "$RUNTIME" in
  systemd|basic) ;;
  *) die "RUNTIME must be systemd or basic" ;;
esac

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
  die "please run as root"
fi

require_cmd awk
require_cmd curl
require_cmd install
require_cmd getent
require_cmd sha256sum
if [ "$RUNTIME" = "systemd" ]; then
  require_cmd systemctl
fi

headers_file=$(mktemp)
tmp_binary=$(mktemp)
cleanup() {
  rm -f "$headers_file" "$tmp_binary"
}
trap cleanup EXIT

curl_args=(-fsS)
if [ -n "${INFRAHUB_KEY:-}" ]; then
  curl_args+=(-H "api-key: ${INFRAHUB_KEY}")
fi

release_url="${GATEWAY_URL}/download"
echo "Reading agent release metadata from ${release_url}"
curl "${curl_args[@]}" -I "$release_url" -o "$headers_file"

digest=$(header_value "Hyperstack-Agent-Digest" "$headers_file")
version=$(header_value "Hyperstack-Agent-Version" "$headers_file")
location=$(header_value "Location" "$headers_file")

expected_sha=${digest#sha256:}
case "$expected_sha" in
  "$digest") die "release digest must be in sha256:<hex> format" ;;
esac
[[ "$expected_sha" =~ ^[A-Fa-f0-9]{64}$ ]] || die "release digest is not a valid SHA-256 value"

binary_url=$(resolve_download_url "$GATEWAY_URL" "$location")
validate_gateway_url "$binary_url"

echo "Downloading Hyperstack Agent ${version:-unknown} from ${binary_url}"
curl "${curl_args[@]}" -fL "$binary_url" -o "$tmp_binary"

printf '%s  %s\n' "$expected_sha" "$tmp_binary" | sha256sum -c - >/dev/null
install -m 0755 -o root -g root "$tmp_binary" "$AGENT_BIN_PATH"

if [ "$RUNTIME" = "basic" ]; then
  echo "Starting ${SERVICE_NAME} in basic runtime"
  export HYPERSTACK_URL
  exec "$AGENT_BIN_PATH"
fi

install_systemd_service
echo "Installed ${SERVICE_NAME}. Check: systemctl status ${SERVICE_NAME}"
