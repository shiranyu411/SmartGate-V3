package commitlane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"smartgate/internal/configapply"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type fakeConfigApplyEngine struct {
	prepared            []configapply.TargetState
	prepareErr          error
	applied             []configapply.Prepared
	failAt              map[int]error
	onApply             func(int)
	respectContext      bool
	returnNilSelections bool
	rollbackSelections  map[string]string
}

func (f *fakeConfigApplyEngine) Prepare(_ context.Context, opID string, target configapply.TargetState, epoch int64) (configapply.Prepared, error) {
	f.prepared = append(f.prepared, target)
	if f.prepareErr != nil {
		return configapply.Prepared{}, f.prepareErr
	}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(target.Spec, target.LastGood)
	if err != nil {
		return configapply.Prepared{}, err
	}
	prepared := configapply.Prepared{
		OpID:           opID,
		Target:         target,
		SpecHash:       provider.HashSpec(target.Spec),
		TopologyHash:   topologyHash,
		GatewayMode:    state.EffectiveGatewayMode(target.Spec.Settings.GatewayMode),
		ConfigEpoch:    epoch,
		OpenWrtPlan:    netgate.DryRunPlan{OpID: opID, DryRunOnly: true},
		ExecutionGraph: target.PreviousGraph,
		GraphHash:      planner.HashExecutionGraph(target.PreviousGraph),
	}
	return prepared, nil
}

func (f *fakeConfigApplyEngine) Checkpoint(string) (configapply.Checkpoint, error) {
	return configapply.Checkpoint{}, nil
}

func (f *fakeConfigApplyEngine) Apply(ctx context.Context, prepared configapply.Prepared, selected map[string]string) (configapply.Result, error) {
	f.applied = append(f.applied, prepared)
	if f.onApply != nil {
		f.onApply(len(f.applied))
	}
	if err := f.failAt[len(f.applied)]; err != nil {
		return configapply.Result{}, err
	}
	if f.respectContext && ctx.Err() != nil {
		return configapply.Result{}, ctx.Err()
	}
	if f.returnNilSelections {
		return configapply.Result{}, nil
	}
	return configapply.Result{FinalSelections: copyStringMap(selected)}, nil
}

func (f *fakeConfigApplyEngine) Rollback(ctx context.Context, prepared configapply.Prepared, selected map[string]string, _ configapply.Checkpoint) (configapply.Result, error) {
	result, err := f.Apply(ctx, prepared, selected)
	if err == nil && f.rollbackSelections != nil {
		result.FinalSelections = copyStringMap(f.rollbackSelections)
	}
	return result, err
}

