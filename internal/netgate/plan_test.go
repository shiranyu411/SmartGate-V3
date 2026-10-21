package netgate

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"smartgate/internal/lockset"
)

func twoWorkerCleanupTargets() []WorkerCleanupTarget {
	return []WorkerCleanupTarget{
		{ID: "worker-alpha", DisplayName: "AlphaCore"},
		{ID: "worker-beta", DisplayName: "BetaCore"},
	}
}

func TestRuntimePlanRequiresOwnerMetadata(t *testing.T) {
	owners := NewOwnerRegistry("smartgate")
	plan := RuntimePlan("apply-1", owners)
	for _, want := range []struct {
		kind string
		id   string
	}{
		{KindTunInterface, "tun-sb"},
		{KindDNSHijack, "lan-ipv4-53"},
		{KindDoTBlock, "lan-ipv4-853"},
		{KindDNSMasq, "wan-cpe-upstream"},
		{KindFirewallHook, "fw4-reconcile"},
	} {
		resource, err := plan.RequireOwned(want.kind, want.id)
		if err != nil {
			t.Fatalf("resource %s/%s missing: %v", want.kind, want.id, err)
		}
		if resource.OwnerTag == "" || resource.Comment == "" {
			t.Fatalf("resource %s/%s missing owner metadata: %+v", want.kind, want.id, resource)
		}
		if !resource.IsOwnerTagged() {
			t.Fatalf("resource %s/%s has malformed owner metadata: %+v", want.kind, want.id, resource)
		}
	}
}

func TestOpenWrtDryRunPlanBuildsOwnerTaggedCommandPlan(t *testing.T) {
	plan, err := OpenWrtDryRunPlan("apply-1", NewOwnerRegistry("smartgate"))
	if err != nil {
		t.Fatalf("dry-run plan: %v", err)
	}
	if !plan.DryRunOnly {
		t.Fatal("expected dry-run plan to be marked dry-run only")
	}
	wantKinds := map[string]bool{
		KindDNSHijack:    false,
		KindDoTBlock:     false,
		KindTunInterface: false,
		KindDNSMasq:      false,
		KindFirewallHook: false,
	}
	for _, step := range plan.Steps {
		if !step.Resource.IsOwnerTagged() {
			t.Fatalf("step %q targets unowned resource: %+v", step.Name, step.Resource)
		}
		if !strings.HasPrefix(step.Resource.OwnerTag, "smartgate:") {
			t.Fatalf("step %q has non-SmartGate owner tag: %s", step.Name, step.Resource.OwnerTag)
		}
		if len(step.Commands) == 0 {
			t.Fatalf("step %q has no command plan", step.Name)
		}
		for _, command := range step.Commands {
			if !commandMentions(command, step.Resource.Comment) {
				t.Fatalf("command for %q does not carry owner comment %q: %+v", step.Name, step.Resource.Comment, command)
			}
		}
		if _, ok := wantKinds[step.Resource.Kind]; ok {
			wantKinds[step.Resource.Kind] = true
		}
	}
	for kind, seen := range wantKinds {
		if !seen {
			t.Fatalf("dry-run plan missing resource kind %s", kind)
		}
	}
}

func TestOpenWrtDryRunPlanRejectsUnsafeOpID(t *testing.T) {
	if _, err := OpenWrtDryRunPlan("../escape", NewOwnerRegistry("smartgate")); err == nil {
		t.Fatal("expected unsafe dry-run op id to be rejected")
	}
	if _, err := OpenWrtRescueCleanupDryRunPlan("../escape", NewOwnerRegistry("smartgate"), twoWorkerCleanupTargets()); err == nil {
		t.Fatal("expected unsafe cleanup op id to be rejected")
	}
}

