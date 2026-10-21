package commitlane

import (
	"context"
	"fmt"
	"os"
	"time"

	"smartgate/internal/configapply"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/opid"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/selection"
	"smartgate/internal/state"
)

// ConfigApplyEngine is the data-plane boundary used by ConfigTxn. Keeping the
// interface here makes commit/recovery behavior testable without starting
// the data plane or mutating OpenWrt.
type ConfigApplyEngine interface {
	Prepare(context.Context, string, configapply.TargetState, int64) (configapply.Prepared, error)
	Checkpoint(string) (configapply.Checkpoint, error)
	Apply(context.Context, configapply.Prepared, map[string]string) (configapply.Result, error)
	Rollback(context.Context, configapply.Prepared, map[string]string, configapply.Checkpoint) (configapply.Result, error)
}

type rollbackCheckpointPreparer interface {
	PrepareRollbackFromCheckpoint(context.Context, string, configapply.TargetState, int64, configapply.Checkpoint) (configapply.Prepared, error)
}

type ConfigTxn struct {
	commit *Manager
	store  *state.Store
	engine ConfigApplyEngine
}

type ConfigTxnApplyRequest struct {
	OpID                 string
	Kind                 string
	Prepared             configapply.Prepared
	ExpectedSpecHash     string
	ExpectedTopologyHash string
	ExpectedSelection    map[string]string
	ExpectedSelections   map[string]state.PoolSelection
	Selected             map[string]string
	BaseFailover         *state.FailoverState
}

func selectionMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func poolSelectionMapsEqual(left, right map[string]state.PoolSelection) bool {
	if len(left) != len(right) {
		return false
	}
	for pool, selection := range left {
		if right[pool] != selection {
			return false
		}
	}
	return true
}

type ConfigTxnResult struct {
	Apply   configapply.Result `json:"apply"`
	Applied state.AppliedState `json:"applied"`
	Runtime state.RuntimeState `json:"runtime"`
}

type ConfigTxnRollbackError struct {
	Cause             error
	RollbackError     error
	Result            configapply.Result
	SelectionRestored bool
}

func (e *ConfigTxnRollbackError) Error() string {
	if e == nil {
		return "configuration transaction rollback failed"
	}
	if e.RollbackError != nil {
		return fmt.Sprintf("%v; automatic rollback incomplete: %v", e.Cause, e.RollbackError)
	}
	return fmt.Sprintf("%v (rolled back to prior applied state)", e.Cause)
}

func (e *ConfigTxnRollbackError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *ConfigTxnRollbackError) OK() bool {
	return e != nil && e.RollbackError == nil && e.Result.WorkersRestored && e.Result.FabricRestored && e.Result.BindingsRestored && e.SelectionRestored
}

func NewConfigTxn(commit *Manager, store *state.Store, engine ConfigApplyEngine) *ConfigTxn {
	return &ConfigTxn{commit: commit, store: store, engine: engine}
}

func (t *ConfigTxn) Prepare(ctx context.Context, opID string, target configapply.TargetState, epoch int64) (configapply.Prepared, error) {
	if t == nil || t.engine == nil {
		return configapply.Prepared{}, fmt.Errorf("config transaction is not configured")
	}
	if len(target.FallbackLastGood) == 0 && t.store != nil {
		if applied, err := t.store.LoadAppliedState(); err == nil && applied.SpecCaptured {
			target.FallbackLastGood = applied.LastGood
		}
	}
	return t.engine.Prepare(ctx, opID, target, epoch)
}

func (t *ConfigTxn) CleanupPrepared(prepared configapply.Prepared) {
	if t == nil || t.engine == nil {
		return
	}
	if cleaner, ok := t.engine.(interface{ Cleanup(configapply.Prepared) }); ok {
		cleaner.Cleanup(prepared)
		return
	}
}

// Recover reconciles durable config transactions before the API starts. A
// committed journal only needs cleanup. Every other phase is treated as an
// interrupted commit and is restored from the durable applied-state capture,
// which is the authoritative description of the data plane that was active
// before the transaction began.
func (t *ConfigTxn) Recover(ctx context.Context) error {
	return t.recover(ctx, false)
}

// RecoverForRescueClear reconciles pending transactions while the rescue flag
// is intentionally still present. Callers must clear rescue only after this
// succeeds; on failure they should re-apply rescue cleanup.
func (t *ConfigTxn) RecoverForRescueClear(ctx context.Context) error {
	return t.recover(ctx, true)
}

func (t *ConfigTxn) recover(ctx context.Context, allowDuringRescue bool) error {
	if t == nil || t.commit == nil || t.store == nil || t.engine == nil {
		return fmt.Errorf("config transaction is not configured")
	}
	transactions := state.NewTransactionStore(t.commit.Layout())
	journals, err := transactions.List()
	if err != nil {
		return err
	}
	for _, journal := range journals {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !isConfigTransactionKind(journal.Kind) {
			continue
		}
		if !allowDuringRescue && journal.Phase != state.TransactionCommitted {
			if _, err := os.Stat(t.commit.Layout().RescueStatusPath()); err == nil {
				// Rescue is an explicit fail-open override. Keep the journal for a
				// later recovery attempt instead of re-enabling the proxy data plane.
				continue
			}
		}
		if err := t.recoverJournal(ctx, transactions, journal); err != nil {
			return fmt.Errorf("recover transaction %s: %w", journal.OpID, err)
		}
	}
	if allowDuringRescue {
		return t.reconcileAppliedDataPlane(ctx, "rescue-clear-reconcile")
	}
	return nil
}

func (t *ConfigTxn) ReconcileAppliedDataPlane(ctx context.Context, opID string) error {
	if t == nil || t.store == nil {
		return fmt.Errorf("config transaction is not configured")
	}
	applied, err := t.store.LoadAppliedState()
	if err != nil {
		return err
	}
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return nil
	}
	return t.reconcileAppliedDataPlane(ctx, opID)
}

