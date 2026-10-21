package commitlane

import (
	"context"
	"time"

	"smartgate/internal/business"
	"smartgate/internal/lockset"
	"smartgate/internal/opid"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type BusinessPoolTxn struct {
	commit    *Manager
	store     *state.Store
	snapshots *state.SnapshotStore
}

type BusinessPoolTxnRequest struct {
	OpID    string
	Changes []business.Change
}

type BusinessPoolTxnResult struct {
	Plan              business.Plan `json:"plan"`
	BusinessPoolCount int           `json:"business_pool_count"`
	ConfigEpoch       int64         `json:"config_epoch"`
	TopologyHash      string        `json:"topology_hash,omitempty"`
	Dirty             bool          `json:"dirty"`
	// Kept for in-process transaction consumers; intentionally excluded from
	// HTTP serialization because Spec/Runtime can contain sensitive state.
	Spec    state.SpecState    `json:"-"`
	Runtime state.RuntimeState `json:"-"`
}

func NewBusinessPoolTxn(commit *Manager, store *state.Store, snapshots *state.SnapshotStore) *BusinessPoolTxn {
	return &BusinessPoolTxn{commit: commit, store: store, snapshots: snapshots}
}

func (t *BusinessPoolTxn) DryRun(ctx context.Context, req BusinessPoolTxnRequest) (*BusinessPoolTxnResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := opid.Validate(req.OpID); err != nil {
		return nil, err
	}
	session := t.commit.locks.Session(req.OpID + "-business-pool-dryrun")
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
	nextSpec, err := business.ApplyChanges(spec, req.Changes)
	if err != nil {
		return nil, err
	}
	plan, err := business.Diff(spec, nextSpec)
	if err != nil {
		return nil, err
	}
	return businessPoolTxnResult(plan, nextSpec, runtimeState), nil
}

func (t *BusinessPoolTxn) Apply(ctx context.Context, req BusinessPoolTxnRequest) (*BusinessPoolTxnResult, error) {
	safeOpID, err := opid.Clean(req.OpID)
	if err != nil {
		return nil, err
	}
	req.OpID = safeOpID
	var result *BusinessPoolTxnResult
	_, err = t.commit.WithCommit(ctx, CommitRequest{
		OpID:      req.OpID,
		Kind:      "business_pool_apply",
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
		nextSpec, err := business.ApplyChanges(spec, req.Changes)
		if err != nil {
			return err
		}
		plan, err := business.Diff(spec, nextSpec)
		if err != nil {
			return err
		}
		if !plan.HasChanges {
			result = businessPoolTxnResult(plan, spec, runtimeState)
			return nil
		}
		lastGoodPreimage, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(t.commit.Layout()))
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
			Kind:       "business_pool_apply",
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
			OpID:         req.OpID,
			Kind:         "business_pool_apply",
			TopologyHash: runtimeState.TopologyHash,
			SavedAt:      time.Now(),
			Spec:         spec,
			Runtime:      runtimeState,
		}); err != nil {
			return err
		}

		runtimeState.CommitInFlight = &state.CommitRef{
			OpID:         req.OpID,
			StartedAt:    time.Now(),
			TopologyHash: plan.PoolsHash,
		}
		if err := t.store.SaveRuntime(runtimeState); err != nil {
			return err
		}
		runtimeState, err = t.cleanupStalePoolReferences(nextSpec, runtimeState)
		if err != nil {
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

		result = businessPoolTxnResult(plan, nextSpec, runtimeState)
		return nil
	})
	return result, err
}

func businessPoolTxnResult(plan business.Plan, spec state.SpecState, runtimeState state.RuntimeState) *BusinessPoolTxnResult {
	return &BusinessPoolTxnResult{
		Plan:              plan,
		BusinessPoolCount: len(spec.BusinessPools),
		ConfigEpoch:       runtimeState.ConfigEpoch,
		TopologyHash:      runtimeState.TopologyHash,
		Dirty:             plan.HasChanges,
		Spec:              spec,
		Runtime:           runtimeState,
	}
}

func (t *BusinessPoolTxn) cleanupStalePoolReferences(spec state.SpecState, runtimeState state.RuntimeState) (state.RuntimeState, error) {
	// A business-pool save changes only the saved spec. The current runtime and
	// failover selections remain authoritative for the applied topology until a
	// ConfigTxn successfully reconciles the new pool set.
	return runtimeState, nil
}
