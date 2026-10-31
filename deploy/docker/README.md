# Container image (linux/arm64)

A container build of the hardened read-only server with a local-only
faster-whisper worker. The application is unchanged: this directory only
packages it. Deployment definitions live in the operator's own repository and
pin this image by digest.

## What is in the image

| Path | What |
|---|---|
| `/usr/local/bin/whatsapp-mcp` | The server, built from this exact commit (`version` prints the commit). |
| `/usr/local/bin/whatsapp-mcp-entrypoint` | Preflight checks, then the server (see below). |
| `/usr/local/bin/whisper-adapter` | Wrapper that runs `tools/whisper_adapter.py` in a locked virtualenv with 2 threads. |
| `/opt/models/faster-whisper-small` | Default model. |
| `/opt/models/faster-whisper-base` | Faster, less accurate fallback. |
| `/data` | The only writable persistent path. Created `0700`, owned by uid/gid `10001`. |

The process runs as `10001:10001`. Base images are pinned by digest, Python
wheels by hash (`requirements-stt.txt`), and model files by Hugging Face commit
plus SHA-256 and size (`model-*.lock.json`). A build fails if any of them differ.

## Models and their lifecycle

Models are baked in at build time and verified. The running container never
downloads anything: the adapter loads a directory with `local_files_only=True`,
`HF_HUB_OFFLINE=1` is set, and there is no hosted fallback. Changing or updating
a model means editing a lock file and publishing a new image; it is never a
runtime action.

Measured on a 4-vCPU Neoverse-N1 machine (the same CPU family as the target
host, but shared with other workloads, so treat it as a proxy), int8, limited
to 2 CPUs, 11 s of public-domain speech:

| Model | Load | Realtime factor | Peak memory |
|---|---|---|---|
| `small` (default) | about 1 s | 0.75 to 1.4 | about 0.8 GiB |
| `base` | about 0.4 s | 0.25 to 0.55 | about 0.3 GiB |

At roughly realtime, a voice note longer than about two minutes may exceed the
server's fixed three-minute per-transcription timeout with `small`. Whether
`small` quality justifies that, or `base` is the better trade for the languages
actually used, is decided by acceptance on the real host with real voice notes,
not by these numbers.

## Runtime contract

```text
--http 127.0.0.1:8787  --token-file /run/whatsapp-mcp/token  --data-dir /data
--transcribe local  --transcribe-bin /usr/local/bin/whisper-adapter
--transcribe-model /opt/models/faster-whisper-small
```

- **Loopback only.** The application refuses any non-loopback bind, and this
  image does not change that. Run it with host networking so `127.0.0.1` is the
  host's loopback, and reach it through an SSH tunnel. Never publish the port.
- **Token** is a file, never argv. The entrypoint refuses to start unless it is
  a regular file (not a symlink), owned by the service user, with no group or
  other access, and 1 to 1024 bytes. The value is never echoed.
- **Data directory** must also be owned by the service user and private. A fresh
  Docker *named volume* mounted at `/data` inherits `0700` and `10001:10001`
  from the image, so no root-side `chown` is needed. A bind mount does not.
- **Read-only root filesystem** works: give it a `tmpfs` at `/tmp`.
- **Plaintext at rest.** Message history, the search index, downloaded media,
  transcripts and the audit log in `/data` are not encrypted. Protect the
  volume, and encrypt any backup before it leaves the host.

## Unpaired hosts do not restart-loop and never show a QR code

`serve` refuses an unpaired session and exits 1. The entrypoint recognises that
exact refusal, prints one `UNPAIRED` line and idles, so a restart policy does not
loop. Nothing listens, the healthcheck reports unhealthy, and no pairing flow is
started. Any other failure exits with the server's own status and is restarted
normally. `docker stop` ends the idle container with exit 0.

Pairing is a separate, deliberate, interactive step, run with the server stopped
and a real terminal (the QR code is drawn on stderr, so do not capture it). The
QR code and the raw `QR-CODE:` line are a live linking credential, so the
container must not write them to a log: `--log-driver none` is required, and
`docker logs` will not work for this container by design.

```sh
docker run --rm -it --log-driver none --network host --user 10001:10001 \
  -v <data volume>:/data \
  --entrypoint /usr/local/bin/whatsapp-mcp <image> login --data-dir /data \
  --device-name "WhatsApp MCP read-only"
```

Then start the server again. `session_status` over MCP reports `paired` and
`connected`; `/healthz` only proves the HTTP server is up.

## Checks

```sh
python3 -m unittest discover -s deploy/docker -p 'test_*.py'   # entrypoint behaviour
deploy/docker/smoke.sh <image> [<commit>]                       # needs Docker on arm64
```

`smoke.sh` uses throwaway volumes and a placeholder token. It checks platform and
user, `--allow-send` rejection, volume inheritance, token preflight, the
unpaired idle behaviour, clean stop, and offline decoding through the adapter for
both models. It does not pair, and it does not prove transcription accuracy.
