package commitlane

import (
	"context"
	"strings"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/opid"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type ProviderTxn struct {
	commit    *Manager
	store     *state.Store
	snapshots *state.SnapshotStore
	lastGood  *provider.LastGoodStore
}

type ProviderTxnRequest struct {
	OpID    string
	Changes []provider.Change
}

type ProviderTxnResult struct {
	Plan          provider.Plan `json:"plan"`
	ProviderCount int           `json:"provider_count"`
	ConfigEpoch   int64         `json:"config_epoch"`
	TopologyHash  string        `json:"topology_hash,omitempty"`
	Dirty         bool          `json:"dirty"`
	// Spec and Runtime remain available to in-process callers, but are never
	// serialized by the management API because they may contain credentials.
	Spec    state.SpecState    `json:"-"`
	Runtime state.RuntimeState `json:"-"`
}

func NewProviderTxn(commit *Manager, store *state.Store, snapshots *state.SnapshotStore) *ProviderTxn {
	return &ProviderTxn{commit: commit, store: store, snapshots: snapshots}
}

func (t *ProviderTxn) WithLastGoodStore(store *provider.LastGoodStore) *ProviderTxn {
	t.lastGood = store
	return t
}

func (t *ProviderTxn) DryRun(ctx context.Context, req ProviderTxnRequest) (*ProviderTxnResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := opid.Validate(req.OpID); err != nil {
		return nil, err
	}
	session := t.commit.locks.Session(req.OpID + "-dryrun")
	releaseStore, err := session.Acquire(lockset.LevelStore)
	if err != nil {
		return nil, err
	}
	releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
	if err != nil {
		releaseStore()
		return nil, err
	}
	spec, err := t.store.LoadSpec()
	if err != nil {
		releaseRuntime()
		releaseStore()
		return nil, err
	}
	runtimeState, err := t.store.LoadRuntime()
	releaseRuntime()
	releaseStore()
	if err != nil {
		return nil, err
	}
	nextSpec, err := applyProviderChanges(spec, req.Changes)
	if err != nil {
		return nil, err
	}
	plan, err := provider.Diff(spec, nextSpec)
	if err != nil {
		return nil, err
	}
	return providerTxnResult(plan, nextSpec, runtimeState), nil
}

func (t *ProviderTxn) Apply(ctx context.Context, req ProviderTxnRequest) (*ProviderTxnResult, error) {
	safeOpID, err := opid.Clean(req.OpID)
	if err != nil {
		return nil, err
	}
	req.OpID = safeOpID
	var result *ProviderTxnResult
	_, err = t.commit.WithCommit(ctx, CommitRequest{
		OpID:      req.OpID,
		Kind:      "provider_apply",
		StartedAt: time.Now(),
	}, func(session *lockset.Session) error {
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		defer releaseStore()

		releaseSnapshot, err := session.Acquire(lockset.LevelSnapshot)
		if err != nil {
			return err
		}
		defer releaseSnapshot()

		releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
		if err != nil {
			return err
		}
		defer releaseRuntime()

		spec, err := t.store.LoadSpec()
		if err != nil {
			return err
		}
		runtimeState, err := t.store.LoadRuntime()
		if err != nil {
			return err
		}
		nextSpec, err := applyProviderChanges(spec, req.Changes)
		if err != nil {
			return err
		}
		plan, err := provider.Diff(spec, nextSpec)
		if err != nil {
			return err
		}
		if !plan.HasChanges {
			result = providerTxnResult(plan, spec, runtimeState)
			return nil
		}

		lastGoodPreimage, err := provider.LoadLastGoodSnapshotsBounded(ctx, t.effectiveLastGoodStore())
		if err != nil {
			return err
		}
		sourceHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, lastGoodPreimage)
		if err != nil {
			return err
		}
		targetHash, err := provider.HashEffectiveTopologyFromSnapshots(nextSpec, lastGoodPreimage)
		if err != nil {
			return err
		}
		journalStore := state.NewTransactionStore(t.commit.Layout())
		journal := state.TransactionJournal{
			OpID:       req.OpID,
			Kind:       "provider_apply",
			Phase:      state.TransactionPrepared,
			SourceHash: sourceHash,
			TargetHash: targetHash,
			Source: state.TransactionState{
				Spec:             spec,
				LastGoodCaptured: true,
				LastGood:         lastGoodPreimage,
				Runtime:          runtimeState,
			},
			Target: state.TransactionState{
				Spec:             nextSpec,
				LastGoodCaptured: true,
				LastGood:         lastGoodPreimage,
			},
		}
		if err := journalStore.Save(journal); err != nil {
			return err
		}
		if err := t.snapshots.SavePreimage(state.PreimageEnvelope{
			OpID:             req.OpID,
			Kind:             "provider_apply",
			TopologyHash:     runtimeState.TopologyHash,
			SavedAt:          time.Now(),
			Spec:             spec,
			Runtime:          runtimeState,
			LastGoodCaptured: true,
			LastGood:         lastGoodPreimage,
		}); err != nil {
			return err
		}

		runtimeState.CommitInFlight = &state.CommitRef{
			OpID:         req.OpID,
			StartedAt:    time.Now(),
			TopologyHash: plan.TopologyHash,
		}
		if err := t.store.SaveRuntime(runtimeState); err != nil {
			return err
		}
		nextSpec, runtimeState, err = t.cleanupStaleReferences(ctx, nextSpec, runtimeState)
		if err != nil {
			return err
		}
		if err := t.pruneOrphanLastGood(ctx, nextSpec); err != nil {
			return err
		}
		if _, err := t.store.SaveSpec(nextSpec); err != nil {
			return err
		}

		runtimeState.CommitInFlight = nil
		if err := t.store.SaveRuntime(runtimeState); err != nil {
			return err
		}
		journal.Phase = state.TransactionCommitted
		journal.Target.Runtime = runtimeState
		if err := journalStore.Save(journal); err != nil {
			return err
		}
		if err := t.snapshots.RemovePreimage(req.OpID); err != nil {
			return err
		}
		if err := journalStore.Remove(req.OpID); err != nil {
			return err
		}

		result = providerTxnResult(plan, nextSpec, runtimeState)
		return nil
	})
	return result, err
}

