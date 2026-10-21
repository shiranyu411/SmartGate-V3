package commitlane

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"smartgate/internal/configapply"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type startupReplayWorkers struct {
	preparedGraphs []state.ExecutionGraph
	applied        []worker.Prepared
	identities     map[string]smartnode.IdentitySource
}

func (controller *startupReplayWorkers) Prepare(_ context.Context, opID string, graph state.ExecutionGraph, identities map[string]smartnode.IdentitySource) (worker.Prepared, error) {
	controller.preparedGraphs = append(controller.preparedGraphs, graph)
	controller.identities = identities
	prepared := worker.Prepared{
		OpID: opID, Candidates: map[string]worker.Candidate{}, Targets: graph.Workers,
		Executors: graph.NodeExecutors, Bindings: graph.PoolBindings,
	}
	for _, target := range graph.Workers {
		prepared.Candidates[target.ID] = worker.Candidate{
			OpID: opID, WorkerID: target.ID, Core: target.Core, Path: "/tmp/" + target.ID,
			Size: 128, ConfigSHA256: target.ID + "-startup-hash", CheckOK: true,
		}
	}
	return prepared, nil
}

func (*startupReplayWorkers) Checkpoint(string) (worker.Checkpoint, error) {
	return worker.Checkpoint{}, nil
}

func (controller *startupReplayWorkers) Apply(_ context.Context, prepared worker.Prepared) (worker.ApplyResult, error) {
	controller.applied = append(controller.applied, prepared)
	return worker.ApplyResult{}, nil
}

func (*startupReplayWorkers) Rollback(context.Context, worker.Checkpoint) (worker.ApplyResult, error) {
	return worker.ApplyResult{}, nil
}

func (*startupReplayWorkers) StopAll(context.Context) error { return nil }
func (*startupReplayWorkers) Cleanup(worker.Prepared)       {}

type startupReplayFabric struct {
	prepared []fabric.PreparedFabric
	applied  []fabric.PreparedFabric
}

func (controller *startupReplayFabric) Prepare(_ context.Context, opID string, spec state.SpecState, graph state.FabricGraph) (fabric.PreparedFabric, error) {
	prepared := fabric.PreparedFabric{
		OpID: opID, Graph: graph, DNS: state.EffectiveDNSSpec(spec.DNS),
		GatewayMode: state.EffectiveGatewayMode(spec.Settings.GatewayMode),
		Plan:        netgate.DryRunPlan{OpID: opID, DryRunOnly: true},
	}
	controller.prepared = append(controller.prepared, prepared)
	return prepared, nil
}

func (controller *startupReplayFabric) Apply(_ context.Context, prepared fabric.PreparedFabric) (fabric.FabricApplyResult, error) {
	controller.applied = append(controller.applied, prepared)
	return fabric.FabricApplyResult{Applied: true}, nil
}

func (*startupReplayFabric) Stop(context.Context) error { return nil }
func (*startupReplayFabric) Current() (fabric.PreparedFabric, bool) {
	return fabric.PreparedFabric{}, false
}

func TestReconcileAppliedDataPlaneReplaysDualWorkersAndDurableBindings(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	spec, snapshots, selections := startupReplayTarget(t)
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	selectionModels := map[string]state.PoolSelection{}
	for pool, nodeID := range selections {
		selectionModels[pool] = state.PoolSelection{SmartNode: nodeID, Target: "node", Generation: 6}
	}
	graphPlan := planner.BuildExecutionGraph(spec, selectionModels, state.ExecutionGraph{}, time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC))
	if !graphPlan.ReadyToApply {
		t.Fatalf("startup fixture graph is blocked: %+v", graphPlan.Issues)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 9, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOn,
		ProxySelection: selections, ExecutionGraph: graphPlan.Graph,
	}, spec, snapshots)
	store := state.NewStore(layout)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(applied, selections)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 9, TopologyHash: topologyHash}); err != nil {
		t.Fatal(err)
	}

	owners := netgate.NewOwnerRegistry("smartgate")
	workers := &startupReplayWorkers{}
	fabricController := &startupReplayFabric{}
	txn := NewConfigTxn(New(lockset.New(), layout, owners), store, configapply.Engine{
		Owners: owners, Workers: workers, Fabric: fabricController,
	})
	if err := txn.ReconcileAppliedDataPlane(context.Background(), "startup-dual-worker-replay"); err != nil {
		t.Fatal(err)
	}
	if len(workers.preparedGraphs) != 1 || len(workers.applied) != 1 || len(fabricController.applied) != 1 {
		t.Fatalf("startup replay did not reach both data-plane layers: worker_prepare=%d worker_apply=%d fabric_apply=%d", len(workers.preparedGraphs), len(workers.applied), len(fabricController.applied))
	}
	if len(workers.identities) != len(graphPlan.Graph.NodeExecutors) {
		t.Fatalf("startup replay lost protected identities: got=%d want=%d", len(workers.identities), len(graphPlan.Graph.NodeExecutors))
	}
	for id, source := range workers.identities {
		if source.Identity.SecretRef != "" && len(source.References) == 0 {
			t.Fatalf("startup replay lost protected references for %s", id)
		}
	}
	replayed := workers.preparedGraphs[0]
	if len(replayed.Workers) != 2 || len(replayed.PoolBindings) != 2 {
		t.Fatalf("startup replay did not restore dual workers: %+v", replayed)
	}
	cores := map[smartnode.CoreID]bool{}
	for _, target := range replayed.Workers {
		cores[target.Core] = true
	}
	if !cores[commitCoreA] || !cores[commitCoreB] {
		t.Fatalf("startup replay worker cores = %+v", cores)
	}
	previousBindings := bindingsByPool(graphPlan.Graph.PoolBindings)
	for _, binding := range replayed.PoolBindings {
		previous := previousBindings[binding.Pool]
		if binding.Generation != previous.Generation || binding.Target.SmartNode != previous.Target.SmartNode || !binding.Since.Equal(previous.Since) {
			t.Fatalf("binding %s was not replayed exactly: previous=%+v replayed=%+v", binding.Pool, previous, binding)
		}
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.ConfigEpoch != applied.ConfigEpoch || runtimeState.ProxySelection["general"] != selections["general"] || runtimeState.ProxySelection["ai"] != selections["ai"] {
		t.Fatalf("startup replay did not restore durable selections: %+v", runtimeState)
	}
}

