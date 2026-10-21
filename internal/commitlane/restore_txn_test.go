package commitlane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"smartgate/internal/backup"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

func TestRestoreTransactionsFailClosedWithoutDataPlaneTxn(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	snapshots := state.NewSnapshotStore(layout)
	if err := snapshots.SaveSnapshot(state.SnapshotEnvelope{ID: "snap-1", Kind: "manual", SavedAt: time.Now(), Spec: state.DefaultSpecState()}); err != nil {
		t.Fatal(err)
	}

	_, err := NewSnapshotTxn(manager, store, snapshots).Rollback(context.Background(), SnapshotRollbackRequest{OpID: "rollback-1", SnapshotID: "snap-1"})
	if err == nil || !strings.Contains(err.Error(), "data-plane transaction is not configured") {
		t.Fatalf("snapshot rollback did not fail closed: %v", err)
	}
	_, err = NewBackupTxn(manager, store, snapshots, layout).Restore(context.Background(), BackupRestoreRequest{
		OpID: "restore-1", Payload: backup.Payload{Spec: state.DefaultSpecState()},
	})
	if err == nil || !strings.Contains(err.Error(), "data-plane transaction is not configured") {
		t.Fatalf("backup restore did not fail closed: %v", err)
	}
}

func TestSnapshotRollbackPrepareFailureStopsAtDryRun(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	snapshots := state.NewSnapshotStore(layout)
	target := state.DefaultSpecState()
	target.Settings.GatewayMode = state.GatewayModeOff
	if err := snapshots.SaveSnapshot(state.SnapshotEnvelope{ID: "snap-prepare", Kind: "manual", SavedAt: time.Now(), Spec: target, LastGoodCaptured: true}); err != nil {
		t.Fatal(err)
	}
	engine := &fakeConfigApplyEngine{prepareErr: errors.New("candidate rejected")}
	txn := NewSnapshotTxn(manager, store, snapshots).WithConfigTxn(NewConfigTxn(manager, store, engine))
	artifact, err := txn.PrepareRollback(context.Background(), SnapshotRollbackDryRunRequest{DryRunID: "rollback-prepare", SnapshotID: "snap-prepare"})
	if err == nil || !strings.Contains(err.Error(), "candidate rejected") {
		t.Fatalf("prepare error=%v", err)
	}
	if artifact == nil {
		t.Fatal("prepare failure should return the partial artifact for cleanup")
	}
}

