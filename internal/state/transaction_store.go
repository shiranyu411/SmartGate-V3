package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"smartgate/internal/opid"
)

const TransactionJournalSchemaVersion = 2
const MaxTransactionJournalBytes = 32 << 20

type TransactionPhase string

const (
	TransactionPrepared         TransactionPhase = "prepared"
	TransactionApplying         TransactionPhase = "applying"
	TransactionCommitted        TransactionPhase = "committed"
	TransactionRecoveryRequired TransactionPhase = "recovery_required"
)

func ValidTransactionPhase(phase TransactionPhase) bool {
	switch phase {
	case TransactionPrepared, TransactionApplying, TransactionCommitted, TransactionRecoveryRequired:
		return true
	default:
		return false
	}
}

type TransactionState struct {
	Spec              SpecState          `json:"spec"`
	LastGoodCaptured  bool               `json:"last_good_captured,omitempty"`
	LastGood          []LastGoodSnapshot `json:"last_good,omitempty"`
	Applied           AppliedState       `json:"applied"`
	Runtime           RuntimeState       `json:"runtime"`
	SelectionCaptured bool               `json:"selection_captured,omitempty"`
	Selection         SelectionState     `json:"selection,omitempty"`
	FailoverCaptured  bool               `json:"failover_captured,omitempty"`
	Failover          FailoverState      `json:"failover,omitempty"`
}

type TransactionJournal struct {
	SchemaVersion int              `json:"schema_version"`
	OpID          string           `json:"op_id"`
	Kind          string           `json:"kind"`
	Phase         TransactionPhase `json:"phase"`
	Source        TransactionState `json:"source"`
	Target        TransactionState `json:"target"`
	SourceHash    string           `json:"source_hash,omitempty"`
	TargetHash    string           `json:"target_hash,omitempty"`
	StartedAt     time.Time        `json:"started_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	LastError     string           `json:"last_error,omitempty"`
	PoolID        string           `json:"pool_id,omitempty"`
	Selector      string           `json:"selector,omitempty"`
	CutoverPolicy string           `json:"cutover_policy,omitempty"`
	SourceBinding *PoolBinding     `json:"source_binding,omitempty"`
	TargetBinding *PoolBinding     `json:"target_binding,omitempty"`
	DrainDeadline *time.Time       `json:"drain_deadline,omitempty"`
}

type TransactionStore struct {
	layout Layout
}

func NewTransactionStore(layout Layout) *TransactionStore {
	return &TransactionStore{layout: layout}
}

func (s *TransactionStore) Save(journal TransactionJournal) error {
	path, err := s.path(journal.OpID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(journal.Kind) == "" {
		return fmt.Errorf("transaction kind is required")
	}
	if !ValidTransactionPhase(journal.Phase) {
		return fmt.Errorf("invalid transaction phase %q", journal.Phase)
	}
	if journal.SchemaVersion == 0 {
		journal.SchemaVersion = TransactionJournalSchemaVersion
	}
	now := time.Now().UTC()
	if journal.StartedAt.IsZero() {
		journal.StartedAt = now
	}
	journal.UpdatedAt = now
	if err := os.MkdirAll(s.layout.TransactionsDir(), 0o755); err != nil {
		return err
	}
	return saveJSONDurableBounded(path, journal, 0o600, MaxTransactionJournalBytes)
}

func (s *TransactionStore) Load(opID string) (TransactionJournal, error) {
	var journal TransactionJournal
	path, err := s.path(opID)
	if err != nil {
		return journal, err
	}
	if err := loadJSONBounded(path, &journal, MaxTransactionJournalBytes); err != nil {
		return journal, err
	}
	if !ValidTransactionPhase(journal.Phase) {
		return TransactionJournal{}, fmt.Errorf("invalid transaction phase %q", journal.Phase)
	}
	return journal, nil
}

func (s *TransactionStore) List() ([]TransactionJournal, error) {
	entries, err := os.ReadDir(s.layout.TransactionsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]TransactionJournal, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		opID := strings.TrimSuffix(entry.Name(), ".json")
		journal, err := s.Load(opID)
		if err != nil {
			return nil, err
		}
		out = append(out, journal)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].OpID < out[j].OpID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, nil
}

func (s *TransactionStore) Remove(opID string) error {
	path, err := s.path(opID)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (s *TransactionStore) path(rawOpID string) (string, error) {
	safeOpID, err := opid.Clean(rawOpID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.layout.TransactionsDir(), safeOpID+".json"), nil
}

func saveJSONDurable(path string, value any, perm os.FileMode) error {
	return saveJSONDurableBounded(path, value, perm, MaxControlStateJSONBytes)
}

func saveJSONDurableBounded(path string, value any, perm os.FileMode, maxBytes int64) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(body)) > maxBytes {
		return &JSONResourceLimitError{Path: path, Limit: maxBytes}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	removeTmp := true
	defer func() {
		_ = file.Close()
		if removeTmp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := os.Chmod(path, perm); err != nil {
		return err
	}
	removeTmp = false
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
