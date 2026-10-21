package planner

import (
	"testing"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func TestBuildExecutionGraphRunsTwoRegisteredCoresTogether(t *testing.T) {
	spec := state.DefaultSpecState()
	primaryNode := testNode(t, "p1", "general", "general.example", testPrimaryCore, nil)
	secondaryNode := testNode(t, "p1", "ai", "ai.example", testSecondaryCore, map[string]any{"type": "xhttp", "path": "/api"})
	spec.SmartNodes = map[string]smartnode.SmartNode{primaryNode.ID: primaryNode, secondaryNode.ID: secondaryNode}
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", NodeIDs: []string{primaryNode.ID}, Policy: state.PoolPolicy{Cutover: state.CutoverGraceful, GracefulDrainSeconds: 30, NoCandidate: state.NoCandidateDirect, AutoFailover: true}},
		{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{secondaryNode.ID}, Policy: state.PoolPolicy{Cutover: state.CutoverStrict, GracefulDrainSeconds: 30, NoCandidate: state.NoCandidateBlock, AutoFailover: true}},
	}
	spec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{"generalproxy": {}, "ai": {}}, spec.BusinessPools)
	plan := BuildExecutionGraph(spec, nil, state.ExecutionGraph{}, time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC))
	// Graph hash golden values in this file were captured from RFC baseline 3451082.
	if plan.GraphHash != "89cae808725ca2292353894471e51685afb08954b4bda7da09f062bb80a5b6a3" {
		t.Fatalf("dual-core graph hash = %s", plan.GraphHash)
	}
	if !plan.ReadyToApply || len(plan.Graph.Workers) != 2 || len(plan.Graph.NodeExecutors) != 2 || len(plan.Graph.PoolBindings) != 2 {
		t.Fatalf("unexpected dual-worker plan: %+v", plan)
	}
	if plan.Graph.PoolBindings[0].Target.Core == plan.Graph.PoolBindings[1].Target.Core {
		t.Fatalf("pools were not bound to distinct cores: %+v", plan.Graph.PoolBindings)
	}
}

func TestBuildExecutionGraphSharesExecutorButNotBinding(t *testing.T) {
	spec := state.DefaultSpecState()
	node := testNode(t, "p1", "shared", "shared.example", testPrimaryCore, nil)
	spec.SmartNodes = map[string]smartnode.SmartNode{node.ID: node}
	policy := state.PoolPolicy{Cutover: state.CutoverStrict, GracefulDrainSeconds: 30, NoCandidate: state.NoCandidateBlock, AutoFailover: true}
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "one", Name: "One", NodeIDs: []string{node.ID}, Policy: policy},
		{ID: "two", Name: "Two", NodeIDs: []string{node.ID}, Policy: policy},
	}
	plan := BuildExecutionGraph(spec, map[string]state.PoolSelection{
		"one": {SmartNode: node.ID, Target: "node", Generation: 3},
		"two": {SmartNode: node.ID, Target: "node", Generation: 8},
	}, state.ExecutionGraph{}, time.Now().UTC())
	if plan.GraphHash != "38f5071c44fb279065879d94a70a71b20e953be551a8396b2164d37f10091c94" {
		t.Fatalf("shared-executor graph hash = %s", plan.GraphHash)
	}
	if len(plan.Graph.NodeExecutors) != 1 || len(plan.Graph.PoolBindings) != 2 {
		t.Fatalf("shared node graph is wrong: %+v", plan.Graph)
	}
	left, right := plan.Graph.PoolBindings[0], plan.Graph.PoolBindings[1]
	if left.ID == right.ID || left.Target.Executor != right.Target.Executor || left.Generation == right.Generation {
		t.Fatalf("binding identity was not isolated: left=%+v right=%+v", left, right)
	}
}

func TestBuildExecutionGraphBlocksUnsetCoreAndUsesExplicitFallback(t *testing.T) {
	spec := state.DefaultSpecState()
	node := testNode(t, "p1", "unset", "unset.example", "", nil)
	spec.SmartNodes = map[string]smartnode.SmartNode{node.ID: node}
	spec.BusinessPools = []state.BusinessPoolSpec{{
		ID: "custom", Name: "Custom", NodeIDs: []string{node.ID},
		Policy: state.PoolPolicy{Cutover: state.CutoverStrict, GracefulDrainSeconds: 30, NoCandidate: state.NoCandidateBlock, AutoFailover: true},
	}}
	plan := BuildExecutionGraph(spec, nil, state.ExecutionGraph{}, time.Now().UTC())
	if plan.GraphHash != "330e74497b25546919a7c32503015a4d57037801debbd2d70ae375539ca7aaff" {
		t.Fatalf("unset-core fallback graph hash = %s", plan.GraphHash)
	}
	if plan.ReadyToApply || len(plan.Issues) == 0 || plan.Issues[0].Code != "node_core_unset" {
		t.Fatalf("unset core did not block graph: %+v", plan)
	}
	if len(plan.Graph.PoolBindings) != 1 || plan.Graph.PoolBindings[0].Target.Kind != state.NoCandidateBlock {
		t.Fatalf("explicit fail-closed fallback was not planned: %+v", plan.Graph.PoolBindings)
	}
}

