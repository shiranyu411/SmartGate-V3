package commitlane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"smartgate/internal/configapply"
	"smartgate/internal/lockset"
	"smartgate/internal/opid"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type SnapshotTxn struct {
	commit    *Manager
	store     *state.Store
	snapshots *state.SnapshotStore
	configTxn *ConfigTxn
}

type SnapshotCreateRequest struct {
	ID          string
	Kind        string
	Description string
}

type SnapshotRollbackDryRunRequest struct {
	DryRunID   string
	SnapshotID string
}

type SnapshotRollbackRequest struct {
	OpID                       string
	SnapshotID                 string
	ExpectedConfigEpoch        int64
	ExpectedSourceTopologyHash string
	ExpectedTargetTopologyHash string
	ExpectedSnapshotTargetHash string
	ExpectedSourceSelection    map[string]string
	ExpectedSourceSelections   map[string]state.PoolSelection
	RequireDryRunMatch         bool
	Prepared                   configapply.Prepared
}

type SnapshotRollbackPrepared struct {
	Plan     SnapshotRollbackPlan `json:"plan"`
	Prepared configapply.Prepared `json:"prepared"`
}

type SnapshotRollbackPlan struct {
	DryRunID             string                     `json:"dryrun_id"`
	SnapshotID           string                     `json:"snapshot_id"`
	SnapshotKind         string                     `json:"snapshot_kind"`
	SnapshotSavedAt      time.Time                  `json:"snapshot_saved_at"`
	CurrentTopologyHash  string                     `json:"current_topology_hash,omitempty"`
	TargetTopologyHash   string                     `json:"target_topology_hash,omitempty"`
	SnapshotTargetHash   string                     `json:"snapshot_target_hash,omitempty"`
	CurrentConfigEpoch   int64                      `json:"current_config_epoch"`
	CurrentProviderCount int                        `json:"current_provider_count"`
	TargetProviderCount  int                        `json:"target_provider_count"`
	CurrentPoolCount     int                        `json:"current_pool_count"`
	TargetPoolCount      int                        `json:"target_pool_count"`
	CurrentLastGoodCount int                        `json:"current_last_good_count"`
	TargetLastGoodCount  int                        `json:"target_last_good_count"`
	SpecChanged          bool                       `json:"spec_changed"`
	LastGoodCaptured     bool                       `json:"last_good_captured"`
	CurrentApplied       state.AppliedState         `json:"current_applied"`
	CurrentSpec          state.SpecState            `json:"current_spec"`
	CurrentRuntime       state.RuntimeState         `json:"current_runtime"`
	CurrentSelection     state.SelectionState       `json:"current_selection"`
	CurrentLastGood      []state.LastGoodSnapshot   `json:"current_last_good,omitempty"`
	TargetSpec           state.SpecState            `json:"target_spec"`
	TargetRuntime        state.RuntimeState         `json:"target_runtime"`
	TargetSelection      state.SelectionState       `json:"target_selection"`
	TargetLastGood       []state.LastGoodSnapshot   `json:"target_last_good,omitempty"`
	TargetRuleSets       []state.RuleSetSnapshotRef `json:"target_rule_sets,omitempty"`
}

type SnapshotRollbackResult struct {
	Plan    SnapshotRollbackPlan `json:"plan"`
	Spec    state.SpecState      `json:"spec"`
	Runtime state.RuntimeState   `json:"runtime"`
}

func NewSnapshotTxn(commit *Manager, store *state.Store, snapshots *state.SnapshotStore) *SnapshotTxn {
	return &SnapshotTxn{commit: commit, store: store, snapshots: snapshots}
}

func (t *SnapshotTxn) WithConfigTxn(configTxn *ConfigTxn) *SnapshotTxn {
	t.configTxn = configTxn
	return t
}

