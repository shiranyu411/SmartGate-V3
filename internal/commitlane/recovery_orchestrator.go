package commitlane

import (
	"context"
	"fmt"
	"log"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	selectionapi "smartgate/internal/selection"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

// RecoveryOrchestrator is the single entry point for data-plane transaction
// recovery. Config recovery runs first so the applied topology is restored and
// verified before selector journals replay the latest durable runtime choice.
type RecoveryOrchestrator struct {
	config     *ConfigTxn
	selector   *SelectorTxn
	legacyCore smartnode.CoreID
}

func NewRecoveryOrchestrator(config *ConfigTxn, selector *SelectorTxn) *RecoveryOrchestrator {
	return &RecoveryOrchestrator{config: config, selector: selector}
}

// Recover is used during daemon startup before the HTTP server begins
// listening. Both transaction families must recover successfully.
func (r *RecoveryOrchestrator) Recover(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if report, err := r.MigrateLegacyBindings(ctx); err != nil {
		return err
	} else if report != nil {
		log.Printf("legacy node bindings migrated: resolved=%d unresolved=%d warnings=%d", len(report.Resolved), len(report.Unresolved), len(report.Warnings))
		for _, warning := range report.Warnings {
			log.Printf("legacy node binding migration: %s", warning)
		}
	}
	pendingAtStart, err := state.NewTransactionStore(r.config.commit.Layout()).List()
	if err != nil {
		return err
	}
	if err := r.config.Recover(ctx); err != nil {
		return fmt.Errorf("recover config transactions: %w", err)
	}
	if err := r.selector.Recover(ctx); err != nil {
		return fmt.Errorf("recover selector transactions: %w", err)
	}
	if active, err := rescueFlagActive(r.config.commit.Layout()); err != nil {
		return err
	} else if active {
		// Non-committed journals are intentionally retained while rescue is
		// active. Allow the gated HTTP server to start so the owner can invoke
		// rescue-clear, which performs full recovery and strict finalization.
		return nil
	}
	if r.selector.graphConfigured() {
		if err := r.config.ReconcileAppliedDataPlane(ctx, "startup-cutover-reconcile"); err != nil {
			return fmt.Errorf("reconcile recovered execution graph: %w", err)
		}
		r.selector.ResumeDrains()
	}
	// A recovered transaction must be verified against the live data plane
	// before its recovery material is considered finalized. On an ordinary
	// journal-free boot without pending recovery material, the data-plane
	// supervisor observes subsequent worker/fabric failures.
	return r.finalizeRecovery(ctx, len(pendingAtStart) != 0)
}

// RecoverForRescueClear runs while the rescue gate remains active. Config
// recovery includes a full applied data-plane reconcile; selector recovery is
// deliberately last so the durable runtime choice is the final live choice.
// The caller may remove the rescue flag only after this method returns nil.
func (r *RecoveryOrchestrator) RecoverForRescueClear(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if _, err := r.MigrateLegacyBindings(ctx); err != nil {
		return err
	}
	if err := r.config.RecoverForRescueClear(ctx); err != nil {
		return fmt.Errorf("recover config transactions: %w", err)
	}
	if err := r.selector.RecoverForRescueClear(ctx); err != nil {
		return fmt.Errorf("recover selector transactions: %w", err)
	}
	if r.selector.graphConfigured() {
		if err := r.config.ReconcileAppliedDataPlane(ctx, "rescue-clear-cutover-reconcile"); err != nil {
			return fmt.Errorf("reconcile recovered execution graph: %w", err)
		}
		r.selector.ResumeDrains()
	}
	return r.finalizeRecovery(ctx, true)
}

func (r *RecoveryOrchestrator) finalizeRecovery(ctx context.Context, verifyLive bool) error {
	_, err := r.config.commit.WithCommit(ctx, CommitRequest{OpID: "recovery-finalize", Kind: "recovery", StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
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

		journals, err := state.NewTransactionStore(r.config.commit.Layout()).List()
		if err != nil {
			return err
		}
		if len(journals) != 0 {
			return fmt.Errorf("recovery left transaction %s (%s) in phase %s", journals[0].OpID, journals[0].Kind, journals[0].Phase)
		}
		applied, err := r.config.store.LoadAppliedState()
		if err != nil {
			return err
		}
		selection, err := r.config.store.LoadSelectionState()
		if err != nil {
			return err
		}
		if applied.SchemaVersion == 0 || !applied.SpecCaptured {
			if selection.SchemaVersion == 0 {
				return nil
			}
			// Legacy/pre-capture upgrades may already have durable SelectionState
			// while AppliedState only holds metadata (or is missing). Capture the
			// currently saved Spec+last-good as applied topology so the new
			// authority model can start without crash-looping the daemon.
			migrated, err := r.captureLegacyAppliedTopology(ctx, applied, selection)
			if err != nil {
				return fmt.Errorf("migrate legacy applied capture: %w", err)
			}
			applied = migrated
			selection, err = r.config.store.LoadSelectionState()
			if err != nil {
				return err
			}
		}
		if selection.SchemaVersion == 0 {
			selection = state.SelectionStateForApplied(applied, applied.ProxySelection)
			if err := r.config.store.SaveSelectionState(selection); err != nil {
				return err
			}
		}
		if !state.SelectionStateMatchesApplied(selection, applied) {
			return fmt.Errorf("durable selection does not match recovered applied topology")
		}
		if verifyLive {
			releaseNet, err := session.Acquire(lockset.LevelNet)
			if err != nil {
				return err
			}
			defer releaseNet()
			if err := r.reconcileLiveSelectionsLocked(ctx, applied, selection); err != nil {
				return err
			}
			if err := r.config.store.SaveDataPlaneObservation(state.DataPlaneObservationForMode(applied.GatewayMode)); err != nil {
				return err
			}
		}
		runtimeState, err := r.config.store.LoadRuntime()
		if err != nil {
			return err
		}
		runtimeState = runtimeMetadataFromApplied(runtimeState, applied)
		runtimeState.ProxySelection = copyStringMap(selection.ProxySelection)
		runtimeState.CommitInFlight = nil
		runtimeState.RecoveryError = ""
		if err := r.config.store.SaveRuntime(runtimeState); err != nil {
			return err
		}
		verified, err := r.config.store.LoadAppliedTopology()
		if err != nil {
			return err
		}
		if verified.ConfigEpoch != runtimeState.ConfigEpoch || verified.TopologyHash != runtimeState.TopologyHash {
			return fmt.Errorf("recovered runtime does not match applied topology")
		}
		return nil
	})
	return err
}

// captureLegacyAppliedTopology builds a SpecCaptured AppliedState from the
// current saved Spec and last-good snapshots. It is only used when a durable
// SelectionState already exists without a captured applied topology (legacy
// RuntimeDir-only selector persistence upgraded onto the new control plane).
func (r *RecoveryOrchestrator) captureLegacyAppliedTopology(ctx context.Context, applied state.AppliedState, selection state.SelectionState) (state.AppliedState, error) {
	spec, err := r.config.store.LoadSpec()
	if err != nil {
		return state.AppliedState{}, err
	}
	lastGood, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(r.config.commit.Layout()))
	if err != nil {
		return state.AppliedState{}, err
	}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, lastGood)
	if err != nil {
		return state.AppliedState{}, err
	}
	runtimeState, err := r.config.store.LoadRuntime()
	if err != nil {
		return state.AppliedState{}, err
	}
	epoch := applied.ConfigEpoch
	if epoch == 0 {
		epoch = selection.ConfigEpoch
	}
	if epoch == 0 {
		epoch = runtimeState.ConfigEpoch
	}
	mode := state.EffectiveGatewayMode(applied.GatewayMode)
	if mode == "" {
		mode = state.EffectiveGatewayMode(spec.Settings.GatewayMode)
	}
	if mode == "" {
		mode = state.GatewayModeOff
	}
	selections := copyStringMap(selection.ProxySelection)
	if len(selections) == 0 {
		selections = copyStringMap(runtimeState.ProxySelection)
	}
	if len(selections) == 0 {
		selections = copyStringMap(applied.ProxySelection)
	}
	next := state.AppliedStateWithTarget(state.AppliedState{
		SchemaVersion:  state.AppliedStateSchemaVersion,
		ConfigEpoch:    epoch,
		TopologyHash:   topologyHash,
		GatewayMode:    mode,
		LastAppliedAt:  runtimeState.LastAppliedAt,
		ProxySelection: selections,
	}, spec, lastGood)
	if err := r.config.store.SaveAppliedState(next); err != nil {
		return state.AppliedState{}, err
	}
	bound := state.SelectionStateForApplied(next, selections)
	if err := r.config.store.SaveSelectionState(bound); err != nil {
		return state.AppliedState{}, err
	}
	return next, nil
}

