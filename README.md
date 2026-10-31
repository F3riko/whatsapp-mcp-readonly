# WhatsApp MCP (Hardened Read-Only Fork)

A hardened, private Model Context Protocol (MCP) server for your personal WhatsApp account.

This repository is a security-hardened, strictly **read-only** fork of [`thenavidm/whatsapp-mcp`](https://github.com/thenavidm/whatsapp-mcp) by [Navid Moazzez](https://navid.me) (licensed under MIT).

It links to WhatsApp as a companion device (identical protocol to WhatsApp Web), persists a local **PLAINTEXT** SQLite/FTS copy of your conversation history, and exposes read tools to AI agents (such as Claude Code) over standard MCP transports (stdio or loopback HTTP).

> [!IMPORTANT]
> The SQLite database, FTS search indexes, and downloaded attachments are stored in **plaintext** on the local filesystem. This server does not provide application-level database encryption. Host-level security must be managed by the owner using encrypted storage volumes (e.g., LUKS or FileVault), restrictive `0700` directory permissions, and secure backup controls.

---

## Hardening & Security Architecture

1. **Structural Read-Only Guarantee**
   - Only 6 read tools are registered: `search_messages`, `list_messages`, `list_chats`, `search_contacts`, `session_status`, and `download_media`.
   - All send and mutation tools (`send_message`, `send_file`, `send_voice_note`, `send_audio`, `speak_message`) and automated agent reply loops are permanently removed from the binary.
   - Low-level application sending and media upload handlers unconditionally reject writes (`ErrReadOnly`). Note that low-level companion protocol synchronization (handshakes, keepalives, and sync acknowledgments managed by `whatsmeow`) continues to operate as required by the WhatsApp protocol.

2. **Durable Tombstone & Revocation Integrity**
   - Incoming message revokes (both live protocol events and history sync backlog) transactionally delete messages, remove FTS search entries, and safely unlink media.
   - A persistent `revocations` tombstone table prevents late transcript insertion or history re-delivery from resurrecting deleted messages.
   - Insertion (`UpsertMessage`), transcript updates (`UpdateTranscript`), and media path updates (`SetMediaPath`) use atomic conditional SQL statements with `NOT EXISTS (SELECT 1 FROM revocations ...)` checks to eliminate concurrency races.
   - Short per-client mutex serialization protects final media publication and unlinking, ensuring that revoked media is never published or left on disk.

3. **Privacy: Ephemeral & View-Once Suppression**
   - View-once photos/videos and disappearing ephemeral messages are strictly excluded from history ingestion.
   - Detection recurses through protobuf wrappers (`EditedMessage`, `DeviceSentMessage`) and inspects ephemeral metadata (`ContextInfo.Expiration`).
   - *Limitation caveat:* Metadata-only privacy detection does not provide universal disappearing-message compliance; it inspects declared client-visible protobuf fields and metadata only. Ephemeral policy toggles managed out-of-band or omitted by the protocol cannot be deduced.

4. **Storage & Media Containment**
   - `Store.Open` and `session.db` creation validate complete directory ancestry component-by-component before creation or chmod, rejecting symlinks for data directories, DB files, and auxiliary WAL/SHM files, enforcing owner UID and private `0700`/`0600` modes without swallowing chmod errors.
   - Media cache keys use SHA-256 hashes of composite `chatJID + \x00 + messageID` identity, eliminating path traversal and collisions.
   - Cache lookups reject directory and symlink hits; media removal (`safeRemoveMedia`) strictly validates ancestry and refuses to traverse symlink parents outside the cache directory.
   - Media downloads use atomic exclusive temporary files (`os.CreateTemp`) and controlled atomic `os.Link` publication with strict cache-hit revalidation upon `EEXIST` (no overwrite or rename fallback).
   - Wire downloads are bounded by `boundedMediaTransport` wrapping `SetMediaHTTPClient` with finite byte limits (50MB) to prevent wire and memory exhaustion.
   - A shared admission budget bounds concurrent downloads to 2 across both MCP media and STT workers; overload is promptly rejected before network downloads.
   - A conservative finite media cache disk quota (500MB default) is enforced; required bytes are checked before download, and an explicit full-cache error is returned without automatic arbitrary deletions.

5. **Single-Worker Bounded STT Admission**
   - STT admission strictly precedes download and memory allocation: jobs are submitted synchronously to a non-blocking bounded queue.
   - Exactly one background worker performs download, inference, and result persistence sequentially, preventing RAM and connection storms.
   - Active tasks and pending queues cancel cleanly on service shutdown (`Close`).

6. **Offline Local STT Execution Contract**
   - Hosted third-party STT APIs (OpenAI, Groq, ElevenLabs) are rejected at configuration time.
   - Local STT requires an explicit, pinned absolute executable path (`--transcribe-bin`). Ambient `PATH` and `WHISPER_BIN` fallbacks are prohibited.
   - The provided `whisper_adapter.py` enforces `local_files_only=True`, rejects symlink inputs/outputs, bounds transcript text to 1MB, and operates under a sanitized child environment.
   - Unit tests for the offline adapter in this repository are fake-backend contract tests validating interface behavior, argument passing, error handling, and bounds without requiring downloaded model weights or GPUs.

7. **Network Transport Isolation**
   - Binds strictly to loopback addresses (`127.0.0.1` or `::1`). Non-loopback bindings are rejected.
   - Enforces exact `Host` header matching and strict canonical `Origin` verification (rejecting cross-port, cross-scheme, or foreign origins).
   - Bearer authentication is required for HTTP and must be supplied via `--token-file` or `WHATSAPP_MCP_TOKEN` (passing tokens on CLI argv is rejected).
   - Concurrency is bounded with prompt HTTP 429 overload responses, finite session timeouts, and per-tool execution deadlines.
   - Normal server startup refuses unpaired sessions; pairing (QR code, or a phone number with `--phone-stdin`) requires running `whatsapp-mcp login` and can never start from normal serve.

---

## Building from Source

No prebuilt binaries or npm packages are distributed from this fork. Build directly from pinned reviewed Go source:

```bash
# Requires Go 1.26+ (tested with Go 1.27)
go build -o bin/whatsapp-mcp ./cmd/whatsapp-mcp
```

---

## Usage

### 1. Link your device interactively
Run the interactive pairing command once in a terminal to scan the QR code:
```bash
./bin/whatsapp-mcp login --data-dir ~/.whatsapp-mcp
```

**Or link by phone number**, when you cannot scan a QR code. Only `login` accepts
this, and only on stdin: the number is never an argument, an environment variable
or a file, so it stays out of your shell history and the process table.
```bash
read -rs -p 'Number to link, as +<country code><number>: ' PHONE; echo
printf '%s\n' "$PHONE" | ./bin/whatsapp-mcp login --phone-stdin --data-dir ~/.whatsapp-mcp
unset PHONE
```
The input must be one international E.164 number (a `+`, a non-zero country code,
7 to 15 digits in all) with at most one trailing newline. Spaces, punctuation,
local formats and anything longer than 32 bytes are rejected, without being
echoed, before any file or network is touched. The country is never guessed.
`--phone-stdin` is a bare flag: a value such as `--phone-stdin=...` is refused
with a fixed message that does not repeat it.

The command prints one line, `PAIR-CODE: XXXX-XXXX`, on stderr. On the phone open
WhatsApp, then Settings, Linked Devices, Link a Device, and choose **Link with
phone number instead**; enter the code there (no SMS is involved, see
<https://faq.whatsapp.com/1324084875126592>). Exactly one code is requested per
run. It lives as long as the connection, about 160 seconds; if it expires, the run
fails and you start again for a new code. The code is a live linking credential:
do not paste it anywhere, and do not run this where output is recorded. The
command refuses a data directory that is already linked, never unlinks one, and
cannot be combined with `--debug` (protocol logs could record the number).

Pairing remains an operational step performed by the device owner.

### 2. Run over stdio (Recommended for Claude Code / local agents)
Configure your client to launch the binary:
```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/path/to/bin/whatsapp-mcp",
      "args": ["--data-dir", "/home/user/.whatsapp-mcp"]
    }
  }
}
```

### 3. Optional: Local Voice Note Transcription
To transcribe incoming voice notes completely offline using faster-whisper:

1. Create a Python virtualenv with `faster-whisper` installed:
   ```bash
   python3 -m venv /opt/whisper-venv
   /opt/whisper-venv/bin/pip install faster-whisper
   ```

2. Pre-provision the model weights into a local directory (e.g., `/opt/models/faster-whisper-tiny`). Note that `--transcribe-model` must point to an explicit local directory path rather than a model name, as the sanitized child environment blocks Hugging Face network downloads.

3. Create an executable wrapper script (e.g., `/opt/bin/run-whisper`) to invoke the virtualenv's Python interpreter directly:
   ```bash
   #!/bin/bash
   exec /opt/whisper-venv/bin/python3 /path/to/tools/whisper_adapter.py "$@"
   ```
   Make it executable: `chmod 755 /opt/bin/run-whisper`.

4. Start the server with the wrapper and local model directory:
   ```bash
   ./bin/whatsapp-mcp \
     --data-dir ~/.whatsapp-mcp \
     --transcribe local \
     --transcribe-bin /opt/bin/run-whisper \
     --transcribe-model /opt/models/faster-whisper-tiny
   ```

*(Note: Live backend performance benchmarking with real models remains pending operational verification).*

### 4. Optional: Hardened HTTP Service
For remote or headless setups behind a trusted reverse proxy:
```bash
# Generate a secret token file with owner-only permissions (set umask 077 BEFORE creating file)
(umask 077 && echo -n "wamcp_$(openssl rand -hex 32)" > ~/.whatsapp-mcp/token)

./bin/whatsapp-mcp \
  --data-dir ~/.whatsapp-mcp \
  --http 127.0.0.1:8787 \
  --token-file ~/.whatsapp-mcp/token \
  --public-host mcp.example.com
```

A production systemd template with system sandboxing (`PrivateTmp`, `ProtectSystem=strict`, `MemoryMax=512M`) is provided in `deploy/whatsapp-mcp.service.template`.

---

## Attribution & License

- Original project by [Navid Moazzez](https://navid.me).
- WhatsApp protocol implementation powered by [whatsmeow](https://github.com/tulir/whatsmeow).
- Licensed under the [MIT License](./LICENSE).