func (t *SnapshotTxn) Create(ctx context.Context, req SnapshotCreateRequest) (*state.SnapshotEnvelope, error) {
	if req.ID == "" {
		req.ID = fmt.Sprintf("snapshot-%d", time.Now().UnixNano())
	}
	safeID, err := opid.Clean(req.ID)
	if err != nil {
		return nil, err
	}
	req.ID = safeID
	if req.Kind == "" {
		req.Kind = "manual"
	}
	var env *state.SnapshotEnvelope
	_, err = t.commit.WithCommit(ctx, CommitRequest{
		OpID:      req.ID,
		Kind:      "snapshot_create",
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
		runtimeState, err = state.RuntimeWithDurableSelection(t.store, runtimeState)
		if err != nil {
			return err
		}
		appliedState, err := t.store.LoadAppliedState()
		if err != nil {
			return err
		}
		appliedState.LastGood, err = provider.NormalizeLastGoodSnapshots(appliedState.LastGood)
		if err != nil {
			return err
		}
		var selection state.SelectionState
		selectionCaptured := appliedState.SchemaVersion != 0 && appliedState.SpecCaptured
		if selectionCaptured {
			selection, err = t.store.LoadSelectionForApplied(appliedState)
			if err != nil {
				return err
			}
		}
		lastGood, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(t.commit.Layout()))
		if err != nil {
			return err
		}
		topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, lastGood)
		if err != nil {
			return err
		}
		snapshot := state.SnapshotEnvelope{
			ID:                req.ID,
			Kind:              req.Kind,
			Description:       req.Description,
			TopologyHash:      topologyHash,
			SavedAt:           time.Now().UTC(),
			Spec:              spec,
			Runtime:           runtimeState,
			AppliedCaptured:   appliedState.SchemaVersion != 0 && appliedState.SpecCaptured,
			Applied:           appliedState,
			SelectionCaptured: selectionCaptured,
			Selection:         selection,
			LastGoodCaptured:  true,
			LastGood:          lastGood,
		}
		if err := t.snapshots.SaveSnapshot(snapshot); err != nil {
			return err
		}
		env = &snapshot
		return nil
	})
	return env, err
}

func (t *SnapshotTxn) DryRunRollback(ctx context.Context, req SnapshotRollbackDryRunRequest) (*SnapshotRollbackPlan, error) {
	if err := opid.Validate(req.DryRunID); err != nil {
		return nil, err
	}
	if err := opid.Validate(req.SnapshotID); err != nil {
		return nil, err
	}
	session := t.commit.locks.Session(req.DryRunID + "-snapshot-rollback-dryrun")
	releaseStore, err := session.Acquire(lockset.LevelStore)
	if err != nil {
		return nil, err
	}
	releaseSnapshot, err := session.Acquire(lockset.LevelSnapshot)
	if err != nil {
		releaseStore()
		return nil, err
	}
	releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
	if err != nil {
		releaseSnapshot()
		releaseStore()
		return nil, err
	}
	defer releaseStore()
	defer releaseSnapshot()
	defer releaseRuntime()

	spec, err := t.store.LoadSpec()
	if err != nil {
		return nil, err
	}
	currentLastGood, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(t.commit.Layout()))
	if err != nil {
		return nil, err
	}
	snapshot, err := t.snapshots.LoadSnapshot(req.SnapshotID)
	if err != nil {
		return nil, err
	}
	snapshot.LastGood, err = provider.NormalizeLastGoodSnapshots(snapshot.LastGood)
	if err != nil {
		return nil, err
	}
	snapshot.Applied.LastGood, err = provider.NormalizeLastGoodSnapshots(snapshot.Applied.LastGood)
	if err != nil {
		return nil, err
	}
	targetLastGood := currentLastGood
	if snapshot.LastGoodCaptured {
		targetLastGood = snapshot.LastGood
	}
	currentHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, currentLastGood)
	if err != nil {
		return nil, err
	}
	targetHash, err := provider.HashEffectiveTopologyFromSnapshots(snapshot.Spec, targetLastGood)
	if err != nil {
		return nil, err
	}
	runtimeState, err := t.store.LoadRuntime()
	if err != nil {
		return nil, err
	}
	runtimeState, err = state.RuntimeWithDurableSelection(t.store, runtimeState)
	if err != nil {
		return nil, err
	}
	appliedState, err := t.store.LoadAppliedState()
	if err != nil {
		return nil, err
	}
	appliedState.LastGood, err = provider.NormalizeLastGoodSnapshots(appliedState.LastGood)
	if err != nil {
		return nil, err
	}
	if appliedState.SchemaVersion == 0 {
		appliedState = state.AppliedStateFromRuntime(runtimeState, spec.Settings.GatewayMode)
		appliedState = state.AppliedStateWithTarget(appliedState, spec, currentLastGood)
	}
	currentSelection, err := t.store.LoadSelectionForApplied(appliedState)
	if err != nil {
		return nil, err
	}
	if currentSelection.SchemaVersion == 0 {
		currentSelection.ProxySelection = copyStringMap(runtimeState.ProxySelection)
	}
	targetSelection, err := snapshotSelection(snapshot)
	if err != nil {
		return nil, err
	}
	targetRuleSets := snapshotRuleSetRefs(snapshot)
	snapshotTargetHash, err := hashSnapshotRollbackTarget(targetHash, targetSelection, targetRuleSets)
	if err != nil {
		return nil, err
	}
	return &SnapshotRollbackPlan{
		DryRunID:             req.DryRunID,
		SnapshotID:           snapshot.ID,
		SnapshotKind:         snapshot.Kind,
		SnapshotSavedAt:      snapshot.SavedAt,
		CurrentTopologyHash:  currentHash,
		TargetTopologyHash:   targetHash,
		SnapshotTargetHash:   snapshotTargetHash,
		CurrentConfigEpoch:   appliedState.ConfigEpoch,
		CurrentProviderCount: len(spec.Providers),
		TargetProviderCount:  len(snapshot.Spec.Providers),
		CurrentPoolCount:     len(spec.BusinessPools),
		TargetPoolCount:      len(snapshot.Spec.BusinessPools),
		CurrentLastGoodCount: len(currentLastGood),
		TargetLastGoodCount:  len(targetLastGood),
		SpecChanged:          provider.HashSpec(spec) != provider.HashSpec(snapshot.Spec),
		LastGoodCaptured:     snapshot.LastGoodCaptured,
		CurrentSpec:          spec,
		CurrentRuntime:       runtimeState,
		CurrentSelection:     currentSelection,
		CurrentLastGood:      currentLastGood,
		CurrentApplied:       appliedState,
		TargetSpec:           snapshot.Spec,
		TargetRuntime:        snapshot.Runtime,
		TargetSelection:      targetSelection,
		TargetLastGood:       targetLastGood,
		TargetRuleSets:       targetRuleSets,
	}, nil
}

