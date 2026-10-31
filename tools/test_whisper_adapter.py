#!/usr/bin/env python3
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

TOOLS_DIR = Path(__file__).resolve().parent
ADAPTER_PATH = TOOLS_DIR / "whisper_adapter.py"

class TestWhisperAdapterContract(unittest.TestCase):
    def run_adapter(self, args, env=None):
        cmd = [sys.executable, str(ADAPTER_PATH)] + args
        merged_env = os.environ.copy()
        if env:
            merged_env.update(env)
        res = subprocess.run(cmd, capture_output=True, text=True, env=merged_env)
        return res

    def test_missing_input_file(self):
        res = self.run_adapter([])
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("usage", res.stderr.lower() + res.stdout.lower())

    def test_nonexistent_input_file(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            res = self.run_adapter(["/nonexistent/audio.ogg", "--output_dir", tmpdir, "--output_format", "txt"])
            self.assertNotEqual(res.returncode, 0)
            self.assertIn("not found", res.stderr.lower())

    def test_invalid_output_format(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            audio_file = Path(tmpdir) / "sample.ogg"
            audio_file.write_bytes(b"dummy audio")
            res = self.run_adapter([str(audio_file), "--output_dir", tmpdir, "--output_format", "wav"])
            self.assertNotEqual(res.returncode, 0)
            self.assertIn("format", res.stderr.lower())

    def test_symlink_input_rejected(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            real_file = Path(tmpdir) / "real.ogg"
            real_file.write_bytes(b"audio content")
            sym_file = Path(tmpdir) / "sym.ogg"
            os.symlink(real_file, sym_file)

            res = self.run_adapter([str(sym_file), "--output_dir", tmpdir, "--output_format", "txt"])
            self.assertNotEqual(res.returncode, 0)
            self.assertIn("symlink", res.stderr.lower())

    def test_symlink_output_dir_rejected(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            real_dir = Path(tmpdir) / "real_out"
            real_dir.mkdir()
            sym_dir = Path(tmpdir) / "sym_out"
            os.symlink(real_dir, sym_dir)

            audio_file = Path(tmpdir) / "audio.ogg"
            audio_file.write_bytes(b"audio content")

            res = self.run_adapter([str(audio_file), "--output_dir", str(sym_dir), "--output_format", "txt"])
            self.assertNotEqual(res.returncode, 0)
            self.assertIn("symlink", res.stderr.lower())

    def test_successful_transcription_with_fake_backend(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            audio_file = Path(tmpdir) / "voice.ogg"
            audio_file.write_bytes(b"OggS dummy opus audio content")
            out_dir = Path(tmpdir) / "out"
            out_dir.mkdir()

            mock_pkg_dir = Path(tmpdir) / "mock_pkg"
            mock_pkg_dir.mkdir()
            fake_fw = mock_pkg_dir / "faster_whisper.py"
            fake_fw.write_text("""
class WhisperModel:
    def __init__(self, model_size_or_path, device="cpu", compute_type="default", local_files_only=True, **kwargs):
        if not local_files_only:
            raise ValueError("local_files_only must be True")
        self.model = model_size_or_path

    def transcribe(self, audio_path, language=None, **kwargs):
        class Segment:
            def __init__(self, text):
                self.text = text
        return [Segment("Hello from fake faster-whisper")], None
""")

            pythonpath = f"{mock_pkg_dir}:{os.environ.get('PYTHONPATH', '')}"
            res = self.run_adapter(
                [str(audio_file), "--output_dir", str(out_dir), "--output_format", "txt", "--model", "tiny", "--language", "en"],
                env={"PYTHONPATH": pythonpath}
            )
            self.assertEqual(res.returncode, 0, f"Adapter failed: {res.stderr}")

            out_txt = out_dir / "audio.txt"
            self.assertTrue(out_txt.exists(), "audio.txt should be created")
            content = out_txt.read_text().strip()
            self.assertEqual(content, "Hello from fake faster-whisper")

    def test_oversized_generated_text_rejected(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            audio_file = Path(tmpdir) / "voice.ogg"
            audio_file.write_bytes(b"OggS dummy opus audio content")
            out_dir = Path(tmpdir) / "out"
            out_dir.mkdir()

            mock_pkg_dir = Path(tmpdir) / "mock_pkg"
            mock_pkg_dir.mkdir()
            fake_fw = mock_pkg_dir / "faster_whisper.py"
            fake_fw.write_text("""
class WhisperModel:
    def __init__(self, *args, **kwargs):
        pass
    def transcribe(self, *args, **kwargs):
        class Segment:
            def __init__(self, text):
                self.text = text
        # Generate 2MB of text across segments
        return [Segment("A" * 10000) for _ in range(200)], None
""")

            pythonpath = f"{mock_pkg_dir}:{os.environ.get('PYTHONPATH', '')}"
            res = self.run_adapter(
                [str(audio_file), "--output_dir", str(out_dir), "--output_format", "txt"],
                env={"PYTHONPATH": pythonpath}
            )
            self.assertNotEqual(res.returncode, 0)
            self.assertIn("exceeds maximum limit", res.stderr.lower())

if __name__ == "__main__":
    unittest.main()
