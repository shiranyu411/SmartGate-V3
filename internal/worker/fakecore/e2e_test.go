package fakecore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/configapply"
	"smartgate/internal/coredesc"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/rescue"
	runtimeapi "smartgate/internal/runtime"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
	workersingbox "smartgate/internal/worker/singbox"
	workerxray "smartgate/internal/worker/xray"
)

func TestFakeCoreThirdCoreE2E(t *testing.T) {
	fixture := []map[string]any{{
		"type": "fooprotocol", "tag": "Foo Protocol Node", "server": "fake.example", "server_port": 443,
		"method": "fake-aead-2026", "plugin_opts": map[string]any{"dialer": "dev-noop"},
	}}
	env := newFakeCoreE2E(t)

	parsed, err := provider.ParseSubscriptionPayload("p1", mustJSONBytes(t, fixture))
	if err != nil {
		t.Fatalf("parse fooprotocol subscription: %v", err)
	}
	if len(parsed.Identities) != 1 {
		t.Fatalf("parsed identities = %d", len(parsed.Identities))
	}
	identity := parsed.Identities[0].Identity
	if identity.Protocol != "fooprotocol" || identity.Extra["method"] == "" || identity.Extra["plugin_opts"] == "" {
		t.Fatalf("fooprotocol identity was not preserved as platform IR: %+v", identity)
	}
	capability, err := smartnode.EvaluateCapability(identity, smartnode.UnassessedReasons(identity))
	if err != nil {
		t.Fatalf("evaluate fooprotocol capability: %v", err)
	}
	assertCapability(t, capability, CoreID, smartnode.CapabilitySupported)
	assertCapability(t, capability, smartnode.CoreID("sing-box"), smartnode.CapabilityUnsupportedByCore)
	assertCapability(t, capability, smartnode.CoreID("xray"), smartnode.CapabilityUnsupportedByCore)
	assertUnsupportedProtocolReason(t, capability, smartnode.CoreID("sing-box"), identity.Protocol)
	assertUnsupportedProtocolReason(t, capability, smartnode.CoreID("xray"), identity.Protocol)

	if _, err := provider.NewLastGoodStore(env.layout).SaveMany(context.Background(), []provider.LastGoodUpdate{{
		ProviderID: "p1", Identities: parsed.Identities,
	}}); err != nil {
		t.Fatalf("save fooprotocol last-good: %v", err)
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "FakeCore Provider", Type: provider.TypeManual, Enabled: true}}
	spec.Settings.GatewayMode = state.GatewayModeOn
	spec.Fabric.DNS.Listen = freeLocalAddress(t)
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(env.layout))
	if err != nil {
		t.Fatalf("load snapshots: %v", err)
	}
	spec, _, err = provider.ReconcileSmartNodes(spec, snapshots, fixedFakeCoreTime())
	if err != nil {
		t.Fatalf("reconcile smartnodes: %v", err)
	}
	nodeID := singleSmartNodeID(t, spec.SmartNodes)
	spec.BusinessPools = state.DefaultBusinessPools()
	spec.BusinessPools[0].NodeIDs = []string{nodeID}
	if _, err := env.store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	env.start(t)

	cores := env.raw(t, http.MethodGet, "/api/cores", nil, http.StatusOK)
	if !strings.Contains(cores, `"id":"fakecore"`) {
		t.Fatalf("/api/cores does not expose FakeCore: %s", cores)
	}

	nodes := env.apiData(t, http.MethodGet, "/api/nodes", nil, http.StatusOK)
	node := findObjectByField(t, nodes["nodes"], "id", nodeID)
	if nestedString(node, "identity", "protocol") != "fooprotocol" {
		t.Fatalf("/api/nodes identity protocol mismatch: %+v", node["identity"])
	}
	if nestedString(node, "capability", string(CoreID), "status") != smartnode.CapabilitySupported {
		t.Fatalf("/api/nodes FakeCore capability mismatch: %+v", node["capability"])
	}

	env.apiData(t, http.MethodPut, "/api/nodes/"+nodeID+"/core-choice", map[string]any{
		"op_id": "fakecore-choice", "core": string(CoreID),
	}, http.StatusOK)
	spec, err = env.store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec after core choice: %v", err)
	}
	if spec.SmartNodes[nodeID].CoreChoice != CoreID {
		t.Fatalf("core choice was not persisted: %+v", spec.SmartNodes[nodeID])
	}

	graphA := planner.BuildExecutionGraph(spec, nil, state.ExecutionGraph{}, fixedFakeCoreTime())
	graphB := planner.BuildExecutionGraph(spec, nil, state.ExecutionGraph{}, fixedFakeCoreTime())
	if !graphA.ReadyToApply || graphA.GraphHash != graphB.GraphHash {
		t.Fatalf("FakeCore graph is not stable or ready: a=%+v b=%+v", graphA, graphB)
	}
	assertGraphContainsFakeCore(t, graphA.Graph)
	assertGraphUsesDescriptor(t, graphA.Graph, env.registry)

	dryRun := env.apiData(t, http.MethodPost, "/api/config/dryrun", map[string]any{
		"op_id": "fakecore-dryrun",
	}, http.StatusOK)
	if !nestedStringArrayContains(dryRun, []string{"execution_graph_summary", "workers", "create"}, "w_fakecore_1") {
		t.Fatalf("dry-run summary does not create w_fakecore_1: %+v", dryRun["execution_graph_summary"])
	}
	if !poolBindingsContain(dryRun["execution_graph_summary"], "general", string(CoreID), "w_fakecore_1") {
		t.Fatalf("dry-run pool binding does not target FakeCore: %+v", dryRun["execution_graph_summary"])
	}

	apply := env.apiData(t, http.MethodPost, "/api/config/apply", map[string]any{
		"dryrun_id": "fakecore-dryrun", "description": "FakeCore e2e",
	}, http.StatusOK)
	if apply["status"] != "applied" {
		t.Fatalf("apply status mismatch: %+v", apply)
	}
	if !arrayContainsObjectField(apply["workers"], "id", "w_fakecore_1") {
		t.Fatalf("apply workers missing FakeCore: %+v", apply["workers"])
	}
	if !arrayContainsObjectField(apply["core_results"], "core", string(CoreID)) {
		t.Fatalf("apply core_results missing FakeCore: %+v", apply["core_results"])
	}
	assertFakeCoreConfigWritten(t, env.layout)
	checkpoint, err := env.workers.Checkpoint("fakecore-e2e-rollback")
	if err != nil {
		t.Fatalf("checkpoint FakeCore worker: %v", err)
	}
	workerID := env.registry.List()[0].WorkerID(1)
	if descriptor, ok := env.registry.Lookup(CoreID); ok {
		workerID = descriptor.WorkerID(1)
	}
	backup, ok := checkpoint.Backups[workerID]
	if !ok || !backup.Existed || !backup.WasActive || backup.Core != CoreID {
		t.Fatalf("checkpoint did not capture active FakeCore worker: %+v", checkpoint.Backups)
	}
	if err := env.workers.StopAll(context.Background()); err != nil {
		t.Fatalf("stop workers before rollback: %v", err)
	}
	if status, ok := env.workers.Status(workerID); !ok || status.State != worker.StateStopped {
		t.Fatalf("FakeCore worker did not stop before rollback: %+v exists=%t", status, ok)
	}
	if _, err := env.workers.Rollback(context.Background(), checkpoint); err != nil {
		t.Fatalf("rollback FakeCore checkpoint: %v", err)
	}
	if status, ok := env.workers.Status(workerID); !ok || status.State != worker.StateRunning {
		t.Fatalf("FakeCore worker was not restored by rollback: %+v exists=%t", status, ok)
	}
	assertFakeCoreConfigWritten(t, env.layout)
	if !env.runner.hasCommand("/usr/libexec/smartgate-worker-control", "stop", workerID) {
		t.Fatalf("worker lifecycle did not execute explicit stop for %s: %+v", workerID, env.runner.commands())
	}

	workers := env.apiData(t, http.MethodGet, "/api/workers", nil, http.StatusOK)
	workerView := findObjectByField(t, workers["workers"], "id", workerID)
	descriptor, _ := env.registry.Lookup(CoreID)
	if workerView["core"] != string(CoreID) || workerView["version"] != descriptor.Version || workerView["state"] != worker.StateRunning {
		t.Fatalf("/api/workers FakeCore status mismatch: %+v", workerView)
	}
	events := env.apiData(t, http.MethodGet, "/api/workers/"+workerID+"/events", nil, http.StatusOK)
	if !arrayContainsObjectField(events["events"], "state", worker.StateRunning) {
		t.Fatalf("/api/workers events missing running event: %+v", events["events"])
	}
	assertPlatformWorkerEvents(t, events["events"])

	systemStatus := env.raw(t, http.MethodGet, "/api/system/status", nil, http.StatusOK)
	if !strings.Contains(systemStatus, "fakecore") || !strings.Contains(systemStatus, "w_fakecore_1") {
		t.Fatalf("/api/system/status missing FakeCore worker/service: %s", systemStatus)
	}
	rescueDryRun := env.apiData(t, http.MethodPost, "/api/runtime/openwrt/rescue-cleanup/dryrun", map[string]any{
		"op_id": "fakecore-rescue",
	}, http.StatusOK)
	if !cleanupContainsWorkerStop(rescueDryRun["cleanup"], workerID) {
		t.Fatalf("rescue cleanup dry-run missing explicit FakeCore stop: %+v", rescueDryRun)
	}
}

