#!/usr/local/bin/python3
"""Container healthcheck: the loopback /healthz endpoint answers "ok".

This proves the HTTP server is up. It does not prove WhatsApp is connected;
use the session_status MCP tool for that. Exit 0 when healthy, 1 otherwise.
"""
import os
import sys
import urllib.request

addr = os.environ.get("WHATSAPP_MCP_HEALTH_ADDR", "127.0.0.1:8787")
if not addr.startswith(("127.", "localhost", "[::1]")):
    sys.stderr.write("healthcheck only probes loopback\n")
    sys.exit(1)
try:
    with urllib.request.urlopen(f"http://{addr}/healthz", timeout=3) as resp:
        sys.exit(0 if resp.status == 200 and resp.read(16) == b"ok" else 1)
except Exception:
    sys.exit(1)
