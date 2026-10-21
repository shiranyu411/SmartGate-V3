import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[2]


class OSSExportTest(unittest.TestCase):
    def make_repo(self, nested=False):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        source = root / "v3" if nested else root
        source.mkdir(exist_ok=True)
        (source / "scripts").mkdir()
        for name in ("secret-scan.sh", "secret-scan.allow"):
            shutil.copy2(SOURCE / "scripts" / name, source / "scripts" / name)
        shutil.copy2(SOURCE / "Makefile", source / "Makefile")
        (source / "VERSION").write_text("3.0.0\n")
        (source / "README.md").write_text("committed source\n")
        (root / ".gitignore").write_text("dist/\n.env\n.local-backups/\n.review/\n")
        self.run_command(root, "git", "init", "-q")
        self.commit(root)
        return root, source

    def run_command(self, root, *args):
        return subprocess.run(args, cwd=root, capture_output=True, text=True, check=True)

    def commit(self, root):
        self.run_command(root, "git", "add", ".")
        self.run_command(root, "git", "-c", "user.name=Export Test", "-c",
                         "user.email=export@example.com", "commit", "-qm", "fixture")

    def export(self, source):
        return subprocess.run(["make", "oss-export"], cwd=source,
                              capture_output=True, text=True)

    def test_local_and_untracked_files_never_enter_tree_or_tarball(self):
        root, source = self.make_repo()
        for name in (".env", ".local-backups/auth.json", ".review/session.txt",
                     "untracked-subscription.txt"):
            path = root / name
            path.parent.mkdir(exist_ok=True)
            path.write_text("local-only-data\n")
        result = self.export(source)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        exported = source / "dist/oss-smartgate"
        self.assertEqual((exported / "README.md").read_text(), "committed source\n")
        self.assertEqual((exported / "scripts/secret-scan.sh").read_bytes(),
                         (source / "scripts/secret-scan.sh").read_bytes())
        for name in (".env", ".local-backups", ".review", "untracked-subscription.txt"):
            self.assertFalse((exported / name).exists(), name)
        with tarfile.open(source / "dist/smartgate-oss-source.tar.gz") as archive:
            self.assertFalse(any(b"local-only-data" in archive.extractfile(m).read()
                                 for m in archive.getmembers() if m.isfile()))

    def test_dirty_tracked_changes_are_refused(self):
        _, source = self.make_repo()
        (source / "README.md").write_text("uncommitted source\n")
        result = self.export(source)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("commit tracked changes", result.stdout + result.stderr)
        self.assertFalse((source / "dist/oss-smartgate").exists())

    def test_monorepo_export_does_not_include_adjacent_projects(self):
        root, source = self.make_repo(nested=True)
        (root / "neighbor.txt").write_text("adjacent-project\n")
        self.commit(root)
        result = self.export(source)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        exported = source / "dist/oss-smartgate"
        self.assertTrue((exported / "README.md").exists())
        self.assertFalse((exported / "neighbor.txt").exists())
        self.assertFalse((exported / "v3").exists())

    def test_tracked_credential_pattern_blocks_export(self):
        root, source = self.make_repo()
        credential_value = "unsafe-placeholder"
        (source / "unsafe.json").write_text(json.dumps({"api_key": credential_value}))
        self.commit(root)
        result = self.export(source)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("secret-scan: FAIL", result.stdout + result.stderr)
        self.assertFalse((source / "dist/smartgate-oss-source.tar.gz").exists())


if __name__ == "__main__":
    unittest.main()