// ReconcileNetwork restores only the last applied network, never dirty Spec.
func (t *ConfigTxn) ReconcileNetwork(ctx context.Context) error {
	if t == nil || t.commit == nil || t.store == nil || t.engine == nil {
		return fmt.Errorf("config transaction is not configured")
	}
	return t.restoreAppliedDataPlane(ctx, "network-repair", true)
}

func (t *ConfigTxn) reconcileAppliedDataPlane(ctx context.Context, opID string) error {
	return t.restoreAppliedDataPlane(ctx, opID, false)
}

func (t *ConfigTxn) restoreAppliedDataPlane(ctx context.Context, opID string, guard bool) error {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return err
	}
	_, err = t.commit.WithCommit(ctx, CommitRequest{
		OpID: safeOpID, Kind: "recovery", StartedAt: time.Now().UTC(),
	}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelSnapshot, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.Acquire(level)
			if err != nil {
				return err
			}
			defer release()
		}
		source, err := t.loadSourceState(ctx)
		if err != nil {
			return fmt.Errorf("load authoritative applied state: %w", err)
		}
		if guard {
			rescue, err := rescueFlagActive(t.commit.Layout())
			if err != nil {
				return err
			}
			if !source.Applied.SpecCaptured || source.Applied.SchemaVersion == 0 {
				return fmt.Errorf("no captured applied configuration to restore")
			}
			if rescue || source.Runtime.RescueActive || (source.Applied.GatewayMode != state.GatewayModeOn && source.Applied.GatewayMode != state.GatewayModeGlobal) {
				return fmt.Errorf("network repair requires an active proxy mode outside rescue")
			}
		}
		target := configRecoveryTarget(source)
		prepared, err := t.engine.Prepare(ctx, safeOpID, target, source.Applied.ConfigEpoch)
		if err != nil {
			t.CleanupPrepared(prepared)
			return fmt.Errorf("prepare rescue data-plane reconcile: %w", err)
		}
		defer t.CleanupPrepared(prepared)
		var checkpoint configapply.Checkpoint
		if guard {
			checkpoint, err = t.engine.Checkpoint(safeOpID)
			if err != nil {
				return fmt.Errorf("backup data plane: %w", err)
			}
		}
		result, err := t.engine.Apply(ctx, prepared, source.Runtime.ProxySelection)
		if err != nil {
			if guard {
				rollbackCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				_, rollbackErr := t.engine.Rollback(rollbackCtx, prepared, source.Runtime.ProxySelection, checkpoint)
				cancel()
				if rollbackErr != nil {
					return fmt.Errorf("restore failed: %v; rollback failed: %w", err, rollbackErr)
				}
			}
			return fmt.Errorf("apply data-plane restore: %w", err)
		}
		if result.FinalSelections != nil {
			requested := source.Runtime.ProxySelection
			setTransactionSelections(&source, result.FinalSelections)
			source.Failover = reconcileTransactionFailover(source.Failover, prepared, result.FinalSelections, requested)
			source.FailoverCaptured = true
		}
		source.Runtime = runtimeMetadataFromApplied(source.Runtime, source.Applied)
		source.Runtime.CommitInFlight = nil
		return t.persistControlWithoutApplied(ctx, source)
	})
	return err
}

