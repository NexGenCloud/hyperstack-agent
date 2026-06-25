#!/usr/bin/env bash
set -euo pipefail

TOOL_BIN="${TOOL_BIN:-"$PWD/.tools/bin"}"

TASK_VERSION="${TASK_VERSION:-v3.51.1}"
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-v2.12.2}"
GOSEC_VERSION="${GOSEC_VERSION:-v2.27.1}"
GOVULNCHECK_VERSION="${GOVULNCHECK_VERSION:-v1.4.0}"
GITLEAKS_VERSION="${GITLEAKS_VERSION:-8.30.1}"
TRIVY_VERSION="${TRIVY_VERSION:-0.71.2}"

mkdir -p "$TOOL_BIN"

cleanup_dirs=()
cleanup() {
	local dir
	for dir in "${cleanup_dirs[@]}"; do
		rm -rf "$dir"
	done
}
trap cleanup EXIT

make_tmpdir() {
	TMPDIR_RESULT="$(mktemp -d)"
	cleanup_dirs+=("$TMPDIR_RESULT")
}

need_cmd() {
	local name="$1"
	if ! command -v "$name" >/dev/null 2>&1; then
		printf 'missing required command: %s\n' "$name" >&2
		exit 1
	fi
}

fetch() {
	local url="$1"
	local output="$2"

	printf 'Downloading %s\n' "$url"
	curl -fsSL "$url" -o "$output"
}

go_install() {
	local bin_name="$1"
	local module="$2"
	local version="$3"
	local version_file="$TOOL_BIN/.${bin_name}.version"

	if [[ -x "$TOOL_BIN/$bin_name" ]] && [[ -f "$version_file" ]] && [[ "$(cat "$version_file")" == "$version" ]]; then
		return
	fi

	printf 'Installing %s@%s into %s\n' "$bin_name" "$version" "$TOOL_BIN"
	GOBIN="$TOOL_BIN" go install "${module}@${version}"
	printf '%s\n' "$version" >"$version_file"
}

install_gitleaks() {
	if "$TOOL_BIN/gitleaks" version 2>/dev/null | grep -Fq "$GITLEAKS_VERSION"; then
		return
	fi

	need_cmd curl
	need_cmd tar
	need_cmd sha256sum

	local tmpdir archive base_url
	make_tmpdir
	tmpdir="$TMPDIR_RESULT"

	archive="gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz"
	base_url="https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}"

	printf 'Installing gitleaks %s into %s\n' "$GITLEAKS_VERSION" "$TOOL_BIN"
	curl -fsSL "${base_url}/${archive}" -o "$tmpdir/${archive}"
	curl -fsSL "${base_url}/gitleaks_${GITLEAKS_VERSION}_checksums.txt" -o "$tmpdir/checksums.txt"
	(cd "$tmpdir" && grep " ${archive}$" checksums.txt | sha256sum -c -)
	tar -xzf "$tmpdir/${archive}" -C "$tmpdir"
	install -m 0755 "$tmpdir/gitleaks" "$TOOL_BIN/gitleaks"
}

install_trivy() {
	if "$TOOL_BIN/trivy" --version 2>/dev/null | grep -Fq "Version: ${TRIVY_VERSION}"; then
		return
	fi
	if command -v trivy >/dev/null 2>&1; then
		local existing_trivy
		existing_trivy="$(command -v trivy)"
		if [[ "$existing_trivy" != "$TOOL_BIN/trivy" ]] && "$existing_trivy" --version 2>/dev/null | grep -Fq "Version: ${TRIVY_VERSION}"; then
			printf 'Copying trivy %s from PATH into %s\n' "$TRIVY_VERSION" "$TOOL_BIN"
			install -m 0755 "$existing_trivy" "$TOOL_BIN/trivy"
			return
		fi
	fi

	need_cmd curl
	need_cmd tar
	need_cmd sha256sum

	local tmpdir archive base_url
	make_tmpdir
	tmpdir="$TMPDIR_RESULT"

	archive="trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz"
	base_url="https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}"

	printf 'Installing trivy %s into %s\n' "$TRIVY_VERSION" "$TOOL_BIN"
	fetch "${base_url}/${archive}" "$tmpdir/${archive}"
	fetch "${base_url}/trivy_${TRIVY_VERSION}_checksums.txt" "$tmpdir/checksums.txt"
	(cd "$tmpdir" && grep " ${archive}$" checksums.txt | sha256sum -c -)
	tar -xzf "$tmpdir/${archive}" -C "$tmpdir"
	install -m 0755 "$tmpdir/trivy" "$TOOL_BIN/trivy"
}

install_shellcheck() {
	if "$TOOL_BIN/shellcheck" --version >/dev/null 2>&1; then
		return
	fi
	if command -v shellcheck >/dev/null 2>&1; then
		printf 'Copying shellcheck from PATH into %s\n' "$TOOL_BIN"
		install -m 0755 "$(command -v shellcheck)" "$TOOL_BIN/shellcheck"
		return
	fi

	cat >&2 <<'EOF'
shellcheck is required but was not found.

Install it with your OS package manager, then rerun this command:
  sudo apt-get install -y shellcheck
  brew install shellcheck
EOF
	exit 1
}

need_cmd go

go_install task github.com/go-task/task/v3/cmd/task "$TASK_VERSION"
go_install golangci-lint github.com/golangci/golangci-lint/v2/cmd/golangci-lint "$GOLANGCI_LINT_VERSION"
go_install gosec github.com/securego/gosec/v2/cmd/gosec "$GOSEC_VERSION"
go_install govulncheck golang.org/x/vuln/cmd/govulncheck "$GOVULNCHECK_VERSION"

install_shellcheck
install_gitleaks
install_trivy

printf 'Tool bootstrap complete. Add this to PATH if needed: %s\n' "$TOOL_BIN"
