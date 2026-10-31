#!/usr/bin/env python3
"""Download a pinned faster-whisper model at image build time and verify it.

Usage: fetch_model.py LOCKFILE DEST_DIR

Every file is fetched from an exact Hugging Face commit and must match the
size and SHA-256 recorded in the lock file, or the build fails. The running
container never downloads anything: the adapter loads DEST_DIR with
local_files_only=True.
"""
import hashlib
import json
import os
import sys
import urllib.request

CHUNK = 1 << 20


def fetch(url, dest):
    digest = hashlib.sha256()
    size = 0
    with urllib.request.urlopen(url, timeout=60) as resp, open(dest, "wb") as out:
        while True:
            block = resp.read(CHUNK)
            if not block:
                break
            digest.update(block)
            size += len(block)
            out.write(block)
    return digest.hexdigest(), size


def main(argv):
    if len(argv) != 3:
        sys.stderr.write(__doc__)
        return 2
    with open(argv[1], encoding="utf-8") as fh:
        lock = json.load(fh)
    dest_dir = argv[2]
    os.makedirs(dest_dir, exist_ok=True)
    for name, want in sorted(lock["files"].items()):
        if "/" in name or name.startswith("."):
            sys.stderr.write(f"refusing unsafe file name {name!r}\n")
            return 1
        url = f"https://huggingface.co/{lock['repo']}/resolve/{lock['revision']}/{name}"
        dest = os.path.join(dest_dir, name)
        got_sha, got_size = fetch(url, dest)
        if got_sha != want["sha256"] or got_size != want["size"]:
            sys.stderr.write(f"{name}: checksum or size mismatch, refusing to build\n")
            return 1
        print(f"verified {name} {got_size} bytes sha256={got_sha}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