func (t *ConfigTxn) recoverJournal(ctx context.Context, transactions *state.TransactionStore, journal state.TransactionJournal) error {
	var err error
	journal.Source, err = normalizeTransactionLastGood(journal.Source)
	if err != nil {
		return err
	}
	journal.Target, err = normalizeTransactionLastGood(journal.Target)
	if err != nil {
		return err
	}
	_, err = t.commit.WithCommit(ctx, CommitRequest{
		OpID:      suffixedOpID(journal.OpID, "-recover"),
		Kind:      "recovery",
		StartedAt: time.Now().UTC(),
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
		releaseNet, err := session.Acquire(lockset.LevelNet)
		if err != nil {
			return err
		}
		defer releaseNet()

		if journal.Phase == state.TransactionCommitted {
			if transactionStateCaptured(journal.Target) {
				if err := t.persistState(ctx, journal.Target); err != nil {
					return fmt.Errorf("replay committed target: %w", err)
				}
			}
			if err := state.NewSnapshotStore(t.commit.Layout()).RemovePreimage(journal.OpID); err != nil {
				return err
			}
			return transactions.Remove(journal.OpID)
		}

		rollbackTarget := configRecoveryTarget(journal.Source)
		recovered := configRecoveryControlSource(journal.Source)
		if journal.Phase == state.TransactionPrepared {
			if err := t.persistControlWithoutApplied(ctx, recovered); err != nil {
				return fmt.Errorf("restore prepared control plane: %w", err)
			}
			if err := state.NewSnapshotStore(t.commit.Layout()).RemovePreimage(journal.OpID); err != nil {
				return err
			}
			return transactions.Remove(journal.OpID)
		}
		journal.Phase = state.TransactionRecoveryRequired
		journal.LastError = "startup recovery required after interrupted transaction"
		if err := transactions.Save(journal); err != nil {
			return err
		}
		if !recovered.Applied.SpecCaptured {
			// Virgin / no-baseline journal: best-effort empty-baseline cleanup then drop journal
			// so the management plane can boot again (B1).
			return t.recoverUncapturedInterrupted(ctx, transactions, journal, recovered)
		}
		prepared, err := t.engine.Prepare(ctx, suffixedOpID(journal.OpID, "-rollback"), configapply.TargetState{
			Spec:             rollbackTarget.Spec,
			LastGood:         rollbackTarget.LastGood,
			FallbackLastGood: rollbackTarget.FallbackLastGood,
			ProxySelection:   recovered.Runtime.ProxySelection,
			Selections:       copyPoolSelections(recovered.Selection.Selections),
			PreviousGraph:    recovered.Applied.ExecutionGraph,
			RuleSetsPinned:   len(recovered.Applied.ExecutionGraph.Fabric.RuleSets) > 0,
			RuleSetRefs:      copyRuleSetRefs(recovered.Applied.ExecutionGraph.Fabric.RuleSets),
		}, recovered.Applied.ConfigEpoch)
		if err != nil {
			t.CleanupPrepared(prepared)
			journal.LastError = fmt.Sprintf("prepare startup recovery: %v", err)
			_ = transactions.Save(journal)
			return fmt.Errorf("prepare data-plane recovery: %w", err)
		}
		defer t.CleanupPrepared(prepared)
		applyResult, err := t.engine.Apply(ctx, prepared, recovered.Runtime.ProxySelection)
		if err != nil {
			journal.LastError = fmt.Sprintf("apply startup recovery: %v", err)
			_ = transactions.Save(journal)
			return fmt.Errorf("apply data-plane recovery: %w", err)
		}
		if applyResult.FinalSelections != nil {
			requestedSelections := recovered.Runtime.ProxySelection
			setTransactionSelections(&recovered, applyResult.FinalSelections)
			recovered.Failover = reconcileTransactionFailover(recovered.Failover, prepared, applyResult.FinalSelections, requestedSelections)
			recovered.FailoverCaptured = true
		}
		recovered.Runtime = runtimeMetadataFromApplied(recovered.Runtime, recovered.Applied)
		now := time.Now().UTC()
		recovered.Runtime.LastRollbackAt = &now
		recovered.Runtime.CommitInFlight = nil
		if err := t.persistControlWithoutApplied(ctx, recovered); err != nil {
			journal.LastError = fmt.Sprintf("persist startup recovery: %v", err)
			_ = transactions.Save(journal)
			return fmt.Errorf("persist recovered control plane: %w", err)
		}
		if recovered.Applied.SchemaVersion != 0 && recovered.Applied.SpecCaptured {
			if err := t.store.SaveDataPlaneObservation(state.DataPlaneObservationForMode(recovered.Applied.GatewayMode)); err != nil {
				journal.LastError = fmt.Sprintf("persist observation after startup recovery: %v", err)
				_ = transactions.Save(journal)
				return fmt.Errorf("persist observation after startup recovery: %w", err)
			}
		}
		if err := state.NewSnapshotStore(t.commit.Layout()).RemovePreimage(journal.OpID); err != nil {
			return err
		}
		return transactions.Remove(journal.OpID)
	})
	return err
}

func transactionStateCaptured(value state.TransactionState) bool {
	return value.Spec.SchemaVersion != 0 || value.LastGoodCaptured || value.Applied.SchemaVersion != 0 || value.Runtime.ConfigEpoch != 0 || value.Runtime.TopologyHash != "" || value.SelectionCaptured
}

func configRecoveryTarget(source state.TransactionState) configapply.TargetState {
	if source.Applied.SpecCaptured {
		return configapply.TargetState{
			Spec:             source.Applied.Spec,
			LastGood:         source.Applied.LastGood,
			FallbackLastGood: source.Applied.LastGood,
			ProxySelection:   copyStringMap(source.Runtime.ProxySelection),
			Selections:       copyPoolSelections(source.Selection.Selections),
			PreviousGraph:    source.Applied.ExecutionGraph,
			RuleSetsPinned:   len(source.Applied.ExecutionGraph.Fabric.RuleSets) > 0,
			RuleSetRefs:      copyRuleSetRefs(source.Applied.ExecutionGraph.Fabric.RuleSets),
		}
	}
	return configapply.TargetState{Spec: source.Spec, LastGood: source.LastGood, ProxySelection: copyStringMap(source.Runtime.ProxySelection), Selections: copyPoolSelections(source.Selection.Selections), PreviousGraph: source.Applied.ExecutionGraph}
}

func configRecoveryControlSource(source state.TransactionState) state.TransactionState {
	recovered := source
	now := time.Now().UTC()
	recovered.Runtime.LastRollbackAt = &now
	recovered.Runtime.CommitInFlight = nil
	return recovered
}

func isConfigTransactionKind(kind string) bool {
	switch kind {
	case "config_apply", "snapshot_rollback", "backup_restore":
		return true
	default:
		return false
	}
}