type fakeCoreE2E struct {
	layout   state.Layout
	store    *state.Store
	server   *runtimeapi.Server
	base     string
	workers  *worker.Manager
	registry *coredesc.Registry
	runner   *fakeCoreNoopRunner
}

func newFakeCoreE2E(t *testing.T) *fakeCoreE2E {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	runner := &fakeCoreNoopRunner{}
	adapters := fakeCoreE2EAdapters(layout, runner)
	registry := coredesc.NewRegistry()
	for _, adapter := range adapters {
		if err := registry.Register(adapter.Descriptor()); err != nil {
			t.Fatalf("register %s: %v", adapter.Core(), err)
		}
	}
	configureFakeCoreE2ERegistry(registry)

	workerManager, err := worker.NewManager(adapters...)
	if err != nil {
		t.Fatalf("worker manager: %v", err)
	}
	fabricManager := fabric.NewManager(owners, runner)
	ruleSets := fabric.NewRuleSetStore(layout)
	ruleSets.ProxyAddress = fabricManager.RuleSetProxyAddress
	fabricManager.WithRuleSetStore(ruleSets)
	fabricManager.WithConnectionTracker(fabric.NoopConnectionTracker{})

	commitMgr := commitlane.New(locks, layout, owners)
	configTxn := commitlane.NewConfigTxn(commitMgr, store, configapply.Engine{
		Owners: owners, WorkerCleanupTargets: fakeCoreCleanupTargets(registry), NetGateRunner: runner,
		Workers: workerManager, Fabric: fabricManager,
	})
	selectorTxn := commitlane.NewCutoverTxn(commitMgr, store, workerManager, fabricManager)
	rescueMgr := rescue.NewWithCleanup(layout.RescueStatusPath(), commitMgr, owners, netgate.OwnerCleanup{
		Owners: owners, WorkerCleanupTargets: fakeCoreCleanupTargets(registry),
	})
	listenAddr := freeLocalAddress(t)
	server := runtimeapi.NewServer(runtimeapi.Config{
		ListenAddr:           listenAddr,
		Layout:               layout,
		Locks:                locks,
		CommitLane:           commitMgr,
		Store:                store,
		SmartNodeTxn:         commitlane.NewSmartNodeTxn(commitMgr, store, state.NewSnapshotStore(layout)),
		ConfigTxn:            configTxn,
		SelectorTxn:          selectorTxn,
		Rescue:               rescueMgr,
		Owners:               owners,
		WorkerCleanupTargets: fakeCoreCleanupTargets(registry),
		NetGateRunner:        runner,
		SystemRunner:         runner,
		Workers:              workerManager,
		Fabric:               fabricManager,
		CoreRegistry:         registry,
		Status:               runtimeapi.NewStatus(listenAddr, state.RuntimeState{}),
		AuthRequired:         false,
	})
	return &fakeCoreE2E{
		layout: layout, store: store, server: server, base: "http://" + listenAddr,
		workers: workerManager, registry: registry, runner: runner,
	}
}