func TestReconcileAppliedDataPlaneReplaysLegacyAppliedStateWithoutExecutionGraph(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	spec, snapshots, selections := startupReplayTarget(t)
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := writeLegacyAppliedFixture(t, layout, legacyAppliedFixture{
		SchemaVersion: 1, ConfigEpoch: 11, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOn,
		ConfigSHA256: "legacy-singbox-config-sha", ProxySelection: selections,
		SpecCaptured: true, Spec: spec, LastGood: snapshots,
		LegacyConfig: map[string]any{"candidate": "legacy-singbox-candidate"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 11, TopologyHash: topologyHash}); err != nil {
		t.Fatal(err)
	}

	workers := &startupReplayWorkers{}
	fabricController := &startupReplayFabric{}
	owners := netgate.NewOwnerRegistry("smartgate")
	txn := NewConfigTxn(New(lockset.New(), layout, owners), store, configapply.Engine{
		Owners: owners, Workers: workers, Fabric: fabricController,
	})
	if err := txn.ReconcileAppliedDataPlane(context.Background(), "legacy-applied-replay"); err != nil {
		t.Fatal(err)
	}
	assertStartupReplayAppliedGraph(t, workers, fabricController)
}

func TestStartupRecoveryReplaysLegacyApplyingJournalWithoutExecutionGraph(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	spec, snapshots, selections := startupReplayTarget(t)
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	desired := state.DefaultSpecState()
	desired.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(desired); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{
		ConfigEpoch: 11, TopologyHash: topologyHash, CommitInFlight: &state.CommitRef{OpID: "legacy-journal"},
	}); err != nil {
		t.Fatal(err)
	}
	journal := legacyTransactionJournalFixture{
		SchemaVersion: state.TransactionJournalSchemaVersion,
		OpID:          "legacy-journal",
		Kind:          "config_apply",
		Phase:         state.TransactionApplying,
		Source: legacyTransactionStateFixture{
			LastGoodCaptured: true,
			LastGood:         snapshots,
			Applied: legacyAppliedFixture{
				SchemaVersion: 1, ConfigEpoch: 11, TopologyHash: topologyHash, GatewayMode: state.GatewayModeOn,
				ConfigSHA256: "legacy-singbox-config-sha", ProxySelection: selections,
				SpecCaptured: true, Spec: spec, LastGood: snapshots,
				LegacyConfig: map[string]any{"checkpoint": "legacy-singbox-checkpoint"},
			},
			Runtime: state.RuntimeState{
				ConfigEpoch: 11, TopologyHash: topologyHash, ProxySelection: selections,
				CommitInFlight: &state.CommitRef{OpID: "legacy-journal"},
			},
		},
		StartedAt: time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC),
	}
	if err := writeLegacyJournalFixture(t, layout, journal); err != nil {
		t.Fatal(err)
	}

	workers := &startupReplayWorkers{}
	fabricController := &startupReplayFabric{}
	owners := netgate.NewOwnerRegistry("smartgate")
	txn := NewConfigTxn(New(lockset.New(), layout, owners), store, configapply.Engine{
		Owners: owners, Workers: workers, Fabric: fabricController,
	})
	if err := txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertStartupReplayAppliedGraph(t, workers, fabricController)
	if _, err := state.NewTransactionStore(layout).Load(journal.OpID); !os.IsNotExist(err) {
		t.Fatalf("legacy journal remained after recovery: %v", err)
	}
}