func TestConfigTxnCommitsTargetStateAndClearsJournal(t *testing.T) {
	txn, store, layout, target, prepared, engine := newConfigTxnFixture(t)
	result, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{
		OpID:                 prepared.OpID,
		Prepared:             prepared,
		ExpectedSpecHash:     provider.HashSpec(target),
		ExpectedTopologyHash: prepared.TopologyHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied.ConfigEpoch != 2 || !result.Applied.SpecCaptured || provider.HashSpec(result.Applied.Spec) != provider.HashSpec(target) {
		t.Fatalf("unexpected applied target: %+v", result.Applied)
	}
	got, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigEpoch != 2 || got.TopologyHash != prepared.TopologyHash {
		t.Fatalf("target applied state not persisted: %+v", got)
	}
	if len(engine.applied) != 1 || provider.HashSpec(engine.applied[0].Target.Spec) != provider.HashSpec(target) {
		t.Fatalf("unexpected data-plane apply calls: %+v", engine.applied)
	}
	if _, err := state.NewTransactionStore(layout).Load(prepared.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal should be removed, got %v", err)
	}
}

func TestConfigTxnFailureRollsBackAppliedSourceButKeepsDesiredSpec(t *testing.T) {
	txn, store, layout, target, prepared, engine := newConfigTxnFixture(t)
	sourceSelection := state.SelectionState{
		SchemaVersion: state.SelectionStateSchemaVersion, ConfigEpoch: 1, TopologyHash: "source-topology",
		ProxySelection: map[string]string{"general": "node-a"},
		Selections: map[string]state.PoolSelection{
			"general": {SmartNode: "node-a", Target: "node", Generation: 9, BoundEpoch: 1, BoundTopologyHash: "source-topology"},
		},
	}
	if err := store.SaveSelectionState(sourceSelection); err != nil {
		t.Fatal(err)
	}
	engine.failAt = map[int]error{1: errors.New("target failed")}
	_, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{
		OpID:                 prepared.OpID,
		Prepared:             prepared,
		ExpectedSpecHash:     provider.HashSpec(target),
		ExpectedTopologyHash: prepared.TopologyHash,
	})
	if err == nil {
		t.Fatal("expected target apply to fail")
	}
	var rollbackErr *ConfigTxnRollbackError
	if !errors.As(err, &rollbackErr) || !rollbackErr.OK() {
		t.Fatalf("missing successful structured rollback result: %T %+v", err, rollbackErr)
	}
	if len(engine.applied) != 2 {
		t.Fatalf("expected target and rollback applies, got %d", len(engine.applied))
	}
	rolledBackSpec := engine.applied[1].Target.Spec
	if len(rolledBackSpec.Providers) != 0 {
		t.Fatalf("rollback used dirty desired spec instead of captured applied spec: %+v", rolledBackSpec.Providers)
	}
	desired, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if provider.HashSpec(desired) != provider.HashSpec(target) {
		t.Fatalf("dirty desired spec was lost during rollback: %+v", desired)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.ConfigEpoch != 1 || len(applied.Spec.Providers) != 0 || applied.LastRollbackAt != nil {
		t.Fatalf("failed transaction mutated applied state: %+v", applied)
	}
	selection, err := store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	if selection.Selections["general"] != sourceSelection.Selections["general"] {
		t.Fatalf("rollback did not restore exact Selection metadata: got=%+v want=%+v", selection.Selections["general"], sourceSelection.Selections["general"])
	}
	if _, err := state.NewTransactionStore(layout).Load(prepared.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered journal should be removed, got %v", err)
	}
}

func TestConfigTxnVirginApplyFailureDoesNotPoisonUnrecoverableJournal(t *testing.T) {
	// B1: first-ever apply has no SpecCaptured baseline. A data-plane failure must
	// not leave recovery_required that startup Recover cannot clear.
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	target := state.DefaultSpecState()
	target.Settings.GatewayMode = state.GatewayModeOff
	target.Providers = []state.ProviderSpec{{ID: "provider-a", Name: "Provider A", Type: provider.TypeManual, Enabled: true}}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSpec(target); err != nil {
		t.Fatal(err)
	}
	// No SaveAppliedState → SpecCaptured == false (virgin machine).
	if err := store.SaveRuntime(state.RuntimeState{}); err != nil {
		t.Fatal(err)
	}
	prepared := configapply.Prepared{
		OpID:           "virgin-apply",
		Target:         configapply.TargetState{Spec: target},
		SpecHash:       provider.HashSpec(target),
		TopologyHash:   topologyHash,
		GatewayMode:    state.GatewayModeOff,
		ConfigEpoch:    0,
		OpenWrtPlan:    netgate.DryRunPlan{OpID: "virgin-apply", DryRunOnly: true},
		ExecutionGraph: state.ExecutionGraph{Workers: []state.WorkerTarget{{ID: "worker-recovery"}}},
	}
	prepared.GraphHash = planner.HashExecutionGraph(prepared.ExecutionGraph)
	locks := lockset.New()
	manager := New(locks, layout, netgate.NewOwnerRegistry("smartgate"))
	engine := &fakeConfigApplyEngine{failAt: map[int]error{1: errors.New("worker wait ready timeout")}}
	txn := NewConfigTxn(manager, store, engine)

	_, applyErr := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{
		OpID:                 prepared.OpID,
		Prepared:             prepared,
		ExpectedSpecHash:     provider.HashSpec(target),
		ExpectedTopologyHash: prepared.TopologyHash,
	})
	if applyErr == nil {
		t.Fatal("expected virgin apply failure")
	}
	foundRecoveryWorker := false
	for _, applied := range engine.applied {
		for _, step := range applied.OpenWrtPlan.Steps {
			if step.Resource.Kind == netgate.KindWorkerService && step.Resource.ID == "worker-recovery" {
				foundRecoveryWorker = true
			}
		}
	}
	if !foundRecoveryWorker {
		t.Fatalf("empty-baseline cleanup omitted registry worker: %+v", engine.applied)
	}
	if _, err := state.NewTransactionStore(layout).Load(prepared.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("virgin apply failure left a residual journal: %v", err)
	}
	// Even if a residual journal were present, Recover must converge.
	if err := NewRecoveryOrchestrator(txn, NewSelectorTxn(manager, store, &selectorControlFake{})).Recover(context.Background()); err != nil {
		t.Fatalf("startup recovery after virgin apply failure: %v", err)
	}
	journals, err := state.NewTransactionStore(layout).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(journals) != 0 {
		t.Fatalf("journals remaining after recover: %+v", journals)
	}
	// Desired dirty spec is preserved; applied still not captured.
	desired, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if provider.HashSpec(desired) != provider.HashSpec(target) {
		t.Fatalf("desired spec lost: %+v", desired)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.SpecCaptured {
		t.Fatalf("virgin failure must not capture applied state: %+v", applied)
	}
}

func TestConfigTxnRecoverClearsUncapturedInterruptedJournal(t *testing.T) {
	// Residual recovery_required journal without SpecCaptured (pre-fix poison)
	// must be cleaned by Recover instead of hard-failing the daemon.
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{CommitInFlight: &state.CommitRef{OpID: "poisoned-virgin"}}); err != nil {
		t.Fatal(err)
	}
	journal := state.TransactionJournal{
		OpID:  "poisoned-virgin",
		Kind:  "config_apply",
		Phase: state.TransactionRecoveryRequired,
		Source: state.TransactionState{
			Spec:    spec,
			Runtime: state.RuntimeState{CommitInFlight: &state.CommitRef{OpID: "poisoned-virgin"}},
			// Applied.SpecCaptured remains false
		},
		LastError: "worker wait ready timeout; no captured applied source exists for safe rollback",
	}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	locks := lockset.New()
	manager := New(locks, layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewConfigTxn(manager, store, &fakeConfigApplyEngine{})
	if err := NewRecoveryOrchestrator(txn, NewSelectorTxn(manager, store, &selectorControlFake{})).Recover(context.Background()); err != nil {
		t.Fatalf("recover poisoned virgin journal: %v", err)
	}
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("poisoned journal remained: %v", err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.CommitInFlight != nil {
		t.Fatalf("commit in flight not cleared: %+v", runtimeState.CommitInFlight)
	}
}

func TestConfigTxnRollbackFailureLeavesRecoveryJournal(t *testing.T) {
	txn, _, layout, target, prepared, engine := newConfigTxnFixture(t)
	engine.failAt = map[int]error{1: errors.New("target failed"), 2: errors.New("rollback failed")}
	_, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{
		OpID:                 prepared.OpID,
		Prepared:             prepared,
		ExpectedSpecHash:     provider.HashSpec(target),
		ExpectedTopologyHash: prepared.TopologyHash,
	})
	if err == nil {
		t.Fatal("expected rollback failure")
	}
	var rollbackErr *ConfigTxnRollbackError
	if !errors.As(err, &rollbackErr) || rollbackErr.OK() || rollbackErr.RollbackError == nil {
		t.Fatalf("missing incomplete structured rollback result: %T %+v", err, rollbackErr)
	}
	journal, err := state.NewTransactionStore(layout).Load(prepared.OpID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Phase != state.TransactionRecoveryRequired || journal.LastError == "" {
		t.Fatalf("unexpected recovery journal: %+v", journal)
	}
}

func TestConfigTxnRejectsStaleEpochBeforeDataPlaneApply(t *testing.T) {
	txn, _, _, target, prepared, engine := newConfigTxnFixture(t)
	prepared.ConfigEpoch = 0
	_, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{
		OpID:                 prepared.OpID,
		Prepared:             prepared,
		ExpectedSpecHash:     provider.HashSpec(target),
		ExpectedTopologyHash: prepared.TopologyHash,
	})
	if err == nil {
		t.Fatal("expected stale epoch rejection")
	}
	if len(engine.applied) != 0 {
		t.Fatalf("stale request reached data plane: %d calls", len(engine.applied))
	}
}

func TestConfigTxnRejectsSameNodeWithChangedGenerationAfterDryRun(t *testing.T) {
	txn, store, _, target, prepared, engine := newConfigTxnFixture(t)
	expected := state.SelectionState{
		SchemaVersion: state.SelectionStateSchemaVersion, ConfigEpoch: 1, TopologyHash: "source-topology",
		ProxySelection: map[string]string{"general": "node-a"},
		Selections: map[string]state.PoolSelection{
			"general": {SmartNode: "node-a", Target: "node", Generation: 4, BoundEpoch: 1, BoundTopologyHash: "source-topology"},
		},
	}
	changed := expected
	changed.Selections = map[string]state.PoolSelection{
		"general": {SmartNode: "node-a", Target: "node", Generation: 6, BoundEpoch: 1, BoundTopologyHash: "source-topology"},
	}
	if err := store.SaveSelectionState(changed); err != nil {
		t.Fatal(err)
	}
	_, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{
		OpID: prepared.OpID, Prepared: prepared,
		ExpectedSpecHash: provider.HashSpec(target), ExpectedTopologyHash: prepared.TopologyHash,
		ExpectedSelection: copyStringMap(expected.ProxySelection), ExpectedSelections: copyPoolSelections(expected.Selections),
	})
	if err == nil || !strings.Contains(err.Error(), "persisted pool selection changed after dry-run") {
		t.Fatalf("expected Generation CAS rejection, got %v", err)
	}
	if len(engine.applied) != 0 {
		t.Fatalf("stale Selection reached data plane: %d calls", len(engine.applied))
	}
}

