package state

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"smartgate/internal/opid"
	"smartgate/internal/smartnode"
)

const (
	SnapshotMaxFileBytes     = 4 << 20
	SnapshotMaxLastGoodBytes = 1 << 20
	SnapshotMaxLastGoodNodes = 4096
)

type SnapshotResourceLimitError struct{ Reason string }

func (e *SnapshotResourceLimitError) Error() string {
	return "snapshot resource limit exceeded: " + e.Reason
}

type SnapshotStore struct {
	layout Layout
}

type PreimageEnvelope struct {
	OpID             string             `json:"op_id"`
	Kind             string             `json:"kind"`
	TopologyHash     string             `json:"topology_hash,omitempty"`
	SavedAt          time.Time          `json:"saved_at"`
	Spec             SpecState          `json:"spec"`
	Runtime          RuntimeState       `json:"runtime"`
	LastGoodCaptured bool               `json:"last_good_captured,omitempty"`
	LastGood         []LastGoodSnapshot `json:"last_good,omitempty"`
}

type SnapshotEnvelope struct {
	ID                string             `json:"id"`
	Kind              string             `json:"kind"`
	Description       string             `json:"description,omitempty"`
	TopologyHash      string             `json:"topology_hash,omitempty"`
	SavedAt           time.Time          `json:"saved_at"`
	Spec              SpecState          `json:"spec"`
	Runtime           RuntimeState       `json:"runtime"`
	AppliedCaptured   bool               `json:"applied_captured,omitempty"`
	Applied           AppliedState       `json:"applied,omitempty"`
	SelectionCaptured bool               `json:"selection_captured,omitempty"`
	Selection         SelectionState     `json:"selection,omitempty"`
	LastGoodCaptured  bool               `json:"last_good_captured,omitempty"`
	LastGood          []LastGoodSnapshot `json:"last_good,omitempty"`
}

type SnapshotSummary struct {
	ID                string    `json:"id"`
	Kind              string    `json:"kind"`
	Description       string    `json:"description,omitempty"`
	TopologyHash      string    `json:"topology_hash,omitempty"`
	SavedAt           time.Time `json:"saved_at"`
	LastGoodCaptured  bool      `json:"last_good_captured"`
	ProviderCount     int       `json:"provider_count"`
	BusinessPoolCount int       `json:"business_pool_count"`
	LastGoodCount     int       `json:"last_good_count"`
}

func SummarizeSnapshot(env SnapshotEnvelope) SnapshotSummary {
	return SnapshotSummary{
		ID:                env.ID,
		Kind:              env.Kind,
		Description:       env.Description,
		TopologyHash:      env.TopologyHash,
		SavedAt:           env.SavedAt,
		LastGoodCaptured:  env.LastGoodCaptured,
		ProviderCount:     len(env.Spec.Providers),
		BusinessPoolCount: len(env.Spec.BusinessPools),
		LastGoodCount:     len(env.LastGood),
	}
}

type LastGoodSnapshot struct {
	ProviderID   string                     `json:"provider_id"`
	SavedAt      time.Time                  `json:"saved_at,omitempty"`
	Subscription *SubscriptionInfo          `json:"subscription,omitempty"`
	Identities   []smartnode.IdentitySource `json:"identities,omitempty"`
	Outbounds    []map[string]any           `json:"outbounds,omitempty"`
}

func NewSnapshotStore(layout Layout) *SnapshotStore {
	return &SnapshotStore{layout: layout}
}

func (s *SnapshotStore) SavePreimage(env PreimageEnvelope) error {
	path, err := s.preimagePath(env.OpID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("preimage already exists for op id %q", env.OpID)
	} else if !os.IsNotExist(err) {
		return err
	}
	return saveSnapshotJSON(path, env, env.LastGood)
}

func (s *SnapshotStore) LoadPreimage(opID string) (PreimageEnvelope, error) {
	var env PreimageEnvelope
	path, err := s.preimagePath(opID)
	if err != nil {
		return env, err
	}
	err = loadSnapshotJSON(path, &env)
	if err == nil {
		err = validateSnapshotValue(env, env.LastGood)
	}
	return env, err
}

func (s *SnapshotStore) RemovePreimage(opID string) error {
	path, err := s.preimagePath(opID)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *SnapshotStore) SaveSnapshot(env SnapshotEnvelope) error {
	path, err := s.snapshotPath(env.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("snapshot already exists for id %q", env.ID)
	} else if !os.IsNotExist(err) {
		return err
	}
	return saveSnapshotJSON(path, env, env.LastGood)
}