// ReconcileLiveSelectionsLocked reapplies and verifies the durable selector
// choices after the data plane has been restarted. The caller must already hold the
// commit lane and Store, Runtime and Net locks in hierarchy order.
func (r *RecoveryOrchestrator) ReconcileLiveSelectionsLocked(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	applied, err := r.config.store.LoadAppliedState()
	if err != nil {
		return err
	}
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return nil
	}
	selection, err := r.config.store.LoadSelectionState()
	if err != nil {
		return err
	}
	if selection.SchemaVersion == 0 || !state.SelectionStateMatchesApplied(selection, applied) {
		return fmt.Errorf("durable selection does not match applied topology")
	}
	return r.reconcileLiveSelectionsLocked(ctx, applied, selection)
}

// ReconcileLiveSelections serializes a restart-triggered selector reconcile
// with every other control-plane transaction. It does not modify AppliedState;
// the durable applied topology and SelectionState are read-only authorities.
func (r *RecoveryOrchestrator) ReconcileLiveSelections(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	_, err := r.config.commit.WithCommit(ctx, CommitRequest{
		OpID: "data-plane-restart-selection-reconcile", Kind: "recovery", StartedAt: time.Now().UTC(),
	}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.Acquire(level)
			if err != nil {
				return err
			}
			defer release()
		}
		return r.ReconcileLiveSelectionsLocked(ctx)
	})
	return err
}

