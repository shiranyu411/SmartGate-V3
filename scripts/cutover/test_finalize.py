import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class FinalizeTest(unittest.TestCase):
    def test_same_day_hotfix_retains_original_production_tag(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repo = root / "v3"
            scripts = repo / "scripts/cutover"
            scripts.mkdir(parents=True)
            source = Path(__file__).resolve().parent
            for name in ("common.sh", "90-finalize.sh"):
                shutil.copy2(source / name, scripts / name)
            ops = root / "ops"
            (ops / "targets").mkdir(parents=True)
            (ops / "targets/router.env").write_text(
                "SSH_HOST=fixture\nSSH_PORT=22\nSSH_USER=root\nAPI_URL=http://fixture\n"
                "REMOTE_DATA_DIR=/etc/smartgate\nREMOTE_RUNTIME_DIR=/tmp/smartgate\n"
                "REMOTE_DEPLOY_DIR=/root/deployments\nREMOTE_STAGE_DIR=/tmp/stage\n"
                "TARGET_KIND=production\n"
            )
            binaries = root / "bin"
            binaries.mkdir()
            ssh = binaries / "ssh"
            ssh.write_text('#!/bin/sh\nprintf "%s\\n" "$FIXTURE_VERSION"\n')
            ssh.chmod(0o755)

            def git(*args):
                return subprocess.check_output(["git", "-C", str(repo), *args], text=True).strip()

            git("init", "-q")
            git("config", "user.name", "Fixture")
            git("config", "user.email", "fixture@example.invalid")
            git("commit", "--allow-empty", "-qm", "first")
            first = git("rev-parse", "--short", "HEAD")
            today = subprocess.check_output(["date", "-u", "+%Y-%m-%d"], text=True).strip()
            original = f"prod/{today}"
            git("tag", original)
            git("commit", "--allow-empty", "-qm", "hotfix")
            current = git("rev-parse", "--short", "HEAD")
            (ops / "deployments.log").write_text(f"now\trouter\t30-verify.sh\tv3@{current}\tpass\tfixture\n")
            env = dict(
                os.environ, TARGET="router", PATH=f"{binaries}:{os.environ['PATH']}",
                FIXTURE_VERSION=json.dumps({"version": "3.0.0", "commit": current, "built_at": "fixture"}),
            )
            for _ in range(2):
                subprocess.run(
                    ["bash", str(scripts / "90-finalize.sh")], env=env,
                    check=True, capture_output=True, text=True,
                )
            self.assertEqual(git("rev-parse", "--short", f"{original}^{{commit}}"), first)
            hotfix = f"{original}-{current}"
            self.assertEqual(git("rev-parse", "--short", f"{hotfix}^{{commit}}"), current)
            self.assertIn(f"tag={hotfix}", (ops / "deployments.log").read_text())

            target_env = ops / "targets/router.env"
            target_env.write_text(target_env.read_text().replace("TARGET_KIND=production", "TARGET_KIND=rehearsal"))
            result = subprocess.run(
                ["bash", str(scripts / "90-finalize.sh")], env=env,
                check=True, capture_output=True, text=True,
            )
            self.assertIn("production STATUS.md, router.md and V2 retirement state are unchanged", result.stdout)
            self.assertNotIn("当前阶段 → P4", result.stdout)
            self.assertNotIn("Human edits still required", result.stdout)
