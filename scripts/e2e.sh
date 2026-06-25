#!/bin/sh

set -e

HYPERSTACK_URL=${HYPERSTACK_URL:-http://gateway:8000}
SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

# Wait for gateway to be ready
max_retries=60
retry_interval=1
attempt=0
while [ "$attempt" -lt "$max_retries" ]; do
    if curl -f -s "$HYPERSTACK_URL/ready" > /dev/null 2>&1; then
        echo "Gateway is ready"
        break
    fi
    attempt=$((attempt + 1))
    echo "Waiting for gateway to be ready... (attempt $attempt/$max_retries)"
    sleep "$retry_interval"
done

if [ "$attempt" -eq "$max_retries" ]; then
    echo "Gateway failed to become ready after ${max_retries}s"
    exit 1
fi

echo "Installing agent from verified download metadata at $HYPERSTACK_URL"

ALLOW_INSECURE_HTTP=1 RUNTIME=basic "$SCRIPT_DIR/install_agent.sh" "$HYPERSTACK_URL"