func TestConfigTxnRejectsConflictingFabricRulesBeforeJournal(t *testing.T) {
	txn, _, layout, _, prepared, engine := newConfigTxnFixture(t)
	prepared.GatewayMode = state.GatewayModeOn
	prepared.Target.Spec.Settings.GatewayMode = state.GatewayModeOn
	prepared.SpecHash = provider.HashSpec(prepared.Target.Spec)
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(prepared.Target.Spec, prepared.Target.LastGood)
	if err != nil {
		t.Fatal(err)
	}
	prepared.TopologyHash = topologyHash
	prepared.ExecutionGraph = state.ExecutionGraph{Fabric: state.FabricGraph{
		FallbackPool: "general",
		Dispatch: []state.PoolDispatchGraph{
			{Pool: "general", Binding: "general-binding", Generation: 1},
			{Pool: "ai", Binding: "ai-binding", Generation: 1},
		},
		Rules: []state.FabricRuleGraph{
			{ID: "general-rule", Kind: fabric.RuleDomainSuffix, Value: "example.com", Target: "general"},
			{ID: "ai-rule", Kind: fabric.RuleDomainSuffix, Value: "example.com", Target: "ai"},
		},
	}}
	prepared.GraphHash = planner.HashExecutionGraph(prepared.ExecutionGraph)
	prepared.Workers = &worker.Prepared{OpID: prepared.OpID}
	prepared.Fabric = &fabric.PreparedFabric{OpID: prepared.OpID, Plan: netgate.DryRunPlan{OpID: prepared.OpID, DryRunOnly: true}}

	_, err = txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{OpID: prepared.OpID, Prepared: prepared})
	if err == nil || !strings.Contains(err.Error(), "multiple owners") {
		t.Fatalf("conflicting fabric rules were not rejected: %v", err)
	}
	if len(engine.applied) != 0 {
		t.Fatalf("conflicting graph reached data plane: %d calls", len(engine.applied))
	}
	if _, err := state.NewTransactionStore(layout).Load(prepared.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting graph created a journal: %v", err)
	}
}