// PrepareRollback turns the snapshot rollback preview into the exact artifact
// that ApplyPrepared will consume. Candidate validation and OpenWrt planning
// therefore happen before the caller asks the user to confirm the rollback.
func (t *SnapshotTxn) PrepareRollback(ctx context.Context, req SnapshotRollbackDryRunRequest) (*SnapshotRollbackPrepared, error) {
	if t.configTxn == nil || t.configTxn.engine == nil {
		return nil, fmt.Errorf("snapshot rollback data-plane transaction is not configured")
	}
	plan, err := t.DryRunRollback(ctx, req)
	if err != nil {
		return nil, err
	}
	prepared, prepareErr := t.configTxn.engine.Prepare(ctx, req.DryRunID, configapply.TargetState{
		Spec:             plan.TargetSpec,
		LastGood:         plan.TargetLastGood,
		FallbackLastGood: plan.CurrentApplied.LastGood,
		ProxySelection:   copyStringMap(plan.TargetRuntime.ProxySelection),
		Selections:       copyPoolSelections(plan.TargetSelection.Selections),
		PreviousGraph:    plan.CurrentApplied.ExecutionGraph,
		RuleSetsPinned:   len(plan.TargetRuleSets) > 0,
		RuleSetRefs:      copyRuleSetRefs(plan.TargetRuleSets),
	}, plan.CurrentConfigEpoch)
	artifact := &SnapshotRollbackPrepared{Plan: *plan, Prepared: prepared}
	artifact.Plan.TargetSpec = prepared.Target.Spec
	artifact.Plan.TargetLastGood = prepared.Target.LastGood
	artifact.Plan.TargetTopologyHash = prepared.TopologyHash
	if prepareErr != nil {
		return artifact, prepareErr
	}
	return artifact, nil
}

