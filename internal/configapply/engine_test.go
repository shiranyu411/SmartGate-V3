package configapply

import (
	"context"
	"encoding/json"
	"testing"

	"smartgate/internal/fabric"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type planRunner struct{ calls int }

func (r *planRunner) Run(context.Context, []string) (string, error) {
	r.calls++
	return "ok", nil
}

func TestApplyExecutesThePreparedOpenWrtPlan(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	runner := &planRunner{}
	var calls []string
	engine := Engine{Owners: netgate.NewOwnerRegistry("smartgate"), NetGateRunner: runner, Workers: &graphWorkerController{calls: &calls}, Fabric: &graphFabricController{calls: &calls}}
	prepared, err := engine.Prepare(context.Background(), "cleanup-1", TargetState{Spec: spec}, 4)
	if err != nil {
		t.Fatal(err)
	}
	prepared.OpenWrtPlan.OpID = "different-op"
	if _, err := engine.Apply(context.Background(), prepared, nil); err == nil {
		t.Fatal("expected tampered prepared plan to be rejected")
	}
	if runner.calls != 0 {
		t.Fatalf("tampered plan executed %d commands", runner.calls)
	}
}

func TestPrepareOffModeUsesInjectedWorkerCleanupTargets(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	var calls []string
	engine := Engine{
		Owners: netgate.NewOwnerRegistry("smartgate"),
		WorkerCleanupTargets: []netgate.WorkerCleanupTarget{
			{ID: "worker-third", DisplayName: "Third Core"},
		},
		Workers: &graphWorkerController{calls: &calls},
		Fabric:  &graphFabricController{calls: &calls},
	}
	prepared, err := engine.Prepare(context.Background(), "cleanup-workers", TargetState{Spec: spec}, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, step := range prepared.OpenWrtPlan.Steps {
		if step.Resource.Kind == netgate.KindWorkerService && step.Resource.ID == "worker-third" && step.Name == "stop Third Core Worker" {
			found = true
		}
	}
	if !found {
		t.Fatalf("configapply cleanup omitted injected worker: %+v", prepared.OpenWrtPlan.Steps)
	}
}

func TestProtectedIdentitiesFallsBackToAppliedReferences(t *testing.T) {
	oldID := "sn_old"
	spec := state.DefaultSpecState()
	spec.SmartNodes = map[string]smartnode.SmartNode{
		oldID: {ID: oldID, Identity: smartnode.NodeIdentity{Protocol: "vless", Server: "old.example", Port: 443}},
	}
	nodes := []provider.NodeView{{
		ID: oldID, Tag: "refreshed-node",
		Identity:   smartnode.NodeIdentity{Protocol: "vless", Server: "new.example", Port: 443},
		References: map[string]json.RawMessage{"ref": json.RawMessage(`{"value":"ok"}`)},
	}}
	fallback := []state.LastGoodSnapshot{{ProviderID: "p1", Identities: []smartnode.IdentitySource{{
		RawTag: "old-node", Identity: spec.SmartNodes[oldID].Identity,
		References: map[string]json.RawMessage{"ref": json.RawMessage(`{"value":"ok"}`)},
	}}}}
	identities := protectedIdentities(spec, fallback, nodes)
	if got := identities[oldID].Identity.Server; got != "new.example" {
		t.Fatalf("refreshed node identity did not win: %q", got)
	}
	if _, ok := identities[oldID].References["ref"]; !ok {
		t.Fatalf("refreshed node references missing: %+v", identities[oldID].References)
	}
	identities = protectedIdentities(spec, fallback, nil)
	if _, ok := identities[oldID].References["ref"]; !ok {
		t.Fatalf("applied node references did not override spec fallback: %+v", identities[oldID].References)
	}
	identities = protectedIdentities(spec, nil, nil)
	if got := identities[oldID].Identity.Server; got != "old.example" {
		t.Fatalf("persisted node fallback missing: %q", got)
	}
}

func TestMergeRequiredLastGoodRetainsRemovedAppliedNode(t *testing.T) {
	identity := smartnode.NodeIdentity{Protocol: "vless", Server: "old.example", Port: 443, SecretRef: "secret-ref"}
	digest := smartnode.IdentityDigest(identity)
	nodeID := "sn_" + digest[:16]
	spec := state.DefaultSpecState()
	// The refreshed Spec no longer contains the previously selected node.
	fallback := []state.LastGoodSnapshot{{ProviderID: "p1", Identities: []smartnode.IdentitySource{{
		RawTag: "old-node", Identity: identity,
		References: map[string]json.RawMessage{"secret-ref": json.RawMessage(`{"value":"ok"}`)},
	}}}}
	previous := state.ExecutionGraph{NodeExecutors: []state.NodeExecutor{{SmartNode: nodeID}}}
	merged := mergeRequiredLastGood(spec, nil, fallback, previous, nil, nil)
	if len(merged) != 1 || len(merged[0].Identities) != 1 {
		t.Fatalf("removed applied node was not retained: %+v", merged)
	}
	if _, ok := merged[0].Identities[0].References["secret-ref"]; !ok {
		t.Fatalf("retained applied node lost protected reference: %+v", merged[0].Identities[0].References)
	}
}

func TestExecutionGraphApplyAndRollbackOrdering(t *testing.T) {
	var calls []string
	workers := &graphWorkerController{calls: &calls}
	sourceFabric := fabric.PreparedFabric{OpID: "source-fabric", GatewayMode: state.GatewayModeOn, Plan: netgate.DryRunPlan{OpID: "source-fabric", DryRunOnly: true}}
	fabricController := &graphFabricController{calls: &calls, current: sourceFabric}
	engine := Engine{
		Owners: netgate.NewOwnerRegistry("smartgate"), NetGateRunner: &planRunner{},
		Workers: workers, Fabric: fabricController,
	}
	spec, snapshots, nodeID := graphTarget(t)
	prepared, err := engine.Prepare(context.Background(), "graph-apply", TargetState{
		Spec: spec, LastGood: snapshots, ProxySelection: map[string]string{"general": nodeID},
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Workers == nil || prepared.Fabric == nil || prepared.GraphHash == "" {
		t.Fatalf("unexpected graph artifacts: %+v", prepared)
	}
	checkpoint, err := engine.Checkpoint("graph-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Apply(context.Background(), prepared, map[string]string{"general": nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "workers-apply" || calls[1] != "fabric-apply" {
		t.Fatalf("apply order = %v", calls)
	}
	if !result.FabricApplied || result.FinalSelections["general"] != nodeID {
		t.Fatalf("unexpected graph apply result: %+v", result)
	}

	calls = nil
	rollback, err := engine.Rollback(context.Background(), prepared, map[string]string{"general": nodeID}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "fabric-apply" || calls[1] != "workers-rollback" {
		t.Fatalf("rollback order = %v", calls)
	}
	if len(fabricController.applied) < 2 || fabricController.applied[len(fabricController.applied)-1].OpID != sourceFabric.OpID {
		t.Fatalf("rollback did not restore captured Fabric: %+v", fabricController.applied)
	}
	if !rollback.FabricRestored || !rollback.WorkersRestored || !rollback.BindingsRestored {
		t.Fatalf("rollback restoration status is incomplete: %+v", rollback)
	}
}

type graphWorkerController struct {
	calls *[]string
}

func (controller *graphWorkerController) Prepare(_ context.Context, opID string, graph state.ExecutionGraph, _ map[string]smartnode.IdentitySource) (worker.Prepared, error) {
	prepared := worker.Prepared{OpID: opID, Candidates: map[string]worker.Candidate{}, Targets: graph.Workers, Executors: graph.NodeExecutors, Bindings: graph.PoolBindings}
	for _, target := range graph.Workers {
		prepared.Candidates[target.ID] = worker.Candidate{OpID: opID, WorkerID: target.ID, Core: target.Core, Path: "/tmp/" + target.ID, Size: 128, ConfigSHA256: target.ID + "-hash", CheckOK: true}
	}
	return prepared, nil
}

func (*graphWorkerController) Checkpoint(string) (worker.Checkpoint, error) {
	return worker.Checkpoint{Backups: map[string]worker.Backup{}}, nil
}

func (controller *graphWorkerController) Apply(context.Context, worker.Prepared) (worker.ApplyResult, error) {
	*controller.calls = append(*controller.calls, "workers-apply")
	return worker.ApplyResult{}, nil
}

func (controller *graphWorkerController) Rollback(context.Context, worker.Checkpoint) (worker.ApplyResult, error) {
	*controller.calls = append(*controller.calls, "workers-rollback")
	return worker.ApplyResult{}, nil
}

func (controller *graphWorkerController) StopAll(context.Context) error {
	*controller.calls = append(*controller.calls, "workers-stop")
	return nil
}

func (*graphWorkerController) Cleanup(worker.Prepared) {}

type graphFabricController struct {
	calls   *[]string
	current fabric.PreparedFabric
	applied []fabric.PreparedFabric
}

func (controller *graphFabricController) Prepare(_ context.Context, opID string, spec state.SpecState, graph state.FabricGraph) (fabric.PreparedFabric, error) {
	prepared := fabric.PreparedFabric{
		OpID: opID, Graph: graph, DNS: state.EffectiveDNSSpec(spec.DNS), GatewayMode: state.EffectiveGatewayMode(spec.Settings.GatewayMode),
		Plan: netgate.DryRunPlan{OpID: opID, DryRunOnly: true},
	}
	return prepared, nil
}

func (controller *graphFabricController) Apply(_ context.Context, prepared fabric.PreparedFabric) (fabric.FabricApplyResult, error) {
	*controller.calls = append(*controller.calls, "fabric-apply")
	controller.current = prepared
	controller.applied = append(controller.applied, prepared)
	return fabric.FabricApplyResult{Applied: true}, nil
}

func (*graphFabricController) Stop(context.Context) error { return nil }

func (controller *graphFabricController) Current() (fabric.PreparedFabric, bool) {
	return controller.current, controller.current.OpID != ""
}

func graphTarget(t *testing.T) (state.SpecState, []state.LastGoodSnapshot, string) {
	t.Helper()
	raw := map[string]any{
		"type": "vless", "tag": "general-node", "server": "general.example", "server_port": 443, "uuid": "test-secret",
	}
	node, err := smartnode.Normalize(smartnode.RawNode{ProviderID: "p1", RawTag: "general-node", Outbound: raw})
	if err != nil {
		t.Fatal(err)
	}
	node.CoreChoice = smartnode.CoreID("sing-box")
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Type: provider.TypeManual, Enabled: true}}
	spec.SmartNodes = map[string]smartnode.SmartNode{node.ID: node}
	pools := state.DefaultBusinessPools()
	for index := range pools {
		if pools[index].ID == "general" {
			pools[index].NodeIDs = []string{node.ID}
		}
	}
	spec.BusinessPools = pools
	return spec, []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{raw}}}, node.ID
}
