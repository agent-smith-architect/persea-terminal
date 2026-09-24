#!/usr/bin/env python3
"""Exercise the installer's real build boundary with a disposable root lock."""

import os
from pathlib import Path
import pwd
import queue
import signal
import subprocess
import sys
import tempfile
import threading
import unittest


DEPLOY = Path(os.environ.get("PERSEA_TEST_DEPLOY_DIR", Path(__file__).resolve().parents[1]))

BUILD = r'''
import fcntl, os, sys
fd, device, inode = map(int, sys.argv[1:])
assert os.getuid() != 0
inherited = []
for name in os.listdir('/proc/self/fd'):
    try:
        info = os.fstat(int(name))
    except OSError:
        continue
    if (info.st_dev, info.st_ino) == (device, inode):
        inherited.append(name)
try:
    fcntl.flock(fd, fcntl.LOCK_UN)
    unlocked = True
except OSError:
    unlocked = False
# A detached descendant must not prolong the transaction's lock lifetime.
child = os.fork()
if child == 0:
    os.setsid()
    for stream in (0, 1, 2):
        os.close(stream)
    signal = __import__('signal')
    signal.pause()
    os._exit(0)
print('build', os.getuid(), inherited, unlocked, child, flush=True)
input()
print('build-done', flush=True)
'''


class PrivilegeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="deploy-privilege-")
        self.root = Path(self.temporary.name)
        self.root.chmod(0o755)
        (self.root / "run").mkdir()
        self.process = None
        self.detached = None
        # Use the actual installer function so omitting its wrapper is observable.
        installer = (DEPLOY / "install.sh").read_text()
        build_function = installer.split("run_build() {", 1)[1].split("\n}\n", 1)[0]
        self.script = self.root / "transaction.sh"
        self.script.write_text('''#!/bin/bash
set -Eeuo pipefail
source "$1/lib.sh"
PERSEA_HERMETIC=1
PERSEA_ROOT_PREFIX=$2
persea_lock_deployment
PERSEA_HERMETIC=0
PERSEA_FRONT_USER=nobody
PERSEA_FRONT_UID=$3
build_path=/usr/bin:/bin
build_workspace=$2
build_tmp=$2
run_build() {''' + build_function + '''
}
read -r device inode < <(stat -c '%d %i' "$2/run/persea-terminal-deploy.lock")
run_build /usr/bin/python3 -c "$4" "$PERSEA_DEPLOY_LOCK_FD" "$device" "$inode"
printf 'transaction-done\\n'
''')

    def tearDown(self):
        if self.detached is not None:
            try:
                os.kill(self.detached, signal.SIGKILL)
            except ProcessLookupError:
                pass
        if self.process is not None:
            try:
                os.killpg(self.process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.process.wait(timeout=5)
            for stream in (self.process.stdin, self.process.stdout, self.process.stderr):
                stream.close()
        self.temporary.cleanup()

    def exercise(self, kill_launcher):
        self.process = subprocess.Popen(
            ["/bin/bash", str(self.script), str(DEPLOY), str(self.root),
             str(pwd.getpwnam("nobody").pw_uid), BUILD],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, start_new_session=True)
        messages = queue.Queue()
        def read_output():
            for line in self.process.stdout:
                messages.put(line.strip())
        threading.Thread(target=read_output, daemon=True).start()
        line = messages.get(timeout=10)
        self.assertTrue(line.startswith("build "), line)
        self.detached = int(line.rsplit(" ", 1)[1])
        self.assertEqual(line.split()[2:4], ["[]", "False"],
                         "build inherited or unlocked the root lock: " + line)
        lock = self.root / "run/persea-terminal-deploy.lock"
        def contender():
            return subprocess.run(["flock", "-n", str(lock), "true"]).returncode
        self.assertNotEqual(contender(), 0)
        if kill_launcher:
            os.kill(self.process.pid, signal.SIGKILL)
            self.assertEqual(self.process.wait(timeout=5), -signal.SIGKILL)
            self.assertNotEqual(contender(), 0, "waiter lost exclusion with a live build")
        self.process.stdin.write("continue\n")
        self.process.stdin.flush()
        self.assertEqual(messages.get(timeout=10), "build-done")
        if not kill_launcher:
            self.assertEqual(messages.get(timeout=10), "transaction-done")
            self.assertEqual(self.process.wait(timeout=5), 0)
        # A blocking contender gives the trusted waiter time to reap its child.
        self.assertEqual(subprocess.run(["flock", "-w", "5", str(lock), "true"]).returncode, 0,
                         "detached build process retained the deployment lock")
        os.kill(self.detached, 0)

    def test_build_cannot_unlock_or_retain_root_lock(self):
        self.exercise(False)

    def test_launcher_kill_keeps_lock_until_unprivileged_step_exits(self):
        self.exercise(True)


if __name__ == "__main__":
    os.umask(0o022)
    if os.geteuid() != 0:
        # Only fixture operations run as root; no installer or host service runs.
        sys.exit(subprocess.run(["sudo", "-n", "/usr/bin/env",
            "PERSEA_TEST_DEPLOY_DIR=" + str(DEPLOY), sys.executable,
            str(Path(__file__).resolve()), *sys.argv[1:]]).returncode)
    unittest.main()