func (t *ConfigTxn) ApplyPrepared(ctx context.Context, req ConfigTxnApplyRequest) (*ConfigTxnResult, error) {
	if t == nil || t.commit == nil || t.store == nil || t.engine == nil {
		return nil, fmt.Errorf("config transaction is not configured")
	}
	defer t.CleanupPrepared(req.Prepared)
	safeOpID, err := opid.Clean(req.OpID)
	if err != nil {
		return nil, err
	}
	if req.Kind == "" {
		req.Kind = "config_apply"
	}
	if req.Prepared.OpID != safeOpID {
		return nil, fmt.Errorf("prepared op id %q does not match %q", req.Prepared.OpID, safeOpID)
	}
	req.Prepared.Target.LastGood, err = provider.NormalizeLastGoodSnapshots(req.Prepared.Target.LastGood)
	if err != nil {
		return nil, err
	}
	if err := validatePreparedTarget(req.Prepared); err != nil {
		return nil, err
	}

	var result *ConfigTxnResult
	_, err = t.commit.WithCommit(ctx, CommitRequest{
		OpID:         safeOpID,
		Kind:         req.Kind,
		TopologyHash: req.Prepared.TopologyHash,
		StartedAt:    time.Now().UTC(),
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
		releaseNet, err := session.Acquire(lockset.LevelNet)
		if err != nil {
			return err
		}
		defer releaseNet()

		source, err := t.loadSourceState(ctx)
		if err != nil {
			return err
		}
		if source.Applied.ConfigEpoch != req.Prepared.ConfigEpoch {
			return fmt.Errorf("applied config epoch changed after dry-run: have %d want %d", source.Applied.ConfigEpoch, req.Prepared.ConfigEpoch)
		}
		sourceHash := provider.HashSpec(source.Spec)
		if req.ExpectedSpecHash != "" && sourceHash != req.ExpectedSpecHash {
			return fmt.Errorf("spec changed after dry-run")
		}
		sourceTopology, err := provider.HashEffectiveTopologyFromSnapshots(source.Spec, source.LastGood)
		if err != nil {
			return err
		}
		if req.ExpectedTopologyHash != "" && sourceTopology != req.ExpectedTopologyHash {
			return fmt.Errorf("effective topology changed after dry-run")
		}
		if req.ExpectedSelection != nil && !selectionMapsEqual(req.ExpectedSelection, source.Runtime.ProxySelection) {
			return fmt.Errorf("proxy selection changed after dry-run")
		}
		if req.ExpectedSelections != nil && !poolSelectionMapsEqual(req.ExpectedSelections, source.Selection.Selections) {
			return fmt.Errorf("persisted pool selection changed after dry-run")
		}

		rollbackSelections := source.Runtime.ProxySelection
		checkpoint, err := t.engine.Checkpoint(safeOpID)
		if err != nil {
			return fmt.Errorf("checkpoint active data plane: %w", err)
		}
		var rollbackPrepared configapply.Prepared
		if source.Applied.SpecCaptured {
			target := configapply.TargetState{
				Spec:             source.Applied.Spec,
				LastGood:         source.Applied.LastGood,
				FallbackLastGood: source.Applied.LastGood,
				ProxySelection:   copyStringMap(source.Runtime.ProxySelection),
				Selections:       copyPoolSelections(source.Selection.Selections),
				PreviousGraph:    source.Applied.ExecutionGraph,
				RuleSetsPinned:   len(source.Applied.ExecutionGraph.Fabric.RuleSets) > 0,
				RuleSetRefs:      copyRuleSetRefs(source.Applied.ExecutionGraph.Fabric.RuleSets),
			}
			if fast, ok := t.engine.(rollbackCheckpointPreparer); ok {
				rollbackPrepared, err = fast.PrepareRollbackFromCheckpoint(ctx, suffixedOpID(safeOpID, "-rollback"), target, source.Applied.ConfigEpoch, checkpoint)
			}
			if err != nil || rollbackPrepared.OpID == "" {
				rollbackPrepared, err = t.engine.Prepare(ctx, suffixedOpID(safeOpID, "-rollback"), target, source.Applied.ConfigEpoch)
			}
			if err != nil {
				t.CleanupPrepared(rollbackPrepared)
				return fmt.Errorf("prepare rollback source: %w", err)
			}
			defer t.CleanupPrepared(rollbackPrepared)
		}
		targetSelections := req.Selected
		if targetSelections == nil {
			targetSelections = source.Runtime.ProxySelection
		}
		targetApplied := buildTargetApplied(source.Applied, req.Prepared, targetSelections, req.Kind)
		targetRuntime := runtimeFromApplied(source.Runtime, targetApplied)
		targetSelection := state.SelectionStateForApplied(targetApplied, targetSelections)
		baseFailover := source.Failover
		if req.BaseFailover != nil {
			baseFailover = *req.BaseFailover
		}
		targetFailover := reconcileTransactionFailover(baseFailover, req.Prepared, targetSelections, targetSelections)
		journal := state.TransactionJournal{
			OpID:       safeOpID,
			Kind:       req.Kind,
			Phase:      state.TransactionPrepared,
			SourceHash: sourceTopology,
			TargetHash: req.Prepared.TopologyHash,
			Source:     source,
			Target: state.TransactionState{
				Spec:              req.Prepared.Target.Spec,
				LastGoodCaptured:  true,
				LastGood:          req.Prepared.Target.LastGood,
				Applied:           targetApplied,
				Runtime:           targetRuntime,
				SelectionCaptured: true,
				Selection:         targetSelection,
				FailoverCaptured:  true,
				Failover:          targetFailover,
			},
		}
		journals := state.NewTransactionStore(t.commit.Layout())
		if err := journals.Save(journal); err != nil {
			return err
		}
		fail := func(cause error, dataPlaneTouched bool) error {
			if source.Applied.SpecCaptured {
				return t.rollback(ctx, journals, journal, rollbackPrepared, rollbackSelections, checkpoint, cause)
			}
			if dataPlaneTouched {
				// No successful apply baseline yet: best-effort empty-baseline cleanup,
				// then always drop the journal so the management plane cannot be bricked (B1).
				return t.rollbackEmptyBaseline(ctx, journals, journal, rollbackSelections, checkpoint, cause)
			}
			source.Runtime.CommitInFlight = nil
			_ = t.store.SaveRuntime(source.Runtime)
			_ = journals.Remove(journal.OpID)
			return cause
		}

		source.Runtime.CommitInFlight = &state.CommitRef{OpID: safeOpID, StartedAt: time.Now().UTC(), TopologyHash: req.Prepared.TopologyHash}
		if err := t.store.SaveRuntime(source.Runtime); err != nil {
			return fail(fmt.Errorf("mark commit in flight: %w", err), false)
		}
		journal.Phase = state.TransactionApplying
		if err := journals.Save(journal); err != nil {
			return fail(fmt.Errorf("mark transaction applying: %w", err), false)
		}

		applyResult, err := t.engine.Apply(ctx, req.Prepared, targetSelections)
		if err != nil {
			return fail(err, true)
		}
		if applyResult.FinalSelections != nil {
			targetApplied.ProxySelection = copyStringMap(applyResult.FinalSelections)
			targetFailover = reconcileTransactionFailover(baseFailover, req.Prepared, applyResult.FinalSelections, targetSelections)
		}
		targetRuntime = runtimeFromApplied(source.Runtime, targetApplied)
		targetRuntime.CommitInFlight = nil
		journal.Target.Applied = targetApplied
		journal.Target.Runtime = targetRuntime
		setTransactionSelections(&journal.Target, targetApplied.ProxySelection)
		journal.Target.Failover = targetFailover

		journal.Phase = state.TransactionCommitted
		if err := journals.Save(journal); err != nil {
			return fail(fmt.Errorf("persist committed transaction: %w", err), true)
		}
		// Once the durable journal points at the target, recovery always completes
		// forward. Persist AppliedState last inside persistState so no failing write
		// after it can turn a failed transaction into a partially advanced applied
		// state.
		if err := t.persistState(ctx, journal.Target); err != nil {
			return fmt.Errorf("persist committed target (startup recovery required): %w", err)
		}
		// Config apply is the control-plane moment when the live data-plane mode
		// becomes known. Publish observation immediately for UI/API state.
		if err := t.store.SaveDataPlaneObservation(state.DataPlaneObservationForMode(targetApplied.GatewayMode)); err != nil {
			return fmt.Errorf("persist data-plane observation after commit: %w", err)
		}
		result = &ConfigTxnResult{Apply: applyResult, Applied: targetApplied, Runtime: targetRuntime}
		if err := journals.Remove(safeOpID); err != nil {
			result.Apply.Warnings = append(result.Apply.Warnings, "配置已提交，但事务记录暂未清理；系统将在恢复流程中继续处理")
			return nil
		}
		return nil
	})
	return result, err
}

