package commitlane

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

func TestRecoveryOrchestratorFinalizesCommittedConfigCrashState(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	oldApplied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, TopologyHash: "old", GatewayMode: state.GatewayModeOff, ProxySelection: map[string]string{"general": "old-node"}}, spec, nil)
	newApplied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, TopologyHash: "new", GatewayMode: state.GatewayModeOff, ProxySelection: map[string]string{"general": "new-node"}}, spec, nil)
	newRuntime := state.RuntimeState{ConfigEpoch: 2, TopologyHash: "new", ProxySelection: map[string]string{"general": "new-node"}}
	if err := store.SaveAppliedState(oldApplied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(newApplied, newRuntime.ProxySelection)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(newRuntime); err != nil {
		t.Fatal(err)
	}
	journal := state.TransactionJournal{
		OpID: "committed-crash", Kind: "config_apply", Phase: state.TransactionCommitted,
		Source: state.TransactionState{Applied: oldApplied},
		Target: state.TransactionState{Spec: newApplied.Spec, LastGoodCaptured: true, Applied: newApplied, Runtime: newRuntime},
	}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	configTxn := NewConfigTxn(manager, store, &fakeConfigApplyEngine{})
	selectorTxn := NewSelectorTxn(manager, store, &selectorControlFake{current: "new-node"})
	if err := NewRecoveryOrchestrator(configTxn, selectorTxn).Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	selection, err := store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if applied.ConfigEpoch != 2 || selection.ConfigEpoch != 2 || runtimeState.ConfigEpoch != 2 || applied.TopologyHash != "new" || selection.TopologyHash != "new" || runtimeState.TopologyHash != "new" || runtimeState.ProxySelection["general"] != "new-node" {
		t.Fatalf("recovery did not converge control state: applied=%+v selection=%+v runtime=%+v", applied, selection, runtimeState)
	}
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed journal remained after recovery: %v", err)
	}
}

// The supervisor re-runs the live selection reconcile under the Store,
// Runtime and Net locks on every poll until it succeeds. On the on-mode devvm
// rehearsal the reconcile probed an unreachable upstream for 5s per poll and
// starved the management API (the deploy gate's 5s wget timed out). The
// reconcile must verify the live selector only; upstream reachability is the
// failover watchdog's job.
func TestReconcileLiveSelectionsVerifiesSelectionWithoutUpstreamProbe(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreA, commitCoreA)
	configTxn := NewConfigTxn(fixture.txn.commit, fixture.store, &fakeConfigApplyEngine{})
	if err := NewRecoveryOrchestrator(configTxn, fixture.txn).ReconcileLiveSelections(context.Background()); err != nil {
		t.Fatalf("live selection reconcile failed: %v", err)
	}
	fixture.faults.mu.Lock()
	defer fixture.faults.mu.Unlock()
	if len(fixture.faults.verifyCalls) != 0 {
		t.Fatalf("live selection reconcile probed the upstream node: %v", fixture.faults.verifyCalls)
	}
	if fixture.faults.selectionVerifyCalls == 0 {
		t.Fatal("live selection reconcile did not verify the live selector")
	}
}

// A core restart reloads the config's older defaults after runtime cutovers.
// Reconcile must restore durable choices locally before checking them.
func TestReconcileLiveSelectionsRestoresTargetsLostOnWorkerRestart(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreA, commitCoreA)
	before, err := fixture.store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	fixture.faults.mu.Lock()
	fixture.faults.liveRouteSelections = map[string]string{}
	fixture.faults.mu.Unlock()
	configTxn := NewConfigTxn(fixture.txn.commit, fixture.store, &fakeConfigApplyEngine{})
	if err := NewRecoveryOrchestrator(configTxn, fixture.txn).ReconcileLiveSelections(context.Background()); err != nil {
		t.Fatalf("restart reconcile did not restore durable targets: %v", err)
	}
	after, err := fixture.store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("restart reconcile changed durable selection: before=%+v after=%+v", before, after)
	}
	fixture.faults.mu.Lock()
	defer fixture.faults.mu.Unlock()
	if len(fixture.faults.liveRouteSelections) == 0 || len(fixture.faults.verifyCalls) != 0 {
		t.Fatalf("expected local restoration without upstream probes: routes=%v probes=%v", fixture.faults.liveRouteSelections, fixture.faults.verifyCalls)
	}
}

