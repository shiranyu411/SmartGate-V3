package commitlane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"smartgate/internal/backup"
	"smartgate/internal/business"
	"smartgate/internal/configapply"
	"smartgate/internal/lockset"
	"smartgate/internal/opid"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type BackupTxn struct {
	commit    *Manager
	store     *state.Store
	snapshots *state.SnapshotStore
	layout    state.Layout
	configTxn *ConfigTxn
}

type BackupRestoreRequest struct {
	OpID    string
	Payload backup.Payload
}

type BackupRestoreResult struct {
	Status        string   `json:"status"`
	ConfigEpoch   int64    `json:"config_epoch"`
	TopologyHash  string   `json:"topology_hash,omitempty"`
	LastGoodCount int      `json:"last_good_count"`
	Warnings      []string `json:"warnings"`
	// In-process restore callers may inspect these states. They are excluded
	// from API serialization because backup contents can include credentials.
	Spec    state.SpecState    `json:"-"`
	Runtime state.RuntimeState `json:"-"`
}

func NewBackupTxn(commit *Manager, store *state.Store, snapshots *state.SnapshotStore, layout state.Layout) *BackupTxn {
	return &BackupTxn{commit: commit, store: store, snapshots: snapshots, layout: layout}
}

func (t *BackupTxn) WithConfigTxn(configTxn *ConfigTxn) *BackupTxn {
	t.configTxn = configTxn
	return t
}

func (t *BackupTxn) Layout() state.Layout { return t.layout }

func (t *BackupTxn) Create(ctx context.Context, opID, kind string) (*backup.Record, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return nil, err
	}
	var record backup.Record
	_, err = t.commit.WithCommit(ctx, CommitRequest{
		OpID:      safeOpID,
		Kind:      "backup_create",
		StartedAt: time.Now(),
	}, func(session *lockset.Session) error {
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		defer releaseStore()

		releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
		if err != nil {
			return err
		}
		defer releaseRuntime()

		record, err = backup.Create(ctx, t.layout, t.store, kind)
		return err
	})
	return &record, err
}

func (t *BackupTxn) Restore(ctx context.Context, req BackupRestoreRequest) (*BackupRestoreResult, error) {
	if t.configTxn == nil {
		return nil, fmt.Errorf("backup restore data-plane transaction is not configured")
	}
	restoreCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return t.restoreWithConfigTxn(restoreCtx, req)
}

func (t *BackupTxn) restoreWithConfigTxn(ctx context.Context, req BackupRestoreRequest) (*BackupRestoreResult, error) {
	safeOpID, err := opid.Clean(req.OpID)
	if err != nil {
		return nil, err
	}
	req.OpID = safeOpID
	normalized, err := provider.ValidateSpecProviders(req.Payload.Spec)
	if err != nil {
		return nil, err
	}
	normalized, err = business.ValidatePools(normalized)
	if err != nil {
		return nil, err
	}
	req.Payload.Spec = normalized
	updates, err := lastGoodUpdates(req.Payload.LastGood)
	if err != nil {
		return nil, err
	}
	source, err := t.configTxn.loadSourceState(ctx)
	if err != nil {
		return nil, err
	}
	targetLastGood := lastGoodSnapshotsFromUpdates(updates)
	selected := req.Payload.Runtime.ProxySelection
	if !req.Payload.RuntimeCaptured && req.Payload.AppliedCaptured {
		selected = req.Payload.Applied.ProxySelection
	}
	var targetSelections map[string]state.PoolSelection
	if req.Payload.SelectionCaptured {
		selected = req.Payload.Selection.ProxySelection
		targetSelections = req.Payload.Selection.Selections
	}
	prepared, err := t.configTxn.engine.Prepare(ctx, safeOpID, configapply.TargetState{
		Spec:             req.Payload.Spec,
		LastGood:         targetLastGood,
		FallbackLastGood: source.Applied.LastGood,
		ProxySelection:   copyStringMap(selected),
		Selections:       copyPoolSelections(targetSelections),
		PreviousGraph:    source.Applied.ExecutionGraph,
		RuleSetsPinned:   req.Payload.AppliedCaptured && len(req.Payload.Applied.ExecutionGraph.Fabric.RuleSets) > 0,
		RuleSetRefs:      copyRuleSetRefs(req.Payload.Applied.ExecutionGraph.Fabric.RuleSets),
	}, source.Applied.ConfigEpoch)
	if err != nil {
		return nil, err
	}
	sourceTopology, err := provider.HashEffectiveTopologyFromSnapshots(source.Spec, source.LastGood)
	if err != nil {
		return nil, err
	}
	var baseFailover *state.FailoverState
	if req.Payload.FailoverCaptured {
		archived := state.NormalizeFailoverState(req.Payload.Failover)
		baseFailover = &archived
	}
	txnResult, err := t.configTxn.ApplyPrepared(ctx, ConfigTxnApplyRequest{
		OpID:                 safeOpID,
		Kind:                 "backup_restore",
		Prepared:             prepared,
		ExpectedSpecHash:     provider.HashSpec(source.Spec),
		ExpectedTopologyHash: sourceTopology,
		ExpectedSelection:    copyStringMap(source.Runtime.ProxySelection),
		ExpectedSelections:   copyPoolSelections(source.Selection.Selections),
		Selected:             selected,
		BaseFailover:         baseFailover,
	})
	if err != nil {
		return nil, err
	}
	result := &BackupRestoreResult{
		Status: "restored", ConfigEpoch: txnResult.Runtime.ConfigEpoch,
		TopologyHash: txnResult.Runtime.TopologyHash, LastGoodCount: len(targetLastGood),
		Warnings: []string{}, Spec: prepared.Target.Spec, Runtime: txnResult.Runtime,
	}
	if !req.Payload.FailoverCaptured {
		result.Warnings = append(result.Warnings, "旧版备份不含故障切换状态；已保留当前故障切换设置")
	}
	if !req.Payload.SelectionCaptured {
		result.Warnings = append(result.Warnings, "旧版备份不含结构化选点状态；已根据当前应用图重新绑定 Pool Generation，并保持单调递增")
	}
	return result, nil
}

func lastGoodSnapshotsFromUpdates(updates []provider.LastGoodUpdate) []state.LastGoodSnapshot {
	out := make([]state.LastGoodSnapshot, 0, len(updates))
	for _, update := range updates {
		out = append(out, state.LastGoodSnapshot{
			ProviderID:   update.ProviderID,
			Subscription: update.Subscription,
			Identities:   update.Identities,
		})
	}
	return out
}

func lastGoodUpdates(raw map[string]json.RawMessage) ([]provider.LastGoodUpdate, error) {
	updates := make([]provider.LastGoodUpdate, 0, len(raw))
	for providerID, body := range raw {
		var env provider.LastGoodEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.ProviderID == "" {
			env.ProviderID = providerID
		}
		if err := provider.ValidateProviderID(env.ProviderID); err != nil {
			return nil, err
		}
		updates = append(updates, provider.LastGoodUpdate{
			ProviderID: env.ProviderID, Subscription: env.Subscription,
			Identities: env.Identities, Outbounds: env.Outbounds,
		})
	}
	return provider.NormalizeLastGoodUpdates(updates)
}
