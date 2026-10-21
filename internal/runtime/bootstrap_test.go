package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/configapply"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/rescue"
	"smartgate/internal/state"
)

func testWorkerCleanupTargets() []netgate.WorkerCleanupTarget {
	return []netgate.WorkerCleanupTarget{{ID: "worker-1", DisplayName: "Core One"}}
}

func newTestRescueManager(layout state.Layout, manager *commitlane.Manager, owners *netgate.OwnerRegistry) *rescue.Manager {
	return rescue.NewWithCleanup(layout.RescueStatusPath(), manager, owners, netgate.OwnerCleanup{
		Owners:               owners,
		WorkerCleanupTargets: testWorkerCleanupTargets(),
	})
}

func TestBootstrapMigratesLegacyRuntimeRescueFlag(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(layout.RuntimeDir, "smartgate-rescue.status")
	if err := rescue.NewStore(legacy).Save(rescue.ReasonManual, netgate.CleanupPlan{}); err != nil {
		t.Fatal(err)
	}
	boot, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets())
	if err != nil {
		t.Fatal(err)
	}
	active, err := boot.Rescue.Active()
	if err != nil || !active {
		t.Fatalf("migrated rescue active=%v err=%v", active, err)
	}
	if _, err := os.Stat(layout.RescueStatusPath()); err != nil {
		t.Fatalf("persistent rescue flag missing: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy rescue flag remained: %v", err)
	}
}

func TestBootstrapRemovesLegacyFlagWhenPersistentRescueExists(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := rescue.NewStore(layout.RescueStatusPath()).Save("persistent", netgate.CleanupPlan{}); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(layout.RuntimeDir, "smartgate-rescue.status")
	if err := rescue.NewStore(legacy).Save("legacy", netgate.CleanupPlan{}); err != nil {
		t.Fatal(err)
	}
	boot, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets())
	if err != nil {
		t.Fatal(err)
	}
	status, err := boot.Rescue.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Reason != "persistent" {
		t.Fatalf("persistent rescue state was replaced: %+v", status)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy rescue flag remained: %v", err)
	}
}

type bootstrapRecoveryRunner struct{ calls int }

func (r *bootstrapRecoveryRunner) Run(context.Context, []string) (string, error) {
	r.calls++
	return "ok", nil
}

func TestBootstrapFreshBoot(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	result, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets())
	if err != nil {
		t.Fatalf("bootstrap fresh boot: %v", err)
	}
	if result.Status.Phase != StatusRunning {
		t.Fatalf("expected running status, got %+v", result.Status)
	}
	runtimeState, err := result.Store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.RescueActive {
		t.Fatalf("fresh boot should not be rescue active: %+v", runtimeState)
	}
}

func TestBootstrapRestoresDurableAppliedStateIntoFreshRuntime(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	now := time.Now().UTC()
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch:    12,
		TopologyHash:   "durable-topology",
		GatewayMode:    state.GatewayModeGlobal,
		LastAppliedAt:  &now,
		ProxySelection: map[string]string{"general": "node-1"},
	}, state.DefaultSpecState(), nil)); err != nil {
		t.Fatal(err)
	}

	result, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets())
	if err != nil {
		t.Fatal(err)
	}
	runtimeState, err := result.Store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.ConfigEpoch != 12 || runtimeState.TopologyHash != "durable-topology" || runtimeState.ProxySelection["general"] != "node-1" {
		t.Fatalf("durable applied state was not restored: %+v", runtimeState)
	}
	if result.Status.ConfigEpoch != 12 || result.Status.TopologyHash != "durable-topology" {
		t.Fatalf("status did not use durable applied state: %+v", result.Status)
	}
}

func TestBootstrapDoesNotPromoteLegacyRuntimeToAppliedState(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	if err := store.SaveRuntime(state.RuntimeState{
		ConfigEpoch:    4,
		TopologyHash:   "legacy-topology",
		ProxySelection: map[string]string{"ai": "node-a"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets()); err != nil {
		t.Fatal(err)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.SchemaVersion != 0 || applied.SpecCaptured {
		t.Fatalf("legacy runtime was promoted to unprovable applied state: %+v", applied)
	}
}

func TestBootstrapDoesNotBackfillAppliedCaptureFromDivergedSavedState(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeGlobal
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, TopologyHash: "topology"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppliedState(state.AppliedState{ConfigEpoch: 2, TopologyHash: "topology", GatewayMode: state.GatewayModeGlobal}); err != nil {
		t.Fatal(err)
	}

	if _, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets()); err != nil {
		t.Fatal(err)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if applied.SpecCaptured {
		t.Fatalf("saved spec was falsely promoted to applied capture: %+v", applied)
	}
}

func TestBootstrapRestoresLatestDurableSelectionInsteadOfAppliedCompatibilityField(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 5, TopologyHash: "topology", ProxySelection: map[string]string{"general": "old-node"},
	}, state.DefaultSpecState(), nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(applied, map[string]string{"general": "latest-node"})); err != nil {
		t.Fatal(err)
	}
	result, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets())
	if err != nil {
		t.Fatal(err)
	}
	if result.Runtime.ProxySelection["general"] != "latest-node" {
		t.Fatalf("bootstrap overwrote latest durable selection: %+v", result.Runtime.ProxySelection)
	}
}

