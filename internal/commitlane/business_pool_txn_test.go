package commitlane

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"smartgate/internal/business"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

func TestBusinessPoolTxnDryRunAndApplyOrderChange(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	initial := state.DefaultSpecState()
	initial.BusinessPools = []state.BusinessPoolSpec{{
		ID:      "ai",
		Name:    "AI",
		Role:    "ai",
		NodeIDs: []string{"n1", "n2"},
	}}
	if _, err := store.SaveSpec(initial); err != nil {
		t.Fatalf("save initial spec: %v", err)
	}
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewBusinessPoolTxn(commit, store, state.NewSnapshotStore(layout))
	req := BusinessPoolTxnRequest{
		OpID: "pool-apply-1",
		Changes: []business.Change{{
			Action: business.ActionUpsert,
			Pool: state.BusinessPoolSpec{
				ID:      "ai",
				Name:    "AI",
				Role:    "ai",
				NodeIDs: []string{"n2", "n1"},
			},
		}},
	}

	dryRun, err := txn.DryRun(context.Background(), req)
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	if !dryRun.Plan.HasChanges || len(dryRun.Plan.OrderChanged) != 1 {
		t.Fatalf("expected order change in dryrun plan, got %+v", dryRun.Plan)
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after dryrun: %v", err)
	}
	if spec.BusinessPools[0].NodeIDs[0] != "n1" {
		t.Fatalf("dryrun should not persist order change: %+v", spec.BusinessPools)
	}

	applied, err := txn.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.Runtime.CommitInFlight != nil {
		t.Fatalf("commit marker should be cleared after pool apply: %+v", applied.Runtime.CommitInFlight)
	}
	if applied.Runtime.ConfigEpoch != 0 {
		t.Fatalf("pool apply should not advance live config epoch before config apply, got %+v", applied.Runtime)
	}
	spec, err = store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after apply: %v", err)
	}
	if spec.BusinessPools[0].NodeIDs[0] != "n2" {
		t.Fatalf("apply should persist order change: %+v", spec.BusinessPools)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime after apply: %v", err)
	}
	if runtimeState.TopologyHash == "" {
		// Empty is fine for a never-applied dev store; this assertion documents
		// that pool apply leaves live topology untouched.
		return
	}
	t.Fatalf("pool apply should leave previous topology hash untouched, got %+v", runtimeState)
}

func TestBusinessPoolTxnApplyBlocksDuringRescue(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := os.WriteFile(layout.RescueStatusPath(), []byte(`{"active":true}`), 0o644); err != nil {
		t.Fatalf("write rescue flag: %v", err)
	}
	store := state.NewStore(layout)
	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewBusinessPoolTxn(commit, store, state.NewSnapshotStore(layout))

	_, err := txn.Apply(context.Background(), BusinessPoolTxnRequest{
		OpID: "pool-apply-locked",
		Changes: []business.Change{{
			Action: business.ActionUpsert,
			Pool: state.BusinessPoolSpec{
				ID:      "ai",
				Name:    "AI",
				Role:    "ai",
				NodeIDs: []string{"n1"},
			},
		}},
	})
	if !errors.Is(err, ErrRescueActive) {
		t.Fatalf("expected ErrRescueActive, got %v", err)
	}
}

func TestBusinessPoolSaveDoesNotMutateRunningSelection(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := state.NewStore(layout)
	initial := state.DefaultSpecState()
	initial.BusinessPools = append(state.DefaultBusinessPools(), state.BusinessPoolSpec{
		ID:      "legacy",
		Name:    "Legacy",
		Role:    "general",
		NodeIDs: []string{"node-old"},
	})
	if _, err := store.SaveSpec(initial); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{
			"legacy":  "node-old",
			"ai":      "Direct",
			"general": "Block",
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	now := time.Unix(1, 0).UTC()
	failoverStore := state.NewFailoverStateStore(layout)
	if err := failoverStore.Save(state.FailoverState{
		Enabled:         true,
		CooldownSeconds: state.DefaultFailoverCooldownSeconds,
		Locks: map[string]state.FailoverLock{
			"legacy": {Reason: "manual", LockedAt: now},
			"ai":     {Reason: "manual", LockedAt: now},
		},
		Pools: map[string]state.FailoverPoolState{
			"legacy":  {PoolID: "legacy", Role: "general", Current: "node-old"},
			"ai":      {PoolID: "ai", Role: "ai", Current: "Direct", Previous: "Block"},
			"general": {PoolID: "general", Role: "general", Current: "Block"},
		},
	}); err != nil {
		t.Fatalf("save failover: %v", err)
	}

	commit := New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	txn := NewBusinessPoolTxn(commit, store, state.NewSnapshotStore(layout))
	_, err := txn.Apply(context.Background(), BusinessPoolTxnRequest{
		OpID: "pool-cleanup",
		Changes: []business.Change{{
			Action: business.ActionDelete,
			ID:     "legacy",
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["legacy"] != "node-old" || runtimeState.ProxySelection["ai"] != "Direct" || runtimeState.ProxySelection["general"] != "Block" {
		t.Fatalf("unapplied pool save mutated running selection: %+v", runtimeState.ProxySelection)
	}
	failoverState, err := failoverStore.Load()
	if err != nil {
		t.Fatalf("load failover: %v", err)
	}
	if failoverState.Pools["legacy"].Current != "node-old" || failoverState.Pools["ai"].Current != "Direct" || failoverState.Pools["general"].Current != "Block" {
		t.Fatalf("unapplied pool save mutated failover state: %+v", failoverState.Pools)
	}
	if _, ok := failoverState.Locks["legacy"]; !ok {
		t.Fatalf("unapplied pool save removed running failover lock: %+v", failoverState.Locks)
	}
}
