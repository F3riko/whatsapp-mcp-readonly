#!/usr/bin/env bash
# Behavioural smoke test for a built image. Needs Docker and an arm64 host.
# Uses only throwaway volumes and a placeholder token fixture: no WhatsApp
# account, no real secret, no pairing, no QR code.
#
#   deploy/docker/smoke.sh IMAGE [EXPECTED_VERSION]
set -euo pipefail

IMG=${1:?usage: smoke.sh IMAGE [EXPECTED_VERSION]}
WANT_VERSION=${2:-}
sfx=$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')
data=wamcp-smoke-data-$sfx
tok=wamcp-smoke-tok-$sfx
tok_open=wamcp-smoke-tokopen-$sfx
name=wamcp-smoke-$sfx

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker volume rm "$data" "$tok" "$tok_open" >/dev/null 2>&1 || true
}
trap cleanup EXIT
fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }

hardening=(--read-only --cap-drop ALL --security-opt no-new-privileges:true
  --tmpfs /tmp:noexec,nosuid,size=256m --memory 2g --memory-swap 2g --cpus 2 --pids-limit 256)

[[ $(docker image inspect -f '{{.Os}}/{{.Architecture}}' "$IMG") == linux/arm64 ]] || fail "image is not linux/arm64"
[[ $(docker image inspect -f '{{.Config.User}}' "$IMG") == 10001:10001 ]] || fail "image does not run as 10001:10001"
ok "platform linux/arm64, user 10001:10001"

version=$(docker run --rm --network none "${hardening[@]}" "$IMG" version)
[[ -z $WANT_VERSION || $version == *"$WANT_VERSION"* ]] || fail "version output mismatch"
ok "version: $version"

if out=$(docker run --rm --network none "${hardening[@]}" --entrypoint /usr/local/bin/whatsapp-mcp "$IMG" --allow-send 2>&1); then
  fail "--allow-send was accepted"
fi
[[ $out == *"sending is permanently disabled"* ]] || fail "unexpected --allow-send rejection text"
ok "--allow-send rejected"

docker volume create "$data" >/dev/null
docker volume create "$tok" >/dev/null
docker volume create "$tok_open" >/dev/null
[[ $(docker run --rm -v "$data:/data" --entrypoint stat "$IMG" -c '%a %u:%g' /data) == "700 10001:10001" ]] \
  || fail "fresh volume did not inherit 0700 10001:10001"
ok "fresh named volume inherits 0700 10001:10001 (no root init)"

# Placeholder token fixtures, created by a throwaway root helper (not a secret).
docker run --rm --user 0 -v "$tok:/t" --entrypoint sh "$IMG" -c \
  'umask 077; printf "wamcp_placeholder_fixture\n" > /t/token; chown 10001:10001 /t/token; chmod 0400 /t/token'
docker run --rm --user 0 -v "$tok_open:/t" --entrypoint sh "$IMG" -c \
  'printf "wamcp_placeholder_fixture\n" > /t/token; chown 10001:10001 /t/token; chmod 0440 /t/token'

rc=0
out=$(docker run --rm --network none "${hardening[@]}" -v "$data:/data" -v "$tok_open:/run/whatsapp-mcp:ro" "$IMG" 2>&1) || rc=$?
[[ $rc -eq 78 ]] || fail "group-readable token should exit 78, got $rc"
[[ $out != *placeholder_fixture* ]] || fail "token value leaked into output"
ok "group-readable token refused with exit 78, value not echoed"

docker run -d --name "$name" --network host --init --restart unless-stopped "${hardening[@]}" \
  --health-interval 2s --health-retries 1 --health-start-period 1s \
  -v "$data:/data" -v "$tok:/run/whatsapp-mcp:ro" "$IMG" >/dev/null
for _ in $(seq 1 60); do
  docker logs "$name" 2>&1 | grep -q 'UNPAIRED' && break
  sleep 1
done
docker logs "$name" 2>&1 | grep -q 'UNPAIRED' || fail "unpaired state was not reported"
sleep 8
[[ $(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$name") == "true 0" ]] \
  || fail "unpaired container must stay up with zero restarts"
! docker logs "$name" 2>&1 | grep -q 'QR-CODE' || fail "a QR code was emitted"
if docker exec "$name" /usr/local/bin/whatsapp-mcp-healthcheck; then fail "healthcheck passed while unpaired"; fi
if command -v ss >/dev/null && ss -ltn | grep -q ':8787 '; then fail "something listens on 8787 while unpaired"; fi
[[ $(docker exec "$name" stat -c '%a' /data /data/session.db /data/messages.db | tr '\n' ' ') == "700 600 600 " ]] \
  || fail "unexpected data permissions"
ok "unpaired: idle, zero restarts, no QR, no listener, unhealthy, data 0700/0600"
docker stop -t 10 "$name" >/dev/null
[[ $(docker inspect -f '{{.State.ExitCode}}' "$name") == 0 ]] || fail "clean stop did not exit 0"
ok "SIGTERM stops the idle container with exit 0"

for model in small base; do
  docker run --rm --network none "${hardening[@]}" --entrypoint sh "$IMG" -c '
    set -e
    python3 - <<PY
import math, struct, wave
with wave.open("/tmp/tone.wav", "wb") as w:
    w.setnchannels(1); w.setsampwidth(2); w.setframerate(16000)
    w.writeframes(b"".join(struct.pack("<h", int(3000 * math.sin(i / 20))) for i in range(32000)))
PY
    mkdir -p /tmp/out
    env -i PATH=/usr/local/bin:/usr/bin:/bin LANG=C.UTF-8 TMPDIR=/tmp/out \
      /usr/local/bin/whisper-adapter /tmp/tone.wav --output_format txt --output_dir /tmp/out \
      --model /opt/models/faster-whisper-'"$model"'
    test -f /tmp/out/audio.txt' || fail "offline adapter failed for model $model"
  ok "offline adapter loads and decodes with model $model (network none)"
done
echo "SMOKE PASS"