func TestConfigTxnGatewayOffKeepsPersistedSelections(t *testing.T) {
	txn, store, _, target, prepared, engine := newConfigTxnFixture(t)
	engine.returnNilSelections = true
	if _, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{
		OpID: prepared.OpID, Prepared: prepared, ExpectedSpecHash: provider.HashSpec(target), ExpectedTopologyHash: prepared.TopologyHash,
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.ProxySelection["general"] != "node-a" {
		t.Fatalf("gateway off discarded persisted selector: %+v", applied.ProxySelection)
	}
}

func TestConfigTxnRollbackDetachesFromCanceledRequest(t *testing.T) {
	txn, _, layout, target, prepared, engine := newConfigTxnFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	engine.respectContext = true
	engine.failAt = map[int]error{1: errors.New("target failed")}
	engine.onApply = func(call int) {
		if call == 1 {
			cancel()
		}
	}
	_, err := txn.ApplyPrepared(ctx, ConfigTxnApplyRequest{
		OpID: prepared.OpID, Prepared: prepared, ExpectedSpecHash: provider.HashSpec(target), ExpectedTopologyHash: prepared.TopologyHash,
	})
	if err == nil || !strings.Contains(err.Error(), "rolled back to prior applied state") {
		t.Fatalf("canceled request did not complete rollback: %v", err)
	}
	if _, err := state.NewTransactionStore(layout).Load(prepared.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left recovery journal: %v", err)
	}
}

func TestConfigTxnRollbackPersistsActualFallbackSelection(t *testing.T) {
	txn, store, _, target, prepared, engine := newConfigTxnFixture(t)
	engine.failAt = map[int]error{1: errors.New("target failed")}
	engine.rollbackSelections = map[string]string{"general": "Direct"}
	_, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{
		OpID: prepared.OpID, Prepared: prepared, ExpectedSpecHash: provider.HashSpec(target), ExpectedTopologyHash: prepared.TopologyHash,
	})
	if err == nil {
		t.Fatal("expected target failure")
	}
	applied, loadErr := store.LoadAppliedState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if applied.ProxySelection["general"] != "node-a" {
		t.Fatalf("rollback mutated applied selection: %+v", applied.ProxySelection)
	}
	runtimeState, loadErr := store.LoadRuntime()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if runtimeState.ProxySelection["general"] != "Direct" {
		t.Fatalf("rollback did not persist actual runtime fallback: %+v", runtimeState.ProxySelection)
	}
	failoverState, loadErr := state.NewFailoverStateStore(txn.commit.Layout()).Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if failoverState.Pools["general"].Current != "Direct" {
		t.Fatalf("rollback failover state did not match actual fallback: %+v", failoverState.Pools)
	}
}

func TestReconcileTransactionFailoverPreservesUnchangedCooldownAndPrevious(t *testing.T) {
	until := time.Now().UTC().Add(time.Minute)
	changed := time.Now().UTC().Add(-time.Minute)
	current := state.DefaultFailoverState()
	current.Pools["general"] = state.FailoverPoolState{
		PoolID: "general", Role: "general", Current: "node-a", Previous: "node-b",
		LastKind: "failover", LastReason: "health", LastChangedAt: &changed, CooldownUntil: &until,
	}
	prepared := configapply.Prepared{
		GatewayMode: state.GatewayModeOn,
		Target: configapply.TargetState{Spec: state.SpecState{BusinessPools: []state.BusinessPoolSpec{{
			ID: "general", Name: "General", Role: "general", NodeIDs: []string{"node-a", "node-b"},
		}}}},
		Nodes: []provider.NodeView{
			{ID: "node-a"},
			{ID: "node-b"},
		},
	}
	got := reconcileTransactionFailover(current, prepared, map[string]string{"general": "node-a"}, map[string]string{"general": "node-a"})
	pool := got.Pools["general"]
	if pool.Previous != "node-b" || pool.CooldownUntil == nil || !pool.CooldownUntil.Equal(until) || pool.LastReason != "health" {
		t.Fatalf("unchanged apply destroyed failover history: %+v", pool)
	}
}