func TestSnapshotRollbackApplyReusesPreparedTarget(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	snapshots := state.NewSnapshotStore(layout)
	current := state.DefaultSpecState()
	current.Settings.GatewayMode = state.GatewayModeOff
	current.DNS.Final = "direct"
	target := current
	target.DNS.Final = "proxy"
	if _, err := store.SaveSpec(current); err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 3, GatewayMode: state.GatewayModeOff, ProxySelection: map[string]string{"general": "node-current"}}, current, nil)
	applied.TopologyHash, _ = provider.HashEffectiveTopologyFromSnapshots(current, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 3, TopologyHash: applied.TopologyHash, ProxySelection: map[string]string{"general": "runtime-stale"}}); err != nil {
		t.Fatal(err)
	}
	currentSelection := state.SelectionState{
		SchemaVersion: state.SelectionStateSchemaVersion, ConfigEpoch: 3, TopologyHash: applied.TopologyHash,
		ProxySelection: map[string]string{"general": "node-current"},
		Selections: map[string]state.PoolSelection{
			"general": {SmartNode: "node-current", Target: "node", Generation: 5, BoundEpoch: 3, BoundTopologyHash: applied.TopologyHash},
		},
	}
	if err := store.SaveSelectionState(currentSelection); err != nil {
		t.Fatal(err)
	}
	snapshotRuntime := state.RuntimeState{ConfigEpoch: 2, TopologyHash: "snapshot-topology", ProxySelection: map[string]string{"general": "node-snapshot"}}
	snapshotSelection := state.SelectionState{
		SchemaVersion: state.SelectionStateSchemaVersion, ConfigEpoch: 2, TopologyHash: "snapshot-topology",
		ProxySelection: map[string]string{"general": "node-snapshot"},
		Selections: map[string]state.PoolSelection{
			"general": {SmartNode: "node-snapshot", Target: "node", Generation: 2, BoundEpoch: 2, BoundTopologyHash: "snapshot-topology"},
		},
	}
	if err := snapshots.SaveSnapshot(state.SnapshotEnvelope{
		ID: "snap-target", Kind: "manual", SavedAt: time.Now(), Spec: target, Runtime: snapshotRuntime,
		SelectionCaptured: true, Selection: snapshotSelection, LastGoodCaptured: true,
	}); err != nil {
		t.Fatal(err)
	}
	engine := &fakeConfigApplyEngine{}
	txn := NewSnapshotTxn(manager, store, snapshots).WithConfigTxn(NewConfigTxn(manager, store, engine))
	artifact, err := txn.PrepareRollback(context.Background(), SnapshotRollbackDryRunRequest{DryRunID: "rollback-target", SnapshotID: "snap-target"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = txn.Rollback(context.Background(), SnapshotRollbackRequest{
		OpID: "rollback-target", SnapshotID: "snap-target", Prepared: artifact.Prepared,
		ExpectedConfigEpoch: artifact.Plan.CurrentConfigEpoch, ExpectedSourceTopologyHash: artifact.Plan.CurrentTopologyHash,
		ExpectedTargetTopologyHash: artifact.Plan.TargetTopologyHash, ExpectedSnapshotTargetHash: artifact.Plan.SnapshotTargetHash,
		ExpectedSourceSelection: copyStringMap(artifact.Plan.CurrentSelection.ProxySelection), ExpectedSourceSelections: copyPoolSelections(artifact.Plan.CurrentSelection.Selections),
		RequireDryRunMatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	targetHash := provider.HashSpec(target)
	targetPrepareCount := 0
	for _, preparedTarget := range engine.prepared {
		if provider.HashSpec(preparedTarget.Spec) == targetHash {
			targetPrepareCount++
		}
	}
	if targetPrepareCount != 1 {
		t.Fatalf("snapshot target was prepared %d times, want exactly once during dry-run", targetPrepareCount)
	}
	if len(engine.prepared) == 0 || engine.prepared[0].Selections["general"] != snapshotSelection.Selections["general"] {
		t.Fatalf("snapshot target Selection was not bound into prepared graph: %+v", engine.prepared)
	}
}

func TestLegacySnapshotRollbackWritesIdentityOnlyJournalAndAppliedState(t *testing.T) {
	configTxn, store, layout, target, _, engine := newConfigTxnFixture(t)
	legacyLastGood := []state.LastGoodSnapshot{{
		ProviderID: "provider-a",
		Outbounds: []map[string]any{{
			"type": "vmess", "tag": "legacy-snapshot-node", "server": "snapshot.example", "server_port": 443, "uuid": "snapshot-test-secret",
		}},
	}}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(target, legacyLastGood)
	if err != nil {
		t.Fatal(err)
	}
	legacyApplied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 1, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOff,
	}, target, legacyLastGood)
	snapshots := state.NewSnapshotStore(layout)
	if err := snapshots.SaveSnapshot(state.SnapshotEnvelope{
		ID: "legacy-last-good", Kind: "manual", SavedAt: time.Now().UTC(),
		Spec: target, Runtime: state.RuntimeState{ConfigEpoch: 1, TopologyHash: topologyHash},
		AppliedCaptured: true, Applied: legacyApplied,
		LastGoodCaptured: true, LastGood: legacyLastGood,
	}); err != nil {
		t.Fatal(err)
	}

	var applying state.TransactionJournal
	var applyingJSON []byte
	var captureErr error
	engine.onApply = func(call int) {
		if call != 1 {
			return
		}
		applying, captureErr = state.NewTransactionStore(layout).Load("rollback-legacy-last-good")
		if captureErr != nil {
			return
		}
		applyingJSON, captureErr = os.ReadFile(layout.TransactionsDir() + "/rollback-legacy-last-good.json")
	}
	snapshotTxn := NewSnapshotTxn(configTxn.commit, store, snapshots).WithConfigTxn(configTxn)
	artifact, err := snapshotTxn.PrepareRollback(context.Background(), SnapshotRollbackDryRunRequest{
		DryRunID: "rollback-legacy-last-good", SnapshotID: "legacy-last-good",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshotTxn.Rollback(context.Background(), SnapshotRollbackRequest{
		OpID: "rollback-legacy-last-good", SnapshotID: "legacy-last-good", Prepared: artifact.Prepared,
		ExpectedConfigEpoch: artifact.Plan.CurrentConfigEpoch, ExpectedSourceTopologyHash: artifact.Plan.CurrentTopologyHash,
		ExpectedTargetTopologyHash: artifact.Plan.TargetTopologyHash, ExpectedSnapshotTargetHash: artifact.Plan.SnapshotTargetHash,
		ExpectedSourceSelection: copyStringMap(artifact.Plan.CurrentSelection.ProxySelection), ExpectedSourceSelections: copyPoolSelections(artifact.Plan.CurrentSelection.Selections),
		RequireDryRunMatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if captureErr != nil {
		t.Fatal(captureErr)
	}
	if applying.Phase != state.TransactionApplying {
		t.Fatalf("captured journal phase = %q, want applying", applying.Phase)
	}
	for name, snapshots := range map[string][]state.LastGoodSnapshot{
		"journal target":         applying.Target.LastGood,
		"journal target applied": applying.Target.Applied.LastGood,
	} {
		if len(snapshots) != 1 || len(snapshots[0].Identities) != 1 || snapshots[0].Identities[0].RawTag != "legacy-snapshot-node" || len(snapshots[0].Outbounds) != 0 {
			t.Fatalf("%s was not identity-only: %+v", name, snapshots)
		}
	}
	if strings.Contains(string(applyingJSON), `"outbounds"`) {
		t.Fatalf("applying journal rewrote legacy outbounds: %s", applyingJSON)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.LastGood) != 1 || len(applied.LastGood[0].Identities) != 1 || applied.LastGood[0].Identities[0].RawTag != "legacy-snapshot-node" || len(applied.LastGood[0].Outbounds) != 0 {
		t.Fatalf("rollback applied state was not identity-only: %+v", applied.LastGood)
	}
	appliedJSON, err := os.ReadFile(layout.AppliedStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(appliedJSON), `"outbounds"`) {
		t.Fatalf("rollback applied.json rewrote legacy outbounds: %s", appliedJSON)
	}
}

func TestSnapshotRollbackRejectsSameNodeGenerationChangeAfterDryRun(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	topologyHash, _ := provider.HashEffectiveTopologyFromSnapshots(spec, nil)
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 4, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOff, ProxySelection: map[string]string{"general": "node-a"}}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 4, TopologyHash: topologyHash}); err != nil {
		t.Fatal(err)
	}
	original := state.SelectionState{
		SchemaVersion: state.SelectionStateSchemaVersion, ConfigEpoch: 4, TopologyHash: topologyHash,
		ProxySelection: map[string]string{"general": "node-a"},
		Selections:     map[string]state.PoolSelection{"general": {SmartNode: "node-a", Target: "node", Generation: 7, BoundEpoch: 4, BoundTopologyHash: topologyHash}},
	}
	if err := store.SaveSelectionState(original); err != nil {
		t.Fatal(err)
	}
	snapshots := state.NewSnapshotStore(layout)
	if err := snapshots.SaveSnapshot(state.SnapshotEnvelope{ID: "snap-generation", Kind: "manual", SavedAt: time.Now(), Spec: spec, Runtime: state.RuntimeState{ProxySelection: map[string]string{"general": "node-a"}}, LastGoodCaptured: true}); err != nil {
		t.Fatal(err)
	}
	engine := &fakeConfigApplyEngine{}
	txn := NewSnapshotTxn(manager, store, snapshots).WithConfigTxn(NewConfigTxn(manager, store, engine))
	artifact, err := txn.PrepareRollback(context.Background(), SnapshotRollbackDryRunRequest{DryRunID: "rollback-generation", SnapshotID: "snap-generation"})
	if err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.Selections = map[string]state.PoolSelection{"general": {SmartNode: "node-a", Target: "node", Generation: 8, BoundEpoch: 4, BoundTopologyHash: topologyHash}}
	if err := store.SaveSelectionState(changed); err != nil {
		t.Fatal(err)
	}
	_, err = txn.Rollback(context.Background(), SnapshotRollbackRequest{
		OpID: "rollback-generation", SnapshotID: "snap-generation", Prepared: artifact.Prepared,
		ExpectedConfigEpoch: artifact.Plan.CurrentConfigEpoch, ExpectedSourceTopologyHash: artifact.Plan.CurrentTopologyHash,
		ExpectedTargetTopologyHash: artifact.Plan.TargetTopologyHash, ExpectedSnapshotTargetHash: artifact.Plan.SnapshotTargetHash,
		ExpectedSourceSelection: copyStringMap(artifact.Plan.CurrentSelection.ProxySelection), ExpectedSourceSelections: copyPoolSelections(artifact.Plan.CurrentSelection.Selections),
		RequireDryRunMatch: true,
	})
	if err == nil || !strings.Contains(err.Error(), "persisted pool selection changed after rollback dry-run") {
		t.Fatalf("expected snapshot Generation CAS rejection, got %v", err)
	}
	if len(engine.applied) != 0 {
		t.Fatalf("stale snapshot rollback reached data plane: %d calls", len(engine.applied))
	}
}

func TestLegacyBackupRestorePreservesCurrentFailoverSettings(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 4, GatewayMode: state.GatewayModeOff}, spec, nil)
	applied.TopologyHash, _ = provider.HashEffectiveTopologyFromSnapshots(spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 4, TopologyHash: applied.TopologyHash}); err != nil {
		t.Fatal(err)
	}
	if err := state.NewFailoverStateStore(layout).Save(state.FailoverState{Enabled: false, CooldownSeconds: 900, Locks: map[string]state.FailoverLock{}, Pools: map[string]state.FailoverPoolState{}}); err != nil {
		t.Fatal(err)
	}
	engine := &fakeConfigApplyEngine{}
	configTxn := NewConfigTxn(manager, store, engine)
	result, err := NewBackupTxn(manager, store, state.NewSnapshotStore(layout), layout).WithConfigTxn(configTxn).Restore(context.Background(), BackupRestoreRequest{
		OpID: "legacy-restore", Payload: backup.Payload{Metadata: backup.Metadata{SchemaVersion: backup.LegacySchemaVersion}, Spec: spec},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := state.NewFailoverStateStore(layout).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatalf("legacy restore changed current failover settings: %+v", got)
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0], "保留") {
		t.Fatalf("legacy migration warning missing: %+v", result)
	}
}

