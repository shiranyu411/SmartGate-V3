package runtime

import (
	"net/http/httptest"
	"testing"

	"smartgate/internal/planner"
	"smartgate/internal/smartnode"
)

func configureTestCoreEvaluators() {
	registry := newRuntimeTestCoreRegistryForInit()
	planner.ConfigureCoreRegistry(registry)
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: runtimeCoreA, Evaluate: runtimeTestWorkerDescriptors[runtimeCoreA].Capability.Evaluator},
		{Core: runtimeCoreB, Evaluate: runtimeTestWorkerDescriptors[runtimeCoreB].Capability.Evaluator},
	})
}

func init() {
	configureTestCoreEvaluators()
}

func TestMemberCoreSummaryUsesRegisteredCoreSet(t *testing.T) {
	defer configureTestCoreEvaluators()
	supported := func(smartnode.NodeIdentity, []string) smartnode.CoreCapability {
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported, Reasons: []string{}, ReasonTexts: []string{}}
	}
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: "alpha", Evaluate: supported},
		{Core: "beta", Evaluate: supported},
		{Core: "gamma", Evaluate: supported},
	})
	summary := memberCoreSummary([]string{"a", "b", "missing"}, map[string]smartnode.SmartNode{
		"a": {CoreChoice: "alpha"},
		"b": {CoreChoice: "gamma"},
	})
	if len(summary) != 4 || summary["alpha"] != 1 || summary["beta"] != 0 || summary["gamma"] != 1 || summary["unset"] != 1 {
		t.Fatalf("unexpected dynamic core summary: %+v", summary)
	}
}

func TestSmartNodeCapabilityFilterChecksDynamicKeys(t *testing.T) {
	view := smartNodeView{Capability: smartnode.CapabilityMatrix{
		"alpha": {Status: smartnode.CapabilitySupported, Reasons: []string{}},
		"gamma": {Status: smartnode.CapabilityDegraded, Reasons: []string{"test.reason"}},
	}}
	request := httptest.NewRequest("GET", "/api/nodes?capability=degraded", nil)
	if !matchesSmartNodeFilters(view, request) {
		t.Fatal("dynamic capability key was ignored by filter")
	}
}
