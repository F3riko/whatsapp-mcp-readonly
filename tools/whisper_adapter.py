#!/usr/bin/env python3
"""whisper_adapter.py: Local-only adapter implementing the OpenAI-whisper argv/output contract.

Usage:
    whisper_adapter.py <input_audio> --output_dir <dir> --output_format txt [--model <name_or_path>] [--language <lang>]

Contract:
    - Accepts single audio input file.
    - Requires --output_dir and --output_format txt.
    - Enforces local_files_only=True: never downloads models from the internet.
    - Rejects symlink inputs, output directories, and output files.
    - Bounds generated transcript text to 1MB to avoid unbounded memory consumption.
    - Produces <output_dir>/audio.txt containing transcribed text.
    - Emits diagnostics to stderr only.
"""

import argparse
import os
import sys
from pathlib import Path

MAX_OUTPUT_CHARS = 1024 * 1024  # 1MB maximum text characters

def main():
    parser = argparse.ArgumentParser(
        description="Local-only Whisper CLI adapter compatible with OpenAI Whisper contract."
    )
    parser.add_argument("input_audio", help="Path to input audio file")
    parser.add_argument("--output_dir", required=True, help="Directory where audio.txt will be written")
    parser.add_argument("--output_format", default="txt", help="Format of output (only 'txt' supported)")
    parser.add_argument("--model", default="tiny", help="Model name or local model directory path")
    parser.add_argument("--language", default=None, help="Optional language hint (e.g. 'en', 'es')")
    parser.add_argument("--device", default="cpu", help="Compute device ('cpu' or 'cuda')")

    args = parser.parse_args()

    # Validate output format
    if args.output_format.lower() != "txt":
        sys.stderr.write(f"Error: unsupported output format {args.output_format!r}. Only 'txt' is supported.\n")
        sys.exit(2)

    # Reject symlink input
    if os.path.islink(args.input_audio):
        sys.stderr.write(f"Error: input audio cannot be a symlink: {args.input_audio}\n")
        sys.exit(2)

    # Validate input audio file
    input_path = Path(args.input_audio).resolve()
    if not input_path.is_file():
        sys.stderr.write(f"Error: input audio file not found: {args.input_audio}\n")
        sys.exit(2)

    # Reject symlink output dir
    if os.path.islink(args.output_dir):
        sys.stderr.write(f"Error: output directory cannot be a symlink: {args.output_dir}\n")
        sys.exit(2)

    # Validate output directory
    out_dir = Path(args.output_dir).resolve()
    out_dir.mkdir(parents=True, exist_ok=True)
    if not out_dir.is_dir():
        sys.stderr.write(f"Error: output path is not a directory: {args.output_dir}\n")
        sys.exit(2)

    # Load faster_whisper backend locally
    try:
        from faster_whisper import WhisperModel
    except ImportError:
        sys.stderr.write(
            "Error: faster-whisper is not installed. Install via: pip install faster-whisper\n"
        )
        sys.exit(3)

    try:
        # local_files_only=True enforces strictly local weights and forbids downloading
        model = WhisperModel(
            args.model,
            device=args.device,
            compute_type="int8" if args.device == "cpu" else "float16",
            local_files_only=True,
        )
    except Exception as e:
        sys.stderr.write(f"Error initializing local Whisper model {args.model!r}: {e}\n")
        sys.exit(4)

    try:
        transcribe_kwargs = {}
        if args.language:
            transcribe_kwargs["language"] = args.language

        segments, _ = model.transcribe(str(input_path), **transcribe_kwargs)
        text_lines = []
        total_chars = 0
        for seg in segments:
            text = getattr(seg, "text", None)
            if not text:
                continue
            stripped = text.strip()
            total_chars += len(stripped) + 1
            if total_chars > MAX_OUTPUT_CHARS:
                sys.stderr.write(f"Error: generated transcript exceeds maximum limit of {MAX_OUTPUT_CHARS} characters\n")
                sys.exit(5)
            text_lines.append(stripped)

        full_text = "\n".join(text_lines)
    except Exception as e:
        sys.stderr.write(f"Error during audio transcription: {e}\n")
        sys.exit(5)

    out_file = out_dir / "audio.txt"
    if out_file.is_symlink():
        sys.stderr.write(f"Error: output file cannot be a symlink: {out_file}\n")
        sys.exit(2)

    try:
        flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0)
        fd = os.open(str(out_file), flags, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(full_text)
    except Exception as e:
        sys.stderr.write(f"Error writing output file {out_file}: {e}\n")
        sys.exit(6)

    sys.exit(0)

if __name__ == "__main__":
    main()