func TestConfigTxnRecoverRestoresAppliedCaptureForInterruptedKinds(t *testing.T) {
	tests := []struct {
		kind  string
		phase state.TransactionPhase
	}{
		{kind: "config_apply", phase: state.TransactionPrepared},
		{kind: "snapshot_rollback", phase: state.TransactionApplying},
		{kind: "backup_restore", phase: state.TransactionRecoveryRequired},
	}
	for _, tt := range tests {
		t.Run(tt.kind+"/"+string(tt.phase), func(t *testing.T) {
			layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
			if err := layout.Ensure(); err != nil {
				t.Fatal(err)
			}
			store := state.NewStore(layout)
			commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
			engine := &fakeConfigApplyEngine{}

			appliedSpec := state.DefaultSpecState()
			appliedSpec.Settings.GatewayMode = state.GatewayModeOff
			appliedSpec.Providers = []state.ProviderSpec{{ID: "source", Name: "Source", Type: provider.TypeManual, Enabled: true}}
			appliedLastGood := []state.LastGoodSnapshot{{ProviderID: "source", Outbounds: []map[string]any{{"type": "vmess", "tag": "source-node", "server": "source.example"}}}}
			dirtySourceSpec := state.DefaultSpecState()
			dirtySourceSpec.Providers = []state.ProviderSpec{{ID: "dirty", Name: "Dirty", Type: provider.TypeManual, Enabled: true}}
			dirtySourceLastGood := []state.LastGoodSnapshot{{ProviderID: "dirty", Outbounds: []map[string]any{{"type": "vmess", "tag": "dirty-node", "server": "dirty.example"}}}}
			targetSpec := state.DefaultSpecState()
			targetSpec.Providers = []state.ProviderSpec{{ID: "target", Name: "Target", Type: provider.TypeManual, Enabled: true}}
			if _, err := store.SaveSpec(targetSpec); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.NewLastGoodStore(layout).ReplaceAll(context.Background(), []provider.LastGoodUpdate{{ProviderID: "target", Outbounds: []map[string]any{{"type": "vmess", "tag": "target-node", "server": "target.example"}}}}); err != nil {
				t.Fatal(err)
			}
			applied := state.AppliedStateWithTarget(state.AppliedState{
				ConfigEpoch:    7,
				TopologyHash:   "source-topology",
				GatewayMode:    state.GatewayModeOff,
				ProxySelection: map[string]string{"general": "source-node"},
			}, appliedSpec, appliedLastGood)
			runtimeState := state.RuntimeState{ConfigEpoch: 8, TopologyHash: "target-topology", CommitInFlight: &state.CommitRef{OpID: "recover-me"}}
			if err := store.SaveAppliedState(applied); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveRuntime(runtimeState); err != nil {
				t.Fatal(err)
			}
			journal := state.TransactionJournal{
				OpID:  "recover-me",
				Kind:  tt.kind,
				Phase: tt.phase,
				Source: state.TransactionState{
					Spec:             dirtySourceSpec,
					LastGoodCaptured: true,
					LastGood:         dirtySourceLastGood,
					Applied:          applied,
					Runtime:          state.RuntimeState{ConfigEpoch: 7, TopologyHash: "source-topology", ProxySelection: map[string]string{"general": "source-node"}, CommitInFlight: &state.CommitRef{OpID: "recover-me"}},
				},
				Target: state.TransactionState{Spec: targetSpec, Runtime: runtimeState},
			}
			if err := state.NewTransactionStore(layout).Save(journal); err != nil {
				t.Fatal(err)
			}
			if err := state.NewSnapshotStore(layout).SavePreimage(state.PreimageEnvelope{OpID: journal.OpID, Kind: tt.kind, Spec: dirtySourceSpec, SavedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}

			if err := NewConfigTxn(commit, store, engine).Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tt.phase == state.TransactionPrepared {
				if len(engine.prepared) != 0 || len(engine.applied) != 0 {
					t.Fatalf("prepared recovery unnecessarily touched data plane: prepared=%d applied=%d", len(engine.prepared), len(engine.applied))
				}
			} else {
				if len(engine.prepared) != 1 || provider.HashSpec(engine.prepared[0].Spec) != provider.HashSpec(appliedSpec) {
					t.Fatalf("recovery did not prepare captured applied spec: %+v", engine.prepared)
				}
				if len(engine.prepared[0].LastGood) != 1 || engine.prepared[0].LastGood[0].ProviderID != "source" {
					t.Fatalf("recovery did not prepare captured last-good: %+v", engine.prepared[0].LastGood)
				}
			}
			gotSpec, err := store.LoadSpec()
			if err != nil {
				t.Fatal(err)
			}
			if provider.HashSpec(gotSpec) != provider.HashSpec(dirtySourceSpec) {
				t.Fatalf("transaction-start desired spec was not preserved: %+v", gotSpec)
			}
			if _, err := provider.NewLastGoodStore(layout).Load("dirty"); err != nil {
				t.Fatalf("transaction-start desired last-good was not preserved: %v", err)
			}
			if _, err := provider.NewLastGoodStore(layout).Load("target"); !os.IsNotExist(err) {
				t.Fatalf("stale target last-good remains: %v", err)
			}
			if _, err := provider.NewLastGoodStore(layout).Load("source"); !os.IsNotExist(err) {
				t.Fatalf("applied-only last-good should not replace desired control state: %v", err)
			}
			gotApplied, err := store.LoadAppliedState()
			if err != nil {
				t.Fatal(err)
			}
			if gotApplied.ConfigEpoch != 7 || gotApplied.LastRollbackAt != nil || provider.HashSpec(gotApplied.Spec) != provider.HashSpec(appliedSpec) {
				t.Fatalf("interrupted transaction mutated applied state: %+v", gotApplied)
			}
			gotRuntime, err := store.LoadRuntime()
			if err != nil {
				t.Fatal(err)
			}
			if gotRuntime.ConfigEpoch != 7 || gotRuntime.CommitInFlight != nil || gotRuntime.LastRollbackAt == nil {
				t.Fatalf("runtime was not reconciled: %+v", gotRuntime)
			}
			if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal was not removed: %v", err)
			}
			if _, err := state.NewSnapshotStore(layout).LoadPreimage(journal.OpID); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preimage was not removed: %v", err)
			}
		})
	}
}

