package commitlane

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

func TestApplyProviderTxnSerializesConcurrentWriters(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewProviderTxn(commit, store, state.NewSnapshotStore(layout))

	errCh := make(chan error, 2)
	go func() {
		_, err := txn.Apply(context.Background(), ProviderTxnRequest{
			OpID: "apply-1",
			Changes: []provider.Change{{
				Action: provider.ActionUpsert,
				Provider: state.ProviderSpec{
					ID:       "p1",
					Name:     "Provider 1",
					Type:     provider.TypeManual,
					Enabled:  true,
					Priority: 10,
				},
			}},
		})
		errCh <- err
	}()
	go func() {
		_, err := txn.Apply(context.Background(), ProviderTxnRequest{
			OpID: "apply-2",
			Changes: []provider.Change{{
				Action: provider.ActionUpsert,
				Provider: state.ProviderSpec{
					ID:       "p2",
					Name:     "Provider 2",
					Type:     provider.TypeManual,
					Enabled:  true,
					Priority: 20,
				},
			}},
		})
		errCh <- err
	}()

	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent apply failed: %v", err)
		}
	}

	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if len(spec.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %+v", spec.Providers)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.CommitInFlight != nil {
		t.Fatalf("commit marker should be cleared after provider apply: %+v", runtimeState.CommitInFlight)
	}
	if runtimeState.ConfigEpoch != 0 || runtimeState.TopologyHash == provider.HashSpec(spec) {
		t.Fatalf("provider apply should leave live topology dirty until config apply, runtime=%+v spec_hash=%s", runtimeState, provider.HashSpec(spec))
	}
	if _, err := state.NewSnapshotStore(layout).LoadPreimage("apply-1"); err == nil {
		t.Fatal("expected apply-1 preimage to be removed after successful commit")
	}
	if _, err := state.NewSnapshotStore(layout).LoadPreimage("apply-2"); err == nil {
		t.Fatal("expected apply-2 preimage to be removed after successful commit")
	}
}

func TestRescueEnterBlocksProviderPublish(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	if err := os.WriteFile(layout.RescueStatusPath(), []byte(`{"active":true}`), 0o644); err != nil {
		t.Fatalf("write rescue flag: %v", err)
	}

	txn := NewProviderTxn(commit, store, state.NewSnapshotStore(layout))
	_, err := txn.Apply(context.Background(), ProviderTxnRequest{
		OpID: "apply-1",
		Changes: []provider.Change{{
			Action: provider.ActionUpsert,
			Provider: state.ProviderSpec{
				ID:       "p1",
				Name:     "Provider 1",
				Type:     provider.TypeManual,
				Enabled:  true,
				Priority: 10,
			},
		}},
	})
	if !errors.Is(err, ErrRescueActive) {
		t.Fatalf("expected ErrRescueActive, got %v", err)
	}
}