func (t *ConfigTxn) loadSourceState(ctx context.Context) (state.TransactionState, error) {
	spec, err := t.store.LoadSpec()
	if err != nil {
		return state.TransactionState{}, err
	}
	runtimeState, err := t.store.LoadRuntime()
	if err != nil {
		return state.TransactionState{}, err
	}
	lastGood, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(t.commit.Layout()))
	if err != nil {
		return state.TransactionState{}, err
	}
	applied, err := t.store.LoadAppliedState()
	if err != nil {
		return state.TransactionState{}, err
	}
	applied.LastGood, err = provider.NormalizeLastGoodSnapshots(applied.LastGood)
	if err != nil {
		return state.TransactionState{}, err
	}
	selection, selectionCaptured, err := t.loadSelectionState(applied)
	if err != nil {
		return state.TransactionState{}, err
	}
	if selectionCaptured {
		runtimeState.ConfigEpoch = applied.ConfigEpoch
		runtimeState.TopologyHash = applied.TopologyHash
		runtimeState.ProxySelection = copyStringMap(selection.ProxySelection)
	}
	failoverState, err := state.NewFailoverStateStore(t.commit.Layout()).Load()
	if err != nil {
		return state.TransactionState{}, err
	}
	return state.TransactionState{
		Spec:              spec,
		LastGoodCaptured:  true,
		LastGood:          lastGood,
		Applied:           applied,
		Runtime:           runtimeState,
		SelectionCaptured: selectionCaptured,
		Selection:         selection,
		FailoverCaptured:  true,
		Failover:          failoverState,
	}, nil
}

func normalizeTransactionLastGood(value state.TransactionState) (state.TransactionState, error) {
	var err error
	value.LastGood, err = provider.NormalizeLastGoodSnapshots(value.LastGood)
	if err != nil {
		return state.TransactionState{}, err
	}
	value.Applied.LastGood, err = provider.NormalizeLastGoodSnapshots(value.Applied.LastGood)
	if err != nil {
		return state.TransactionState{}, err
	}
	return value, nil
}