func TestConfigTxnRecoverCommittedJournalOnlyCleansUp(t *testing.T) {
	txn, store, layout, target, _, engine := newConfigTxnFixture(t)
	journal := state.TransactionJournal{OpID: "committed-config", Kind: "config_apply", Phase: state.TransactionCommitted}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	if err := state.NewSnapshotStore(layout).SavePreimage(state.PreimageEnvelope{OpID: journal.OpID, Kind: journal.Kind, Spec: target, SavedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.prepared) != 0 || len(engine.applied) != 0 {
		t.Fatalf("committed journal unexpectedly touched data plane: prepared=%d applied=%d", len(engine.prepared), len(engine.applied))
	}
	after, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if provider.HashSpec(after) != provider.HashSpec(before) {
		t.Fatal("committed cleanup changed control plane")
	}
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal was not removed: %v", err)
	}
}

func TestConfigTxnRecoverCommittedJournalReplaysDurableTarget(t *testing.T) {
	txn, store, layout, target, prepared, engine := newConfigTxnFixture(t)
	targetApplied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 8, TopologyHash: prepared.TopologyHash, GatewayMode: state.GatewayModeOff, ProxySelection: map[string]string{"general": "node-a"}}, target, nil)
	targetRuntime := state.RuntimeState{ConfigEpoch: 8, TopologyHash: prepared.TopologyHash, ProxySelection: map[string]string{"general": "node-a"}}
	targetSelection := state.SelectionState{
		SchemaVersion: state.SelectionStateSchemaVersion, ConfigEpoch: 8, TopologyHash: prepared.TopologyHash,
		ProxySelection: map[string]string{"general": "node-a"},
		Selections: map[string]state.PoolSelection{
			"general": {SmartNode: "node-a", Target: "node", Generation: 42, BoundEpoch: 8, BoundTopologyHash: prepared.TopologyHash},
		},
	}
	journal := state.TransactionJournal{
		OpID: "committed-replay", Kind: "config_apply", Phase: state.TransactionCommitted,
		Target: state.TransactionState{Spec: target, LastGoodCaptured: true, Applied: targetApplied, Runtime: targetRuntime, SelectionCaptured: true, Selection: targetSelection},
	}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	old := state.DefaultSpecState()
	old.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(old); err != nil {
		t.Fatal(err)
	}
	if err := txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigEpoch != 8 || provider.HashSpec(got.Spec) != provider.HashSpec(target) {
		t.Fatalf("committed target was not replayed: %+v", got)
	}
	selection, err := store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	if selection.Selections["general"] != targetSelection.Selections["general"] {
		t.Fatalf("committed replay lost Selection metadata: got=%+v want=%+v", selection.Selections["general"], targetSelection.Selections["general"])
	}
	if len(engine.applied) != 0 {
		t.Fatal("committed replay must not touch data plane")
	}
}

func TestConfigTxnRecoverCommittedLegacyJournalWritesIdentityOnlyAppliedState(t *testing.T) {
	txn, store, layout, target, _, engine := newConfigTxnFixture(t)
	legacyLastGood := []state.LastGoodSnapshot{{
		ProviderID: "provider-a",
		Outbounds: []map[string]any{{
			"type": "vmess", "tag": "legacy-journal-node", "server": "journal.example", "server_port": 443, "uuid": "journal-test-secret",
		}},
	}}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(target, legacyLastGood)
	if err != nil {
		t.Fatal(err)
	}
	targetApplied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 8, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOff,
	}, target, legacyLastGood)
	targetSelection := state.SelectionStateForApplied(targetApplied, nil)
	journal := state.TransactionJournal{
		OpID: "committed-legacy-last-good", Kind: "config_apply", Phase: state.TransactionCommitted,
		Target: state.TransactionState{
			Spec: target, LastGoodCaptured: true, LastGood: legacyLastGood,
			Applied: targetApplied, Runtime: state.RuntimeState{ConfigEpoch: 8, TopologyHash: topologyHash},
			SelectionCaptured: true, Selection: targetSelection,
		},
	}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	legacyJSON, err := os.ReadFile(layout.TransactionsDir() + "/" + journal.OpID + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacyJSON), `"outbounds"`) {
		t.Fatalf("legacy journal fixture does not contain outbounds: %s", legacyJSON)
	}
	if err := txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.applied) != 0 {
		t.Fatal("committed replay must not touch data plane")
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.LastGood) != 1 || len(applied.LastGood[0].Identities) != 1 || applied.LastGood[0].Identities[0].RawTag != "legacy-journal-node" || len(applied.LastGood[0].Outbounds) != 0 {
		t.Fatalf("committed replay applied state was not identity-only: %+v", applied.LastGood)
	}
	appliedJSON, err := os.ReadFile(layout.AppliedStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(appliedJSON), `"outbounds"`) {
		t.Fatalf("committed replay applied.json rewrote legacy outbounds: %s", appliedJSON)
	}
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed legacy journal was not removed: %v", err)
	}
}

