#!/usr/bin/env python3
"""Signal the public launcher PID while a competing transaction waits."""

import os
from pathlib import Path
import queue
import signal
import subprocess
import sys
import tempfile
import threading
import unittest


LIB = Path(os.environ.get("PERSEA_TEST_LOCK_LIB", Path(__file__).resolve().parents[1] / "lib.sh"))

HARNESS = r'''#!/bin/bash
set -Eeuo pipefail
source "$TEST_LIB"
PERSEA_HERMETIC=1
PERSEA_ROOT_PREFIX=$TEST_ROOT
# Also exercise the former supervisor implementation in sensitivity runs.
PERSEA_DEPLOY_ARGUMENTS=("$@")
umask 0022
persea_lock_deployment
printf 'locked %s %s\n' "$$" "$(umask)"
if [[ $1 == contender ]]; then exit; fi
restore() {
  trap - TERM
  printf 'restoring\n'
  read -r _
  exit 143
}
trap restore TERM
trap 'printf "cleanup\n"; read -r _' EXIT
# A synchronous transaction step must keep exclusion if its parent is killed.
/bin/bash -c 'printf "step\n"; read -r _; printf "step-done\n"'
printf 'normal\n'
'''


class LockTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="deploy-lock-")
        self.root = Path(self.temporary.name)
        (self.root / "run").mkdir()
        self.script = self.root / "transaction.sh"
        self.script.write_text(HARNESS)
        self.script.chmod(0o755)
        self.processes = []

    def tearDown(self):
        for process, _ in self.processes:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=5)
            process.stdin.close()
            process.stdout.close()
            process.stderr.close()
        self.temporary.cleanup()

    def launch(self, mode):
        process = subprocess.Popen(
            [str(self.script), mode],
            env={**os.environ, "TEST_LIB": str(LIB), "TEST_ROOT": str(self.root)},
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, start_new_session=True,
        )
        messages = queue.Queue()
        def read_output():
            for line in process.stdout:
                messages.put(line.strip())
        threading.Thread(target=read_output, daemon=True).start()
        self.processes.append((process, messages))
        return process, messages

    def receive(self, messages, expected):
        try:
            line = messages.get(timeout=5)
        except queue.Empty:
            self.fail(f"timed out waiting for {expected}")
        self.assertTrue(line.startswith(expected), line)
        return line

    def release_step(self, process):
        process.stdin.write("continue\n")
        process.stdin.flush()

    def excluded(self, contender_messages):
        lock = self.root / "run/persea-terminal-deploy.lock"
        self.assertNotEqual(subprocess.run(["flock", "-n", str(lock), "true"]).returncode, 0,
                            "launcher released exclusion while a transaction step could still run")
        with self.assertRaises(queue.Empty):
            contender_messages.get(timeout=0.1)

    def exercise(self, sig):
        process, messages = self.launch("transaction")
        locked = self.receive(messages, "locked")
        self.assertEqual(locked.split()[2], "0022")
        self.assertEqual((self.root / "run/persea-terminal-deploy.lock").stat().st_mode & 0o777, 0o600)
        self.receive(messages, "step")
        contender, contender_messages = self.launch("contender")
        self.excluded(contender_messages)
        if sig is not None:
            os.kill(process.pid, sig)
            if sig == signal.SIGKILL:
                self.assertEqual(process.wait(timeout=5), -signal.SIGKILL)
            self.excluded(contender_messages)
        self.release_step(process)
        self.receive(messages, "step-done")
        if sig != signal.SIGKILL:
            if sig == signal.SIGTERM:
                self.receive(messages, "restoring")
                self.excluded(contender_messages)
                self.release_step(process)
            else:
                self.receive(messages, "normal")
            self.receive(messages, "cleanup")
            self.excluded(contender_messages)
            self.release_step(process)
            self.assertEqual(process.wait(timeout=5), 143 if sig else 0)
        self.receive(contender_messages, "locked")
        self.assertEqual(contender.wait(timeout=5), 0)

    def test_normal_completion_and_exit_cleanup(self):
        self.exercise(None)

    def test_launcher_term_preserves_step_and_restoration_exclusion(self):
        self.exercise(signal.SIGTERM)

    def test_launcher_kill_preserves_live_step_exclusion(self):
        self.exercise(signal.SIGKILL)


if __name__ == "__main__":
    os.umask(0o022)
    unittest.main()
