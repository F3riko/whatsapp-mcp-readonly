#!/usr/bin/env bash
# Install whatsapp-mcp as a hardened systemd service on a Linux server.
#
# Hardened minimal read-only fork:
#   own user      whatsappmcp, no login shell, no sudo
#   own directory /var/lib/whatsapp-mcp, mode 700, owned by that user
#   own token     /etc/whatsapp-mcp/token, mode 600, kept out of argv
#   own port      8787 by default, bound to 127.0.0.1 loopback only
#   own service   whatsapp-mcp.service
#
# Usage: sudo bash install.sh [--port 8787]

set -euo pipefail

PORT=8787
while [[ $# -gt 0 ]]; do
  case $1 in
    --port) PORT="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

USER_NAME=whatsappmcp
DATA_DIR=/var/lib/whatsapp-mcp
BIN=/usr/local/bin/whatsapp-mcp

[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }

if ss -ltn 2>/dev/null | grep -q ":${PORT} "; then
  echo "port ${PORT} is already in use. Pick another with --port." >&2
  exit 1
fi

# The binary must be pre-built from pinned reviewed source.
# No @latest, npm postinstall, or unverified binary downloads.
if [[ ! -x "$BIN" ]]; then
  echo "no executable at ${BIN}. Compile and install from reviewed source first:" >&2
  echo "  CGO_ENABLED=0 go build -o /usr/local/bin/whatsapp-mcp ./cmd/whatsapp-mcp" >&2
  exit 1
fi
chmod 755 "$BIN"

echo "==> creating the service user and data directory"
id -u "$USER_NAME" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$USER_NAME"
mkdir -p "$DATA_DIR"
chown "$USER_NAME:$USER_NAME" "$DATA_DIR"
chmod 700 "$DATA_DIR"

# Token file is kept in an owner-only private directory, never passed via argv
TOKEN_DIR=/etc/whatsapp-mcp
mkdir -p "$TOKEN_DIR"
chmod 700 "$TOKEN_DIR"
chown "$USER_NAME:$USER_NAME" "$TOKEN_DIR"

TOKEN_FILE="${TOKEN_DIR}/token"
if [[ ! -f "$TOKEN_FILE" ]]; then
  TOKEN="wamcp_$(head -c32 /dev/urandom | xxd -p -c64)"
  printf '%s\n' "$TOKEN" > "$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
  chown "$USER_NAME:$USER_NAME" "$TOKEN_FILE"
fi

echo "==> writing the hardened systemd unit"
cat > /etc/systemd/system/whatsapp-mcp.service <<UNIT
[Unit]
Description=WhatsApp MCP server (Hardened Read-Only Fork)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${USER_NAME}
Group=${USER_NAME}
UMask=0077
ExecStart=${BIN} --http 127.0.0.1:${PORT} --token-file ${TOKEN_FILE} --data-dir ${DATA_DIR}
Restart=always
RestartSec=5

# Hardening sandbox
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
LockPersonality=true
RestrictRealtime=true
RestrictSUIDSGID=true
RestrictAddressFamilies=AF_INET AF_INET6
MemoryMax=512M
TasksMax=64
ReadWritePaths=${DATA_DIR}
ReadOnlyPaths=${TOKEN_DIR}

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable whatsapp-mcp >/dev/null 2>&1 || true

cat <<DONE

Installed. It is NOT running yet, because WhatsApp has to be linked first and
that needs an interactive login command:

  1. Link it interactively:

       sudo -u ${USER_NAME} ${BIN} login --data-dir ${DATA_DIR}

  2. Start the service:

       sudo systemctl start whatsapp-mcp
       systemctl status whatsapp-mcp

  3. Your bearer token (owner-only):

       sudo cat ${TOKEN_FILE}

DONE