func TestConfigTxnRescueClearReconcilesCommittedTargetDataPlane(t *testing.T) {
	txn, _, layout, target, prepared, engine := newConfigTxnFixture(t)
	targetApplied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 8, TopologyHash: prepared.TopologyHash, GatewayMode: state.GatewayModeOff,
	}, target, nil)
	journal := state.TransactionJournal{
		OpID: "committed-rescue-replay", Kind: "config_apply", Phase: state.TransactionCommitted,
		Target: state.TransactionState{
			Spec: target, LastGoodCaptured: true, Applied: targetApplied,
			Runtime:          state.RuntimeState{ConfigEpoch: 8, TopologyHash: prepared.TopologyHash},
			FailoverCaptured: true, Failover: state.DefaultFailoverState(),
		},
	}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	if err := txn.RecoverForRescueClear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.applied) != 1 {
		t.Fatalf("rescue clear did not reconcile committed target data plane: applied=%d", len(engine.applied))
	}
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed journal remained after reconcile: %v", err)
	}
}

func TestConfigTxnRescueClearReconcilesAppliedDataPlaneWithoutJournal(t *testing.T) {
	txn, _, _, _, _, engine := newConfigTxnFixture(t)
	if err := txn.RecoverForRescueClear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.applied) != 1 {
		t.Fatalf("rescue clear without journal did not reconcile applied data plane: %d", len(engine.applied))
	}
	if got := engine.applied[0].GatewayMode; got != state.GatewayModeOff {
		t.Fatalf("reconciled wrong applied gateway mode: %q", got)
	}
}

func TestConfigTxnRescueClearReconcilesPreparedSourceDataPlane(t *testing.T) {
	txn, store, layout, _, _, engine := newConfigTxnFixture(t)
	source, err := txn.loadSourceState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	journal := state.TransactionJournal{OpID: "prepared-rescue-replay", Kind: "config_apply", Phase: state.TransactionPrepared, Source: source}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	if err := txn.RecoverForRescueClear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.applied) != 1 {
		t.Fatalf("prepared rescue recovery did not reconcile source data plane: %d", len(engine.applied))
	}
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared journal remained after reconcile: %v", err)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.ConfigEpoch != source.Applied.ConfigEpoch {
		t.Fatalf("prepared recovery changed authoritative epoch: got %d want %d", applied.ConfigEpoch, source.Applied.ConfigEpoch)
	}
}