func (env *fakeCoreE2E) start(t *testing.T) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- env.server.ListenAndServe() }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("server exited before health check: %v", err)
		default:
		}
		resp, err := http.Get(env.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if err := env.server.Shutdown(ctx); err != nil {
						t.Fatalf("shutdown server: %v", err)
					}
					if err := <-errCh; err != nil && !errors.Is(err, runtimeapi.ErrServerClosed) {
						t.Fatalf("server shutdown: %v", err)
					}
				})
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not become healthy")
}

func fakeCoreE2EAdapters(layout state.Layout, runner netgate.Runner) []worker.Adapter {
	activeDir := filepath.Join(layout.RuntimeDir, "dev-workers")
	commands := func(action string) func(string) []string {
		return func(workerID string) []string {
			return []string{"/usr/libexec/smartgate-worker-control", action, workerID}
		}
	}
	newRuntime := func() *worker.FileRuntime {
		return &worker.FileRuntime{
			RuntimeDir: filepath.Join(layout.RuntimeDir, "worker-runtime"),
			Checker:    fakeCoreNoopChecker{},
			Runner:     runner,
			ConfigPath: func(workerID string) string {
				return filepath.Join(activeDir, workerID+".json")
			},
			StartCommand:  commands("restart"),
			StopCommand:   commands("stop"),
			StatusCommand: commands("status"),
			SkipReadiness: true,
		}
	}
	return []worker.Adapter{
		&workersingbox.Adapter{Runtime: newRuntime()},
		&workerxray.Adapter{Runtime: newRuntime(), Binary: "/usr/bin/xray"},
		&Adapter{Runtime: newRuntime()},
	}
}

