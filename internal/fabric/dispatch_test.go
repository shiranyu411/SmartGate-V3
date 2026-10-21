package fabric_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"smartgate/internal/fabric"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func TestDispatchPlanCoversDualStackUDPQUICAndOwnerTags(t *testing.T) {
	graph := dualWorkerGraph(t)
	owners := netgate.NewOwnerRegistry("smartgate")
	plan, err := fabric.BuildDispatchDryRunPlan("fabric-test", owners, graph.Fabric, fabric.DispatchPlanOptions{LANDevice: "br-lan"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRunOnly || len(plan.Steps) == 0 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, resource := range plan.Resources {
		if !resource.IsOwnerTagged() {
			t.Fatalf("resource is not owner tagged: %+v", resource)
		}
	}
	script := plan.Steps[0].Commands[0].Argv[len(plan.Steps[0].Commands[0].Argv)-1]
	for _, want := range []string{
		"type filter hook prerouting priority mangle", "iifname \"__SMARTGATE_LAN_DEVICE__\"",
		"ip daddr {", "ip6 daddr {", "meta l4proto tcp tproxy", "meta l4proto udp tproxy",
		"udp dport { 53, 67, 68, 5353, 1900 }", "tcp dport 853 reject", "udp dport 853 reject",
		"no-resolv", "conf-dir=", "dnsmasq --test",
		"ct mark", "0x50000000", "network.interface.lan", "server=127.0.0.1#1053",
		"iifname != \"lo\" iifname != \"__SMARTGATE_LAN_DEVICE__\" tcp dport 1053 drop",
		"iifname != \"lo\" iifname != \"__SMARTGATE_LAN_DEVICE__\" udp dport 1053 drop",
		owners.Comment(netgate.KindFabricDispatch, "general"), owners.Comment(netgate.KindFabricDispatch, "ai"),
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("reconcile script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "udp dport 443 return") {
		t.Fatal("QUIC was bypassed instead of following pool dispatch")
	}
}

func TestDispatchPlanDoesNotInferPolicyFromPoolName(t *testing.T) {
	graph := dualWorkerGraph(t)
	graph.Fabric.FallbackPool = "ai"
	plan, err := fabric.BuildDispatchDryRunPlan("custom-fallback", netgate.NewOwnerRegistry("smartgate"), graph.Fabric, fabric.DispatchPlanOptions{LANDevice: "lan0"})
	if err != nil {
		t.Fatal(err)
	}
	script := plan.Steps[0].Commands[0].Argv[len(plan.Steps[0].Commands[0].Argv)-1]
	ai := dispatchForPool(t, graph.Fabric.Dispatch, "ai")
	if !strings.Contains(script, "meta mark set "+fmt.Sprintf("0x%08x", ai.Mark)) {
		t.Fatalf("configured fallback did not drive dispatch:\n%s", script)
	}
}

func TestDispatchGraphRejectsMissingWorkerPort(t *testing.T) {
	graph := dualWorkerGraph(t)
	graph.Fabric.Dispatch[0].Port = 0
	if _, err := fabric.BuildDispatchDryRunPlan("missing-port", netgate.NewOwnerRegistry("smartgate"), graph.Fabric, fabric.DispatchPlanOptions{}); err == nil {
		t.Fatal("expected missing worker port to fail closed")
	}
}

func dualWorkerGraph(t *testing.T) state.ExecutionGraph {
	t.Helper()
	spec := state.DefaultSpecState()
	primaryNode := graphNode(t, "general", smartnode.CoreID("sing-box"))
	secondaryNode := graphNode(t, "ai", smartnode.CoreID("xray"))
	spec.SmartNodes = map[string]smartnode.SmartNode{primaryNode.ID: primaryNode, secondaryNode.ID: secondaryNode}
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Role: "general", NodeIDs: []string{primaryNode.ID}, Policy: state.TemplatePoolPolicy("general")},
		{ID: "ai", Role: "ai", NodeIDs: []string{secondaryNode.ID}, Policy: state.TemplatePoolPolicy("ai")},
	}
	spec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{
		"direct":       {IPCIDR: []string{"198.51.100.0/24"}},
		"generalproxy": {},
		"ai":           {DomainSuffix: []string{"ai.example"}, IPCIDR: []string{"203.0.113.0/24"}},
		"block":        {DomainSuffix: []string{"blocked.example"}},
	}, spec.BusinessPools)
	plan := planner.BuildExecutionGraph(spec, nil, state.ExecutionGraph{}, time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC))
	if !plan.ReadyToApply {
		t.Fatalf("graph is not ready: %+v", plan.Issues)
	}
	return plan.Graph
}

func graphNode(t *testing.T, tag string, core smartnode.CoreID) smartnode.SmartNode {
	t.Helper()
	node, err := smartnode.Normalize(smartnode.RawNode{ProviderID: "p1", RawTag: tag, Outbound: map[string]any{
		"type": "vless", "tag": tag, "server": tag + ".example", "server_port": 443, "uuid": tag + "-secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	node.CoreChoice = core
	return node
}

func dispatchForPool(t *testing.T, dispatches []state.PoolDispatchGraph, pool string) state.PoolDispatchGraph {
	t.Helper()
	for _, dispatch := range dispatches {
		if dispatch.Pool == pool {
			return dispatch
		}
	}
	t.Fatalf("missing dispatch for %s", pool)
	return state.PoolDispatchGraph{}
}