func (t *SnapshotTxn) Rollback(ctx context.Context, req SnapshotRollbackRequest) (*SnapshotRollbackResult, error) {
	if t.configTxn == nil {
		return nil, fmt.Errorf("snapshot rollback data-plane transaction is not configured")
	}
	rollbackCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = rollbackCtx
	safeOpID, err := opid.Clean(req.OpID)
	if err != nil {
		return nil, err
	}
	if err := opid.Validate(req.SnapshotID); err != nil {
		return nil, err
	}
	source, err := t.configTxn.loadSourceState(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := t.DryRunRollback(ctx, SnapshotRollbackDryRunRequest{DryRunID: safeOpID, SnapshotID: req.SnapshotID})
	if err != nil {
		return nil, err
	}
	sourceTopology, err := provider.HashEffectiveTopologyFromSnapshots(source.Spec, source.LastGood)
	if err != nil {
		return nil, err
	}
	plan.CurrentTopologyHash = sourceTopology
	if req.RequireDryRunMatch && req.ExpectedConfigEpoch != source.Applied.ConfigEpoch {
		return nil, fmt.Errorf("applied config epoch changed after rollback dry-run")
	}
	if req.RequireDryRunMatch && req.ExpectedSourceTopologyHash != sourceTopology {
		return nil, fmt.Errorf("source topology changed after rollback dry-run")
	}
	if req.RequireDryRunMatch && !selectionMapsEqual(req.ExpectedSourceSelection, source.Runtime.ProxySelection) {
		return nil, fmt.Errorf("proxy selection changed after rollback dry-run")
	}
	if req.RequireDryRunMatch && !poolSelectionMapsEqual(req.ExpectedSourceSelections, source.Selection.Selections) {
		return nil, fmt.Errorf("persisted pool selection changed after rollback dry-run")
	}
	if req.RequireDryRunMatch && req.ExpectedSnapshotTargetHash != plan.SnapshotTargetHash {
		return nil, fmt.Errorf("snapshot target changed after rollback dry-run")
	}
	prepared := req.Prepared
	if prepared.OpID != safeOpID || prepared.ConfigEpoch != req.ExpectedConfigEpoch {
		return nil, fmt.Errorf("prepared rollback artifact does not match rollback dry-run")
	}
	if req.RequireDryRunMatch && prepared.TopologyHash != req.ExpectedTargetTopologyHash {
		return nil, fmt.Errorf("prepared rollback artifact changed after rollback dry-run")
	}
	loader, err := provider.NewSnapshotLastGoodLoader(prepared.Target.LastGood)
	if err != nil {
		return nil, err
	}
	prepared.Nodes, err = provider.BuildNodeViews(prepared.Target.Spec, loader)
	if err != nil {
		return nil, err
	}
	plan.TargetSpec = prepared.Target.Spec
	plan.TargetLastGood = prepared.Target.LastGood
	plan.TargetTopologyHash = prepared.TopologyHash
	txnResult, err := t.configTxn.ApplyPrepared(ctx, ConfigTxnApplyRequest{
		OpID:                 safeOpID,
		Kind:                 "snapshot_rollback",
		Prepared:             prepared,
		ExpectedSpecHash:     provider.HashSpec(source.Spec),
		ExpectedTopologyHash: sourceTopology,
		ExpectedSelection:    req.ExpectedSourceSelection,
		ExpectedSelections:   req.ExpectedSourceSelections,
		Selected:             plan.TargetRuntime.ProxySelection,
	})
	if err != nil {
		return nil, err
	}
	return &SnapshotRollbackResult{Plan: *plan, Spec: prepared.Target.Spec, Runtime: txnResult.Runtime}, nil
}

func snapshotSelection(snapshot state.SnapshotEnvelope) (state.SelectionState, error) {
	if !snapshot.SelectionCaptured {
		return state.SelectionState{ProxySelection: copyStringMap(snapshot.Runtime.ProxySelection)}, nil
	}
	selection := copySelectionState(snapshot.Selection)
	if selection.SchemaVersion == 0 {
		return state.SelectionState{}, fmt.Errorf("snapshot marks Selection captured but has no Selection schema")
	}
	if selection.ConfigEpoch != snapshot.Runtime.ConfigEpoch || selection.TopologyHash != snapshot.Runtime.TopologyHash {
		return state.SelectionState{}, fmt.Errorf("snapshot Selection does not match snapshot runtime metadata")
	}
	return selection, nil
}

func snapshotRuleSetRefs(snapshot state.SnapshotEnvelope) []state.RuleSetSnapshotRef {
	if !snapshot.AppliedCaptured || !snapshot.Applied.SpecCaptured {
		return nil
	}
	return copyRuleSetRefs(snapshot.Applied.ExecutionGraph.Fabric.RuleSets)
}

func hashSnapshotRollbackTarget(topologyHash string, selection state.SelectionState, ruleSets []state.RuleSetSnapshotRef) (string, error) {
	body, err := json.Marshal(struct {
		TopologyHash   string                         `json:"topology_hash"`
		ProxySelection map[string]string              `json:"proxy_selection,omitempty"`
		Selections     map[string]state.PoolSelection `json:"selections,omitempty"`
		RuleSets       []state.RuleSetSnapshotRef     `json:"rule_sets,omitempty"`
	}{
		TopologyHash: topologyHash, ProxySelection: selection.ProxySelection, Selections: selection.Selections, RuleSets: ruleSets,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
