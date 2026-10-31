# Hardened read-only whatsapp-mcp for linux/arm64, with a local-only
# faster-whisper worker (models: small by default, base as a faster fallback,
# both baked in and hash-verified). Built from this exact source tree.
#
#   docker build --build-arg VERSION=<commit> -t whatsapp-mcp-readonly .
#
# Every base image is pinned by digest and every Python wheel and model file is
# pinned by hash, so a rebuild of the same commit pulls the same bytes.

ARG GO_IMAGE=golang:1.26-bookworm@sha256:23ce71337b726608afb1bc00986e630a05034a87a87ea1fc2cd1ae5cf33387eb
ARG PY_IMAGE=python:3.12-slim-bookworm@sha256:e697c81897302a4c4333bade000d98038fbb21eb064f8ee1fd42d02ebcd9d337

FROM ${GO_IMAGE} AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/whatsapp-mcp ./cmd/whatsapp-mcp

FROM ${PY_IMAGE} AS model
COPY deploy/docker/fetch_model.py deploy/docker/model-small.lock.json deploy/docker/model-base.lock.json /build/
RUN python3 /build/fetch_model.py /build/model-small.lock.json /models/faster-whisper-small \
    && python3 /build/fetch_model.py /build/model-base.lock.json /models/faster-whisper-base

FROM ${PY_IMAGE} AS venv
COPY deploy/docker/requirements-stt.txt /build/
RUN python3 -m venv /opt/venv \
    && /opt/venv/bin/pip install --no-cache-dir --require-hashes --only-binary=:all: \
       -r /build/requirements-stt.txt

FROM ${PY_IMAGE}
ARG VERSION=dev
LABEL org.opencontainers.image.title="whatsapp-mcp-readonly" \
      org.opencontainers.image.description="Hardened read-only WhatsApp MCP server with local faster-whisper" \
      org.opencontainers.image.source="https://github.com/F3riko/whatsapp-mcp-readonly" \
      org.opencontainers.image.revision="${VERSION}" \
      org.opencontainers.image.licenses="MIT"

# Fixed numeric identity so host-side file ownership is deterministic.
# /data is created private (0700) and owned by the service user, so a fresh
# named volume mounted there inherits that ownership and mode.
RUN groupadd --gid 10001 whatsapp \
    && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent \
       --shell /usr/sbin/nologin whatsapp \
    && install -d -m 0700 -o 10001 -g 10001 /data

COPY --from=venv /opt/venv /opt/venv
COPY --from=model /models /opt/models
COPY --from=gobuild /out/whatsapp-mcp /usr/local/bin/whatsapp-mcp
COPY tools/whisper_adapter.py /opt/whatsapp-mcp/whisper_adapter.py
COPY deploy/docker/whisper-adapter /usr/local/bin/whisper-adapter
COPY deploy/docker/entrypoint.py /usr/local/bin/whatsapp-mcp-entrypoint
COPY deploy/docker/healthcheck.py /usr/local/bin/whatsapp-mcp-healthcheck
RUN chmod 0755 /usr/local/bin/whatsapp-mcp /usr/local/bin/whisper-adapter \
       /usr/local/bin/whatsapp-mcp-entrypoint /usr/local/bin/whatsapp-mcp-healthcheck \
    && chmod 0644 /opt/whatsapp-mcp/whisper_adapter.py

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1

USER 10001:10001
WORKDIR /data
VOLUME ["/data"]

# Loopback only: the application itself refuses any non-loopback bind, and this
# image is meant to run with host networking so that 127.0.0.1 is the host's.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/whatsapp-mcp-healthcheck"]

ENTRYPOINT ["/usr/local/bin/whatsapp-mcp-entrypoint"]
CMD ["--http", "127.0.0.1:8787", "--token-file", "/run/whatsapp-mcp/token", "--data-dir", "/data", \
     "--transcribe", "local", "--transcribe-bin", "/usr/local/bin/whisper-adapter", \
     "--transcribe-model", "/opt/models/faster-whisper-small"]
