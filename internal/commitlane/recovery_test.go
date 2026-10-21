package commitlane

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

func TestRecoveryFailsClosedOnOversizedPendingPreimage(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	if _, err := store.SaveSpec(state.DefaultSpecState()); err != nil {
		t.Fatal(err)
	}
	runtimeState := state.RuntimeState{CommitInFlight: &state.CommitRef{OpID: "oversized", StartedAt: time.Now()}}
	if err := store.SaveRuntime(runtimeState); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.SnapshotsDir(), "pending-oversized.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), state.SnapshotMaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery := NewRecovery(New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")), store, state.NewSnapshotStore(layout))
	err := recovery.Recover(context.Background())
	var limitErr *state.SnapshotResourceLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("oversized recovery preimage was not rejected: %T %v", err, err)
	}
	got, _ := store.LoadRuntime()
	if got.CommitInFlight == nil {
		t.Fatal("failed recovery cleared commit marker")
	}
}

func TestRecoverRollsBackHalfCommittedProviderTxn(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	snapshots := state.NewSnapshotStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))

	oldSpec := state.DefaultSpecState()
	oldRuntime := state.RuntimeState{ConfigEpoch: 1, TopologyHash: "old"}
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

	newSpec := state.DefaultSpecState()
	newSpec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: "manual", Enabled: true}}
	if _, err := store.SaveSpec(newSpec); err != nil {
		t.Fatalf("save half-committed spec: %v", err)
	}

	inflightRuntime := oldRuntime
	inflightRuntime.CommitInFlight = &state.CommitRef{
		OpID:         "apply-1",
		StartedAt:    time.Now(),
		TopologyHash: "new",
	}
	if err := store.SaveRuntime(inflightRuntime); err != nil {
		t.Fatalf("save inflight runtime: %v", err)
	}

	recovery := NewRecovery(commit, store, snapshots)
	if err := recovery.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}

	gotSpec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load recovered spec: %v", err)
	}
	if len(gotSpec.Providers) != 0 {
		t.Fatalf("expected spec rollback, got %+v", gotSpec.Providers)
	}
	gotRuntime, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load recovered runtime: %v", err)
	}
	if gotRuntime.CommitInFlight != nil {
		t.Fatalf("expected commit_in_flight cleared, got %+v", gotRuntime.CommitInFlight)
	}
	if _, err := snapshots.LoadPreimage("apply-1"); err == nil {
		t.Fatal("expected recovered preimage to be removed")
	}
}