func TestBootstrapRejectsDurableSelectionForDifferentTopology(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 5, TopologyHash: "topology-a"}, state.DefaultSpecState(), nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionState{ConfigEpoch: 5, TopologyHash: "topology-b", ProxySelection: map[string]string{"general": "node"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets()); err == nil {
		t.Fatal("expected mismatched durable selection to fail closed")
	}
}

func TestBootstrapDefersSelectionMismatchWhenCommittedConfigRecoveryIsPending(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	oldApplied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 1, TopologyHash: "old"}, state.DefaultSpecState(), nil)
	newApplied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, TopologyHash: "new", ProxySelection: map[string]string{"general": "new-node"}}, state.DefaultSpecState(), nil)
	if err := store.SaveAppliedState(oldApplied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(newApplied, newApplied.ProxySelection)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, TopologyHash: "new", ProxySelection: map[string]string{"general": "new-node"}}); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID: "committed-crash", Kind: "config_apply", Phase: state.TransactionCommitted,
		Source: state.TransactionState{Applied: oldApplied}, Target: state.TransactionState{Spec: newApplied.Spec, LastGoodCaptured: true, Applied: newApplied, Runtime: state.RuntimeState{ConfigEpoch: 2, TopologyHash: "new", ProxySelection: map[string]string{"general": "new-node"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets()); err != nil {
		t.Fatalf("bootstrap made committed recovery unreachable: %v", err)
	}
	if journals, err := state.NewTransactionStore(layout).List(); err != nil || len(journals) != 1 {
		t.Fatalf("bootstrap consumed recovery material: journals=%+v err=%v", journals, err)
	}
}

func TestBootstrapRescueBoot(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := rescue.NewStore(layout.RescueStatusPath())
	if err := store.Save(rescue.ReasonManual, netgate.CleanupPlan{}); err != nil {
		t.Fatalf("save rescue flag: %v", err)
	}

	result, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets())
	if err != nil {
		t.Fatalf("bootstrap rescue boot: %v", err)
	}
	if result.Status.Phase != StatusRescue || !result.Status.RescueActive {
		t.Fatalf("expected rescue status, got %+v", result.Status)
	}
	runtimeState, err := result.Store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if !runtimeState.RescueActive || runtimeState.RescueReason != rescue.ReasonManual {
		t.Fatalf("runtime rescue state mismatch: %+v", runtimeState)
	}
	rescueState, err := result.Rescue.Status()
	if err != nil {
		t.Fatal(err)
	}
	foundWorker := false
	for _, action := range rescueState.CleanupPlan.Actions {
		if action.Kind == netgate.KindWorkerService && action.ID == "worker-1" {
			foundWorker = true
		}
	}
	if !foundWorker {
		t.Fatalf("bootstrap rescue cleanup omitted registry worker: %+v", rescueState.CleanupPlan.Actions)
	}
}