func TestBuildExecutionGraphBlocksUnknownCore(t *testing.T) {
	spec := state.DefaultSpecState()
	node := testNode(t, "p1", "unknown", "unknown.example", "unregistered", nil)
	spec.SmartNodes = map[string]smartnode.SmartNode{node.ID: node}
	spec.BusinessPools = []state.BusinessPoolSpec{{
		ID: "custom", Name: "Custom", NodeIDs: []string{node.ID},
		Policy: state.PoolPolicy{Cutover: state.CutoverStrict, GracefulDrainSeconds: 30, NoCandidate: state.NoCandidateBlock, AutoFailover: true},
	}}

	plan := BuildExecutionGraph(spec, nil, state.ExecutionGraph{}, time.Now().UTC())
	if plan.ReadyToApply || len(plan.Issues) == 0 || plan.Issues[0].Code != "node_core_unknown" || !plan.Issues[0].Blocking {
		t.Fatalf("unknown core did not block graph: %+v", plan)
	}
}

func TestBuildExecutionGraphReconcilesRemovedPoolSelection(t *testing.T) {
	spec := state.DefaultSpecState()
	first := testNode(t, "p1", "first", "first.example", testPrimaryCore, nil)
	second := testNode(t, "p1", "second", "second.example", testPrimaryCore, nil)
	spec.SmartNodes = map[string]smartnode.SmartNode{first.ID: first, second.ID: second}
	spec.BusinessPools = []state.BusinessPoolSpec{{
		ID: "general", Name: "General", Role: "general", NodeIDs: []string{first.ID, second.ID},
		Policy: state.TemplatePoolPolicy("general"),
	}}
	spec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{"generalproxy": {}}, spec.BusinessPools)
	initial := BuildExecutionGraph(spec, map[string]state.PoolSelection{
		"general": {SmartNode: first.ID, Target: "node", Generation: 1},
	}, state.ExecutionGraph{}, time.Now().UTC())
	if !initial.ReadyToApply {
		t.Fatalf("initial graph: %+v", initial.Issues)
	}
	for _, removed := range []string{first.ID, second.ID} {
		t.Run(removed, func(t *testing.T) {
			remaining := second.ID
			if removed == second.ID {
				remaining = first.ID
			}
			nextSpec := spec
			nextSpec.BusinessPools = append([]state.BusinessPoolSpec(nil), spec.BusinessPools...)
			nextSpec.BusinessPools[0].NodeIDs = []string{remaining}
			selections := map[string]state.PoolSelection{
				"general": {SmartNode: removed, Target: "node", Generation: 2},
			}
			next := BuildExecutionGraph(nextSpec, selections, initial.Graph, time.Now().UTC())
			if !next.ReadyToApply || next.Graph.PoolBindings[0].Target.SmartNode != remaining {
				t.Fatalf("removed selection blocked the replacement: %+v", next)
			}
			if selections["general"].SmartNode != removed || initial.Graph.PoolBindings[0].Target.SmartNode != first.ID {
				t.Fatal("preview changed the source selection or graph")
			}
			nextSpec.BusinessPools[0].NodeIDs = nil
			empty := BuildExecutionGraph(nextSpec, selections, initial.Graph, time.Now().UTC())
			if !empty.ReadyToApply || empty.Graph.PoolBindings[0].Target.Kind != state.NoCandidateDirect {
				t.Fatalf("empty pool did not use its explicit fallback: %+v", empty)
			}
		})
	}
	invalid := BuildExecutionGraph(spec, map[string]state.PoolSelection{
		"general": {SmartNode: "sn_unknown", Target: "node", Generation: 2},
	}, initial.Graph, time.Now().UTC())
	if invalid.ReadyToApply {
		t.Fatal("an unrelated invalid selection was accepted")
	}
}