func TestOpenWrtDNSRulesReplaceOwnerTaggedRulesBeforeAdding(t *testing.T) {
	plan, err := OpenWrtDryRunPlan("apply-1", NewOwnerRegistry("smartgate"))
	if err != nil {
		t.Fatalf("dry-run plan: %v", err)
	}

	dns := requireStep(t, plan, KindDNSHijack)
	if len(dns.Commands) != 1 || !commandMentions(dns.Commands[0], "dstnat_lan") || !commandMentions(dns.Commands[0], "dstnat") {
		t.Fatalf("DNS step should only verify the lock-protected hook result, got %+v", dns.Commands)
	}

	dot := requireStep(t, plan, KindDoTBlock)
	if len(dot.Commands) != 1 || !commandMentions(dot.Commands[0], "forward_lan") {
		t.Fatalf("DoT step should only verify the lock-protected hook result, got %+v", dot.Commands)
	}

	hook := requireStep(t, plan, KindFirewallHook)
	if len(hook.Commands) != 1 {
		t.Fatalf("fw4 reconcile hook commands=%+v", hook.Commands)
	}
	joined := strings.Join(hook.Commands[0].Argv, "\n")
	for _, want := range []string{firewallHookPath, "dstnat_lan", "udp dport 53", "tcp dport 53", "tcp dport 853", "udp dport 853", "smartgate_acquire_lock", "network.interface.lan"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("fw4 reconcile hook missing %q: %+v", want, hook.Commands[0])
		}
	}
	if strings.Contains(joined, "trap '") {
		t.Fatalf("sourced hotplug hook must not install a process-wide EXIT trap: %+v", hook.Commands[0])
	}
	if strings.Index(joined, "nft delete rule") > strings.Index(joined, "nft add rule") {
		t.Fatalf("fw4 reconcile hook must delete prior owner rules before re-adding them: %+v", hook.Commands[0])
	}
	if !strings.Contains(joined, "owner=smartgate:dns-hijack:lan-ipv4-53") || !strings.Contains(joined, "owner=smartgate:dot-block:lan-ipv4-853") {
		t.Fatalf("fw4 hook must reconcile the same owner rules as the live plan: %+v", hook.Commands[0])
	}
}

func TestFirewallReconcileHookHasValidShellSyntax(t *testing.T) {
	plan, err := OpenWrtDryRunPlan("apply-hook-syntax", NewOwnerRegistry("smartgate"))
	if err != nil {
		t.Fatal(err)
	}
	hook := requireStep(t, plan, KindFirewallHook)
	script := hook.Commands[0].Argv[len(hook.Commands[0].Argv)-1]
	check := exec.Command("sh", "-n")
	check.Stdin = strings.NewReader(script)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("fw4 reconcile hook shell syntax: %v: %s\n%s", err, output, script)
	}
}

func TestOpenWrtRescueCleanupOnlyTargetsOwnerTaggedResources(t *testing.T) {
	dryRun, err := OpenWrtRescueCleanupDryRunPlan("cleanup-1", NewOwnerRegistry("smartgate"), twoWorkerCleanupTargets())
	if err != nil {
		t.Fatalf("cleanup dry-run plan: %v", err)
	}
	cleanup, err := CleanupPlanFromDryRun(dryRun)
	if err != nil {
		t.Fatalf("cleanup plan: %v", err)
	}
	if len(cleanup.Actions) == 0 {
		t.Fatal("expected cleanup actions")
	}
	if cleanup.Actions[len(cleanup.Actions)-1].Kind != KindTunInterface {
		t.Fatalf("rescue cleanup must delete TUN last, got %+v", cleanup.Actions)
	}
	fabricIndex, firstWorkerIndex := -1, -1
	workerIDs := map[string]bool{}
	for index, action := range cleanup.Actions {
		if action.Kind == KindFabricTable {
			fabricIndex = index
		}
		if action.Kind == KindWorkerService {
			if firstWorkerIndex < 0 {
				firstWorkerIndex = index
			}
			workerIDs[action.ID] = true
		}
	}
	if fabricIndex < 0 || firstWorkerIndex <= fabricIndex {
		t.Fatalf("rescue cleanup must remove Fabric before stopping Workers: %+v", cleanup.Actions)
	}
	if !workerIDs["worker-alpha"] || !workerIDs["worker-beta"] {
		t.Fatalf("rescue cleanup does not cover both Workers: %+v", cleanup.Actions)
	}
	for _, action := range cleanup.Actions {
		if action.OwnerTag == "" || action.Comment == "" {
			t.Fatalf("cleanup action missing owner metadata: %+v", action)
		}
		if !strings.HasPrefix(action.OwnerTag, "smartgate:") {
			t.Fatalf("cleanup action has non-SmartGate owner tag: %s", action.OwnerTag)
		}
		if len(action.Commands) == 0 {
			t.Fatalf("cleanup action missing command plan: %+v", action)
		}
		for _, command := range action.Commands {
			if !commandMentions(command, action.Comment) {
				t.Fatalf("cleanup command does not constrain by owner comment %q: %+v", action.Comment, command)
			}
		}
	}
}