func (t *ConfigTxn) rollback(ctx context.Context, journals *state.TransactionStore, journal state.TransactionJournal, prepared configapply.Prepared, selected map[string]string, checkpoint configapply.Checkpoint, cause error) error {
	journal.Phase = state.TransactionRecoveryRequired
	journal.LastError = cause.Error()
	if err := journals.Save(journal); err != nil {
		return fmt.Errorf("%v; persist recovery journal: %w", cause, err)
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	rollbackResult, err := t.engine.Rollback(recoveryCtx, prepared, selected, checkpoint)
	report := &ConfigTxnRollbackError{Cause: cause, Result: rollbackResult}
	if err != nil {
		journal.LastError = fmt.Sprintf("%v; rollback data plane: %v", cause, err)
		_ = journals.Save(journal)
		report.RollbackError = fmt.Errorf("rollback data plane: %w", err)
		return report
	}
	report.Result.WorkersRestored = true
	report.Result.FabricRestored = true
	report.Result.BindingsRestored = true
	source := journal.Source
	if rollbackResult.FinalSelections != nil {
		setTransactionSelections(&source, rollbackResult.FinalSelections)
		source.Failover = reconcileTransactionFailover(source.Failover, prepared, rollbackResult.FinalSelections, selected)
		source.FailoverCaptured = true
	}
	source.Runtime.CommitInFlight = nil
	now := time.Now().UTC()
	source.Runtime.LastRollbackAt = &now
	if err := t.persistControlWithoutApplied(recoveryCtx, source); err != nil {
		journal.LastError = fmt.Sprintf("%v; restore source state: %v", cause, err)
		_ = journals.Save(journal)
		report.RollbackError = fmt.Errorf("restore source state: %w", err)
		return report
	}
	report.SelectionRestored = true
	// Rollback restored the prior data plane; publish observation so UI/API do
	// not keep reporting the failed target mode.
	if source.Applied.SchemaVersion != 0 && source.Applied.SpecCaptured {
		if err := t.store.SaveDataPlaneObservation(state.DataPlaneObservationForMode(source.Applied.GatewayMode)); err != nil {
			journal.LastError = fmt.Sprintf("%v; persist observation after rollback: %v", cause, err)
			_ = journals.Save(journal)
			report.RollbackError = fmt.Errorf("persist observation after rollback: %w", err)
			return report
		}
	}
	if err := journals.Remove(journal.OpID); err != nil {
		report.RollbackError = fmt.Errorf("remove recovered journal: %w", err)
		return report
	}
	return report
}

// rollbackEmptyBaseline best-effort restores the pre-apply checkpoint and runs
// rescue cleanup when there is no SpecCaptured applied source. Cleanup failures
// are recorded but the journal is still removed so serve can start again (B1).
func (t *ConfigTxn) rollbackEmptyBaseline(ctx context.Context, journals *state.TransactionStore, journal state.TransactionJournal, selected map[string]string, checkpoint configapply.Checkpoint, cause error) error {
	journal.Phase = state.TransactionRecoveryRequired
	journal.LastError = cause.Error()
	if err := journals.Save(journal); err != nil {
		return fmt.Errorf("%v; persist recovery journal: %w", cause, err)
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	emptyPrepared, prepErr := t.emptyBaselineRollbackPrepared(journal.OpID, journal.Target.Applied.ExecutionGraph.Workers)
	if prepErr != nil {
		journal.LastError = fmt.Sprintf("%v; prepare empty-baseline rollback: %v", cause, prepErr)
		_ = journals.Save(journal)
	} else {
		if _, rollbackErr := t.engine.Rollback(recoveryCtx, emptyPrepared, selected, checkpoint); rollbackErr != nil {
			journal.LastError = fmt.Sprintf("%v; empty-baseline rollback: %v", cause, rollbackErr)
			_ = journals.Save(journal)
		}
	}

	source := journal.Source
	source.Runtime.CommitInFlight = nil
	now := time.Now().UTC()
	source.Runtime.LastRollbackAt = &now
	if err := t.persistControlWithoutApplied(recoveryCtx, source); err != nil {
		// Still drop the journal below so a control-plane write failure cannot brick serve.
		journal.LastError = fmt.Sprintf("%v; restore empty-baseline control plane: %v", cause, err)
		_ = journals.Save(journal)
		_ = journals.Remove(journal.OpID)
		return fmt.Errorf("%v; restore empty-baseline control plane: %w", cause, err)
	}
	if err := journals.Remove(journal.OpID); err != nil {
		return fmt.Errorf("%v; remove empty-baseline journal: %w", cause, err)
	}
	if prepErr != nil {
		return fmt.Errorf("%w; prepare empty-baseline rollback: %v", cause, prepErr)
	}
	if journal.LastError != cause.Error() {
		return fmt.Errorf("%s (empty-baseline cleanup best-effort; journal cleared)", journal.LastError)
	}
	return fmt.Errorf("%w (empty-baseline cleanup; journal cleared)", cause)
}

// emptyBaselineRollbackPrepared builds a synthetic prepared target that only
// runs the owner-tagged rescue cleanup plan. Used when there is no captured
// applied source to regenerate (first-ever apply failure / B1).
func (t *ConfigTxn) emptyBaselineRollbackPrepared(opID string, workers []state.WorkerTarget) (configapply.Prepared, error) {
	owners := t.commit.Owners()
	if owners == nil {
		owners = netgate.NewOwnerRegistry("smartgate")
	}
	plan, err := netgate.OpenWrtRescueCleanupDryRunPlan(
		suffixedOpID(opID, "-empty-baseline"),
		owners,
		workerCleanupTargetsFromGraph(workers),
	)
	if err != nil {
		return configapply.Prepared{}, err
	}
	// GatewayModeOff avoids requiring a data-plane checkpoint in engine.Rollback.
	return configapply.Prepared{
		OpID:        plan.OpID,
		GatewayMode: state.GatewayModeOff,
		OpenWrtPlan: plan,
		GraphHash:   planner.HashExecutionGraph(state.ExecutionGraph{}),
	}, nil
}

// recoverUncapturedInterrupted clears a residual journal that never had a
// SpecCaptured applied source. Prefer data-plane rescue cleanup; if cleanup
// itself fails, still drop the journal so the daemon can boot (LAN stays
// fail-open).
func (t *ConfigTxn) recoverUncapturedInterrupted(ctx context.Context, transactions *state.TransactionStore, journal state.TransactionJournal, recovered state.TransactionState) error {
	emptyPrepared, err := t.emptyBaselineRollbackPrepared(journal.OpID, journal.Target.Applied.ExecutionGraph.Workers)
	if err != nil {
		journal.LastError = fmt.Sprintf("prepare empty-baseline recovery: %v", err)
		_ = transactions.Save(journal)
	} else {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, rollbackErr := t.engine.Rollback(recoveryCtx, emptyPrepared, recovered.Runtime.ProxySelection, configapply.Checkpoint{}); rollbackErr != nil {
			journal.LastError = fmt.Sprintf("empty-baseline recovery cleanup: %v", rollbackErr)
			_ = transactions.Save(journal)
		}
	}
	if err := t.persistControlWithoutApplied(ctx, recovered); err != nil {
		journal.LastError = fmt.Sprintf("persist empty-baseline control plane: %v", err)
		_ = transactions.Save(journal)
		return fmt.Errorf("persist empty-baseline control plane: %w", err)
	}
	if err := state.NewSnapshotStore(t.commit.Layout()).RemovePreimage(journal.OpID); err != nil {
		return err
	}
	return transactions.Remove(journal.OpID)
}

func workerCleanupTargetsFromGraph(workers []state.WorkerTarget) []netgate.WorkerCleanupTarget {
	targets := make([]netgate.WorkerCleanupTarget, len(workers))
	for index, worker := range workers {
		targets[index] = netgate.WorkerCleanupTarget{ID: worker.ID, DisplayName: worker.ID}
	}
	return targets
}

func (t *ConfigTxn) persistState(ctx context.Context, next state.TransactionState) error {
	if _, err := t.store.SaveSpec(next.Spec); err != nil {
		return err
	}
	if next.LastGoodCaptured {
		if _, err := provider.NewLastGoodStore(t.commit.Layout()).ReplaceAll(ctx, lastGoodSnapshotUpdates(next.LastGood)); err != nil {
			return err
		}
	}
	if next.FailoverCaptured {
		if err := state.NewFailoverStateStore(t.commit.Layout()).Save(next.Failover); err != nil {
			return err
		}
	}
	if err := t.store.SaveRuntime(next.Runtime); err != nil {
		return err
	}
	selection, err := transactionSelectionForPersistence(next)
	if err != nil {
		return err
	}
	if err := t.store.SaveSelectionState(selection); err != nil {
		return err
	}
	return t.store.SaveAppliedState(next.Applied)
}

// persistControlWithoutApplied restores saved/runtime control state after a
// failed or interrupted transaction. AppliedState is intentionally excluded:
// only a successfully committed ConfigTxn may advance or rewrite it.
func (t *ConfigTxn) persistControlWithoutApplied(ctx context.Context, next state.TransactionState) error {
	if _, err := t.store.SaveSpec(next.Spec); err != nil {
		return err
	}
	if next.LastGoodCaptured {
		if _, err := provider.NewLastGoodStore(t.commit.Layout()).ReplaceAll(ctx, lastGoodSnapshotUpdates(next.LastGood)); err != nil {
			return err
		}
	}
	if next.FailoverCaptured {
		if err := state.NewFailoverStateStore(t.commit.Layout()).Save(next.Failover); err != nil {
			return err
		}
	}
	if err := t.store.SaveRuntime(next.Runtime); err != nil {
		return err
	}
	if next.Applied.SchemaVersion != 0 {
		selection, err := transactionSelectionForPersistence(next)
		if err != nil {
			return err
		}
		return t.store.SaveSelectionState(selection)
	}
	return nil
}

func (t *ConfigTxn) loadSelectionState(applied state.AppliedState) (state.SelectionState, bool, error) {
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return state.SelectionState{}, false, nil
	}
	selection, err := t.store.LoadSelectionForApplied(applied)
	if err != nil {
		return state.SelectionState{}, false, err
	}
	return copySelectionState(selection), true, nil
}

func transactionSelectionForPersistence(next state.TransactionState) (state.SelectionState, error) {
	if next.Applied.SchemaVersion == 0 || !next.Applied.SpecCaptured {
		return state.SelectionState{}, nil
	}
	if !next.SelectionCaptured {
		return state.SelectionStateForApplied(next.Applied, next.Runtime.ProxySelection), nil
	}
	selection := copySelectionState(next.Selection)
	if !state.SelectionStateMatchesApplied(selection, next.Applied) {
		return state.SelectionState{}, fmt.Errorf("captured selection does not match applied topology")
	}
	if !selectionMapsEqual(selection.ProxySelection, next.Runtime.ProxySelection) {
		return state.SelectionState{}, fmt.Errorf("captured selection does not match runtime selection")
	}
	return selection, nil
}

func setTransactionSelections(target *state.TransactionState, selections map[string]string) {
	if target == nil {
		return
	}
	target.Runtime.ProxySelection = copyStringMap(selections)
	if target.Applied.SchemaVersion == 0 || !target.Applied.SpecCaptured {
		target.SelectionCaptured = false
		target.Selection = state.SelectionState{}
		return
	}
	if target.SelectionCaptured && selectionMapsEqual(target.Selection.ProxySelection, selections) && state.SelectionStateMatchesApplied(target.Selection, target.Applied) {
		return
	}
	target.SelectionCaptured = true
	target.Selection = state.SelectionStateForApplied(target.Applied, selections)
}

func copySelectionState(selection state.SelectionState) state.SelectionState {
	selection.ProxySelection = copyStringMap(selection.ProxySelection)
	if len(selection.Selections) == 0 {
		selection.Selections = nil
		return selection
	}
	selections := make(map[string]state.PoolSelection, len(selection.Selections))
	for pool, item := range selection.Selections {
		selections[pool] = item
	}
	selection.Selections = selections
	return selection
}

func copyPoolSelections(in map[string]state.PoolSelection) map[string]state.PoolSelection {
	if in == nil {
		return nil
	}
	out := make(map[string]state.PoolSelection, len(in))
	for pool, selection := range in {
		out[pool] = selection
	}
	return out
}

func copyRuleSetRefs(in []state.RuleSetSnapshotRef) []state.RuleSetSnapshotRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]state.RuleSetSnapshotRef, len(in))
	copy(out, in)
	return out
}

