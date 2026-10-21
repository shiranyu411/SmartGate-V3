package state

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLayoutEnsureTightensInheritedPermissions covers audit finding S2-3: a
// V3 daemon starting on top of state written by an older release (0755 dirs,
// 0644 files) must leave nothing group/other-readable under the data tree.
func TestLayoutEnsureTightensInheritedPermissions(t *testing.T) {
	root := t.TempDir()
	layout := Layout{DataDir: filepath.Join(root, "etc"), RuntimeDir: filepath.Join(root, "tmp")}

	legacyDirs := []string{
		layout.DataDir,
		layout.LastGoodDir(),
		layout.SnapshotsDir(),
		layout.BackupsDir(),
		filepath.Join(layout.DataDir, "legacy-subdir"),
		layout.RuntimeDir,
	}
	for _, dir := range legacyDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	legacyFiles := map[string]os.FileMode{
		layout.SpecPath("spec"):                                  0o644,
		layout.AuthPath():                                        0o644,
		layout.AppliedStatePath():                                0o664,
		filepath.Join(layout.LastGoodDir(), "airport.json"):      0o644,
		filepath.Join(layout.SnapshotsDir(), "snap-1.json"):      0o644,
		filepath.Join(layout.BackupsDir(), "backup-1.tar.gz"):    0o644,
		filepath.Join(layout.DataDir, "legacy-subdir", "x.json"): 0o666,
		filepath.Join(layout.DataDir, "hook.sh"):                 0o755,
		layout.RuntimePath("runtime"):                            0o644,
	}
	for path, mode := range legacyFiles {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}

	// A symlink pointing outside the tree must not have its target touched.
	outside := filepath.Join(root, "outside.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(layout.DataDir, "link.json")); err != nil {
		t.Fatal(err)
	}

	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	for _, dir := range legacyDirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("dir %s mode = %o, want 700", dir, info.Mode().Perm())
		}
	}
	for path, before := range legacyFiles {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		perm := info.Mode().Perm()
		if perm&0o077 != 0 {
			t.Fatalf("file %s mode = %o (was %o): group/other bits remain", path, perm, before)
		}
		if perm&0o600 != 0o600 {
			t.Fatalf("file %s mode = %o: owner must keep read/write", path, perm)
		}
		if before&0o100 != 0 && perm&0o100 == 0 {
			t.Fatalf("file %s lost its owner exec bit: %o", path, perm)
		}
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target outside the tree was modified: %o", info.Mode().Perm())
	}

	// Second run must be a no-op that still succeeds.
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure (second run): %v", err)
	}
}

func TestLayoutEnsureRejectsEmptyDirectories(t *testing.T) {
	if err := (Layout{DataDir: "", RuntimeDir: t.TempDir()}).Ensure(); err == nil {
		t.Fatal("expected error for empty DataDir")
	}
	if err := (Layout{DataDir: t.TempDir(), RuntimeDir: ""}).Ensure(); err == nil {
		t.Fatal("expected error for empty RuntimeDir")
	}
}