func TestConfigTxnRecoverDoesNotReenableDataPlaneDuringRescue(t *testing.T) {
	txn, _, layout, _, _, engine := newConfigTxnFixture(t)
	journal := state.TransactionJournal{OpID: "rescue-pending", Kind: "config_apply", Phase: state.TransactionApplying}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.RescueStatusPath(), []byte(`{"active":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.applied) != 0 || len(engine.prepared) != 0 {
		t.Fatalf("rescue recovery touched data plane: prepared=%d applied=%d", len(engine.prepared), len(engine.applied))
	}
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); err != nil {
		t.Fatalf("rescue must preserve pending journal: %v", err)
	}
}

func TestConfigTxnRecoverFailureKeepsRecoveryJournal(t *testing.T) {
	txn, _, layout, _, _, engine := newConfigTxnFixture(t)
	engine.failAt = map[int]error{1: errors.New("data plane unavailable")}
	appliedSpec := state.DefaultSpecState()
	journal := state.TransactionJournal{
		OpID:  "failed-recovery",
		Kind:  "backup_restore",
		Phase: state.TransactionApplying,
		Source: state.TransactionState{
			Applied: state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, GatewayMode: state.GatewayModeOff}, appliedSpec, nil),
			Runtime: state.RuntimeState{ConfigEpoch: 1},
		},
	}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	if err := txn.Recover(context.Background()); err == nil {
		t.Fatal("expected recovery failure")
	}
	got, err := state.NewTransactionStore(layout).Load(journal.OpID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != state.TransactionRecoveryRequired || got.LastError == "" {
		t.Fatalf("recovery journal was not retained with failure: %+v", got)
	}
}

func newConfigTxnFixture(t *testing.T) (*ConfigTxn, *state.Store, state.Layout, state.SpecState, configapply.Prepared, *fakeConfigApplyEngine) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	source := state.DefaultSpecState()
	source.Settings.GatewayMode = state.GatewayModeOff
	target := source
	target.Providers = []state.ProviderSpec{{ID: "provider-a", Name: "Provider A", Type: provider.TypeManual, Enabled: true}}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSpec(target); err != nil {
		t.Fatal(err)
	}
	runtimeState := state.RuntimeState{ConfigEpoch: 1, TopologyHash: "source-topology", ProxySelection: map[string]string{"general": "node-a"}}
	if err := store.SaveRuntime(runtimeState); err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, TopologyHash: "source-topology", GatewayMode: state.GatewayModeOff, ProxySelection: runtimeState.ProxySelection}, source, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	prepared := configapply.Prepared{
		OpID:           "config-apply-1",
		Target:         configapply.TargetState{Spec: target},
		SpecHash:       provider.HashSpec(target),
		TopologyHash:   topologyHash,
		GatewayMode:    state.GatewayModeOff,
		ConfigEpoch:    1,
		OpenWrtPlan:    netgate.DryRunPlan{OpID: "config-apply-1", DryRunOnly: true},
		ExecutionGraph: state.ExecutionGraph{},
		GraphHash:      planner.HashExecutionGraph(state.ExecutionGraph{}),
	}
	locks := lockset.New()
	manager := New(locks, layout, netgate.NewOwnerRegistry("smartgate"))
	engine := &fakeConfigApplyEngine{failAt: map[int]error{}}
	return NewConfigTxn(manager, store, engine), store, layout, target, prepared, engine
}

func TestConfigTxnLoadSourceStatePrefersDurableSelection(t *testing.T) {
	txn, store, _, _, _, _ := newConfigTxnFixture(t)
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	// RuntimeDir may lag after reboot; SelectionState is the durable authority.
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 1, TopologyHash: "source-topology", ProxySelection: map[string]string{"general": "runtime-stale"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(applied, map[string]string{"general": "durable-live"})); err != nil {
		t.Fatal(err)
	}
	source, err := txn.loadSourceState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if source.Runtime.ProxySelection["general"] != "durable-live" {
		t.Fatalf("loadSourceState ignored durable selection: %+v", source.Runtime.ProxySelection)
	}
}

func TestConfigTxnApplyPublishesDataPlaneObservation(t *testing.T) {
	txn, store, _, _, prepared, _ := newConfigTxnFixture(t)
	result, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{OpID: prepared.OpID, Prepared: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("missing apply result")
	}
	observation, err := store.LoadDataPlaneObservation()
	if err != nil {
		t.Fatal(err)
	}
	if observation.RunningGatewayMode != state.GatewayModeOff || observation.Planes[state.DataPlaneGateway].Running {
		t.Fatalf("config apply did not publish running observation: %+v", observation)
	}
}

func TestConfigTxnRollbackPublishesDataPlaneObservation(t *testing.T) {
	txn, store, _, _, prepared, engine := newConfigTxnFixture(t)
	// Source applied is off; force apply failure after data plane is touched so
	// rollback restores the prior applied mode and must publish observation.
	engine.failAt = map[int]error{1: errors.New("target failed")}
	// Publish a misleading observation as if a previous on mode was reported.
	if err := store.SaveDataPlaneObservation(state.DataPlaneObservationForGateway(state.GatewayModeOn, true)); err != nil {
		t.Fatal(err)
	}
	_, err := txn.ApplyPrepared(context.Background(), ConfigTxnApplyRequest{OpID: prepared.OpID, Prepared: prepared})
	if err == nil {
		t.Fatal("expected apply failure")
	}
	observation, err := store.LoadDataPlaneObservation()
	if err != nil {
		t.Fatal(err)
	}
	if observation.RunningGatewayMode != state.GatewayModeOff || observation.Planes[state.DataPlaneGateway].Running {
		t.Fatalf("rollback did not publish prior mode observation: %+v", observation)
	}
}

func TestNetworkRestoreKeepsDirtySpecAndDurableSelection(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			txn, store, layout, desired, _, engine := newConfigTxnFixture(t)
			applied, err := store.LoadAppliedState()
			if err != nil {
				t.Fatal(err)
			}
			applied.GatewayMode = state.GatewayModeOn
			applied.Spec.Settings.GatewayMode = state.GatewayModeOn
			if err := store.SaveAppliedState(applied); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveSelectionState(state.SelectionStateForApplied(applied, map[string]string{"general": "durable-choice"})); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(layout.SpecPath("spec"))
			if fail {
				engine.failAt[1] = errors.New("restore failed")
			}
			err = txn.ReconcileNetwork(context.Background())
			if fail != (err != nil) {
				t.Fatalf("restore err=%v", err)
			}
			after, _ := os.ReadFile(layout.SpecPath("spec"))
			if string(before) != string(after) {
				t.Fatal("dirty Spec changed")
			}
			saved, _ := store.LoadSpec()
			if provider.HashSpec(saved) != provider.HashSpec(desired) {
				t.Fatal("desired config changed")
			}
			if len(engine.prepared) != 1 || provider.HashSpec(engine.prepared[0].Spec) != provider.HashSpec(applied.Spec) || engine.prepared[0].ProxySelection["general"] != "durable-choice" {
				t.Fatalf("did not restore captured config and choice: %+v", engine.prepared)
			}
			if fail && len(engine.applied) != 2 {
				t.Fatal("restore failure did not roll back")
			}
			final, _ := store.LoadAppliedState()
			if final.ConfigEpoch != applied.ConfigEpoch {
				t.Fatal("repair advanced epoch")
			}
		})
	}
}

func TestNetworkRestoreRefusesDisabledRescueAndMissingCapture(t *testing.T) {
	for _, mode := range []string{"off", "safe", "rescue", "missing"} {
		t.Run(mode, func(t *testing.T) {
			txn, store, layout, _, _, engine := newConfigTxnFixture(t)
			applied, _ := store.LoadAppliedState()
			applied.GatewayMode = state.GatewayModeOn
			switch mode {
			case "off", "safe":
				applied.GatewayMode = mode
			case "missing":
				applied.SpecCaptured = false
			case "rescue":
				if err := os.WriteFile(layout.RescueStatusPath(), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SaveAppliedState(applied); err != nil {
				t.Fatal(err)
			}
			if err := txn.ReconcileNetwork(context.Background()); err == nil {
				t.Fatal("disabled network was enabled")
			}
			if len(engine.applied) != 0 || len(engine.prepared) != 0 {
				t.Fatal("guard ran data-plane actions")
			}
		})
	}
}
