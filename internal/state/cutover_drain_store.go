package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	CutoverDrainStatusDraining = "draining"
	CutoverDrainStatusRetrying = "retrying"
	CutoverDrainStatusFailed   = "failed"
)

type CutoverDrainState struct {
	Pool          string            `json:"pool"`
	OldGeneration uint64            `json:"old_generation"`
	Remaining     int               `json:"remaining"`
	Deadline      time.Time         `json:"deadline"`
	Status        string            `json:"status"`
	LastError     string            `json:"last_error,omitempty"`
	RetryCount    int               `json:"retry_count"`
	Binding       PoolBinding       `json:"binding"`
	Dispatch      PoolDispatchGraph `json:"dispatch"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

type CutoverDrainStore struct {
	layout Layout
	mu     sync.Mutex
}

func NewCutoverDrainStore(layout Layout) *CutoverDrainStore {
	return &CutoverDrainStore{layout: layout}
}

func (s *CutoverDrainStore) List() ([]CutoverDrainState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *CutoverDrainStore) listLocked() ([]CutoverDrainState, error) {
	var drains []CutoverDrainState
	err := loadJSON(s.layout.CutoverDrainsPath(), &drains)
	if errors.Is(err, os.ErrNotExist) {
		return []CutoverDrainState{}, nil
	}
	if err != nil {
		return nil, err
	}
	for _, drain := range drains {
		if err := validateCutoverDrainState(drain); err != nil {
			return nil, err
		}
	}
	sort.Slice(drains, func(i, j int) bool { return drains[i].Pool < drains[j].Pool })
	return drains, nil
}

func (s *CutoverDrainStore) Save(drain CutoverDrainState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCutoverDrainState(drain); err != nil {
		return err
	}
	drains, err := s.listLocked()
	if err != nil {
		return err
	}
	replaced := false
	for index := range drains {
		if drains[index].Pool == drain.Pool {
			// UpdatedAt is store-owned. Keep it stable when the durable state is
			// unchanged so polling callers do not turn a no-op into an fsync.
			drain.UpdatedAt = drains[index].UpdatedAt
			if sameCutoverDrainState(drain, drains[index]) {
				_, err := chmodFileIfNeeded(s.layout.CutoverDrainsPath(), 0o600)
				return err
			}
			drain.UpdatedAt = time.Now().UTC()
			drains[index] = drain
			replaced = true
			break
		}
	}
	if !replaced {
		drain.UpdatedAt = time.Now().UTC()
		drains = append(drains, drain)
	}
	sort.Slice(drains, func(i, j int) bool { return drains[i].Pool < drains[j].Pool })
	_, err = saveJSONAtomicIfChanged(s.layout.CutoverDrainsPath(), drains, 0o600)
	return err
}

func sameCutoverDrainState(left, right CutoverDrainState) bool {
	left.UpdatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func (s *CutoverDrainStore) Remove(pool string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pool = strings.TrimSpace(pool)
	if pool == "" {
		return fmt.Errorf("cutover drain pool is required")
	}
	drains, err := s.listLocked()
	if err != nil {
		return err
	}
	filtered := drains[:0]
	for _, drain := range drains {
		if drain.Pool != pool {
			filtered = append(filtered, drain)
		}
	}
	if len(filtered) == len(drains) {
		return nil
	}
	if len(filtered) == 0 {
		if err := os.Remove(s.layout.CutoverDrainsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.Stat(s.layout.DataDir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return syncDir(s.layout.DataDir)
	}
	_, err = saveJSONAtomicIfChanged(s.layout.CutoverDrainsPath(), filtered, 0o600)
	return err
}

func validateCutoverDrainState(drain CutoverDrainState) error {
	if strings.TrimSpace(drain.Pool) == "" {
		return fmt.Errorf("cutover drain pool is required")
	}
	if drain.OldGeneration == 0 || drain.Binding.Generation != drain.OldGeneration || drain.Binding.Pool != drain.Pool {
		return fmt.Errorf("cutover drain binding metadata is invalid for pool %s", drain.Pool)
	}
	if drain.Dispatch.Pool != drain.Pool || drain.Dispatch.Generation != drain.OldGeneration {
		return fmt.Errorf("cutover drain dispatch metadata is invalid for pool %s", drain.Pool)
	}
	if drain.Deadline.IsZero() {
		return fmt.Errorf("cutover drain deadline is required for pool %s", drain.Pool)
	}
	switch drain.Status {
	case CutoverDrainStatusDraining, CutoverDrainStatusRetrying, CutoverDrainStatusFailed:
	default:
		return fmt.Errorf("invalid cutover drain status %q", drain.Status)
	}
	return nil
}