func TestBuildExecutionGraphAdvancesOnlyChangedPoolGeneration(t *testing.T) {
	spec := state.DefaultSpecState()
	first := testNode(t, "p1", "first", "first.example", testPrimaryCore, nil)
	second := testNode(t, "p1", "second", "second.example", testPrimaryCore, nil)
	spec.SmartNodes = map[string]smartnode.SmartNode{first.ID: first, second.ID: second}
	spec.BusinessPools = []state.BusinessPoolSpec{{
		ID: "general", Name: "General", Role: "general", NodeIDs: []string{first.ID, second.ID},
		Policy: state.PoolPolicy{Cutover: state.CutoverStrict, GracefulDrainSeconds: 30, NoCandidate: state.NoCandidateBlock, AutoFailover: true},
	}}
	initial := BuildExecutionGraph(spec, map[string]state.PoolSelection{
		"general": {SmartNode: first.ID, Target: "node", Generation: 4},
	}, state.ExecutionGraph{}, time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC))
	if initial.GraphHash != "8dad13f1e279b079740e87a67a40ffe9463301c4c33c4607699684cf932b512e" {
		t.Fatalf("initial generation graph hash = %s", initial.GraphHash)
	}
	next := BuildExecutionGraph(spec, map[string]state.PoolSelection{
		"general": {SmartNode: second.ID, Target: "node", Generation: 4},
	}, initial.Graph, time.Date(2026, 7, 28, 2, 1, 0, 0, time.UTC))
	if next.GraphHash != "02e9c5088fdce22a6bf6e4418fc06513ade8bcae783a4a06564e654a31998e65" {
		t.Fatalf("advanced generation graph hash = %s", next.GraphHash)
	}
	if got := next.Graph.PoolBindings[0].Generation; got != 5 {
		t.Fatalf("changed binding generation = %d, want 5", got)
	}
	stable := BuildExecutionGraph(spec, map[string]state.PoolSelection{
		"general": {SmartNode: second.ID, Target: "node"},
	}, next.Graph, time.Date(2026, 7, 28, 2, 2, 0, 0, time.UTC))
	if stable.GraphHash != "02e9c5088fdce22a6bf6e4418fc06513ade8bcae783a4a06564e654a31998e65" {
		t.Fatalf("stable generation graph hash = %s", stable.GraphHash)
	}
	if got := stable.Graph.PoolBindings[0].Generation; got != 5 {
		t.Fatalf("stable binding generation = %d, want 5", got)
	}
	if !stable.Graph.PoolBindings[0].Since.Equal(next.Graph.PoolBindings[0].Since) {
		t.Fatalf("stable binding changed its since timestamp")
	}
}

func TestBuildExecutionGraphRepairsZeroGenerationFromLegacyBinding(t *testing.T) {
	spec := state.DefaultSpecState()
	node := testNode(t, "p1", "general", "general.example", testPrimaryCore, nil)
	spec.SmartNodes = map[string]smartnode.SmartNode{node.ID: node}
	spec.BusinessPools = []state.BusinessPoolSpec{{
		ID: "general", Name: "General", Role: "general", NodeIDs: []string{node.ID},
		Policy: state.PoolPolicy{Cutover: state.CutoverStrict, GracefulDrainSeconds: 30, NoCandidate: state.NoCandidateBlock, AutoFailover: true},
	}}
	initial := BuildExecutionGraph(spec, map[string]state.PoolSelection{
		"general": {SmartNode: node.ID, Target: "node", Generation: 1},
	}, state.ExecutionGraph{}, time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC))
	if initial.GraphHash != "3252a2e56f1ab5fb194b95664f23e4c77f2325180b0404e4953b006f555d0f87" {
		t.Fatalf("legacy-repair initial graph hash = %s", initial.GraphHash)
	}
	legacy := initial.Graph
	legacy.PoolBindings[0].Generation = 0
	legacySince := legacy.PoolBindings[0].Since

	repaired := BuildExecutionGraph(spec, map[string]state.PoolSelection{
		"general": {SmartNode: node.ID, Target: "node"},
	}, legacy, time.Date(2026, 7, 28, 2, 1, 0, 0, time.UTC))
	if repaired.GraphHash != "3252a2e56f1ab5fb194b95664f23e4c77f2325180b0404e4953b006f555d0f87" {
		t.Fatalf("legacy-repair repaired graph hash = %s", repaired.GraphHash)
	}
	if got := repaired.Graph.PoolBindings[0].Generation; got != 1 {
		t.Fatalf("repaired binding generation = %d, want 1", got)
	}
	if !repaired.Graph.PoolBindings[0].Since.Equal(legacySince) {
		t.Fatal("repair changed stable binding timestamp")
	}
}

func testNode(t *testing.T, providerID, tag, server string, core smartnode.CoreID, transport map[string]any) smartnode.SmartNode {
	t.Helper()
	outbound := map[string]any{
		"type": "vless", "tag": tag, "server": server, "server_port": 443, "uuid": tag + "-secret",
	}
	if transport != nil {
		outbound["transport"] = transport
	}
	node, err := smartnode.Normalize(smartnode.RawNode{ProviderID: providerID, RawTag: tag, Outbound: outbound})
	if err != nil {
		t.Fatal(err)
	}
	node.CoreChoice = core
	return node
}
