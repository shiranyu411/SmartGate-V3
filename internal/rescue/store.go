package rescue

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"smartgate/internal/netgate"
)

type Store struct {
	path string
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Load() (State, error) {
	var state State
	b, err := os.ReadFile(s.path)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(b, &state)
	return state, err
}

func (s *Store) Save(reason string, cleanupPlan netgate.CleanupPlan, cleanupResult ...netgate.CleanupResult) error {
	var result netgate.CleanupResult
	if len(cleanupResult) > 0 {
		result = cleanupResult[0]
	}
	return s.SaveState(State{
		Active:          true,
		Reason:          reason,
		EnteredAt:       time.Now(),
		CleanupPlan:     cleanupPlan,
		Cleanup:         result,
		CleanupComplete: len(cleanupResult) > 0,
	})
}

func (s *Store) SaveState(value State) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	removeTmp := true
	defer func() {
		_ = file.Close()
		if removeTmp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := file.Write(b); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return err
	}
	removeTmp = false
	return syncDirectory(filepath.Dir(s.path))
}

func (s *Store) Clear() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(s.path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) Active() (bool, error) {
	_, err := os.Stat(s.path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