func reconcileTransactionFailover(current state.FailoverState, prepared configapply.Prepared, selections, requested map[string]string) state.FailoverState {
	current = state.NormalizeFailoverState(current)
	selectors := selection.BuildSelectorStates(prepared.Nodes, prepared.Target.Spec.BusinessPools, prepared.GatewayMode, selection.DefaultMaxPoolCandidates, selections)
	now := time.Now().UTC()
	active := make(map[string]struct{}, len(selectors))
	for _, selector := range selectors {
		active[selector.PoolID] = struct{}{}
		pool := current.Pools[selector.PoolID]
		previousCurrent := pool.Current
		pool.PoolID = selector.PoolID
		pool.Role = selector.Role
		pool.Current = selector.Selected
		if previousCurrent == selector.Selected {
			if pool.Previous != "" && !containsSelection(selector.Candidates, pool.Previous) {
				pool.Previous = ""
				pool.CooldownUntil = nil
			}
			current.Pools[selector.PoolID] = pool
			continue
		}
		if requested[selector.PoolID] != "" && requested[selector.PoolID] == selector.Selected {
			pool.Previous = ""
			pool.LastKind = "restore"
			pool.LastReason = "config_apply_restore_proxy_selection"
		} else if previousCurrent != selector.Selected {
			pool.Previous = ""
			pool.LastKind = "sync"
			pool.LastReason = "config_apply_sync_selector_state"
		}
		pool.LastChangedAt = &now
		pool.CooldownUntil = nil
		pool.Terminal = false
		current.Pools[selector.PoolID] = pool
	}
	for poolID := range current.Pools {
		if _, ok := active[poolID]; !ok {
			delete(current.Pools, poolID)
		}
	}
	for poolID := range current.Locks {
		if _, ok := active[poolID]; !ok {
			delete(current.Locks, poolID)
		}
	}
	return current
}

