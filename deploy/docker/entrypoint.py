#!/usr/local/bin/python3
"""Container entrypoint for whatsapp-mcp.

Three jobs, nothing else:

1. Refuse to start when the mounted token, the data directory or the local
   model are not exactly what the deployment promises (private, owned by the
   service user, present). It never creates, repairs or downloads anything.
2. Run the server, relaying its stderr unchanged.
3. When the server exits because the WhatsApp session is not paired, say so
   once and idle instead of exiting. An unpaired host then shows up as an
   unhealthy container rather than a restart loop, and nothing here ever runs
   the QR pairing flow. Pairing is the separate, interactive `login` command.

Any other exit status is passed through, so real crashes still restart.
"""
import os
import signal
import stat
import subprocess
import sys
import threading

APP = "/usr/local/bin/whatsapp-mcp"
UNPAIRED_MARKER = b"whatsapp session is not paired"
EX_CONFIG = 78
MODEL_FILES = ("config.json", "model.bin", "tokenizer.json", "vocabulary.txt")


def flag_value(args, name):
    """Value of a Go-style flag (-name v, --name v, -name=v, --name=v)."""
    for i, arg in enumerate(args):
        for prefix in ("--", "-"):
            if arg == prefix + name:
                return args[i + 1] if i + 1 < len(args) else ""
            if arg.startswith(prefix + name + "="):
                return arg.split("=", 1)[1]
    return None


def private_to_owner(path, want_dir):
    """Return a problem string, or None when path is private to this user."""
    try:
        st = os.lstat(path)
    except OSError as exc:
        return f"{path}: {exc.strerror or exc}"
    if stat.S_ISLNK(st.st_mode):
        return f"{path}: must not be a symlink"
    if want_dir and not stat.S_ISDIR(st.st_mode):
        return f"{path}: must be a directory"
    if not want_dir and not stat.S_ISREG(st.st_mode):
        return f"{path}: must be a regular file"
    if st.st_uid != os.geteuid():
        return f"{path}: must be owned by uid {os.geteuid()}, found uid {st.st_uid}"
    if st.st_mode & 0o077:
        return f"{path}: mode {st.st_mode & 0o777:04o} grants group/other access"
    if not want_dir and not 0 < st.st_size <= 1024:
        return f"{path}: token file must be 1..1024 bytes"
    return None


def preflight(args):
    problems = []
    token_file = flag_value(args, "token-file")
    if token_file is None:
        problems.append("--token-file is required (the token is never taken from argv)")
    else:
        problems.append(private_to_owner(token_file, want_dir=False))
    data_dir = flag_value(args, "data-dir")
    if not data_dir:
        problems.append("--data-dir is required")
    else:
        problems.append(private_to_owner(data_dir, want_dir=True))

    if (flag_value(args, "transcribe") or "").lower() == "local":
        binary = flag_value(args, "transcribe-bin") or ""
        if not (os.path.isabs(binary) and os.access(binary, os.X_OK)):
            problems.append(f"--transcribe-bin {binary!r} is not an executable absolute path")
        model = flag_value(args, "transcribe-model") or ""
        if not os.path.isabs(model):
            problems.append("--transcribe-model must be an absolute path to a baked model directory")
        else:
            for name in MODEL_FILES:
                if not os.path.isfile(os.path.join(model, name)):
                    problems.append(f"{model}/{name}: missing (models are baked into the image, never downloaded)")
    return [p for p in problems if p]


def main(argv):
    args = argv[1:]
    command = args[0] if args and not args[0].startswith("-") else ""
    if command not in ("", "serve"):
        # version, login and anything else run unsupervised with a real terminal.
        os.execv(APP, [APP] + args)

    problems = preflight(args)
    if problems:
        for problem in problems:
            sys.stderr.write(f"entrypoint: preflight failed: {problem}\n")
        return EX_CONFIG

    stopping = threading.Event()
    child = {}

    def on_signal(signum, _frame):
        stopping.set()
        proc = child.get("proc")
        if proc is not None and proc.poll() is None:
            proc.send_signal(signum)

    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)

    unpaired = threading.Event()
    proc = subprocess.Popen([APP] + args, stderr=subprocess.PIPE)
    child["proc"] = proc
    if stopping.is_set():
        proc.terminate()

    def relay():
        for line in proc.stderr:
            sys.stderr.buffer.write(line)
            sys.stderr.buffer.flush()
            if UNPAIRED_MARKER in line:
                unpaired.set()

    reader = threading.Thread(target=relay, daemon=True)
    reader.start()
    status = proc.wait()
    reader.join(timeout=5)

    if unpaired.is_set() and status != 0 and not stopping.is_set():
        sys.stderr.write(
            "entrypoint: UNPAIRED: no WhatsApp session in /data. Idling without restarting; "
            "nothing listens and no QR code was generated. Pair privately with the interactive "
            "`login` command, then restart this service.\n"
        )
        sys.stderr.flush()
        stopping.wait()
        return 0
    return status if status >= 0 else 128 - status


if __name__ == "__main__":
    sys.exit(main(sys.argv))
