package runtime

import (
	"fmt"
	"os"

	"smartgate/internal/state"
)

func Preflight(layout state.Layout, listenAddr string) error {
	if err := ValidateLANOnly(listenAddr); err != nil {
		return err
	}
	for _, dir := range []string{layout.DataDir, layout.RuntimeDir} {
		if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o222 == 0 {
			return fmt.Errorf("directory %s is not writable", dir)
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("directory %s is not accessible: %w", dir, err)
		}
	}
	if err := layout.Ensure(); err != nil {
		return err
	}
	for _, dir := range []string{layout.DataDir, layout.RuntimeDir} {
		if err := ensureWritable(dir); err != nil {
			return err
		}
	}
	return nil
}

func ensureWritable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("directory %s is not accessible: %w", dir, err)
	}
	if info.Mode().Perm()&0o222 == 0 {
		return fmt.Errorf("directory %s is not writable", dir)
	}
	f, err := os.CreateTemp(dir, ".smartgate-preflight-*")
	if err != nil {
		return fmt.Errorf("directory %s is not writable: %w", dir, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}