func TestOpenWrtRescueCleanupDryRunPlanPreservesDualCoreLineOrder(t *testing.T) {
	plan, err := OpenWrtRescueCleanupDryRunPlan("cleanup-equivalence", NewOwnerRegistry("smartgate"), twoWorkerCleanupTargets())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"remove Gateway Fabric fw4 reconcile hook\t" + KindFirewallHook + "\tfabric-reconcile",
		"remove fw4 reconcile hook\t" + KindFirewallHook + "\tfw4-reconcile",
		"restore Gateway Fabric dnsmasq override\t" + KindDNSMasq + "\tsmartgate-upstream",
		"delete owner-tagged Gateway Fabric\t" + KindFabricTable + "\tsmartgate",
		"delete owner-tagged DNS 53 hijack\t" + KindDNSHijack + "\tlan-ipv4-53",
		"delete owner-tagged DoT 853 block\t" + KindDoTBlock + "\tlan-ipv4-853",
		"restore owner-tagged dnsmasq override\t" + KindDNSMasq + "\twan-cpe-upstream",
		"delete SmartGate policy routes\t" + KindFabricRoute + "\tprotocol-186",
		"stop AlphaCore Worker\t" + KindWorkerService + "\tworker-alpha",
		"stop BetaCore Worker\t" + KindWorkerService + "\tworker-beta",
		"delete owner-tagged TUN interface\t" + KindTunInterface + "\ttun-sb",
	}
	got := make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		got = append(got, step.Name+"\t"+step.Resource.Kind+"\t"+step.Resource.ID)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dual-core rescue dry-run lines changed:\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestOpenWrtRescueCleanupDryRunPlanIncludesThirdWorker(t *testing.T) {
	workers := append(twoWorkerCleanupTargets(), WorkerCleanupTarget{ID: "worker-gamma", DisplayName: "GammaCore"})
	plan, err := OpenWrtRescueCleanupDryRunPlan("cleanup-third", NewOwnerRegistry("smartgate"), workers)
	if err != nil {
		t.Fatal(err)
	}
	third := plan.Steps[10]
	if third.Name != "stop GammaCore Worker" || third.Resource.Kind != KindWorkerService || third.Resource.ID != "worker-gamma" {
		t.Fatalf("third worker cleanup step missing or out of order: %+v", third)
	}
	if plan.Steps[11].Resource.Kind != KindTunInterface {
		t.Fatalf("third worker must precede TUN cleanup: %+v", plan.Steps[11])
	}
	resource := plan.Resources[len(plan.Resources)-1]
	if resource.Kind != KindWorkerService || resource.ID != "worker-gamma" {
		t.Fatalf("third worker resource missing from rescue plan: %+v", resource)
	}
}

func TestCleanupPlanRejectsUntaggedResources(t *testing.T) {
	owned := OwnedResource{
		Kind:     KindDNSHijack,
		ID:       "lan-ipv4-53",
		OwnerTag: "smartgate:dns-hijack:lan-ipv4-53",
		Comment:  "owner=smartgate:dns-hijack:lan-ipv4-53",
	}
	_, err := CleanupPlanFromDryRun(DryRunPlan{
		OpID: "bad-cleanup",
		Steps: []DryRunStep{
			{
				Name:     "bad delete",
				Resource: OwnedResource{Kind: KindDNSHijack, ID: "lan-ipv4-53"},
				Commands: []PlannedCommand{
					cmd("nft", "flush", "chain", "inet", "fw4", "dstnat_lan"),
				},
				Destructive: true,
			},
		},
	})
	if err == nil {
		t.Fatal("expected untagged cleanup resource to be rejected")
	}

	_, err = CleanupPlanFromDryRun(DryRunPlan{
		OpID: "bad-cleanup",
		Steps: []DryRunStep{
			{
				Name:     "bad delete",
				Resource: owned,
				Commands: []PlannedCommand{
					cmd("nft", "flush", "chain", "inet", "fw4", "dstnat_lan"),
				},
				Destructive: true,
			},
		},
	})
	if err == nil {
		t.Fatal("expected cleanup command without owner match to be rejected")
	}
}