func TestRecoverUsesDurableJournalWhenRuntimeDirectoryWasLost(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	snapshots := state.NewSnapshotStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))

	sourceSpec := state.DefaultSpecState()
	sourceSpec.Providers = []state.ProviderSpec{{ID: "source", Name: "Source", Type: provider.TypeManual, Enabled: true}}
	sourceRuntime := state.RuntimeState{ConfigEpoch: 3, TopologyHash: "source-hash", ProxySelection: map[string]string{"general": "source-node"}}
	sourceApplied := state.AppliedStateFromRuntime(sourceRuntime, sourceSpec.Settings.GatewayMode)
	sourceLastGood := []state.LastGoodSnapshot{{ProviderID: "source", Outbounds: []map[string]any{{"type": "vmess", "tag": "source-node", "server": "source.example"}}}}
	currentApplied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 9, TopologyHash: "still-applied", ProxySelection: map[string]string{"general": "live-node"}}, sourceSpec, sourceLastGood)
	if err := store.SaveAppliedState(currentApplied); err != nil {
		t.Fatal(err)
	}

	targetSpec := state.DefaultSpecState()
	targetSpec.Providers = []state.ProviderSpec{{ID: "target", Name: "Target", Type: provider.TypeManual, Enabled: true}}
	if _, err := store.SaveSpec(targetSpec); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewLastGoodStore(layout).ReplaceAll(context.Background(), []provider.LastGoodUpdate{{ProviderID: "target", Outbounds: []map[string]any{{"type": "vmess", "tag": "target-node", "server": "target.example"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID:  "durable-provider",
		Kind:  "provider_apply",
		Phase: state.TransactionPrepared,
		Source: state.TransactionState{
			Spec:             sourceSpec,
			LastGoodCaptured: true,
			LastGood:         sourceLastGood,
			Applied:          sourceApplied,
			Runtime:          sourceRuntime,
		},
		Target: state.TransactionState{Spec: targetSpec},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(layout.RuntimePath("runtime")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	if err := NewRecovery(commit, store, snapshots).Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	gotSpec, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSpec.Providers) != 1 || gotSpec.Providers[0].ID != "source" {
		t.Fatalf("source spec was not restored: %+v", gotSpec.Providers)
	}
	if _, err := provider.NewLastGoodStore(layout).Load("source"); err != nil {
		t.Fatalf("source last-good was not restored: %v", err)
	}
	if _, err := provider.NewLastGoodStore(layout).Load("target"); !os.IsNotExist(err) {
		t.Fatalf("target last-good should have been removed, got %v", err)
	}
	gotApplied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	if gotApplied.TopologyHash != currentApplied.TopologyHash || gotApplied.ProxySelection["general"] != "live-node" {
		t.Fatalf("save transaction recovery mutated applied state: %+v", gotApplied)
	}
	gotRuntime, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if gotRuntime.LastRollbackAt == nil || gotRuntime.CommitInFlight != nil {
		t.Fatalf("runtime recovery metadata missing: %+v", gotRuntime)
	}
	if journals, err := state.NewTransactionStore(layout).List(); err != nil || len(journals) != 0 {
		t.Fatalf("journal was not removed: journals=%+v err=%v", journals, err)
	}
}

func TestRecoverRestoresLastGoodPreimage(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	snapshots := state.NewSnapshotStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	oldSpec := state.DefaultSpecState()
	oldRuntime := state.RuntimeState{ConfigEpoch: 4, TopologyHash: "old"}
	if err := snapshots.SavePreimage(state.PreimageEnvelope{
		OpID:         "restore-1",
		Kind:         "backup_restore",
		TopologyHash: "old",
		SavedAt:      time.Now(),
		Spec:         oldSpec,
		Runtime:      oldRuntime,
		LastGood: []state.LastGoodSnapshot{{
			ProviderID: "p1",
			Outbounds:  []map[string]any{{"type": "vmess", "tag": "old-node", "server": "old.example"}},
		}},
	}); err != nil {
		t.Fatalf("save preimage: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{CommitInFlight: &state.CommitRef{OpID: "restore-1", StartedAt: time.Now(), TopologyHash: "new"}}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	if _, err := provider.NewLastGoodStore(layout).Save("p1", []map[string]any{{"type": "trojan", "tag": "new-node", "server": "new.example"}}); err != nil {
		t.Fatalf("save new last-good: %v", err)
	}
	if _, err := provider.NewLastGoodStore(layout).Save("stale", []map[string]any{{"type": "trojan", "tag": "stale-node", "server": "stale.example"}}); err != nil {
		t.Fatalf("save stale last-good: %v", err)
	}

	if err := NewRecovery(commit, store, snapshots).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	env, err := provider.NewLastGoodStore(layout).Load("p1")
	if err != nil {
		t.Fatalf("load restored last-good: %v", err)
	}
	if len(env.Outbounds) != 0 || len(env.Identities) != 1 || env.Identities[0].RawTag != "old-node" {
		t.Fatalf("expected old last-good restored, got %+v", env)
	}
	if provider.NewLastGoodStore(layout).Exists("stale") {
		t.Fatal("recovery should remove last-good entries not present in preimage")
	}
}

func TestRecoverClearsLastGoodWhenCapturedPreimageWasEmpty(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	snapshots := state.NewSnapshotStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	oldRuntime := state.RuntimeState{ConfigEpoch: 2, TopologyHash: "old"}
	if err := snapshots.SavePreimage(state.PreimageEnvelope{
		OpID:             "restore-empty-lastgood",
		Kind:             "backup_restore",
		TopologyHash:     "old",
		SavedAt:          time.Now(),
		Spec:             state.DefaultSpecState(),
		Runtime:          oldRuntime,
		LastGoodCaptured: true,
		LastGood:         nil,
	}); err != nil {
		t.Fatalf("save preimage: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{CommitInFlight: &state.CommitRef{OpID: "restore-empty-lastgood", StartedAt: time.Now(), TopologyHash: "new"}}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	if _, err := provider.NewLastGoodStore(layout).Save("p1", []map[string]any{{"type": "vmess", "tag": "new-node", "server": "new.example"}}); err != nil {
		t.Fatalf("save new last-good: %v", err)
	}

	if err := NewRecovery(commit, store, snapshots).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if provider.NewLastGoodStore(layout).Exists("p1") {
		t.Fatal("recovery should clear last-good when captured preimage was empty")
	}
}
