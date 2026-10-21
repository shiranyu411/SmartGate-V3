package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/coredesc"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type descriptorOnlyAdapter struct {
	worker.Adapter
	descriptor coredesc.Descriptor
}

func (adapter descriptorOnlyAdapter) Descriptor() coredesc.Descriptor {
	return adapter.descriptor
}

func TestNewCoreRegistryRegistersAdaptersInStableOrder(t *testing.T) {
	primary := descriptorOnlyAdapter{descriptor: coredesc.Descriptor{
		ID: "core-a", DisplayName: "Core A", Control: coredesc.Control{Port: 9001},
		Ports: coredesc.Ports{TProxyBase: 16001, DNSBase: 18001},
	}}
	secondary := descriptorOnlyAdapter{descriptor: coredesc.Descriptor{
		ID: "core-b", DisplayName: "Core B", Control: coredesc.Control{Port: 9002},
		Ports: coredesc.Ports{TProxyBase: 17001, DNSBase: 19001},
	}}
	registry, err := newCoreRegistry(primary, secondary)
	if err != nil {
		t.Fatal(err)
	}
	descriptors := registry.List()
	if len(descriptors) != 2 || descriptors[0].ID != primary.Descriptor().ID || descriptors[1].ID != secondary.Descriptor().ID {
		t.Fatalf("unexpected core registry: %+v", descriptors)
	}
	configureSmartNodeEvaluators(registry)
	cores := smartnode.RegisteredCoreIDs()
	if len(cores) != 2 || cores[0] != primary.Descriptor().ID || cores[1] != secondary.Descriptor().ID {
		t.Fatalf("unexpected smartnode evaluator injection: %v", cores)
	}
}