func TestLegacyBackupRestoreWritesIdentityOnlyJournalAndAppliedState(t *testing.T) {
	configTxn, store, layout, target, _, engine := newConfigTxnFixture(t)
	legacyBody, err := json.Marshal(provider.LastGoodEnvelope{
		ProviderID: "provider-a",
		Count:      1,
		Outbounds: []map[string]any{{
			"type": "vmess", "tag": "legacy-backup-node", "server": "backup.example", "server_port": 443, "uuid": "backup-test-secret",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var applying state.TransactionJournal
	var applyingJSON []byte
	var captureErr error
	engine.onApply = func(call int) {
		if call != 1 {
			return
		}
		applying, captureErr = state.NewTransactionStore(layout).Load("legacy-last-good-restore")
		if captureErr != nil {
			return
		}
		applyingJSON, captureErr = os.ReadFile(layout.TransactionsDir() + "/legacy-last-good-restore.json")
	}
	_, err = NewBackupTxn(configTxn.commit, store, state.NewSnapshotStore(layout), layout).WithConfigTxn(configTxn).Restore(context.Background(), BackupRestoreRequest{
		OpID: "legacy-last-good-restore",
		Payload: backup.Payload{
			Metadata: backup.Metadata{SchemaVersion: backup.LegacySchemaVersion},
			Spec:     target,
			LastGood: map[string]json.RawMessage{"provider-a": legacyBody},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captureErr != nil {
		t.Fatal(captureErr)
	}
	if applying.Phase != state.TransactionApplying {
		t.Fatalf("captured journal phase = %q, want applying", applying.Phase)
	}
	for name, snapshots := range map[string][]state.LastGoodSnapshot{
		"journal target":         applying.Target.LastGood,
		"journal target applied": applying.Target.Applied.LastGood,
	} {
		if len(snapshots) != 1 || len(snapshots[0].Identities) != 1 || snapshots[0].Identities[0].RawTag != "legacy-backup-node" || len(snapshots[0].Outbounds) != 0 {
			t.Fatalf("%s was not identity-only: %+v", name, snapshots)
		}
	}
	if strings.Contains(string(applyingJSON), `"outbounds"`) {
		t.Fatalf("applying journal rewrote legacy outbounds: %s", applyingJSON)
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.LastGood) != 1 || len(applied.LastGood[0].Identities) != 1 || applied.LastGood[0].Identities[0].RawTag != "legacy-backup-node" || len(applied.LastGood[0].Outbounds) != 0 {
		t.Fatalf("restored applied state was not identity-only: %+v", applied.LastGood)
	}
	appliedJSON, err := os.ReadFile(layout.AppliedStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(appliedJSON), `"outbounds"`) {
		t.Fatalf("restored applied.json rewrote legacy outbounds: %s", appliedJSON)
	}
}

func TestBackupRestoreUsesArchivedRuntimeSelectionOverStaleAppliedField(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	manager := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	topologyHash, _ := provider.HashEffectiveTopologyFromSnapshots(spec, nil)
	current := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOff}, spec, nil)
	if err := store.SaveAppliedState(current); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, TopologyHash: topologyHash}); err != nil {
		t.Fatal(err)
	}
	engine := &fakeConfigApplyEngine{}
	_, err := NewBackupTxn(manager, store, state.NewSnapshotStore(layout), layout).WithConfigTxn(NewConfigTxn(manager, store, engine)).Restore(context.Background(), BackupRestoreRequest{
		OpID: "durable-selection-restore",
		Payload: backup.Payload{
			Metadata: backup.Metadata{SchemaVersion: backup.SchemaVersion}, Spec: spec,
			RuntimeCaptured: true, Runtime: state.RuntimeState{ProxySelection: map[string]string{"general": "runtime-new"}},
			AppliedCaptured: true, Applied: state.AppliedState{ProxySelection: map[string]string{"general": "applied-old"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := store.LoadSelectionState()
	if err != nil || selection.ProxySelection["general"] != "runtime-new" {
		t.Fatalf("restore used stale applied selection: %+v err=%v", selection, err)
	}
}