func startupReplayTarget(t *testing.T) (state.SpecState, []state.LastGoodSnapshot, map[string]string) {
	t.Helper()
	generalRaw := map[string]any{"type": "vless", "tag": "general-node", "server": "general.example", "server_port": 443, "uuid": "general-test-secret"}
	aiRaw := map[string]any{
		"type": "vless", "tag": "ai-node", "server": "ai.example", "server_port": 443, "uuid": "ai-test-secret",
		"transport": map[string]any{"type": "xhttp", "path": "/api"},
	}
	general, err := smartnode.Normalize(smartnode.RawNode{ProviderID: "p1", RawTag: "general-node", Outbound: generalRaw})
	if err != nil {
		t.Fatal(err)
	}
	ai, err := smartnode.Normalize(smartnode.RawNode{ProviderID: "p1", RawTag: "ai-node", Outbound: aiRaw})
	if err != nil {
		t.Fatal(err)
	}
	general.CoreChoice = commitCoreA
	ai.CoreChoice = commitCoreB
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "P1", Type: provider.TypeManual, Enabled: true}}
	spec.SmartNodes = map[string]smartnode.SmartNode{general.ID: general, ai.ID: ai}
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", NodeIDs: []string{general.ID}, Policy: state.TemplatePoolPolicy("general")},
		{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{ai.ID}, Policy: state.TemplatePoolPolicy("ai")},
	}
	spec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{"generalproxy": {}, "ai": {DomainSuffix: []string{"ai.example"}}}, spec.BusinessPools)
	snapshots := []state.LastGoodSnapshot{{ProviderID: "p1", Outbounds: []map[string]any{generalRaw, aiRaw}}}
	return spec, snapshots, map[string]string{"general": general.ID, "ai": ai.ID}
}

type legacyAppliedFixture struct {
	SchemaVersion  int                        `json:"schema_version,omitempty"`
	ConfigEpoch    int64                      `json:"config_epoch"`
	TopologyHash   string                     `json:"topology_hash,omitempty"`
	GatewayMode    string                     `json:"gateway_mode,omitempty"`
	ConfigSHA256   string                     `json:"config_sha256,omitempty"`
	ProxySelection map[string]string          `json:"proxy_selection,omitempty"`
	SpecCaptured   bool                       `json:"spec_captured,omitempty"`
	Spec           state.SpecState            `json:"spec,omitempty"`
	LastGood       []state.LastGoodSnapshot   `json:"last_good,omitempty"`
	LegacyConfig   map[string]any             `json:"singbox,omitempty"`
	LegacyRuntime  map[string]map[string]bool `json:"sing_box_runtime,omitempty"`
}

type legacyTransactionStateFixture struct {
	Spec             state.SpecState          `json:"spec"`
	LastGoodCaptured bool                     `json:"last_good_captured,omitempty"`
	LastGood         []state.LastGoodSnapshot `json:"last_good,omitempty"`
	Applied          legacyAppliedFixture     `json:"applied"`
	Runtime          state.RuntimeState       `json:"runtime"`
}

type legacyTransactionJournalFixture struct {
	SchemaVersion int                           `json:"schema_version"`
	OpID          string                        `json:"op_id"`
	Kind          string                        `json:"kind"`
	Phase         state.TransactionPhase        `json:"phase"`
	Source        legacyTransactionStateFixture `json:"source"`
	StartedAt     time.Time                     `json:"started_at"`
}

func writeLegacyAppliedFixture(t *testing.T, layout state.Layout, applied legacyAppliedFixture) error {
	t.Helper()
	body, err := json.MarshalIndent(applied, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(layout.AppliedStatePath(), body, 0o600)
}

func writeLegacyJournalFixture(t *testing.T, layout state.Layout, journal legacyTransactionJournalFixture) error {
	t.Helper()
	body, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(layout.TransactionsDir()+"/"+journal.OpID+".json", body, 0o600)
}

func assertStartupReplayAppliedGraph(t *testing.T, workers *startupReplayWorkers, fabricController *startupReplayFabric) {
	t.Helper()
	if len(workers.preparedGraphs) != 1 || len(workers.applied) != 1 || len(fabricController.applied) != 1 {
		t.Fatalf("legacy replay did not reach graph data plane: worker_prepare=%d worker_apply=%d fabric_apply=%d", len(workers.preparedGraphs), len(workers.applied), len(fabricController.applied))
	}
	replayed := workers.preparedGraphs[0]
	if len(replayed.Workers) != 2 || len(replayed.PoolBindings) != 2 {
		t.Fatalf("legacy replay did not rebuild dual-worker graph: %+v", replayed)
	}
	cores := map[smartnode.CoreID]bool{}
	for _, target := range replayed.Workers {
		cores[target.Core] = true
	}
	if !cores[commitCoreA] || !cores[commitCoreB] {
		t.Fatalf("legacy replay worker cores = %+v", cores)
	}
}

func bindingsByPool(bindings []state.PoolBinding) map[string]state.PoolBinding {
	out := make(map[string]state.PoolBinding, len(bindings))
	for _, binding := range bindings {
		out[binding.Pool] = binding
	}
	return out
}
