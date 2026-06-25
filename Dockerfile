ARG VERSION=dev
ARG DATE=unknown
ARG GO_IMAGE=golang:1.25@sha256:995e25c0e1868fa30a57236d5d8c2252b94b8716e53eae5895cd70dcce532cf0
ARG RUNTIME_IMAGE=debian:13-slim@sha256:28de0877c2189802884ccd20f15ee41c203573bd87bb6b883f5f46362d24c5c2

# Build stage
FROM ${GO_IMAGE} AS builder
ARG VERSION
ARG DATE
RUN apt-get update && apt-get install -y --no-install-recommends build-essential && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.date=${DATE}" -o hyperstack-agent ./cmd/agent

FROM ${RUNTIME_IMAGE} AS runtime-base
ARG VERSION
ARG DATE
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libc6 \
    passwd \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --no-create-home --shell /usr/sbin/nologin hyperstack-agent

WORKDIR /app
COPY --from=builder /app/hyperstack-agent /usr/local/bin/hyperstack-agent

ENV AGENT_VERSION=${VERSION} \
    AGENT_BUILD_DATE=${DATE} \
    HYPERSTACK_HEALTH_ADDR=127.0.0.1:9100

FROM runtime-base AS agent-host
USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
    busybox \
    curl \
    && rm -rf /var/lib/apt/lists/*
COPY scripts /scripts
RUN chmod 0755 /scripts/*.sh
USER hyperstack-agent
CMD ["/scripts/serve.sh"]

FROM runtime-base AS agent
USER hyperstack-agent

CMD ["/usr/local/bin/hyperstack-agent"]