func TestSavedProviderCleanupDoesNotMutateAppliedSelection(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	lastGood := provider.NewLastGoodStore(layout)
	if _, err := lastGood.Save("p1", []map[string]any{{
		"type":   "vmess",
		"tag":    "p1-node",
		"server": "example.com",
	}}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	initial := state.DefaultSpecState()
	initial.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
		Roles:   []string{"ai"},
	}}
	nodes, err := provider.BuildNodeViews(initial, lastGood)
	if err != nil {
		t.Fatalf("build nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected one node, got %+v", nodes)
	}
	nodeID := nodes[0].ID
	initial.BusinessPools = []state.BusinessPoolSpec{{
		ID:      "ai",
		Name:    "AI",
		Role:    "ai",
		NodeIDs: []string{nodeID},
	}}
	if _, err := store.SaveSpec(initial); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"ai": nodeID},
		NodeHealth:     map[string]state.NodeHealth{nodeID: {Status: "ok"}},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 1, ProxySelection: map[string]string{"ai": nodeID},
	}, initial, []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{{"type": "vmess", "tag": "p1-node", "server": "example.com"}}}})); err != nil {
		t.Fatalf("save applied: %v", err)
	}
	now := time.Unix(1, 0).UTC()
	failoverStore := state.NewFailoverStateStore(layout)
	if err := failoverStore.Save(state.FailoverState{
		Enabled:         true,
		CooldownSeconds: state.DefaultFailoverCooldownSeconds,
		Locks:           map[string]state.FailoverLock{"ai": {Reason: "manual", LockedAt: now}},
		Pools: map[string]state.FailoverPoolState{"ai": {
			PoolID:   "ai",
			Role:     "ai",
			Current:  nodeID,
			Previous: nodeID,
		}},
	}); err != nil {
		t.Fatalf("save failover: %v", err)
	}

	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewProviderTxn(commit, store, state.NewSnapshotStore(layout)).WithLastGoodStore(lastGood)
	_, err = txn.Apply(context.Background(), ProviderTxnRequest{
		OpID: "disable-p1",
		Changes: []provider.Change{{
			Action: provider.ActionUpsert,
			Provider: state.ProviderSpec{
				ID:      "p1",
				Name:    "Provider 1",
				Type:    provider.TypeManual,
				Enabled: false,
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if len(spec.BusinessPools[0].NodeIDs) != 1 || spec.BusinessPools[0].NodeIDs[0] != nodeID {
		t.Fatalf("referenced SmartNode must survive provider source loss: %+v", spec.BusinessPools)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["ai"] != nodeID || runtimeState.NodeHealth[nodeID].Status != "ok" {
		t.Fatalf("unapplied provider save mutated running state: %+v", runtimeState)
	}
	appliedState, err := store.LoadAppliedState()
	if err != nil {
		t.Fatalf("load applied: %v", err)
	}
	if appliedState.ProxySelection["ai"] != nodeID {
		t.Fatalf("unapplied provider save mutated applied selection: %+v", appliedState.ProxySelection)
	}
	failoverState, err := failoverStore.Load()
	if err != nil {
		t.Fatalf("load failover: %v", err)
	}
	if failoverState.Pools["ai"].Current != nodeID || len(failoverState.Locks) != 1 {
		t.Fatalf("unapplied provider save mutated running failover state: %+v", failoverState)
	}
	if !lastGood.Exists("p1") {
		t.Fatal("disabled provider should keep last-good for future re-enable")
	}
}

func TestProviderTxnApplyPrunesDeletedAndOrphanLastGood(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	lastGood := provider.NewLastGoodStore(layout)
	outbounds := []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}}
	for _, id := range []string{"p1", "p2", "orphan"} {
		if _, err := lastGood.Save(id, outbounds); err != nil {
			t.Fatalf("save %s last-good: %v", id, err)
		}
	}
	initial := state.DefaultSpecState()
	initial.Providers = []state.ProviderSpec{
		{ID: "p1", Name: "Provider 1", Type: provider.TypeManual, Enabled: true},
		{ID: "p2", Name: "Provider 2", Type: provider.TypeManual, Enabled: true},
	}
	if _, err := store.SaveSpec(initial); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewProviderTxn(commit, store, state.NewSnapshotStore(layout)).WithLastGoodStore(lastGood)
	_, err := txn.Apply(context.Background(), ProviderTxnRequest{
		OpID: "delete-p2",
		Changes: []provider.Change{{
			Action: provider.ActionDelete,
			ID:     "p2",
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !lastGood.Exists("p1") {
		t.Fatal("active provider last-good should remain")
	}
	if lastGood.Exists("p2") {
		t.Fatal("deleted provider last-good should be pruned")
	}
	if lastGood.Exists("orphan") {
		t.Fatal("orphan last-good should be pruned during provider apply")
	}
}

func TestProviderTxnRejectsUnsafeOpID(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewProviderTxn(commit, store, state.NewSnapshotStore(layout))
	req := ProviderTxnRequest{
		OpID: "../escape",
		Changes: []provider.Change{{
			Action: provider.ActionUpsert,
			Provider: state.ProviderSpec{
				ID:      "p1",
				Name:    "Provider 1",
				Type:    provider.TypeManual,
				Enabled: true,
			},
		}},
	}
	if _, err := txn.DryRun(context.Background(), req); err == nil {
		t.Fatal("expected dryrun unsafe op id to be rejected")
	}
	if _, err := txn.Apply(context.Background(), req); err == nil {
		t.Fatal("expected apply unsafe op id to be rejected")
	}
}

func TestApplyProviderChangesPreservesWriteOnlySourcesFromCurrentSpec(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "remote", Name: "Remote", Type: provider.TypeSubscription, URL: "https://current.example/sub", Enabled: true},
		{ID: "local", Name: "Local", Type: provider.TypeLocalFile, FilePath: "/etc/smartgate/current.yaml", Enabled: true},
	}
	next, err := applyProviderChanges(spec, []provider.Change{
		{Action: provider.ActionUpsert, Provider: state.ProviderSpec{ID: "remote", Name: "Renamed", Type: provider.TypeSubscription, Enabled: true}},
		{Action: provider.ActionUpsert, Provider: state.ProviderSpec{ID: "local", Name: "Renamed Local", Type: provider.TypeLocalFile, Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]state.ProviderSpec{}
	for _, item := range next.Providers {
		providers[item.ID] = item
	}
	if providers["remote"].URL != "https://current.example/sub" || providers["local"].FilePath != "/etc/smartgate/current.yaml" {
		t.Fatalf("write-only sources were not preserved: %+v", providers)
	}
}

func TestProviderTxnDryRunHonorsCanceledContext(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewProviderTxn(commit, store, state.NewSnapshotStore(layout))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := txn.DryRun(ctx, ProviderTxnRequest{OpID: "dryrun-canceled"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
