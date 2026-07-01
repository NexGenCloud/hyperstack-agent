# Hyperstack Agent

Hyperstack Agent is a Go-based VM monitoring agent with embedded node and GPU
probes. It collects VM metrics and submits them to a compatible Hyperstack
gateway.

## Install On A VM

Download the public install helper, review it if required by your change-control
process, then run it with the gateway URL. The helper downloads the agent binary
from the gateway `/download` endpoint, verifies the `Hyperstack-Agent-Digest`
SHA-256 header, installs the binary, and creates the systemd service.

```bash
curl -fsSLO https://raw.githubusercontent.com/NexGenCloud/hyperstack-agent/main/scripts/install_agent.sh
chmod +x install_agent.sh
sudo ./install_agent.sh https://gateway.example.com
```

If the gateway requires a VM key to authorize release metadata or downloads,
provide `INFRAHUB_KEY` in the environment. The key is only sent as an HTTP
header and is not written to disk or systemd.

## Build

Install [Task](https://taskfile.dev/) before running project commands, or run
`make tools` to install pinned project tools into `.tools/bin`. Equivalent
`make` targets are also kept for environments that already standardize on Make.

```bash
make tools
.tools/bin/task build-linux-amd64
.tools/bin/task build-linux-arm64
```

Build artifacts are written to `bin/`.

## Run Locally

```bash
HYPERSTACK_URL="http://localhost:8000" .tools/bin/task run
```

## Scripts

The `scripts/` directory is part of the public release because it shows how the
agent is built, installed, tested, and removed.

- `scripts/install_agent.sh`: VM install helper. Requires a gateway URL argument
  or `GATEWAY_URL`; optionally sends `INFRAHUB_KEY` as a download authorization
  header without persisting it.
- `scripts/install.sh`: advanced systemd installer for a local agent binary.
  Requires `BINARY_SOURCE` and does not embed credentials.
- `scripts/uninstall_agent.sh`: removes the systemd service and agent runtime
  files.
- `scripts/build.sh`: builds linux release artifacts and SHA-256 checksums.
- `scripts/serve.sh`: local Docker helper that serves `/download` and
  `/version` for install/update tests.
- `scripts/e2e.sh`: local Docker end-to-end test helper.

Do not commit `.env` files, real gateway URLs, private IPs, credentials, tokens,
or customer-specific metadata into this repository. Use placeholders such as
`gateway.example.com` in docs and test fixtures.

## Configuration

The agent is configured through environment variables:

- `HYPERSTACK_URL`: Gateway base URL. Defaults to `http://localhost:8000`.
- `HYPERSTACK_INTERVAL`: Collection interval. Defaults to `15s`.
- `HYPERSTACK_ENABLE_NODE`: Enable node metrics. Defaults to `true`.
- `HYPERSTACK_ENABLE_GPU`: Enable GPU metrics. Defaults to `true`.
- `HYPERSTACK_HEALTH_ADDR`: Health and self-metrics bind address. Defaults to
  `127.0.0.1:9100`.
- `METADATA_URL`: Optional metadata service URL override.

## Task Targets

```bash
task tools               # install pinned verification tools into .tools/bin
task build-linux-amd64   # build linux/amd64 binary
task build-linux-arm64   # build linux/arm64 binary
task run                 # run agent locally
task test                # run Go tests
task lint                # run Go lint checks
task security            # run gosec checks
task govulncheck         # run Go vulnerability checks
task trivy-fs            # run filesystem vulnerability/misconfiguration scan
task sbom                # generate a local CycloneDX SBOM
task secrets             # run gitleaks secret scan
task verify              # run tests, shellcheck, lint, gosec, govulncheck, Trivy, and gitleaks
task release-agent       # build amd64 binary and checksum
task release-agent-full  # build amd64, arm64, static amd64, and checksums
```

The same target names are available through `make`, for example `make tools` and
`make verify`. The verification targets install and use pinned local tools from
`.tools/bin` instead of relying on globally installed binaries. If you already
have `task` on your PATH, `task verify` works as well.

## Release Artifacts

`task release-agent` creates:

- `bin/hyperstack-agent-linux-amd64`
- `bin/hyperstack-agent-linux-amd64.sha256`

`task release-agent-full` creates linux amd64, linux arm64, static linux amd64,
and matching `.sha256` files.

## Publishing a Release

Releases are created automatically by CI when a semver tag is pushed. The
`release` job runs after `lint`, `test`, and `scan` all pass, then uses
GoReleaser to build the binary and create the GitHub Release.

```bash
git tag v1.2.3
git push origin v1.2.3
```

Tags must match `v[0-9]+.[0-9]+.[0-9]+` (e.g. `v1.2.3`). Tags with a
pre-release segment (e.g. `v1.2.3-alpha`) are published as pre-releases.

Each release contains:

- `hyperstack-agent_linux_amd64` — raw binary, no archive wrapper
- `checksums.txt` — SHA-256 checksum

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
