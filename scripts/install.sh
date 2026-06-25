#!/usr/bin/env bash
set -euo pipefail

# Install script for Hyperstack agent (systemd service)

BINARY_SOURCE=${BINARY_SOURCE:-}
INSTALL_DIR=${INSTALL_DIR:-/opt/hyperstack-agent}
BIN_NAME=${BIN_NAME:-hyperstack-agent}
SERVICE_NAME=${SERVICE_NAME:-hyperstack-agent}
USER_NAME=${USER_NAME:-hyperstack-agent}
GROUP_NAME=${GROUP_NAME:-hyperstack-agent}

die() {
  echo "ERROR: $*" >&2
  exit 1
}

validate_name() {
  local label=$1
  local value=$2
  case "$value" in
    ""|*/*|*[[:space:]]*|*[^A-Za-z0-9_.@-]*)
      die "$label contains unsafe characters: $value"
      ;;
  esac
}

validate_path() {
  local label=$1
  local value=$2
  case "$value" in
    /*) ;;
    *) die "$label must be an absolute path" ;;
  esac
  case "$value" in
    *[[:space:]\"\'\\]*|*';'*|*'|'*|*'&'*|*'$'*|*'`'*)
      die "$label contains unsafe characters: $value"
      ;;
  esac
}

if [[ -z "${BINARY_SOURCE}" ]]; then
  die "BINARY_SOURCE is required (path to agent binary)"
fi

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  die "please run as root"
fi

validate_path "BINARY_SOURCE" "$BINARY_SOURCE"
validate_path "INSTALL_DIR" "$INSTALL_DIR"
validate_name "BIN_NAME" "$BIN_NAME"
validate_name "SERVICE_NAME" "$SERVICE_NAME"
validate_name "USER_NAME" "$USER_NAME"
validate_name "GROUP_NAME" "$GROUP_NAME"

[[ -f "$BINARY_SOURCE" ]] || die "BINARY_SOURCE does not exist or is not a file: $BINARY_SOURCE"

getent group "$GROUP_NAME" >/dev/null 2>&1 || groupadd --system "$GROUP_NAME"
id -u "$USER_NAME" &>/dev/null || useradd --system --no-create-home --shell /usr/sbin/nologin --gid "$GROUP_NAME" "$USER_NAME"
mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/logs" "$INSTALL_DIR/buffer"
install -m 0755 -o root -g root "$BINARY_SOURCE" "$INSTALL_DIR/bin/$BIN_NAME"
chown root:root "$INSTALL_DIR" "$INSTALL_DIR/bin"
chown -R "$USER_NAME":"$GROUP_NAME" "$INSTALL_DIR/logs" "$INSTALL_DIR/buffer"

# Create systemd service
SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
cat > "$SERVICE_PATH" <<SERVICE
[Unit]
Description=Hyperstack Monitoring Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$USER_NAME
Group=$GROUP_NAME
ExecStart=$INSTALL_DIR/bin/$BIN_NAME
Restart=always
RestartSec=5s
NoNewPrivileges=yes
CapabilityBoundingSet=
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=$INSTALL_DIR/logs $INSTALL_DIR/buffer
ProtectControlGroups=yes
ProtectKernelLogs=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
ProcSubset=pid
SystemCallFilter=@system-service
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
# Modest resource limits; adjust per environment
CPUQuota=10%
MemoryMax=300M
Environment=HYPERSTACK_INTERVAL=15s
Environment=HYPERSTACK_ENABLE_NODE=true
Environment=HYPERSTACK_ENABLE_GPU=true
# Environment=HYPERSTACK_URL=https://gateway.example.com
# Environment=METADATA_URL=http://169.254.169.254/openstack/latest/meta_data.json

[Install]
WantedBy=multi-user.target
SERVICE

systemctl daemon-reload
systemctl enable --now "$SERVICE_NAME"

echo "Installed and started $SERVICE_NAME"