func (r *RecoveryOrchestrator) reconcileLiveSelectionsLocked(ctx context.Context, applied state.AppliedState, selection state.SelectionState) error {
	mode := state.EffectiveGatewayMode(applied.GatewayMode)
	if mode != state.GatewayModeOn && mode != state.GatewayModeGlobal {
		return nil
	}
	if len(applied.ExecutionGraph.PoolBindings) > 0 {
		plan := planner.BuildExecutionGraph(applied.Spec, selection.Selections, applied.ExecutionGraph, time.Now().UTC())
		if !plan.ReadyToApply {
			return fmt.Errorf("durable selection cannot build an execution graph")
		}
		if err := validateGraphSelections(plan.Graph, selection.ProxySelection); err != nil {
			return err
		}
		if !r.selector.graphConfigured() {
			return nil
		}
		preparedFabric, ok := r.selector.fabric.Current()
		if !ok {
			return fmt.Errorf("Gateway Fabric is not applied")
		}
		for _, binding := range plan.Graph.PoolBindings {
			dispatch, ok := dispatchForPoolGraph(preparedFabric.Graph, binding.Pool)
			if !ok || dispatch.Binding != binding.ID || dispatch.Generation != binding.Generation {
				return fmt.Errorf("live Fabric Binding for pool %s does not match durable Generation", binding.Pool)
			}
			if binding.Target.Kind != "node" {
				continue
			}
			path, err := r.selector.workers.ResolvePath(binding.Target.Worker, binding.Pool, planner.BindingLane(binding.Generation), binding.Target.Executor)
			if err != nil {
				return err
			}
			if !r.selector.workers.IsReady(binding.Target.Worker) {
				return fmt.Errorf("worker %s for pool %s is not ready", binding.Target.Worker, binding.Pool)
			}
			// This runs under the Store/Runtime/Net locks on every supervisor
			// poll until it succeeds, so it must not probe the upstream node: an
			// unreachable node would otherwise stall the management API for the
			// probe timeout on each poll (seen on the devvm rehearsal). Node
			// reachability is the failover watchdog's job.
			// A restarted Worker loads the defaults captured at config apply.
			// A later Pool cutover can have a different durable target on the
			// same lane, so restore that target before verifying the selector.
			if err := r.selector.workers.SelectPath(ctx, path); err != nil {
				return fmt.Errorf("restore live path for pool %s: %w", binding.Pool, err)
			}
			if err := r.selector.workers.VerifyPathSelection(ctx, path); err != nil {
				return fmt.Errorf("verify live path for pool %s: %w", binding.Pool, err)
			}
		}
		return nil
	}
	reader, ok := r.selector.control.(selectionapi.ProxySelectionReader)
	if !ok {
		return fmt.Errorf("live selector verification is unavailable")
	}
	nodes, err := provider.BuildNodeViewsFromSnapshots(applied.Spec, applied.LastGood)
	if err != nil {
		return fmt.Errorf("build applied selector topology: %w", err)
	}
	selectors := selectionapi.BuildSelectorStates(nodes, applied.Spec.BusinessPools, mode, selectionapi.DefaultMaxPoolCandidates, selection.ProxySelection)
	active := make(map[string]struct{}, len(selectors))
	for _, selector := range selectors {
		active[selector.PoolID] = struct{}{}
		requested := selection.ProxySelection[selector.PoolID]
		if requested != "" && requested != selector.Selected {
			return fmt.Errorf("durable selection %s=%s is not valid in applied topology", selector.PoolID, requested)
		}
		if selector.Selected == "" {
			return fmt.Errorf("applied selector %s has no target", selector.PoolID)
		}
		if err := restoreAndVerifySelector(ctx, r.selector.control, reader, selector.Selector, selector.Selected); err != nil {
			return fmt.Errorf("restore live selector %s: %w", selector.PoolID, err)
		}
	}
	for poolID, target := range selection.ProxySelection {
		if target == "" {
			continue
		}
		if _, ok := active[poolID]; !ok {
			return fmt.Errorf("durable selection references missing applied pool %s", poolID)
		}
	}
	return nil
}

