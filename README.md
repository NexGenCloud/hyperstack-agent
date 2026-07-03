# Hyperstack Agent

Hyperstack Agent is a Go-based VM monitoring agent with embedded node and GPU
probes. It collects VM metrics and submits them to a compatible Hyperstack
gateway.

## Install On A VM

Installation is handled by the Hyperstack platform.

## Build

```bash
task build-linux-amd64
task build-linux-arm64
```

Build artifacts are written to `bin/`.

## Run Locally

```bash
HYPERSTACK_URL="http://localhost:8000" task run
```

## Development

Install [pre-commit](https://pre-commit.com/) then run:

```bash
pre-commit install
```

Hooks run automatically on `git commit`. To run them manually:

```bash
pre-commit run --all-files
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
task build-linux-amd64   # build linux/amd64 binary
task build-linux-arm64   # build linux/arm64 binary
task run                 # run agent locally
task test                # run Go tests
task static              # build a static linux/amd64 binary
task clean               # remove build artifacts
```

## Publishing a Release

Releases are created automatically by CI when a semver tag is pushed. The
`release` job runs after `test`, `lint`, and `scan` all pass, then uses
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
