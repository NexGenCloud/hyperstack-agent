#!/bin/sh

set -e

tmpdir="$(mktemp -d)"
cd "$tmpdir"

echo "ok" > healthz
cp /opt/hyperstack-agent/bin/hyperstack-agent download

sha256=$(sha256sum download | cut -d' ' -f1)
digest="sha256:${sha256}"

cat > version << EOF
{
  "version": "${AGENT_VERSION}",
  "digest": "${digest}"
}
EOF

echo "Starting HTTP server on port 8000 from ${tmpdir}, version=${AGENT_VERSION}, digest=${digest}"

exec busybox httpd -f -v -p 8000
