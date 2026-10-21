package commitlane

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type LastGoodTxn struct {
	commit     *Manager
	store      *provider.LastGoodStore
	stateStore *state.Store
	seq        atomic.Uint64
}

func NewLastGoodTxn(commit *Manager, store *provider.LastGoodStore) *LastGoodTxn {
	return &LastGoodTxn{commit: commit, store: store}
}

func (t *LastGoodTxn) WithStateStore(store *state.Store) *LastGoodTxn {
	t.stateStore = store
	return t
}

func (t *LastGoodTxn) Exists(providerID string) bool {
	return t.store.Exists(providerID)
}

func (t *LastGoodTxn) SaveMany(ctx context.Context, updates []provider.LastGoodUpdate) (map[string]provider.LastGoodEnvelope, error) {
	if len(updates) == 0 {
		return map[string]provider.LastGoodEnvelope{}, nil
	}
	opID := fmt.Sprintf("provider-refresh-%d-%d", time.Now().UnixNano(), t.seq.Add(1))
	var saved map[string]provider.LastGoodEnvelope
	_, err := t.commit.WithCommit(ctx, CommitRequest{
		OpID:      opID,
		Kind:      "provider_refresh",
		StartedAt: time.Now(),
	}, func(session *lockset.Session) error {
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		defer releaseStore()

		var specBefore state.SpecState
		var snapshotsBefore []state.LastGoodSnapshot
		if t.stateStore != nil {
			specBefore, err = t.stateStore.LoadSpec()
			if err != nil {
				return err
			}
			snapshotsBefore, err = provider.LoadLastGoodSnapshotsBounded(ctx, t.store)
			if err != nil {
				return err
			}
		}

		saved, err = t.store.SaveMany(ctx, updates)
		if err != nil {
			return err
		}
		if t.stateStore == nil {
			return nil
		}
		metadataOnly, err := classifyMetadataOnlyRefreshes(specBefore, snapshotsBefore, updates)
		if err != nil {
			return err
		}
		for providerID := range metadataOnly {
			envelope := saved[providerID]
			envelope.MetadataOnly = true
			saved[providerID] = envelope
		}
		return t.cleanupSavedNodeReferences(ctx)
	})
	return saved, err
}

func classifyMetadataOnlyRefreshes(spec state.SpecState, snapshots []state.LastGoodSnapshot, updates []provider.LastGoodUpdate) (map[string]bool, error) {
	metadataOnly := map[string]bool{}
	for _, update := range updates {
		candidate := replaceLastGoodSnapshot(snapshots, update)
		_, report, err := provider.ReconcileSmartNodes(spec, candidate, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if len(report.MetadataOnly) > 0 && len(report.Added) == 0 && len(report.Revised) == 0 && len(report.Removed) == 0 {
			metadataOnly[update.ProviderID] = true
		}
	}
	return metadataOnly, nil
}

func replaceLastGoodSnapshot(snapshots []state.LastGoodSnapshot, update provider.LastGoodUpdate) []state.LastGoodSnapshot {
	next := append([]state.LastGoodSnapshot(nil), snapshots...)
	replacement := state.LastGoodSnapshot{
		ProviderID:   update.ProviderID,
		SavedAt:      time.Now().UTC(),
		Subscription: update.Subscription,
		Identities:   update.Identities,
	}
	replaced := false
	for index := range next {
		if next[index].ProviderID == update.ProviderID {
			next[index] = replacement
			replaced = true
			break
		}
	}
	if !replaced {
		next = append(next, replacement)
	}
	sort.Slice(next, func(left, right int) bool { return next[left].ProviderID < next[right].ProviderID })
	return next
}

func (t *LastGoodTxn) cleanupSavedNodeReferences(ctx context.Context) error {
	spec, err := t.stateStore.LoadSpec()
	if err != nil {
		return err
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(ctx, t.store)
	if err != nil {
		return err
	}
	nextSpec, report, err := provider.ReconcileSmartNodes(spec, snapshots, time.Now().UTC())
	if err != nil {
		return err
	}
	if len(report.Added) > 0 || len(report.Revised) > 0 || len(report.MetadataOnly) > 0 || len(report.Removed) > 0 {
		if _, err := t.stateStore.SaveSpec(nextSpec); err != nil {
			return err
		}
	}
	return nil
}
