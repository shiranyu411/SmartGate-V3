package fabric

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

func TestRouteRulePatternAcceptsFullMaskWithOrWithoutSuffix(t *testing.T) {
	dispatch := state.PoolDispatchGraph{Mark: 0x50010001, MarkMask: 0xffffffff, Table: 1001}
	pattern := routeRulePattern(dispatch)
	for _, line := range []string{
		"11001:\tfrom all fwmark 0x50010001 lookup 1001",
		"11001:\tfrom all fwmark 0x50010001/0xffffffff lookup 1001",
	} {
		if !pattern.MatchString(line) {
			t.Fatalf("%q did not match %q", pattern, line)
		}
	}
	for _, line := range []string{
		"11001:\tfrom all fwmark 0x50010001 lookup 10010",
		"11001:\tfrom all fwmark 0x50010002 lookup 1001",
		"11001:\tfrom all fwmark 0x50010001/0xffffff00 lookup 1001",
	} {
		if pattern.MatchString(line) {
			t.Fatalf("%q matched unrelated rule %q", pattern, line)
		}
	}
	partial := routeRulePattern(state.PoolDispatchGraph{Mark: 0x50010000, MarkMask: 0xffffff00, Table: 1002})
	if !partial.MatchString("from all fwmark 0x50010000/0xffffff00 lookup 1002") || partial.MatchString("from all fwmark 0x50010000 lookup 1002") {
		t.Fatalf("partial mask pattern %q must require the mask", partial)
	}
}

func TestDispatchPlanRouteVerificationToleratesOmittedFullMask(t *testing.T) {
	graph := verifyTestGraph()
	plan, err := BuildDispatchDryRunPlan("route-verify", netgate.NewOwnerRegistry("smartgate"), graph, DispatchPlanOptions{LANDevice: "br-lan"})
	if err != nil {
		t.Fatal(err)
	}
	var routeStep *netgate.DryRunStep
	for index := range plan.Steps {
		if plan.Steps[index].Resource.Kind == netgate.KindFabricRoute && plan.Steps[index].Resource.ID == "general" {
			routeStep = &plan.Steps[index]
		}
	}
	if routeStep == nil {
		t.Fatalf("plan has no route verification for general: %+v", plan.Steps)
	}
	joined := strings.Join(routeStep.Commands[0].Argv, " ")
	if !strings.Contains(joined, "grep -E") || !strings.Contains(joined, "(/0xffffffff)?") {
		t.Fatalf("route verification must accept iproute2 output without the full mask: %s", joined)
	}
}

type verifyTestRunner struct {
	mu       sync.Mutex
	table    string
	tableErr error
	rules4   string
	rules6   string
	fileErr  map[string]error
	commands [][]string
}

func (runner *verifyTestRunner) Run(_ context.Context, argv []string) (string, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.commands = append(runner.commands, argv)
	joined := strings.Join(argv, " ")
	switch {
	case strings.HasPrefix(joined, "nft -t list table"):
		return runner.table, runner.tableErr
	case joined == "ip -4 rule show":
		return runner.rules4, nil
	case joined == "ip -6 rule show":
		return runner.rules6, nil
	case len(argv) > 4 && argv[4] == FabricDNSMasqConfigPath:
		return "", runner.fileErr[FabricDNSMasqConfigPath]
	case len(argv) > 4 && argv[4] == FabricFirewallHookPath:
		return "", runner.fileErr[FabricFirewallHookPath]
	}
	return "", nil
}

