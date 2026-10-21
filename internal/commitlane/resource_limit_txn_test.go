package commitlane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"smartgate/internal/business"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

func TestProviderApplyRejectsBoundedLastGoodWithoutStateMutation(t *testing.T) {
	for _, resource := range []string{"oversized_file", "too_many_nodes"} {
		t.Run(resource, func(t *testing.T) {
			layout, store, manager, snapshots, before := boundedTxnFixture(t, resource)
			txn := NewProviderTxn(manager, store, snapshots).WithLastGoodStore(provider.NewLastGoodStore(layout))
			_, err := txn.Apply(context.Background(), ProviderTxnRequest{OpID: "provider-limit", Changes: []provider.Change{{Action: provider.ActionUpsert, Provider: state.ProviderSpec{ID: "p2", Name: "P2", Type: provider.TypeManual, Enabled: true}}}})
			assertTopologyLimitAndUnchanged(t, err, layout, store, before)
		})
	}
}

func TestBusinessPoolApplyRejectsBoundedLastGoodWithoutStateMutation(t *testing.T) {
	for _, resource := range []string{"oversized_file", "too_many_nodes"} {
		t.Run(resource, func(t *testing.T) {
			layout, store, manager, snapshots, before := boundedTxnFixture(t, resource)
			txn := NewBusinessPoolTxn(manager, store, snapshots)
			_, err := txn.Apply(context.Background(), BusinessPoolTxnRequest{OpID: "pool-limit", Changes: []business.Change{{Action: business.ActionUpsert, Pool: state.BusinessPoolSpec{ID: "extra", Name: "Extra", Role: "general"}}}})
			assertTopologyLimitAndUnchanged(t, err, layout, store, before)
		})
	}
}

type boundedControlState struct {
	spec     state.SpecState
	applied  state.AppliedState
	runtime  state.RuntimeState
	failover state.FailoverState
}

func boundedTxnFixture(t *testing.T, resource string) (state.Layout, *state.Store, *Manager, *state.SnapshotStore, boundedControlState) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Type: provider.TypeManual, Enabled: true}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 3, TopologyHash: "applied", ProxySelection: map[string]string{"general": "node-a"}}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	runtimeState := state.RuntimeState{ConfigEpoch: 3, TopologyHash: "applied", ProxySelection: map[string]string{"general": "node-a"}}
	if err := store.SaveRuntime(runtimeState); err != nil {
		t.Fatal(err)
	}
	failover := state.DefaultFailoverState()
	failover.Pools["general"] = state.FailoverPoolState{PoolID: "general", Current: "node-a"}
	if err := state.NewFailoverStateStore(layout).Save(failover); err != nil {
		t.Fatal(err)
	}
	path, err := provider.NewLastGoodStore(layout).Path("p1")
	if err != nil {
		t.Fatal(err)
	}
	switch resource {
	case "oversized_file":
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), provider.MaxDryRunLastGoodBytes+1), 0o644); err != nil {
			t.Fatal(err)
		}
	case "too_many_nodes":
		outbounds := make([]map[string]any, provider.MaxDryRunLastGoodNodes+1)
		for i := range outbounds {
			outbounds[i] = map[string]any{"tag": "n"}
		}
		body, err := json.Marshal(provider.LastGoodEnvelope{ProviderID: "p1", Count: len(outbounds), Outbounds: outbounds})
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > provider.MaxDryRunLastGoodBytes {
			t.Fatalf("node-count fixture unexpectedly exceeds byte limit: %d", len(body))
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown resource fixture %q", resource)
	}
	loadedSpec, _ := store.LoadSpec()
	loadedApplied, _ := store.LoadAppliedState()
	loadedRuntime, _ := store.LoadRuntime()
	loadedFailover, _ := state.NewFailoverStateStore(layout).Load()
	return layout, store, New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")), state.NewSnapshotStore(layout), boundedControlState{loadedSpec, loadedApplied, loadedRuntime, loadedFailover}
}

func assertTopologyLimitAndUnchanged(t *testing.T, err error, layout state.Layout, store *state.Store, before boundedControlState) {
	t.Helper()
	var limit *provider.TopologyResourceLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("expected typed topology resource limit, got %v", err)
	}
	afterSpec, _ := store.LoadSpec()
	afterApplied, _ := store.LoadAppliedState()
	afterRuntime, _ := store.LoadRuntime()
	afterFailover, _ := state.NewFailoverStateStore(layout).Load()
	if !reflect.DeepEqual(afterSpec, before.spec) || !reflect.DeepEqual(afterApplied, before.applied) || !reflect.DeepEqual(afterRuntime, before.runtime) || !reflect.DeepEqual(afterFailover, before.failover) {
		t.Fatalf("resource rejection mutated control state:\nbefore=%+v\nafter=%+v", before, boundedControlState{afterSpec, afterApplied, afterRuntime, afterFailover})
	}
	if journals, listErr := state.NewTransactionStore(layout).List(); listErr != nil || len(journals) != 0 {
		t.Fatalf("resource rejection left transaction material: journals=%+v err=%v", journals, listErr)
	}
}
