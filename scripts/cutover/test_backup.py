import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

import backup


class BackupTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.files = {
            "usr/bin/smartgate": b"binary-fixture",
            "etc/init.d/smartgate": b"init-fixture",
            "www/smartgate/index.html": b"frontend-fixture",
            "etc/smartgate/version.json": b'{"commit":"baseline"}',
            "etc/smartgate/spec.json": b'{"providers":[{"id":"provider1"}]}',
            "etc/smartgate/applied-state.json": b'{"schema_version":1,"gateway_mode":"on"}',
            "etc/smartgate/auth.json": b"fixture-secret",
            "etc/config/network": b"network-fixture",
        }

    def write_archive(self, path, files):
        with tarfile.open(path, "w:gz") as archive:
            for name, data in files.items():
                member = tarfile.TarInfo(name)
                member.mode = 0o600
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
            member = tarfile.TarInfo("etc/rc.d/S99smartgate")
            member.type = tarfile.SYMTYPE
            member.linkname = "../init.d/smartgate"
            archive.addfile(member)
        path.chmod(0o600)

    def prepare(self):
        self.write_archive(self.root / "persistent-root.tar.gz", self.files)
        self.write_archive(self.root / "v2-files.tar.gz", self.files)
        hashes = "".join(
            f"{hashlib.sha256(data).hexdigest()}  {name}\n"
            for name, data in sorted(self.files.items())
        )
        for name in ("v2-before.sha256", "v2-after.sha256"):
            (self.root / name).write_text(hashes)
        (self.root / "services.txt").write_text("smartgate=yes\nlegacy=yes\n")
        (self.root / "recovery-paths.txt").write_text(
            "usr/bin/smartgate\netc/init.d/smartgate\nwww/smartgate\netc/smartgate\netc/rc.d\n"
        )

    def finalize(self, legacy_directory=None):
        with contextlib.redirect_stdout(io.StringIO()):
            backup.finalize(self.root, "router", "test-host", "baseline", legacy_directory)
        (self.root / "v2-rollback.tar.gz").chmod(0o600)

    def test_roundtrip_preserves_every_file_and_symlink(self):
        self.prepare()
        self.finalize()
        marker = json.loads((self.root / "BACKUP_VERIFIED.json").read_text())
        self.assertEqual(marker["verified_v2_files"], len(self.files))
        with contextlib.redirect_stdout(io.StringIO()):
            backup.check(self.root, "router", "test-host", "baseline", marker["spec_sha256"])
        with tarfile.open(self.root / "v2-rollback.tar.gz", "r:gz") as archive:
            for name, data in self.files.items():
                if name != "etc/config/network":
                    self.assertEqual(archive.extractfile(name).read(), data)
            self.assertNotIn("etc/config/network", archive.getnames())
            self.assertEqual(archive.extractfile("old-mode").read(), b"on\n")
            self.assertEqual(archive.getmember("etc/rc.d/S99smartgate").linkname, "../init.d/smartgate")
        hashes = backup.restore_hashes(self.root)
        self.assertIn("  etc/smartgate/auth.json\n", hashes)
        self.assertNotIn("old-mode", hashes)
        self.assertNotIn("etc/config/network", hashes)

    def test_changed_source_never_gets_verified_marker(self):
        self.prepare()
        (self.root / "v2-after.sha256").write_text("")
        with self.assertRaises(ValueError):
            self.finalize()
        self.assertFalse((self.root / "BACKUP_VERIFIED.json").exists())

    def test_v3_baseline_preserves_schema_and_does_not_require_legacy_core(self):
        self.files["etc/smartgate/applied-state.json"] = b'{"schema_version":2,"gateway_mode":"on"}'
        self.prepare()
        self.finalize("/etc/retired-core")
        marker = json.loads((self.root / "BACKUP_VERIFIED.json").read_text())
        self.assertEqual(marker["applied_schema"], 2)
        with contextlib.redirect_stdout(io.StringIO()):
            backup.check(self.root, "router", "test-host", "baseline", marker["spec_sha256"])
        with tarfile.open(self.root / "v2-rollback.tar.gz", "r:gz") as archive:
            self.assertEqual(archive.extractfile("old-schema").read(), b"2\n")
            self.assertNotIn("legacy-directory-complete", archive.getnames())
            self.assertEqual(
                archive.extractfile("etc/smartgate/applied-state.json").read(),
                self.files["etc/smartgate/applied-state.json"],
            )

    def test_unknown_baseline_schema_is_refused(self):
        self.files["etc/smartgate/applied-state.json"] = b'{"schema_version":99}'
        self.prepare()
        with self.assertRaises(ValueError):
            self.finalize()
        self.assertFalse((self.root / "BACKUP_VERIFIED.json").exists())

    def test_firewall_lifecycle_files_are_in_private_rollback(self):
        lifecycle = {
            "etc/config/firewall": b"baseline-firewall",
            "etc/firewall.smartgate-reconcile": b"baseline-bridge",
        }
        self.files.update(lifecycle)
        self.prepare()
        with (self.root / "recovery-paths.txt").open("a") as stream:
            stream.write("etc/config/firewall\netc/firewall.smartgate-reconcile\n")
        self.finalize()
        with tarfile.open(self.root / "v2-rollback.tar.gz", "r:gz") as archive:
            for name, data in lifecycle.items():
                self.assertEqual(archive.extractfile(name).read(), data)

    def test_complete_legacy_directory_includes_rules_and_service_configuration(self):
        legacy_files = {
            "etc/legacy-core/config.json": b"legacy-config-fixture",
            "etc/legacy-core/rule-set/local.srs": b"local-rules-fixture",
            "etc/config/legacy-core": b"legacy-uci-fixture",
            "etc/init.d/legacy-core": b"legacy-init-fixture",
        }
        self.files.update(legacy_files)
        self.prepare()
        with (self.root / "recovery-paths.txt").open("a") as stream:
            stream.write("etc/legacy-core\netc/config/legacy-core\netc/init.d/legacy-core\n")
        self.finalize("/etc/legacy-core")
        with tarfile.open(self.root / "v2-rollback.tar.gz", "r:gz") as archive:
            for name, data in legacy_files.items():
                self.assertEqual(archive.extractfile(name).read(), data)
            self.assertEqual(
                archive.extractfile("legacy-directory-complete").read(), b"etc/legacy-core\n",
            )
        hashes = backup.restore_hashes(self.root)
        for name in legacy_files:
            self.assertIn(f"  {name}\n", hashes)
        self.assertNotIn("legacy-directory-complete", hashes)

    def test_complete_legacy_directory_cannot_be_omitted_from_recovery(self):
        self.files["etc/legacy-core/rule-set/local.srs"] = b"local-rules-fixture"
        self.prepare()
        with self.assertRaises(ValueError):
            self.finalize("/etc/legacy-core")
        self.assertFalse((self.root / "BACKUP_VERIFIED.json").exists())

    def test_complete_legacy_directory_must_exist_in_source(self):
        self.prepare()
        with (self.root / "recovery-paths.txt").open("a") as stream:
            stream.write("etc/legacy-core\n")
        with self.assertRaises(ValueError):
            self.finalize("/etc/legacy-core")
        self.assertFalse((self.root / "BACKUP_VERIFIED.json").exists())

    def test_corrupted_content_never_gets_verified_marker(self):
        self.prepare()
        self.files["etc/smartgate/spec.json"] = b"unexpected-change"
        self.write_archive(self.root / "v2-files.tar.gz", self.files)
        with self.assertRaises(ValueError):
            self.finalize()
        self.assertFalse((self.root / "BACKUP_VERIFIED.json").exists())

    def test_wrong_identity_and_changed_spec_are_refused(self):
        self.prepare()
        self.finalize()
        for args in (
            ("devvm", "test-host", "baseline", ""),
            ("router", "other-host", "baseline", ""),
            ("router", "test-host", "other-commit", ""),
            ("router", "test-host", "baseline", "changed-spec"),
        ):
            with self.subTest(args=args), self.assertRaises(ValueError):
                backup.check(self.root, *args)

    def test_archive_tampering_is_refused(self):
        self.prepare()
        self.finalize()
        with (self.root / "v2-rollback.tar.gz").open("ab") as stream:
            stream.write(b"changed")
        with self.assertRaises(ValueError):
            backup.check(self.root, "router", "test-host", "baseline", "")

    def test_rehearsal_fixture_cannot_be_restored_to_production(self):
        self.prepare()
        self.finalize()
        path = self.root / "BACKUP_VERIFIED.json"
        marker = json.loads(path.read_text())
        marker["rehearsal_only"] = True
        path.write_text(json.dumps(marker))
        with self.assertRaises(ValueError):
            backup.check(self.root, "router", "test-host", "baseline", "")
        marker.update(target="devvm", host="127.0.0.1")
        path.write_text(json.dumps(marker))
        with contextlib.redirect_stdout(io.StringIO()):
            backup.check(self.root, "devvm", "127.0.0.1", "baseline", "")

    def test_unsafe_member_is_refused(self):
        for name in ("../etc/config/network", "/etc/config/network"):
            with self.subTest(name=name):
                self.write_archive(self.root / "unsafe.tar.gz", {name: b"fixture"})
                with self.assertRaises(ValueError):
                    backup.archive_manifest(self.root / "unsafe.tar.gz")

    def test_private_directory_rejects_workspace_and_symlink(self):
        workspace = self.root / "workspace"
        workspace.mkdir()
        link = self.root / "link"
        link.symlink_to(workspace, target_is_directory=True)
        for path in (workspace / "backups", link / "backups"):
            with self.assertRaises(ValueError):
                backup.private_directory(path, [workspace], create=True)
        public = self.root / "public"
        public.mkdir(mode=0o755)
        public.chmod(0o755)
        with self.assertRaises(ValueError):
            backup.private_directory(public)

    def test_wan_override_keeps_lan_api(self):
        repo = Path(__file__).resolve().parents[2]
        ops = self.root / "ops"
        (ops / "targets").mkdir(parents=True)
        (ops / "targets/router.env").write_text(
            (repo / "configs/targets/router.env.example").read_text()
        )
        env = dict(os.environ, TARGET="router", CUTOVER_SSH_HOST="192.0.2.10",
                   CUTOVER_SSH_KEY="", CUTOVER_SSH_PORT="22", SMARTGATE_OPS_DIR=str(ops))
        result = subprocess.run(
            ["bash", "-c", '. scripts/cutover/common.sh\nload_target\nprintf "%s %s" "$SSH_HOST" "$API_URL"'],
            cwd=repo, env=env, check=True, capture_output=True, text=True,
        )
        self.assertEqual(result.stdout, "192.0.2.10 http://192.0.2.1:3000")

    def test_reclaim_plan_requires_verified_copies_and_retains_newest_three(self):
        names = [f"before-2026090{day}T000000Z.tar.gz" for day in range(1, 6)]
        sha = hashlib.sha256(b"backup-fixture").hexdigest()
        prefix = "root/smartgate-deployments/"
        manifest = {prefix + name: {"sha256": sha} for name in names}
        (self.root / "persistent-root.manifest.json").write_text(json.dumps(manifest))
        listing = "".join(f"{sha}  /{prefix}{name}\n" for name in names)
        plan = backup.reclaim_plan(self.root, listing, "/root/smartgate-deployments")
        self.assertEqual(plan, "".join(f"{sha}  {name}\n" for name in names[:2]))
        manifest[prefix + names[0]]["sha256"] = "changed"
        (self.root / "persistent-root.manifest.json").write_text(json.dumps(manifest))
        with self.assertRaises(ValueError):
            backup.reclaim_plan(self.root, listing, "/root/smartgate-deployments")


if __name__ == "__main__":
    unittest.main()