func (s *SnapshotStore) LoadSnapshot(id string) (SnapshotEnvelope, error) {
	var env SnapshotEnvelope
	path, err := s.snapshotPath(id)
	if err != nil {
		return env, err
	}
	err = loadSnapshotJSON(path, &env)
	if err == nil {
		err = validateSnapshotValue(env, env.LastGood)
	}
	return env, err
}

func saveSnapshotJSON(path string, value any, lastGood []LastGoodSnapshot) error {
	if err := validateSnapshotValue(value, lastGood); err != nil {
		return err
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(body) > SnapshotMaxFileBytes {
		return &SnapshotResourceLimitError{Reason: fmt.Sprintf("encoded file is %d bytes", len(body))}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func loadSnapshotJSON(path string, target any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > SnapshotMaxFileBytes {
		return &SnapshotResourceLimitError{Reason: fmt.Sprintf("%s is %d bytes", filepath.Base(path), info.Size())}
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, SnapshotMaxFileBytes+1))
	if err != nil {
		return err
	}
	if len(body) > SnapshotMaxFileBytes {
		return &SnapshotResourceLimitError{Reason: fmt.Sprintf("%s grew beyond %d bytes", filepath.Base(path), SnapshotMaxFileBytes)}
	}
	return json.Unmarshal(body, target)
}

func validateSnapshotValue(value any, lastGood []LastGoodSnapshot) error {
	nodes := 0
	for _, snapshot := range lastGood {
		count := len(snapshot.Outbounds)
		if len(snapshot.Identities) > count {
			count = len(snapshot.Identities)
		}
		nodes += count
		if nodes > SnapshotMaxLastGoodNodes {
			return &SnapshotResourceLimitError{Reason: fmt.Sprintf("last-good nodes exceed %d", SnapshotMaxLastGoodNodes)}
		}
	}
	total := 0
	var walk func(reflect.Value) bool
	walk = func(v reflect.Value) bool {
		if !v.IsValid() {
			return true
		}
		if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return true
			}
			return walk(v.Elem())
		}
		switch v.Kind() {
		case reflect.String:
			total += v.Len()
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				if !walk(iter.Key()) || !walk(iter.Value()) {
					return false
				}
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				if !walk(v.Index(i)) {
					return false
				}
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if !walk(v.Field(i)) {
					return false
				}
			}
		default:
			total += 16
		}
		return total <= SnapshotMaxFileBytes
	}
	if !walk(reflect.ValueOf(value)) {
		return &SnapshotResourceLimitError{Reason: fmt.Sprintf("aggregate scalar data exceeds %d bytes", SnapshotMaxFileBytes)}
	}
	lastGoodBytes := 0
	for _, snapshot := range lastGood {
		if !walkLastGoodScalars(reflect.ValueOf(snapshot.Identities), &lastGoodBytes) ||
			!walkLastGoodScalars(reflect.ValueOf(snapshot.Outbounds), &lastGoodBytes) {
			return &SnapshotResourceLimitError{Reason: fmt.Sprintf("last-good scalar data exceeds %d bytes", SnapshotMaxLastGoodBytes)}
		}
	}
	return nil
}

func walkLastGoodScalars(v reflect.Value, total *int) bool {
	if !v.IsValid() {
		return true
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return true
		}
		return walkLastGoodScalars(v.Elem(), total)
	}
	switch v.Kind() {
	case reflect.String:
		*total += v.Len()
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if !walkLastGoodScalars(iter.Key(), total) || !walkLastGoodScalars(iter.Value(), total) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if !walkLastGoodScalars(v.Index(i), total) {
				return false
			}
		}
	default:
		*total += 16
	}
	return *total <= SnapshotMaxLastGoodBytes
}

func (s *SnapshotStore) RemoveSnapshot(id string) error {
	path, err := s.snapshotPath(id)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *SnapshotStore) ListSnapshots(ctx context.Context) ([]SnapshotSummary, error) {
	entries, err := os.ReadDir(s.layout.SnapshotsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SnapshotSummary
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "snapshot-") || filepath.Ext(name) != ".json" {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "snapshot-"), ".json")
		env, err := s.LoadSnapshot(id)
		if err != nil {
			return nil, err
		}
		out = append(out, SummarizeSnapshot(env))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SavedAt.After(out[j].SavedAt)
	})
	return out, nil
}

func (s *SnapshotStore) preimagePath(rawOpID string) (string, error) {
	safeOpID, err := opid.Clean(rawOpID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.layout.SnapshotsDir(), "pending-"+safeOpID+".json"), nil
}

func (s *SnapshotStore) snapshotPath(rawID string) (string, error) {
	safeID, err := opid.Clean(rawID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.layout.SnapshotsDir(), "snapshot-"+safeID+".json"), nil
}
