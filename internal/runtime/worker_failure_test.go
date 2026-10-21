package runtime

import (
	"context"
	"testing"
	"time"

	"smartgate/internal/provider"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

func TestWorkerFailureMovesOnlyBoundPoolToHealthyOtherWorker(t *testing.T) {
	server, store, nodes := newWorkerFailureTestServer(t, state.NoCandidateBlock, true, true)
	before := bindingsByPool(server.currentStatus().Bindings)
	controller := newWorkerFailureController(server.cfg)
	primaryWorker := runtimeTestWorkerDescriptors[runtimeCoreA].WorkerID(1)
	secondaryWorker := runtimeTestWorkerDescriptors[runtimeCoreB].WorkerID(1)
	if err := controller.Handle(context.Background(), worker.Failure{
		WorkerID: secondaryWorker, Core: runtimeCoreB, Pools: []string{"ai"},
	}); err != nil {
		t.Fatal(err)
	}
	topology, err := store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	if topology.ProxySelection["ai"] != nodes["ai-target"].ID || topology.Selections["ai"].Generation != before["ai"].Generation+1 {
		t.Fatalf("AI did not migrate to the healthy sing-box candidate: %+v", topology.Selections["ai"])
	}
	after := bindingsByPool(server.currentStatus().Bindings)
	if after["general"].ID != before["general"].ID || after["general"].Generation != before["general"].Generation || after["general"].Target != before["general"].Target {
		t.Fatalf("Xray failure changed General Binding: before=%+v after=%+v", before["general"], after["general"])
	}
	if after["ai"].Target.Core != runtimeCoreA || !server.cfg.Workers.IsRunning(primaryWorker) || server.cfg.Workers.IsRunning(secondaryWorker) {
		t.Fatalf("worker lifecycle did not isolate the failed Xray worker: bindings=%+v workers=%+v", after, server.cfg.Workers.Statuses())
	}
	events, err := state.NewFailoverEventStore(server.cfg.Layout).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].PoolID != "ai" || events[0].Source != "worker_supervisor" || events[0].CutoverResult != "cutover_complete" || events[0].GenerationTo != before["ai"].Generation+1 {
		t.Fatalf("worker failure event is incomplete: %+v", events)
	}
}

func TestWorkerFailureBlocksOnlyAffectedPoolWhenNoCandidateExists(t *testing.T) {
	server, store, _ := newWorkerFailureTestServer(t, state.NoCandidateBlock, true, false)
	before := bindingsByPool(server.currentStatus().Bindings)
	controller := newWorkerFailureController(server.cfg)
	if err := controller.Handle(context.Background(), worker.Failure{
		WorkerID: runtimeTestWorkerDescriptors[runtimeCoreB].WorkerID(1), Core: runtimeCoreB, Pools: []string{"ai"},
	}); err != nil {
		t.Fatal(err)
	}
	topology, err := store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	selection := topology.Selections["ai"]
	if topology.ProxySelection["ai"] != "Block" || selection.Target != state.NoCandidateBlock || selection.Generation != before["ai"].Generation+1 {
		t.Fatalf("affected AI pool was not forced to Block: %+v", selection)
	}
	after := bindingsByPool(server.currentStatus().Bindings)
	if after["general"].ID != before["general"].ID || after["general"].Generation != before["general"].Generation {
		t.Fatalf("Block fallback changed General: before=%+v after=%+v", before["general"], after["general"])
	}
}

func TestWorkerFailureUsesExplicitDirectPolicyRegardlessOfAIRole(t *testing.T) {
	server, store, _ := newWorkerFailureTestServer(t, state.NoCandidateDirect, true, false)
	controller := newWorkerFailureController(server.cfg)
	if err := controller.Handle(context.Background(), worker.Failure{
		WorkerID: runtimeTestWorkerDescriptors[runtimeCoreB].WorkerID(1), Core: runtimeCoreB, Pools: []string{"ai"},
	}); err != nil {
		t.Fatal(err)
	}
	topology, err := store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	if topology.ProxySelection["ai"] != "Direct" || topology.Selections["ai"].Target != state.NoCandidateDirect {
		t.Fatalf("AI role overrode its explicit Direct policy: %+v", topology.Selections["ai"])
	}
}