func configureFakeCoreE2ERegistry(registry *coredesc.Registry) {
	descriptors := registry.List()
	evaluators := make([]smartnode.CoreEvaluator, 0, len(descriptors))
	for _, descriptor := range descriptors {
		evaluators = append(evaluators, smartnode.CoreEvaluator{Core: descriptor.ID, Evaluate: descriptor.Capability.Evaluator})
	}
	smartnode.ConfigureCoreEvaluators(evaluators)
	planner.ConfigureCoreRegistry(registry)
}

func fakeCoreCleanupTargets(registry *coredesc.Registry) []netgate.WorkerCleanupTarget {
	descriptors := registry.List()
	targets := make([]netgate.WorkerCleanupTarget, 0, len(descriptors))
	for _, descriptor := range descriptors {
		targets = append(targets, netgate.WorkerCleanupTarget{ID: descriptor.WorkerID(1), DisplayName: descriptor.DisplayName})
	}
	return targets
}

type fakeCoreNoopRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (runner *fakeCoreNoopRunner) Run(_ context.Context, argv []string) (string, error) {
	runner.mu.Lock()
	runner.calls = append(runner.calls, append([]string(nil), argv...))
	runner.mu.Unlock()
	if len(argv) == 0 {
		return "smartgate dev no-op: empty command", nil
	}
	return "smartgate dev no-op: " + strings.Join(argv, " "), nil
}

func (runner *fakeCoreNoopRunner) commands() [][]string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	commands := make([][]string, len(runner.calls))
	for index := range runner.calls {
		commands[index] = append([]string(nil), runner.calls[index]...)
	}
	return commands
}

