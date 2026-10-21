package state

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Layout struct {
	DataDir    string
	RuntimeDir string
}

const (
	stateDirMode  fs.FileMode = 0o700
	stateFileMode fs.FileMode = 0o600
)

// Ensure creates the state tree and enforces its permissions. Directories are
// forced to 0700 and every regular file already present is forced to 0600, so
// state inherited from an older installation (which wrote 0644) is tightened
// on the first start of a newer daemon rather than only when a file happens to
// be rewritten. Symlinks are never followed.
func (l Layout) Ensure() error {
	for _, dir := range []string{
		l.DataDir,
		l.RuntimeDir,
		l.SnapshotsDir(),
		l.BackupsDir(),
		l.LastGoodDir(),
		l.TransactionsDir(),
		l.RuleSetContentDir(),
		l.RuleSetMatcherDir(),
		l.RuleSetSourcesDir(),
		l.RuleSetNFTDir(),
		l.RuleSetRuntimeDir(),
	} {
		if dir == "" {
			return fmt.Errorf("empty state directory")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(dir, stateDirMode); err != nil {
			return err
		}
	}
	for _, root := range []string{l.DataDir, l.RuntimeDir} {
		if err := tightenStateTree(root); err != nil {
			return err
		}
	}
	return nil
}

// tightenStateTree removes group/other permission bits from every directory
// and regular file under root. It is idempotent and only calls chmod when a
// mode actually needs to change, so repeated starts are cheap.
func tightenStateTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if mode.Perm() != stateDirMode {
				return os.Chmod(path, stateDirMode)
			}
		case mode.IsRegular():
			perm := mode.Perm()
			if perm&0o077 != 0 || perm&stateFileMode != stateFileMode {
				// Keep an owner exec bit if one exists; only owner may read/write.
				return os.Chmod(path, (perm&0o700)|stateFileMode)
			}
		}
		return nil
	})
}

func (l Layout) SpecPath(name string) string {
	return filepath.Join(l.DataDir, name+".json")
}

func (l Layout) RuntimePath(name string) string {
	return filepath.Join(l.RuntimeDir, name+".json")
}

func (l Layout) RescueStatusPath() string {
	return filepath.Join(l.DataDir, "smartgate-rescue.status")
}

func (l Layout) AuthPath() string {
	return filepath.Join(l.DataDir, "auth.json")
}

func (l Layout) AppliedStatePath() string {
	return filepath.Join(l.DataDir, "applied-state.json")
}

func (l Layout) SelectionStatePath() string {
	return filepath.Join(l.DataDir, "selection-state.json")
}

func (l Layout) TransactionsDir() string {
	return filepath.Join(l.DataDir, "transactions")
}

func (l Layout) FailoverEventsPath() string {
	return filepath.Join(l.DataDir, "failover_events.json")
}

func (l Layout) FailoverStatePath() string {
	return filepath.Join(l.DataDir, "failover_state.json")
}

func (l Layout) DryRunConfigPath() string {
	return filepath.Join(l.RuntimeDir, "candidate.json")
}

func (l Layout) SnapshotsDir() string {
	return filepath.Join(l.DataDir, "snapshots")
}

func (l Layout) BackupsDir() string {
	return filepath.Join(l.DataDir, "backups")
}

func (l Layout) CutoverDrainsPath() string {
	return filepath.Join(l.DataDir, "cutover-drains.json")
}

func (l Layout) LastGoodDir() string {
	return filepath.Join(l.DataDir, "lastgood")
}

func (l Layout) RuleSetCacheDir() string {
	return filepath.Join(l.DataDir, "rule-sets")
}

func (l Layout) RuleSetContentDir() string {
	return filepath.Join(l.RuleSetCacheDir(), "content")
}

func (l Layout) RuleSetMatcherDir() string {
	return filepath.Join(l.RuleSetCacheDir(), "matchers")
}

func (l Layout) RuleSetSourcesDir() string {
	return filepath.Join(l.RuleSetCacheDir(), "sources")
}

func (l Layout) RuleSetNFTDir() string {
	return filepath.Join(l.RuleSetCacheDir(), "nft")
}

func (l Layout) RuleSetRuntimeDir() string {
	return filepath.Join(l.RuntimeDir, "rule-sets")
}
