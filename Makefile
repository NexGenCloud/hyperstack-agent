.PHONY: tools build run clean test lint security govulncheck trivy-fs sbom shellcheck secrets verify build-linux-amd64 build-linux-arm64 static release-agent release-agent-full

BINARY_NAME=hyperstack-agent
AMD64_BIN = bin/$(BINARY_NAME)-linux-amd64
AMD64_SHA = bin/$(BINARY_NAME)-linux-amd64.sha256
TOOL_BIN = $(CURDIR)/.tools/bin

export PATH := $(TOOL_BIN):$(PATH)

tools:
	TOOL_BIN="$(TOOL_BIN)" ./scripts/bootstrap_tools.sh

build:
	GOOS=linux GOARCH=amd64 go build -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/agent

build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/agent

build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -o bin/$(BINARY_NAME)-linux-arm64 ./cmd/agent

run:
	go run ./cmd/agent

test:
	go test ./...

lint: tools
	golangci-lint run ./...

security: tools
	gosec ./...

govulncheck: tools
	CGO_ENABLED=1 govulncheck ./...

trivy-fs: tools
	trivy fs --scanners vuln,secret,misconfig --severity HIGH,CRITICAL --exit-code 1 .

sbom: tools
	trivy fs --format cyclonedx --output sbom.cdx.json .

shellcheck: tools
	shellcheck scripts/*.sh

secrets: tools
	gitleaks detect --source . --redact --no-banner

verify: tools test shellcheck lint security govulncheck trivy-fs secrets

clean:
	rm -rf bin

static:
	CGO_ENABLED=0 go build -trimpath -a -ldflags '-s -w -extldflags "-static"' -o bin/$(BINARY_NAME)-static ./cmd/agent

release-agent: build-linux-amd64
	@sha256sum "$(AMD64_BIN)" > "$(AMD64_SHA)"
	@echo "Wrote $(AMD64_BIN) and $(AMD64_SHA)"

release-agent-full:
	./scripts/build.sh