func providerTxnResult(plan provider.Plan, spec state.SpecState, runtimeState state.RuntimeState) *ProviderTxnResult {
	return &ProviderTxnResult{
		Plan:          plan,
		ProviderCount: len(spec.Providers),
		ConfigEpoch:   runtimeState.ConfigEpoch,
		TopologyHash:  runtimeState.TopologyHash,
		Dirty:         plan.HasChanges,
		Spec:          spec,
		Runtime:       runtimeState,
	}
}

func applyProviderChanges(spec state.SpecState, changes []provider.Change) (state.SpecState, error) {
	return provider.ApplyChanges(spec, preserveWriteOnlyProviderSources(spec, changes))
}

func preserveWriteOnlyProviderSources(spec state.SpecState, changes []provider.Change) []provider.Change {
	current := make(map[string]state.ProviderSpec, len(spec.Providers))
	for _, item := range spec.Providers {
		current[item.ID] = item
	}
	out := append([]provider.Change(nil), changes...)
	for index := range out {
		change := &out[index]
		if change.Action != provider.ActionUpsert {
			continue
		}
		id := change.Provider.ID
		if id == "" {
			id = change.ID
		}
		previous, ok := current[id]
		if !ok || change.Provider.Type != previous.Type {
			continue
		}
		if strings.TrimSpace(change.Provider.URL) == "" {
			change.Provider.URL = previous.URL
		}
		if strings.TrimSpace(change.Provider.FilePath) == "" {
			change.Provider.FilePath = previous.FilePath
		}
	}
	return out
}

func (t *ProviderTxn) cleanupStaleReferences(ctx context.Context, spec state.SpecState, runtimeState state.RuntimeState) (state.SpecState, state.RuntimeState, error) {
	lastGood := t.effectiveLastGoodStore()
	if lastGood == nil {
		return spec, runtimeState, nil
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(ctx, lastGood)
	if err != nil {
		return spec, runtimeState, err
	}
	nextSpec, _, err := provider.ReconcileSmartNodes(spec, snapshots, time.Now().UTC())
	if err != nil {
		return spec, runtimeState, err
	}
	return nextSpec, runtimeState, nil
}

func (t *ProviderTxn) pruneOrphanLastGood(ctx context.Context, spec state.SpecState) error {
	lastGood := t.effectiveLastGoodStore()
	if lastGood == nil {
		return nil
	}
	keep := make(map[string]struct{}, len(spec.Providers))
	for _, item := range spec.Providers {
		keep[item.ID] = struct{}{}
	}
	_, err := lastGood.PruneExcept(ctx, keep)
	return err
}

func (t *ProviderTxn) effectiveLastGoodStore() *provider.LastGoodStore {
	if t.lastGood != nil {
		return t.lastGood
	}
	if t.commit != nil {
		return provider.NewLastGoodStore(t.commit.Layout())
	}
	return nil
}
