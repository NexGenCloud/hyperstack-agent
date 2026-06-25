#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="hyperstack-agent"
INSTALL_ROOT="/opt/hyperstack-agent"
AGENT_ENV="${INSTALL_ROOT}/agent.env"
BUFFER_DIR="${INSTALL_ROOT}/buffer"
STATE_DIR="${INSTALL_ROOT}/state"
USER_NAME="hyperstack-agent"

echo "Stopping and disabling ${SERVICE_NAME}..."
sudo systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
sudo systemctl disable "${SERVICE_NAME}" 2>/dev/null || true

echo "Removing systemd unit..."
sudo rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
sudo systemctl daemon-reload

echo "Removing agent environment file (if present)..."
sudo rm -f "${AGENT_ENV}"

echo "Removing agent runtime directories..."
sudo rm -rf "${BUFFER_DIR}" "${STATE_DIR}"

echo "Removing install directory ${INSTALL_ROOT}..."
sudo rm -rf "${INSTALL_ROOT}"

# Optional: remove dedicated user if it exists
if id -u "${USER_NAME}" >/dev/null 2>&1; then
  echo "Removing user ${USER_NAME}..."
  sudo userdel -r "${USER_NAME}" 2>/dev/null || sudo userdel "${USER_NAME}" || true
fi

echo "Cleanup complete."