func TestBootstrapRunsCrashRecoveryBeforeServing(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	snapshots := state.NewSnapshotStore(layout)

	oldSpec := state.DefaultSpecState()
	oldRuntime := state.RuntimeState{ConfigEpoch: 7, TopologyHash: "old"}
	if _, err := store.SaveSpec(oldSpec); err != nil {
		t.Fatalf("save old spec: %v", err)
	}
	if err := store.SaveRuntime(oldRuntime); err != nil {
		t.Fatalf("save old runtime: %v", err)
	}
	if err := snapshots.SavePreimage(state.PreimageEnvelope{
		OpID:         "apply-1",
		Kind:         "provider_apply",
		TopologyHash: "old",
		SavedAt:      time.Now(),
		Spec:         oldSpec,
		Runtime:      oldRuntime,
	}); err != nil {
		t.Fatalf("save preimage: %v", err)
	}

	halfSpec := state.DefaultSpecState()
	halfSpec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: "manual", Enabled: true}}
	if _, err := store.SaveSpec(halfSpec); err != nil {
		t.Fatalf("save half spec: %v", err)
	}
	halfRuntime := oldRuntime
	halfRuntime.CommitInFlight = &state.CommitRef{
		OpID:         "apply-1",
		StartedAt:    time.Now(),
		TopologyHash: "new",
	}
	if err := store.SaveRuntime(halfRuntime); err != nil {
		t.Fatalf("save half runtime: %v", err)
	}

	result, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets())
	if err != nil {
		t.Fatalf("bootstrap with recovery: %v", err)
	}
	gotSpec, err := result.Store.LoadSpec()
	if err != nil {
		t.Fatalf("load recovered spec: %v", err)
	}
	if len(gotSpec.Providers) != 0 {
		t.Fatalf("expected spec rollback before serving, got %+v", gotSpec.Providers)
	}
	gotRuntime, err := result.Store.LoadRuntime()
	if err != nil {
		t.Fatalf("load recovered runtime: %v", err)
	}
	if gotRuntime.CommitInFlight != nil || gotRuntime.LastRollbackAt == nil {
		t.Fatalf("expected runtime rollback before serving, got %+v", gotRuntime)
	}
	if _, err := snapshots.LoadPreimage("apply-1"); err == nil {
		t.Fatal("expected bootstrap recovery to remove preimage")
	}
}

func TestBootstrapDefersConfigJournalWithoutLegacyPreimageThenConfigTxnRecovers(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	sourceSpec := state.DefaultSpecState()
	sourceSpec.Settings.GatewayMode = state.GatewayModeOff
	targetSpec := sourceSpec
	targetSpec.Providers = []state.ProviderSpec{{ID: "target", Name: "Target", Type: "manual", Enabled: true}}
	sourceApplied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 3, TopologyHash: "source", GatewayMode: state.GatewayModeOff}, sourceSpec, nil)
	targetRuntime := state.RuntimeState{ConfigEpoch: 3, TopologyHash: "source", CommitInFlight: &state.CommitRef{OpID: "config-crash"}}
	if _, err := store.SaveSpec(targetSpec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppliedState(sourceApplied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(targetRuntime); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID: "config-crash", Kind: "config_apply", Phase: state.TransactionApplying,
		Source: state.TransactionState{Spec: sourceSpec, LastGoodCaptured: true, Applied: sourceApplied, Runtime: state.RuntimeState{ConfigEpoch: 3, TopologyHash: "source"}},
		Target: state.TransactionState{Spec: targetSpec, Runtime: targetRuntime},
	}); err != nil {
		t.Fatal(err)
	}

	boot, err := Bootstrap(context.Background(), layout, "127.0.0.1:3000", lockset.New(), testWorkerCleanupTargets())
	if err != nil {
		t.Fatalf("bootstrap must defer config journal recovery: %v", err)
	}
	runner := &bootstrapRecoveryRunner{}
	workers, fabricManager := newRuntimeTestDataPlane(t, boot.Owners, nil, runner)
	txn := commitlane.NewConfigTxn(boot.CommitLane, boot.Store, configapply.Engine{
		Owners: boot.Owners, NetGateRunner: runner,
		Workers: workers, Fabric: fabricManager,
	})
	if err := txn.Recover(context.Background()); err != nil {
		t.Fatalf("config transaction recovery: %v", err)
	}
	if runner.calls == 0 {
		t.Fatal("config recovery did not reconcile OpenWrt data plane")
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.CommitInFlight != nil || runtimeState.ConfigEpoch != 3 {
		t.Fatalf("runtime was not recovered: %+v", runtimeState)
	}
	if _, err := state.NewTransactionStore(layout).Load("config-crash"); !os.IsNotExist(err) {
		t.Fatalf("recovered journal remains: %v", err)
	}
}

func TestBootstrapRejectsBadListen(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if _, err := Bootstrap(context.Background(), layout, "0.0.0.0:3000", lockset.New(), testWorkerCleanupTargets()); err == nil {
		t.Fatal("expected bootstrap to reject wildcard listen")
	}
}

func TestPreflightRequiresWritableDirs(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := os.Chmod(layout.RuntimeDir, 0o500); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(layout.RuntimeDir, 0o700)
	})
	if err := Preflight(layout, "127.0.0.1:3000"); err == nil {
		t.Fatal("expected preflight to reject unwritable runtime dir")
	}
}