func TestWorkerFailureHonorsAutoFailoverDisabled(t *testing.T) {
	server, store, nodes := newWorkerFailureTestServer(t, state.NoCandidateBlock, false, true)
	before := bindingsByPool(server.currentStatus().Bindings)
	controller := newWorkerFailureController(server.cfg)
	if err := controller.Handle(context.Background(), worker.Failure{
		WorkerID: runtimeTestWorkerDescriptors[runtimeCoreB].WorkerID(1), Core: runtimeCoreB, Pools: []string{"ai"},
	}); err != nil {
		t.Fatal(err)
	}
	topology, err := store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	if topology.ProxySelection["ai"] != nodes["ai-source"].ID || topology.Selections["ai"].Generation != before["ai"].Generation {
		t.Fatalf("disabled auto_failover mutated the Binding: %+v", topology.Selections["ai"])
	}
	events, err := state.NewFailoverEventStore(server.cfg.Layout).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].CutoverResult != "skipped" || events[0].Reason != "worker_failed_auto_failover_disabled" {
		t.Fatalf("disabled auto_failover was not observable: %+v", events)
	}
}

func newWorkerFailureTestServer(t *testing.T, noCandidate string, autoFailover, includeAlternative bool) (*Server, *state.Store, map[string]smartnode.SmartNode) {
	t.Helper()
	server, store := newNodesTestServerWithControl(t, &fakeProxyControl{})
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: provider.TypeManual, Enabled: true}}
	raw := []map[string]any{
		{"type": "vmess", "tag": "general", "server": "general.example", "server_port": 443},
		{"type": "vless", "tag": "ai-source", "server": "ai-source.example", "server_port": 443, "uuid": "ai-source-secret"},
	}
	if includeAlternative {
		raw = append(raw, map[string]any{"type": "vmess", "tag": "ai-target", "server": "ai-target.example", "server_port": 443})
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", raw); err != nil {
		t.Fatal(err)
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err = provider.ReconcileSmartNodes(spec, snapshots, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]smartnode.SmartNode{}
	for id, node := range spec.SmartNodes {
		if len(node.Aliases) == 0 {
			continue
		}
		tag := node.Aliases[len(node.Aliases)-1].RawTag
		switch tag {
		case "ai-source":
			node.CoreChoice = runtimeCoreB
		default:
			node.CoreChoice = runtimeCoreA
		}
		spec.SmartNodes[id] = node
		nodes[tag] = node
	}
	aiNodeIDs := []string{nodes["ai-source"].ID}
	if includeAlternative {
		aiNodeIDs = append(aiNodeIDs, nodes["ai-target"].ID)
	}
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", NodeIDs: []string{nodes["general"].ID}, Policy: state.PoolPolicy{Cutover: state.CutoverGraceful, GracefulDrainSeconds: 1, NoCandidate: state.NoCandidateDirect, AutoFailover: true}},
		{ID: "ai", Name: "AI", Role: "ai", NodeIDs: aiNodeIDs, Policy: state.PoolPolicy{Cutover: state.CutoverStrict, GracefulDrainSeconds: 1, NoCandidate: noCandidate, AutoFailover: autoFailover}},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	health := map[string]state.NodeHealth{}
	if includeAlternative {
		health[nodes["ai-target"].ID] = state.NodeHealth{Status: "ok", UpdatedAt: &now}
	}
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": nodes["general"].ID, "ai": nodes["ai-source"].ID},
		NodeHealth:     health,
	}); err != nil {
		t.Fatal(err)
	}
	captureAppliedTopology(t, server, store, spec)
	return server, store, nodes
}

func bindingsByPool(bindings []state.PoolBinding) map[string]state.PoolBinding {
	out := make(map[string]state.PoolBinding, len(bindings))
	for _, binding := range bindings {
		out[binding.Pool] = binding
	}
	return out
}