func verifyTestGraph() state.FabricGraph {
	return state.FabricGraph{
		Takeover: state.FabricTakeoverGraph{Enabled: true, TCP: true, UDP: true, IPv4: true, IPv6: true},
		DNS: state.FabricDNSGraph{
			Listen: "[::]:1053", RedirectLAN53: true, RejectHTTPSRecords: true, BlockDoT853: true,
			PoolSets: map[string]state.FabricPoolSets{
				TargetDirect: {IPv4: "direct_4", IPv6: "direct_6"},
				TargetBlock:  {IPv4: "block_4", IPv6: "block_6"},
				"general":    {IPv4: "general_4", IPv6: "general_6"},
			},
		},
		FallbackPool: "general",
		Dispatch: []state.PoolDispatchGraph{{
			Pool: "general", Binding: "binding-general", Generation: 1, Mark: 0x50010001,
			MarkMask: 0xffffffff, Table: 1001, Port: 16000, DNSPort: 18000, Target: "w_core_1",
		}},
	}
}

func healthyVerifyRunner(plan netgate.DryRunPlan) *verifyTestRunner {
	var table strings.Builder
	for _, resource := range plan.Resources {
		table.WriteString("comment \"" + resource.Comment + "\"\n")
	}
	return &verifyTestRunner{
		table:  table.String(),
		rules4: "0:\tfrom all lookup local\n11001:\tfrom all fwmark 0x50010001 lookup 1001\n32766:\tfrom all lookup main\n",
		rules6: "0:\tfrom all lookup local\n11001:\tfrom all fwmark 0x50010001/0xffffffff lookup 1001\n",
	}
}

func TestVerifyDispatchPassesWhenHostCarriesEveryOwnedResource(t *testing.T) {
	graph := verifyTestGraph()
	plan, err := BuildDispatchDryRunPlan("verify-ok", netgate.NewOwnerRegistry("smartgate"), graph, DispatchPlanOptions{LANDevice: "br-lan"})
	if err != nil {
		t.Fatal(err)
	}
	runner := healthyVerifyRunner(plan)
	if err := VerifyDispatch(context.Background(), runner, plan, graph); err != nil {
		t.Fatalf("healthy host failed verification: %v", err)
	}
	listings := 0
	for _, argv := range runner.commands {
		if strings.HasPrefix(strings.Join(argv, " "), "nft -t list table") {
			listings++
		}
	}
	if listings != 1 {
		t.Fatalf("verification must list the owner table exactly once (terse), got %d listings: %v", listings, runner.commands)
	}
}

func TestVerifyDispatchReportsMissingTableRulesAndRoutes(t *testing.T) {
	graph := verifyTestGraph()
	owners := netgate.NewOwnerRegistry("smartgate")
	plan, err := BuildDispatchDryRunPlan("verify-drift", owners, graph, DispatchPlanOptions{LANDevice: "br-lan"})
	if err != nil {
		t.Fatal(err)
	}

	gone := healthyVerifyRunner(plan)
	gone.table, gone.tableErr = "", errors.New("Error: No such file or directory")
	if err := VerifyDispatch(context.Background(), gone, plan, graph); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("deleted owner table was not detected: %v", err)
	}

	noHijack := healthyVerifyRunner(plan)
	noHijack.table = strings.ReplaceAll(noHijack.table, owners.Comment(netgate.KindDNSHijack, "lan-dualstack-53"), "")
	err = VerifyDispatch(context.Background(), noHijack, plan, graph)
	if err == nil || !strings.Contains(err.Error(), netgate.KindDNSHijack+":lan-dualstack-53") {
		t.Fatalf("missing DNS hijack rule was not detected: %v", err)
	}

	noRoute := healthyVerifyRunner(plan)
	noRoute.rules6 = "0:\tfrom all lookup local\n"
	err = VerifyDispatch(context.Background(), noRoute, plan, graph)
	if err == nil || !strings.Contains(err.Error(), netgate.KindFabricRoute+":general(ip -6)") {
		t.Fatalf("missing IPv6 policy route was not detected: %v", err)
	}

	noHook := healthyVerifyRunner(plan)
	noHook.fileErr = map[string]error{FabricFirewallHookPath: errors.New("exit status 1")}
	err = VerifyDispatch(context.Background(), noHook, plan, graph)
	if err == nil || !strings.Contains(err.Error(), netgate.KindFirewallHook) {
		t.Fatalf("missing reconcile hook was not detected: %v", err)
	}
}