func validateGraphSelections(graph state.ExecutionGraph, selections map[string]string) error {
	active := make(map[string]string, len(graph.PoolBindings))
	for _, binding := range graph.PoolBindings {
		target := binding.Target.SmartNode
		switch binding.Target.Kind {
		case state.NoCandidateDirect:
			target = "Direct"
		case state.NoCandidateBlock:
			target = "Block"
		}
		active[binding.Pool] = target
		if requested := selections[binding.Pool]; requested != "" && requested != target {
			return fmt.Errorf("durable selection %s=%s does not match applied Binding target %s", binding.Pool, requested, target)
		}
	}
	for pool, target := range selections {
		if target == "" {
			continue
		}
		if _, ok := active[pool]; !ok {
			return fmt.Errorf("durable selection references missing applied pool %s", pool)
		}
	}
	return nil
}

func restoreAndVerifySelector(ctx context.Context, control selectionapi.ProxyController, reader selectionapi.ProxySelectionReader, selector, target string) error {
	const retryWindow = 8 * time.Second
	const retryInterval = 250 * time.Millisecond
	deadline := time.Now().Add(retryWindow)
	var lastErr error
	for {
		if err := control.Select(ctx, selector, target); err != nil {
			lastErr = err
		} else if live, err := reader.Selected(ctx, selector); err != nil {
			lastErr = err
		} else if live != target {
			lastErr = fmt.Errorf("live target is %s, expected %s", live, target)
		} else {
			return nil
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *RecoveryOrchestrator) validate() error {
	if r == nil || r.config == nil || r.selector == nil {
		return fmt.Errorf("data-plane recovery orchestrator is not fully configured")
	}
	return nil
}
