package netgate

import (
	"context"
	"strings"
	"testing"
)

type applyTestRunner struct {
	calls [][]string
}

func (r *applyTestRunner) Run(_ context.Context, argv []string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	return strings.Join(argv, " "), nil
}

func TestExecutePlanRejectsDryRunOnly(t *testing.T) {
	plan, err := OpenWrtDryRunPlan("apply-test", NewOwnerRegistry("smartgate"))
	if err != nil {
		t.Fatalf("dryrun plan: %v", err)
	}
	if _, err := ExecutePlan(context.Background(), plan, &applyTestRunner{}); err == nil {
		t.Fatal("dry-run-only plan should not execute")
	}
}

func TestExecuteApplyPlanRunsOwnerTaggedCommands(t *testing.T) {
	plan, err := OpenWrtApplyPlan("apply-test", NewOwnerRegistry("smartgate"))
	if err != nil {
		t.Fatalf("apply plan: %v", err)
	}
	runner := &applyTestRunner{}
	result, err := ExecutePlan(context.Background(), plan, runner)
	if err != nil {
		t.Fatalf("execute plan: %v", err)
	}
	if result.Steps == 0 || len(runner.calls) != result.Steps {
		t.Fatalf("unexpected executed steps result=%+v calls=%+v", result, runner.calls)
	}
	for _, call := range runner.calls {
		foundOwner := false
		for _, arg := range call {
			if strings.Contains(arg, "owner=smartgate:") {
				foundOwner = true
				break
			}
		}
		if !foundOwner {
			t.Fatalf("command missing owner tag: %+v", call)
		}
	}
}

func TestExecuteCleanupApplyPlanRunsOwnerTaggedCommands(t *testing.T) {
	plan, err := OpenWrtRescueCleanupApplyPlan("safe-mode", NewOwnerRegistry("smartgate"), twoWorkerCleanupTargets())
	if err != nil {
		t.Fatalf("cleanup apply plan: %v", err)
	}
	if plan.DryRunOnly {
		t.Fatal("cleanup apply plan should be executable")
	}
	runner := &applyTestRunner{}
	result, err := ExecutePlan(context.Background(), plan, runner)
	if err != nil {
		t.Fatalf("execute cleanup plan: %v", err)
	}
	if result.Steps == 0 || len(runner.calls) != result.Steps {
		t.Fatalf("unexpected cleanup result=%+v calls=%+v", result, runner.calls)
	}
	if !strings.Contains(strings.Join(runner.calls[0], " "), firewallHookPath) {
		t.Fatalf("cleanup should remove fw4 reconcile hook first, calls=%+v", runner.calls)
	}
	fabricIndex, alphaWorkerIndex, betaWorkerIndex, tunIndex := -1, -1, -1, -1
	for index, call := range runner.calls {
		joined := strings.Join(call, " ")
		switch {
		case strings.Contains(joined, "nft delete table inet smartgate"):
			fabricIndex = index
		case strings.Contains(joined, workerControlPath) && strings.Contains(joined, " worker-alpha "):
			alphaWorkerIndex = index
		case strings.Contains(joined, workerControlPath) && strings.Contains(joined, " worker-beta "):
			betaWorkerIndex = index
		case strings.Contains(joined, "ip link delete dev"):
			tunIndex = index
		}
	}
	if fabricIndex < 0 || alphaWorkerIndex <= fabricIndex || betaWorkerIndex <= fabricIndex {
		t.Fatalf("cleanup must remove Fabric before stopping both Workers, calls=%+v", runner.calls)
	}
	if tunIndex <= alphaWorkerIndex || tunIndex <= betaWorkerIndex {
		t.Fatalf("cleanup must delete the legacy TUN after stopping Workers, calls=%+v", runner.calls)
	}
}