func TestWorkerCleanupTargetsIncludesEveryRegisteredAdapter(t *testing.T) {
	registry, err := newCoreRegistry(
		descriptorOnlyAdapter{descriptor: coredesc.Descriptor{
			ID: "core-a", DisplayName: "Core A", Control: coredesc.Control{Port: 9001},
			Ports: coredesc.Ports{TProxyBase: 16001, DNSBase: 18001},
		}},
		descriptorOnlyAdapter{descriptor: coredesc.Descriptor{
			ID: "core-b", DisplayName: "Core B", Control: coredesc.Control{Port: 9002},
			Ports: coredesc.Ports{TProxyBase: 17001, DNSBase: 19001},
		}},
		descriptorOnlyAdapter{descriptor: coredesc.Descriptor{
			ID:          smartnode.CoreID("core-c"),
			DisplayName: "Core C",
			Control:     coredesc.Control{Port: 12000},
			Ports:       coredesc.Ports{TProxyBase: 20000, DNSBase: 21000},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	targets := workerCleanupTargets(registry)
	if len(targets) != 3 {
		t.Fatalf("cleanup targets = %+v", targets)
	}
	want := map[string]string{
		"w_core_a_1": "Core A",
		"w_core_b_1": "Core B",
		"w_core_c_1": "Core C",
	}
	for _, target := range targets {
		if want[target.ID] != target.DisplayName {
			t.Fatalf("unexpected registry-derived cleanup target: %+v", target)
		}
		delete(want, target.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing registry-derived cleanup targets: %+v", want)
	}
}

// Cores are discovered on the host. A missing optional core (the router ships
// the legacy core only) must not stop the daemon; it is simply absent from the
// registry, while rescue keeps a cleanup target for every Worker this build
// knows.
func TestInstalledWorkerAdaptersSkipsMissingCoresButKeepsCleanupTargets(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	known := newWorkerAdapters(layout, noopRunner{}, true, false)
	if len(known) != 2 {
		t.Fatalf("expected the two live adapters, got %d", len(known))
	}
	legacy := legacyCoreID(known)
	legacyBinary := ""
	for _, adapter := range known {
		if adapter.Descriptor().ID == legacy {
			legacyBinary = liveCoreBinary(adapter)
		}
	}
	if legacy == "" || legacyBinary == "" {
		t.Fatalf("legacy core adapter must resolve to a binary: core=%q binary=%q", legacy, legacyBinary)
	}
	onlyLegacyCore := func(path string) bool { return path == legacyBinary }
	installed, err := installedWorkerAdapters(known, onlyLegacyCore)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 {
		t.Fatalf("expected only the installed core, got %d adapters", len(installed))
	}
	if installed[0].Descriptor().ID != legacy {
		t.Fatalf("installed adapter is %s, want the legacy core %s", installed[0].Descriptor().ID, legacy)
	}
	registry, err := newCoreRegistry(installed...)
	if err != nil {
		t.Fatal(err)
	}
	if cores := registry.List(); len(cores) != 1 || cores[0].ID != installed[0].Descriptor().ID {
		t.Fatalf("registry must expose only installed cores: %+v", cores)
	}
	if targets := workerCleanupTargetsForAdapters(known); len(targets) != 2 {
		t.Fatalf("rescue cleanup must still cover every known Worker: %+v", targets)
	}
	if _, err := installedWorkerAdapters(known, func(string) bool { return false }); err == nil {
		t.Fatal("no installed core must be an error, not an empty registry")
	}
	everything, err := installedWorkerAdapters(known, func(string) bool { return true })
	if err != nil || len(everything) != 2 {
		t.Fatalf("all cores installed: adapters=%d err=%v", len(everything), err)
	}
}

func TestRunRescueLifecycle(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "etc")
	runtimeDir := filepath.Join(root, "tmp")

	enterArgs := []string{"-data-dir", dataDir, "-runtime-dir", runtimeDir, "enter"}
	if err := runRescue(enterArgs); err != nil {
		t.Fatalf("runRescue enter: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "smartgate-rescue.status")); err != nil {
		t.Fatalf("rescue flag missing after enter: %v", err)
	}
	layout := state.Layout{DataDir: dataDir, RuntimeDir: runtimeDir}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{ConfigEpoch: 2, GatewayMode: state.GatewayModeOff}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, CommitInFlight: &state.CommitRef{OpID: "cli-rescue-pending", StartedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransactionStore(layout).Save(state.TransactionJournal{
		OpID: "cli-rescue-pending", Kind: "config_apply", Phase: state.TransactionApplying,
		Source: state.TransactionState{Spec: spec, LastGoodCaptured: true, Applied: applied, Runtime: state.RuntimeState{ConfigEpoch: 2}},
	}); err != nil {
		t.Fatal(err)
	}

	statusArgs := []string{"-data-dir", dataDir, "-runtime-dir", runtimeDir, "status"}
	if err := runRescue(statusArgs); err != nil {
		t.Fatalf("runRescue status: %v", err)
	}

	clearArgs := []string{"-data-dir", dataDir, "-runtime-dir", runtimeDir, "clear"}
	if err := runRescue(clearArgs); err != nil {
		t.Fatalf("runRescue clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "smartgate-rescue.status")); !os.IsNotExist(err) {
		t.Fatalf("rescue flag should be removed, got err=%v", err)
	}
	if _, err := state.NewTransactionStore(layout).Load("cli-rescue-pending"); !os.IsNotExist(err) {
		t.Fatalf("CLI clear should recover pending config journal, got %v", err)
	}
}

func TestNoopRunnerReportsCommandWithoutExecuting(t *testing.T) {
	out, err := noopRunner{}.Run(context.Background(), []string{"nft", "list", "ruleset"})
	if err != nil {
		t.Fatalf("noop runner should not fail: %v", err)
	}
	if !strings.Contains(out, "smartgate dev no-op") || !strings.Contains(out, "nft list ruleset") {
		t.Fatalf("unexpected noop output: %q", out)
	}
}

func TestDefaultCorePolicyUsesSingBoxAndPreservesSavedChoices(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	adapters := newWorkerAdapters(layout, noopRunner{}, false, false)
	registry, err := newCoreRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	configureSmartNodeEvaluators(registry)
	if got := smartnode.DefaultCoreChoice(); got != "sing-box" {
		t.Fatalf("default core = %q, want sing-box", got)
	}
	spec := state.NormalizeSpecState(state.SpecState{SmartNodes: map[string]smartnode.SmartNode{
		"new": {ID: "new"}, "chosen": {ID: "chosen", CoreChoice: "xray"},
	}})
	if spec.SmartNodes["new"].CoreChoice != "sing-box" || spec.SmartNodes["chosen"].CoreChoice != "xray" {
		t.Fatalf("wrong product default: %+v", spec.SmartNodes)
	}
}