func TestOwnerCleanupAcquiresNetLock(t *testing.T) {
	locks := lockset.New()
	session := locks.Session("cleanup-test")
	releaseCommit, err := session.Acquire(lockset.LevelCommitLane)
	if err != nil {
		t.Fatalf("acquire commit lane: %v", err)
	}
	defer releaseCommit()

	plan, err := OwnerCleanup{
		Owners:               NewOwnerRegistry("smartgate"),
		WorkerCleanupTargets: twoWorkerCleanupTargets(),
	}.Cleanup(context.Background(), session)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(plan.Actions) == 0 {
		t.Fatal("expected cleanup actions")
	}
	for _, action := range plan.Actions {
		if action.OwnerTag == "" || action.Comment == "" {
			t.Fatalf("cleanup action missing owner metadata: %+v", action)
		}
		if len(action.Commands) == 0 {
			t.Fatalf("cleanup action missing command plan: %+v", action)
		}
	}
}

func TestOwnerCleanupExecutesPlanWhenEnabled(t *testing.T) {
	locks := lockset.New()
	session := locks.Session("cleanup-test")
	releaseCommit, err := session.Acquire(lockset.LevelCommitLane)
	if err != nil {
		t.Fatalf("acquire commit lane: %v", err)
	}
	defer releaseCommit()

	runner := &cleanupTestRunner{}
	cleanup := OwnerCleanup{
		Owners:               NewOwnerRegistry("smartgate"),
		WorkerCleanupTargets: twoWorkerCleanupTargets(),
		Runner:               runner,
		Execute:              true,
	}
	plan, err := cleanup.Cleanup(context.Background(), session)
	if err != nil {
		t.Fatalf("cleanup plan: %v", err)
	}
	result, err := cleanup.ExecuteCleanup(context.Background(), session, plan)
	if err != nil {
		t.Fatalf("execute cleanup: %v", err)
	}
	if result.Actions != len(plan.Actions) || result.Steps == 0 || len(runner.calls) != result.Steps {
		t.Fatalf("cleanup execution mismatch result=%+v actions=%d calls=%d", result, len(plan.Actions), len(runner.calls))
	}
	if !containsCallParts(runner.calls, workerControlPath, "worker-alpha") || !containsCallParts(runner.calls, workerControlPath, "worker-beta") {
		t.Fatalf("rescue cleanup should stop registered Workers, calls=%+v", runner.calls)
	}
}

func TestExecuteCleanupPlanRejectsUnsafeCommands(t *testing.T) {
	plan := CleanupPlan{Actions: []CleanupAction{{
		Kind:     KindDNSHijack,
		ID:       "lan-ipv4-53",
		OwnerTag: "smartgate:dns-hijack:lan-ipv4-53",
		Comment:  "owner=smartgate:dns-hijack:lan-ipv4-53",
		Commands: []PlannedCommand{cmd("nft", "flush", "chain", "inet", "fw4", "dstnat_lan")},
	}}}
	if _, err := ExecuteCleanupPlan(context.Background(), plan, &cleanupTestRunner{}); err == nil {
		t.Fatal("expected unsafe cleanup command to be rejected")
	}
}

type cleanupTestRunner struct {
	calls [][]string
}

func (r *cleanupTestRunner) Run(_ context.Context, argv []string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	return strings.Join(argv, " "), nil
}

func containsCall(calls [][]string, want string) bool {
	for _, call := range calls {
		if strings.Contains(strings.Join(call, " "), want) {
			return true
		}
	}
	return false
}

func containsCallParts(calls [][]string, wants ...string) bool {
	for _, call := range calls {
		joined := strings.Join(call, " ")
		found := true
		for _, want := range wants {
			if !strings.Contains(joined, want) {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}
	return false
}

func requireStep(t *testing.T, plan DryRunPlan, kind string) DryRunStep {
	t.Helper()
	for _, step := range plan.Steps {
		if step.Resource.Kind == kind {
			return step
		}
	}
	t.Fatalf("missing step for kind %s in %+v", kind, plan.Steps)
	return DryRunStep{}
}

func commandMentions(command PlannedCommand, needle string) bool {
	for _, arg := range command.Argv {
		if strings.Contains(arg, needle) {
			return true
		}
	}
	return false
}
