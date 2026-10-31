#!/usr/bin/env python3
"""Behavioural tests for entrypoint.py using fake server binaries.

No WhatsApp, no network, no model. Run: python3 -m unittest deploy/docker/test_entrypoint.py
"""
import os
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
SECRET = "wamcp_not-a-real-token-for-tests"

DRIVER = (
    "import sys; sys.path.insert(0, %r); import entrypoint; "
    "entrypoint.APP = sys.argv[1]; sys.exit(entrypoint.main(['entrypoint'] + sys.argv[2:]))" % str(HERE)
)


class EntrypointTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.data = root / "data"
        self.data.mkdir(mode=0o700)
        os.chmod(self.data, 0o700)
        self.token = root / "token"
        self.token.write_text(SECRET + "\n")
        os.chmod(self.token, 0o400)
        self.root = root

    def fake_app(self, body):
        path = self.root / "fake-app"
        path.write_text("#!/bin/sh\n" + body)
        os.chmod(path, 0o755)
        return str(path)

    def args(self, **extra):
        out = ["--http", "127.0.0.1:8787", "--token-file", str(self.token), "--data-dir", str(self.data)]
        for k, v in extra.items():
            out += ["--" + k.replace("_", "-"), v]
        return out

    def start(self, app, args):
        return subprocess.Popen(
            [sys.executable, "-c", DRIVER, app] + args,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )

    def run_to_end(self, app, args, timeout=20):
        proc = self.start(app, args)
        out, err = proc.communicate(timeout=timeout)
        return proc.returncode, out, err

    def test_preflight_rejects_group_readable_token_without_leaking_it(self):
        os.chmod(self.token, 0o440)
        rc, out, err = self.run_to_end(self.fake_app("exit 0\n"), self.args())
        self.assertEqual(rc, 78)
        self.assertIn("grants group/other access", err)
        self.assertNotIn(SECRET, out + err)

    def test_preflight_rejects_open_data_dir_and_missing_flags(self):
        os.chmod(self.data, 0o755)
        rc, _, err = self.run_to_end(self.fake_app("exit 0\n"), self.args())
        self.assertEqual(rc, 78)
        self.assertIn("grants group/other access", err)
        rc, _, err = self.run_to_end(self.fake_app("exit 0\n"), ["--http", "127.0.0.1:8787"])
        self.assertEqual(rc, 78)
        self.assertIn("--token-file is required", err)
        self.assertIn("--data-dir is required", err)

    def test_preflight_rejects_symlinked_token(self):
        link = self.root / "token-link"
        link.symlink_to(self.token)
        rc, _, err = self.run_to_end(self.fake_app("exit 0\n"), self.args(token_file=str(link))[:0] + [
            "--token-file", str(link), "--data-dir", str(self.data)])
        self.assertEqual(rc, 78)
        self.assertIn("must not be a symlink", err)

    def test_preflight_requires_baked_model_when_local_transcription_is_on(self):
        rc, _, err = self.run_to_end(
            self.fake_app("exit 0\n"),
            self.args(transcribe="local", transcribe_bin=sys.executable, transcribe_model=str(self.root / "nomodel")),
        )
        self.assertEqual(rc, 78)
        self.assertIn("never downloaded", err)

    def test_preflight_accepts_complete_model(self):
        model = self.root / "model"
        model.mkdir()
        for name in ("config.json", "model.bin", "tokenizer.json", "vocabulary.txt"):
            (model / name).write_text("x")
        rc, _, _ = self.run_to_end(
            self.fake_app("exit 0\n"),
            self.args(transcribe="local", transcribe_bin=sys.executable, transcribe_model=str(model)),
        )
        self.assertEqual(rc, 0)

    def test_unpaired_exit_idles_instead_of_restarting_then_stops_on_sigterm(self):
        app = self.fake_app(
            "echo 'whatsapp-mcp: whatsapp session is not paired; run login' >&2\nexit 1\n"
        )
        proc = self.start(app, self.args())
        time.sleep(1.5)
        self.assertIsNone(proc.poll(), "unpaired must idle, not exit (exit would restart-loop)")
        proc.send_signal(signal.SIGTERM)
        out, err = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 0)
        self.assertIn("UNPAIRED", err)
        self.assertNotIn("QR-CODE", out + err)

    def test_other_failures_pass_through_and_stderr_is_relayed(self):
        rc, out, err = self.run_to_end(self.fake_app("echo boom >&2\nexit 3\n"), self.args())
        self.assertEqual(rc, 3)
        self.assertIn("boom", err)
        self.assertNotIn("UNPAIRED", err)

    def test_sigterm_is_forwarded_to_the_server(self):
        marker = self.root / "got-term"
        ready = self.root / "ready"
        app = self.fake_app(
            f"trap 'echo term > {marker}; exit 0' TERM\ntouch {ready}\nwhile :; do sleep 0.1; done\n"
        )
        proc = self.start(app, self.args())
        deadline = time.time() + 10
        while time.time() < deadline and not ready.exists():
            time.sleep(0.1)
        self.assertTrue(ready.exists(), "fake server never started")
        proc.send_signal(signal.SIGTERM)
        proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 0)
        self.assertTrue(marker.exists())

    def test_non_serve_commands_run_unsupervised_and_skip_preflight(self):
        rc, out, _ = self.run_to_end(self.fake_app('echo "ran $*"\n'), ["version"])
        self.assertEqual(rc, 0)
        self.assertIn("ran version", out)

    def test_login_phone_stdin_is_exec_ed_with_stdin_intact_and_no_preflight(self):
        # A synthetic marker, never a real number: the fake app records what it
        # was given so the test can prove stdin reaches it untouched.
        seen = self.root / "seen"
        app = self.fake_app(f'echo "$*" > {seen}.argv\ncat > {seen}.stdin\n')
        proc = subprocess.Popen(
            [sys.executable, "-c", DRIVER, app, "login", "--phone-stdin", "--data-dir", str(self.data)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        out, err = proc.communicate("+10000000000\n", timeout=20)
        self.assertEqual(proc.returncode, 0, err)
        self.assertEqual(Path(f"{seen}.argv").read_text().strip(), f"login --phone-stdin --data-dir {self.data}")
        self.assertEqual(Path(f"{seen}.stdin").read_text(), "+10000000000\n")
        self.assertNotIn("preflight", err)
        self.assertNotIn("+10000000000", out + err)


if __name__ == "__main__":
    unittest.main()