func (runner *fakeCoreNoopRunner) hasCommand(parts ...string) bool {
	for _, command := range runner.commands() {
		if len(command) < len(parts) {
			continue
		}
		for offset := 0; offset <= len(command)-len(parts); offset++ {
			match := true
			for index := range parts {
				if command[offset+index] != parts[index] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

type fakeCoreNoopChecker struct{}

func (fakeCoreNoopChecker) Check(context.Context, string) (string, error) {
	return "smartgate dev no-op config check", nil
}

func freeLocalAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release local address: %v", err)
	}
	return address
}

func (env *fakeCoreE2E) apiData(t *testing.T, method, path string, payload any, want int) map[string]any {
	t.Helper()
	body := env.raw(t, method, path, payload, want)
	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode envelope for %s %s: %v body=%s", method, path, err, body)
	}
	if !envelope.OK {
		t.Fatalf("%s %s returned error envelope: %s", method, path, envelope.Error)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		t.Fatalf("decode data for %s %s: %v data=%s", method, path, err, envelope.Data)
	}
	return data
}

func (env *fakeCoreE2E) raw(t *testing.T, method, path string, payload any, want int) string {
	t.Helper()
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		body = bytes.NewReader(mustJSONBytes(t, payload))
	}
	req, err := http.NewRequest(method, env.base+path, body)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	responseBody := new(bytes.Buffer)
	if _, err := responseBody.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read response %s %s: %v", method, path, err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, resp.StatusCode, want, responseBody.String())
	}
	return responseBody.String()
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return body
}

func fixedFakeCoreTime() time.Time {
	return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
}

func assertCapability(t *testing.T, matrix smartnode.CapabilityMatrix, core smartnode.CoreID, status string) {
	t.Helper()
	capability, ok := matrix.ForCore(core)
	if !ok || capability.Status != status {
		t.Fatalf("capability[%s]=%+v exists=%t want=%s", core, capability, ok, status)
	}
}

func assertUnsupportedProtocolReason(t *testing.T, matrix smartnode.CapabilityMatrix, core smartnode.CoreID, protocol string) {
	t.Helper()
	capability, ok := matrix.ForCore(core)
	want := "protocol." + protocol + "_not_supported"
	if !ok || !containsString(capability.Reasons, want) || len(capability.ReasonTexts) == 0 {
		t.Fatalf("capability[%s] missing protocol reason %q: %+v", core, want, capability)
	}
}

func singleSmartNodeID(t *testing.T, catalog map[string]smartnode.SmartNode) string {
	t.Helper()
	if len(catalog) != 1 {
		t.Fatalf("catalog size = %d want 1: %+v", len(catalog), catalog)
	}
	for id := range catalog {
		return id
	}
	t.Fatal("unreachable")
	return ""
}

func assertGraphContainsFakeCore(t *testing.T, graph state.ExecutionGraph) {
	t.Helper()
	if len(graph.Workers) != 1 || graph.Workers[0].ID != "w_fakecore_1" || graph.Workers[0].Core != CoreID {
		t.Fatalf("graph workers do not contain only FakeCore: %+v", graph.Workers)
	}
	if len(graph.NodeExecutors) != 1 || graph.NodeExecutors[0].Core != CoreID {
		t.Fatalf("graph executors do not carry FakeCore as data: %+v", graph.NodeExecutors)
	}
}

