import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

from rehearsal_api import RehearsalAPI


class RehearsalGuardTest(unittest.TestCase):
    def test_ssh_helper_refuses_nonlocal_rehearsal(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "targets").mkdir()
            (root / "targets/devvm.env").write_text(
                "TARGET_KIND=rehearsal\nSSH_HOST=192.0.2.1\nSSH_PORT=2222\nSSH_USER=root\n"
                "API_URL=http://fixture\nREMOTE_DATA_DIR=/etc/smartgate\n"
                "REMOTE_RUNTIME_DIR=/tmp/smartgate\nREMOTE_DEPLOY_DIR=/root/deployments\n"
                "REMOTE_STAGE_DIR=/tmp/stage\n"
            )
            token = root / "token"; token.write_text("dummy-fixture")
            client = RehearsalAPI(token)
            with mock.patch.dict(os.environ, {"SMARTGATE_OPS_DIR": str(root)}):
                with self.assertRaisesRegex(RuntimeError, "local rehearsal target required"):
                    client.remote("exit 99")

    def test_fault_fixture_refuses_production_before_connecting(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "targets").mkdir()
            (root / "targets/router.env").write_text(
                "TARGET_KIND=production\nSSH_HOST=fixture\nSSH_PORT=22\nSSH_USER=root\n"
                "API_URL=http://fixture\nREMOTE_DATA_DIR=/etc/smartgate\n"
                "REMOTE_RUNTIME_DIR=/tmp/smartgate\nREMOTE_DEPLOY_DIR=/root/deployments\n"
                "REMOTE_STAGE_DIR=/tmp/stage\n"
            )
            binary = root / "bin"
            binary.mkdir()
            marker = root / "connected"
            ssh = binary / "ssh"
            ssh.write_text(f"#!/bin/sh\ntouch '{marker}'\nexit 99\n")
            ssh.chmod(0o755)
            script = Path(__file__).with_name("31-rehearse-regressions.sh")
            result = subprocess.run(
                ["bash", str(script), "tools"], capture_output=True, text=True,
                env={**os.environ, "TARGET": "router", "SMARTGATE_OPS_DIR": str(root),
                     "PATH": f"{binary}:{os.environ['PATH']}"},
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("restricted to devvm", result.stderr)
            self.assertFalse(marker.exists())