func TestJournalFreeStartupDefersLiveSelectionUntilExplicitDataPlaneReconcile(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOn
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider", Type: provider.TypeManual, Enabled: true}}
	raw := []map[string]any{
		{"type": "vmess", "tag": "first", "server": "first.example"},
		{"type": "vmess", "tag": "second", "server": "second.example"},
	}
	normalized := provider.NormalizeProviderOutbounds(spec.Providers[0], raw)
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{normalized[0].ID, normalized[1].ID}}}
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 3, TopologyHash: "topology", GatewayMode: state.GatewayModeOn,
		ProxySelection: map[string]string{"general": normalized[0].ID},
	}, spec, []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: raw}})
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(applied, map[string]string{"general": normalized[1].ID})); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 3, TopologyHash: "topology", ProxySelection: map[string]string{"general": normalized[0].ID}}); err != nil {
		t.Fatal(err)
	}
	control := &selectorControlFake{current: normalized[0].ID}
	orchestrator := NewRecoveryOrchestrator(NewConfigTxn(manager, store, &fakeConfigApplyEngine{}), NewSelectorTxn(manager, store, control))
	if err := orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if control.current != normalized[0].ID {
		t.Fatalf("journal-free startup touched not-yet-ready data plane: got %s", control.current)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.ProxySelection["general"] != normalized[1].ID || runtimeState.GatewayMode != state.GatewayModeOn {
		t.Fatalf("runtime metadata did not converge to durable selection: %+v", runtimeState)
	}
}

func TestRecoveryOrchestratorAllowsRescueServerWithDeferredJournal(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.RescueStatusPath(), []byte(`{"active":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, TopologyHash: "old"}, state.DefaultSpecState(), nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID: "deferred", Kind: "config_apply", Phase: state.TransactionApplying,
		Source: state.TransactionState{Applied: applied, Runtime: state.RuntimeState{ConfigEpoch: 1, TopologyHash: "old"}},
	}); err != nil {
		t.Fatal(err)
	}
	orchestrator := NewRecoveryOrchestrator(NewConfigTxn(manager, store, &fakeConfigApplyEngine{}), NewSelectorTxn(manager, store, &selectorControlFake{current: "Direct"}))
	if err := orchestrator.Recover(context.Background()); err != nil {
		t.Fatalf("rescue-gated startup was blocked: %v", err)
	}
	if _, err := state.NewTransactionStore(layout).Load("deferred"); err != nil {
		t.Fatalf("rescue startup consumed deferred recovery journal: %v", err)
	}
}

func TestFinalizeRecoveryMigratesLegacySelectionWithoutCapture(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOn
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	// No SpecCaptured applied state, but durable selection already exists (legacy upgrade).
	if err := store.SaveSelectionState(state.SelectionState{
		SchemaVersion:  state.SelectionStateSchemaVersion,
		ConfigEpoch:    7,
		TopologyHash:   "legacy-hash",
		ProxySelection: map[string]string{"general": "node-a"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 7, TopologyHash: "legacy-hash", ProxySelection: map[string]string{"general": "node-a"}}); err != nil {
		t.Fatal(err)
	}
	orch := NewRecoveryOrchestrator(NewConfigTxn(manager, store, &fakeConfigApplyEngine{}), NewSelectorTxn(manager, store, nil))
	if err := orch.finalizeRecovery(context.Background(), false); err != nil {
		t.Fatalf("finalizeRecovery migrate: %v", err)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if !applied.SpecCaptured || applied.ConfigEpoch != 7 {
		t.Fatalf("expected captured applied migration, got %+v", applied)
	}
	selection, err := store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	if !state.SelectionStateMatchesApplied(selection, applied) {
		t.Fatalf("selection not rebound to migrated applied: selection=%+v applied=%+v", selection, applied)
	}
}