func assertGraphUsesDescriptor(t *testing.T, graph state.ExecutionGraph, registry *coredesc.Registry) {
	t.Helper()
	descriptor, ok := registry.Lookup(CoreID)
	if !ok || len(graph.Workers) != 1 {
		t.Fatalf("FakeCore descriptor/worker missing: descriptor=%t workers=%+v", ok, graph.Workers)
	}
	target := graph.Workers[0]
	if target.Control.Kind != descriptor.Control.Kind || target.Control.Listen != descriptor.Control.Listen || target.Control.Port != descriptor.Control.Port {
		t.Fatalf("worker control did not come from Descriptor: target=%+v descriptor=%+v", target.Control, descriptor.Control)
	}
	tproxyPort := 0
	dnsPort := 0
	for _, inbound := range target.Inbounds {
		switch inbound.Kind {
		case "tproxy":
			if tproxyPort == 0 || inbound.Port < tproxyPort {
				tproxyPort = inbound.Port
			}
		case "socks":
			if dnsPort == 0 || inbound.Port < dnsPort {
				dnsPort = inbound.Port
			}
		}
	}
	if tproxyPort != descriptor.Ports.TProxyBase || dnsPort != descriptor.Ports.DNSBase {
		t.Fatalf("worker ports did not start at Descriptor ranges: tproxy=%d dns=%d descriptor=%+v", tproxyPort, dnsPort, descriptor.Ports)
	}
}

func assertPlatformWorkerEvents(t *testing.T, value any) {
	t.Helper()
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("worker events are missing: %+v", value)
	}
	allowed := map[string]bool{
		"start": true, "ready": true, "draining": true, "rescue_stop": true, "stopped": true, "restored": true,
	}
	for _, item := range items {
		event, ok := item.(map[string]any)
		kind, _ := event["kind"].(string)
		if !ok || !allowed[kind] || strings.Contains(strings.ToLower(kind), string(CoreID)) {
			t.Fatalf("worker event is not a platform lifecycle event: %+v", item)
		}
	}
}

func cleanupContainsWorkerStop(value any, workerID string) bool {
	cleanup, ok := value.(map[string]any)
	if !ok {
		return false
	}
	actions, ok := cleanup["actions"].([]any)
	if !ok {
		return false
	}
	for _, rawAction := range actions {
		action, ok := rawAction.(map[string]any)
		if !ok || action["kind"] != netgate.KindWorkerService || action["id"] != workerID {
			continue
		}
		commands, ok := action["commands"].([]any)
		if !ok {
			return false
		}
		for _, rawCommand := range commands {
			command, ok := rawCommand.(map[string]any)
			argv, ok := command["argv"].([]any)
			if !ok || !arrayContainsString(argv, workerID) || !arrayContainsSubstring(argv, " stop ") {
				continue
			}
			return true
		}
	}
	return false
}

func arrayContainsString(items []any, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func arrayContainsSubstring(items []any, want string) bool {
	for _, item := range items {
		if value, ok := item.(string); ok && strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func assertFakeCoreConfigWritten(t *testing.T, layout state.Layout) {
	t.Helper()
	path := filepath.Join(layout.RuntimeDir, "dev-workers", "w_fakecore_1.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read FakeCore config %s: %v", path, err)
	}
	var config Config
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatalf("decode FakeCore config: %v body=%s", err, body)
	}
	if config.Core != CoreID || config.Worker.ID != "w_fakecore_1" || len(config.Nodes) != 1 || config.Nodes[0].Identity.Protocol != "fooprotocol" {
		t.Fatalf("FakeCore config content mismatch: %+v", config)
	}
}

func findObjectByField(t *testing.T, value any, field string, want string) map[string]any {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("value is not an array: %+v", value)
	}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if ok && object[field] == want {
			return object
		}
	}
	t.Fatalf("array has no object with %s=%s: %+v", field, want, value)
	return nil
}

func arrayContainsObjectField(value any, field string, want string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if ok && object[field] == want {
			return true
		}
	}
	return false
}

func nestedString(root map[string]any, path ...string) string {
	var current any = root
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	value, _ := current.(string)
	return value
}

func nestedStringArrayContains(root map[string]any, path []string, want string) bool {
	var current any = root
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current = object[key]
	}
	items, ok := current.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func poolBindingsContain(summary any, pool string, core string, workerID string) bool {
	object, ok := summary.(map[string]any)
	if !ok {
		return false
	}
	items, ok := object["pool_bindings"].([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		binding, ok := item.(map[string]any)
		if ok && binding["pool"] == pool && binding["core"] == core && binding["worker"] == workerID {
			return true
		}
	}
	return false
}