func containsSelection(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validatePreparedTarget(prepared configapply.Prepared) error {
	conflicts, err := state.RuleConflicts(prepared.Target.Spec)
	if err != nil {
		return fmt.Errorf("prepared rules are invalid: %w", err)
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("prepared target has unresolved rule conflict %s", conflicts[0].RuleKey)
	}
	if provider.HashSpec(prepared.Target.Spec) != prepared.SpecHash {
		return fmt.Errorf("prepared spec hash mismatch")
	}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(prepared.Target.Spec, prepared.Target.LastGood)
	if err != nil {
		return err
	}
	if topologyHash != prepared.TopologyHash {
		return fmt.Errorf("prepared topology hash mismatch")
	}
	if state.EffectiveGatewayMode(prepared.Target.Spec.Settings.GatewayMode) != state.EffectiveGatewayMode(prepared.GatewayMode) {
		return fmt.Errorf("prepared gateway mode mismatch")
	}
	if prepared.OpenWrtPlan.OpID != prepared.OpID || !prepared.OpenWrtPlan.DryRunOnly {
		return fmt.Errorf("prepared OpenWrt plan is not bound to the dry-run")
	}
	dataPlaneMode := effectiveGatewayModeRunsDataPlane(prepared.GatewayMode)
	if prepared.GraphHash == "" {
		return fmt.Errorf("prepared execution graph is required")
	}
	if planner.HashExecutionGraph(prepared.ExecutionGraph) != prepared.GraphHash {
		return fmt.Errorf("prepared execution graph hash mismatch")
	}
	if dataPlaneMode {
		if _, err := fabric.ClassifierFromGraph(prepared.ExecutionGraph.Fabric); err != nil {
			return fmt.Errorf("prepared Gateway Fabric classifier is invalid: %w", err)
		}
		if prepared.Workers == nil || prepared.Workers.OpID != prepared.OpID {
			return fmt.Errorf("prepared workers are not bound to the dry-run")
		}
		if prepared.Fabric == nil || prepared.Fabric.OpID != prepared.OpID || prepared.Fabric.Plan.OpID != prepared.OpID || !prepared.Fabric.Plan.DryRunOnly {
			return fmt.Errorf("prepared Gateway Fabric is not bound to the dry-run")
		}
	}
	return nil
}

func buildTargetApplied(previous state.AppliedState, prepared configapply.Prepared, selected map[string]string, kind string) state.AppliedState {
	now := time.Now().UTC()
	next := previous
	next.ConfigEpoch++
	next.TopologyHash = prepared.TopologyHash
	next.GatewayMode = state.EffectiveGatewayMode(prepared.GatewayMode)
	next.ConfigSHA256 = ""
	next.ExecutionGraph = prepared.ExecutionGraph
	next.ConfigSHA256 = prepared.GraphHash
	next.ProxySelection = copyStringMap(selected)
	switch kind {
	case "snapshot_rollback":
		next.LastRollbackAt = &now
	case "backup_restore":
		next.LastRestoreAt = &now
	default:
		next.LastAppliedAt = &now
	}
	return state.AppliedStateWithTarget(next, prepared.Target.Spec, prepared.Target.LastGood)
}

func effectiveGatewayModeRunsDataPlane(mode string) bool {
	switch state.EffectiveGatewayMode(mode) {
	case state.GatewayModeOn, state.GatewayModeGlobal:
		return true
	default:
		return false
	}
}

func runtimeFromApplied(runtimeState state.RuntimeState, applied state.AppliedState) state.RuntimeState {
	runtimeState = runtimeMetadataFromApplied(runtimeState, applied)
	runtimeState.ProxySelection = copyStringMap(applied.ProxySelection)
	return runtimeState
}

func runtimeMetadataFromApplied(runtimeState state.RuntimeState, applied state.AppliedState) state.RuntimeState {
	runtimeState.ConfigEpoch = applied.ConfigEpoch
	runtimeState.TopologyHash = applied.TopologyHash
	runtimeState.GatewayMode = applied.GatewayMode
	runtimeState.LastAppliedAt = applied.LastAppliedAt
	runtimeState.LastRollbackAt = applied.LastRollbackAt
	runtimeState.LastRestoreAt = applied.LastRestoreAt
	return runtimeState
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func suffixedOpID(opID, suffix string) string {
	limit := opid.MaxLength - len(suffix)
	if len(opID) > limit {
		opID = opID[:limit]
	}
	return opID + suffix
}
