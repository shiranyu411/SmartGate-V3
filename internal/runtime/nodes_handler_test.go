package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/coredesc"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/selection"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

var (
	runtimeCoreA = smartnode.CoreID("core-a")
	runtimeCoreB = smartnode.CoreID("core-b")
)

type fakeProxyControl struct {
	selectSelector         string
	selectOutbound         string
	selectCalls            []string
	selectErrors           map[string]error
	delayErrorsByURL       map[string]error
	delayErrors            map[string]error
	delayWait              map[string]time.Duration
	delayOutbound          string
	delayCalls             []string
	delayMu                sync.Mutex
	connectionsPayload     map[string]any
	connectionsCalled      bool
	closeConnectionsCalled bool
	closedConnectionIDs    []string
	selectedLookup         func(string) (string, error)
	cutoverReady           bool
	routeMu                sync.Mutex
	routeConnections       int
	routeConnectionErr     error
	workerConnections      []worker.Connection
	workerPrepareErr       error
}

type runtimeTestWorkerAdapter struct {
	core    smartnode.CoreID
	control *fakeProxyControl
}

var runtimeTestWorkerDescriptors = map[smartnode.CoreID]coredesc.Descriptor{
	runtimeCoreA: {
		ID:         runtimeCoreA,
		Control:    coredesc.Control{Kind: "http", Listen: "127.0.0.1", Port: 20001},
		Ports:      coredesc.Ports{TProxyBase: 16000, DNSBase: 18000},
		Process:    coredesc.Process{ServiceUnit: state.SchedulerTaskRestartDefaultService(), ProcessName: state.SchedulerTaskRestartDefaultService()},
		Capability: coredesc.Capability{Evaluator: runtimeTestPrimaryCapability},
		ConfigDirs: []string{"/etc/runtime-test-core"},
	},
	runtimeCoreB: {
		ID:         runtimeCoreB,
		Control:    coredesc.Control{Kind: "grpc", Listen: "127.0.0.1", Port: 10085},
		Ports:      coredesc.Ports{TProxyBase: 17000, DNSBase: 19000},
		Process:    coredesc.Process{ProcessName: "core-b-process"},
		Capability: coredesc.Capability{Evaluator: runtimeTestSecondaryCapability},
	},
}

func runtimeTestPrimaryCapability(identity smartnode.NodeIdentity, unassessed []string) smartnode.CoreCapability {
	if identity.Protocol == "naive" {
		return smartnode.CoreCapability{Status: smartnode.CapabilityUnsupportedByCore, Reasons: []string{"protocol.unsupported"}}
	}
	if identity.Transport.Type == "xhttp" || len(unassessed) > 0 {
		return smartnode.CoreCapability{Status: smartnode.CapabilityDegraded, Reasons: append([]string(nil), unassessed...)}
	}
	return smartnode.CoreCapability{Status: smartnode.CapabilitySupported, Reasons: []string{}}
}

func runtimeTestSecondaryCapability(identity smartnode.NodeIdentity, unassessed []string) smartnode.CoreCapability {
	if identity.Protocol == "naive" {
		return smartnode.CoreCapability{Status: smartnode.CapabilityUnsupportedByCore, Reasons: []string{"protocol.unsupported"}}
	}
	if len(unassessed) > 0 {
		return smartnode.CoreCapability{Status: smartnode.CapabilityDegraded, Reasons: append([]string(nil), unassessed...)}
	}
	return smartnode.CoreCapability{Status: smartnode.CapabilitySupported, Reasons: []string{}}
}

func newRuntimeTestCoreRegistry(t *testing.T) *coredesc.Registry {
	t.Helper()
	registry, err := buildRuntimeTestCoreRegistry()
	if err != nil {
		t.Fatalf("build runtime test core registry: %v", err)
	}
	return registry
}

func newRuntimeTestCoreRegistryForInit() *coredesc.Registry {
	registry, err := buildRuntimeTestCoreRegistry()
	if err != nil {
		panic(err)
	}
	return registry
}

func buildRuntimeTestCoreRegistry() (*coredesc.Registry, error) {
	registry := coredesc.NewRegistry()
	ids := make([]smartnode.CoreID, 0, len(runtimeTestWorkerDescriptors))
	for id := range runtimeTestWorkerDescriptors {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool {
		return ids[left] < ids[right]
	})
	for _, id := range ids {
		if err := registry.Register(runtimeTestWorkerDescriptors[id]); err != nil {
			return nil, fmt.Errorf("register runtime test core %s: %w", id, err)
		}
	}
	return registry, nil
}

func (adapter runtimeTestWorkerAdapter) Core() smartnode.CoreID { return adapter.core }
func (adapter runtimeTestWorkerAdapter) Descriptor() coredesc.Descriptor {
	if descriptor, ok := runtimeTestWorkerDescriptors[adapter.core]; ok {
		return descriptor
	}
	return coredesc.Descriptor{ID: adapter.core}
}
func (adapter runtimeTestWorkerAdapter) Prepare(_ context.Context, opID string, request worker.CompileRequest) (worker.Candidate, error) {
	if adapter.control != nil && adapter.control.workerPrepareErr != nil {
		return worker.Candidate{OpID: opID, WorkerID: request.Worker.ID, Core: adapter.core, Path: "/tmp/" + request.Worker.ID}, adapter.control.workerPrepareErr
	}
	return worker.Candidate{OpID: opID, WorkerID: request.Worker.ID, Core: adapter.core, Path: "/tmp/" + request.Worker.ID, CheckOK: true}, nil
}
func (runtimeTestWorkerAdapter) Verify(candidate worker.Candidate) error {
	if !candidate.CheckOK {
		return fmt.Errorf("candidate was not checked")
	}
	return nil
}
func (runtimeTestWorkerAdapter) Install(_ context.Context, candidate worker.Candidate) (worker.Activation, error) {
	return worker.Activation{WorkerID: candidate.WorkerID, ConfigPath: candidate.Path}, nil
}
func (runtimeTestWorkerAdapter) Activate(_ context.Context, candidate worker.Candidate) (worker.Activation, error) {
	return worker.Activation{WorkerID: candidate.WorkerID, ConfigPath: candidate.Path, Restarted: true}, nil
}
func (runtimeTestWorkerAdapter) Start(_ context.Context, target state.WorkerTarget) (worker.Activation, error) {
	return worker.Activation{WorkerID: target.ID, Restarted: true}, nil
}
func (adapter runtimeTestWorkerAdapter) Backup(_ string, workerID string) (worker.Backup, error) {
	return worker.Backup{WorkerID: workerID, Core: adapter.core, Existed: true}, nil
}
func (runtimeTestWorkerAdapter) Restore(_ context.Context, backup worker.Backup) (worker.Activation, error) {
	return worker.Activation{WorkerID: backup.WorkerID, Restarted: backup.WasActive}, nil
}
func (runtimeTestWorkerAdapter) Stop(context.Context, string) error     { return nil }
func (runtimeTestWorkerAdapter) Probe(context.Context, string) error    { return nil }
func (runtimeTestWorkerAdapter) RemoveCandidate(worker.Candidate) error { return nil }
func (adapter runtimeTestWorkerAdapter) SelectRoute(_ context.Context, path worker.BindingPath) error {
	if adapter.control == nil || !adapter.control.cutoverReady {
		return nil
	}
	selector := "Pool_" + path.Route.Pool
	adapter.control.selectCalls = append(adapter.control.selectCalls, selector+"="+path.Executor.SmartNode)
	adapter.control.selectSelector = selector
	adapter.control.selectOutbound = path.Executor.SmartNode
	if adapter.control.selectErrors != nil {
		return adapter.control.selectErrors[selector]
	}
	return nil
}
func (runtimeTestWorkerAdapter) VerifyRoute(context.Context, worker.BindingPath) error { return nil }
func (adapter runtimeTestWorkerAdapter) DelayRoute(ctx context.Context, path worker.BindingPath, testURL string, timeout time.Duration) (worker.DelayResult, error) {
	if adapter.control == nil {
		return worker.DelayResult{}, fmt.Errorf("test route delay control is not configured")
	}
	result, err := adapter.control.Delay(ctx, path.Executor.SmartNode, testURL, timeout)
	return worker.DelayResult{DelayMS: result.DelayMS, URL: result.URL}, err
}
func (adapter runtimeTestWorkerAdapter) CountRouteConnections(context.Context, worker.BindingPath) (int, error) {
	if adapter.control == nil {
		return 0, nil
	}
	adapter.control.routeMu.Lock()
	defer adapter.control.routeMu.Unlock()
	return adapter.control.routeConnections, adapter.control.routeConnectionErr
}
func (adapter runtimeTestWorkerAdapter) ListRouteConnections(_ context.Context, path worker.BindingPath) ([]worker.Connection, error) {
	if adapter.control == nil {
		return nil, nil
	}
	adapter.control.routeMu.Lock()
	defer adapter.control.routeMu.Unlock()
	if adapter.control.routeConnectionErr != nil {
		return nil, adapter.control.routeConnectionErr
	}
	connections := make([]worker.Connection, 0, len(adapter.control.workerConnections))
	for _, connection := range adapter.control.workerConnections {
		if workerConnectionMatchesPath(connection, path) {
			connections = append(connections, cloneWorkerConnection(connection))
		}
	}
	return connections, nil
}
func (adapter runtimeTestWorkerAdapter) CloseRouteConnection(_ context.Context, _ worker.BindingPath, connectionID string) error {
	if adapter.control == nil {
		return nil
	}
	adapter.control.routeMu.Lock()
	defer adapter.control.routeMu.Unlock()
	if adapter.control.routeConnectionErr != nil {
		return adapter.control.routeConnectionErr
	}
	for index, connection := range adapter.control.workerConnections {
		if connection.ID != connectionID {
			continue
		}
		adapter.control.closedConnectionIDs = append(adapter.control.closedConnectionIDs, connectionID)
		adapter.control.workerConnections = append(adapter.control.workerConnections[:index], adapter.control.workerConnections[index+1:]...)
		return adapter.control.routeConnectionErr
	}
	return adapter.control.routeConnectionErr
}
func (adapter runtimeTestWorkerAdapter) CloseRouteConnections(_ context.Context, path worker.BindingPath) (int, error) {
	if adapter.control == nil || !adapter.control.cutoverReady {
		return 0, nil
	}
	adapter.control.routeMu.Lock()
	defer adapter.control.routeMu.Unlock()
	if adapter.control.routeConnectionErr != nil {
		return 0, adapter.control.routeConnectionErr
	}
	closed := 0
	remaining := adapter.control.workerConnections[:0]
	for _, connection := range adapter.control.workerConnections {
		if workerConnectionMatchesPath(connection, path) {
			adapter.control.closedConnectionIDs = append(adapter.control.closedConnectionIDs, connection.ID)
			closed++
			continue
		}
		remaining = append(remaining, connection)
	}
	adapter.control.workerConnections = remaining
	return closed, nil
}

func workerConnectionMatchesPath(connection worker.Connection, path worker.BindingPath) bool {
	for _, chain := range connection.Chains {
		if chain == path.Route.ID || chain == path.Route.Selector || chain == path.TProxy.ID || chain == path.DNS.ID {
			return true
		}
	}
	return false
}

func cloneWorkerConnection(connection worker.Connection) worker.Connection {
	connection.Metadata = cloneMapAny(connection.Metadata)
	connection.Chains = append([]string(nil), connection.Chains...)
	return connection
}

func cloneMapAny(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

type runtimeTestFabricRunner struct{}

func (runtimeTestFabricRunner) Run(context.Context, []string) (string, error) { return "ok", nil }

func newRuntimeTestDataPlane(t *testing.T, owners *netgate.OwnerRegistry, control *fakeProxyControl, runner netgate.Runner) (*worker.Manager, *fabric.Manager) {
	t.Helper()
	workers, err := worker.NewManager(
		runtimeTestWorkerAdapter{core: runtimeCoreA, control: control},
		runtimeTestWorkerAdapter{core: runtimeCoreB, control: control},
	)
	if err != nil {
		t.Fatalf("new runtime test worker manager: %v", err)
	}
	if runner == nil {
		runner = runtimeTestFabricRunner{}
	}
	fabricManager := fabric.NewManager(owners, runner).WithConnectionTracker(fabric.NoopConnectionTracker{})
	t.Cleanup(func() {
		_ = fabricManager.Stop(context.Background())
		_ = workers.StopAll(context.Background())
	})
	return workers, fabricManager
}

func configureRuntimeGraphReadyManualProvider(t *testing.T, layout state.Layout, spec *state.SpecState, providerID string) string {
	t.Helper()
	if providerID == "" {
		providerID = "manual"
	}
	rawNodes := []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com", "server_port": 443}}
	views := configureRuntimeManualProviderNodes(t, layout, spec, providerID, rawNodes)
	if len(views) != 1 || views[0].ID == "" {
		t.Fatalf("expected one graph-ready node, got %+v", views)
	}
	nodeID := views[0].ID
	if len(spec.BusinessPools) == 0 {
		spec.BusinessPools = state.DefaultBusinessPools()
	}
	generalFound := false
	for index := range spec.BusinessPools {
		if spec.BusinessPools[index].ID != "general" {
			continue
		}
		spec.BusinessPools[index].NodeIDs = []string{nodeID}
		generalFound = true
		break
	}
	if !generalFound {
		t.Fatalf("graph-ready spec has no general business pool: %+v", spec.BusinessPools)
	}
	return nodeID
}

func configureRuntimeManualProviderNodes(t *testing.T, layout state.Layout, spec *state.SpecState, providerID string, rawNodes []map[string]any) []provider.NodeView {
	t.Helper()
	providerSpec := state.ProviderSpec{ID: providerID, Name: "Manual", Type: provider.TypeManual, Enabled: true}
	spec.Providers = []state.ProviderSpec{providerSpec}
	env, err := provider.NewLastGoodStore(layout).Save(providerID, rawNodes)
	if err != nil {
		t.Fatalf("save graph-ready last-good: %v", err)
	}
	snapshots := []state.LastGoodSnapshot{{
		ProviderID: env.ProviderID, SavedAt: env.SavedAt, Subscription: env.Subscription,
		Identities: env.Identities, Outbounds: env.Outbounds,
	}}
	reconciled, _, err := provider.ReconcileSmartNodes(*spec, snapshots, time.Now().UTC())
	if err != nil {
		t.Fatalf("reconcile graph-ready SmartNodes: %v", err)
	}
	views, err := provider.BuildNodeViewsFromSnapshots(reconciled, snapshots)
	if err != nil {
		t.Fatalf("build graph-ready nodes: %v", err)
	}
	for _, view := range views {
		if view.ID == "" {
			t.Fatalf("graph-ready node has empty ID: %+v", view)
		}
		node := reconciled.SmartNodes[view.ID]
		node.CoreChoice = runtimeCoreA
		reconciled.SmartNodes[view.ID] = node
	}
	reconciled.Fabric.DNS.Listen = freeRuntimeDNSListen(t)
	*spec = reconciled
	return views
}

func setRuntimeTestPoolNodeIDs(t *testing.T, spec *state.SpecState, poolID string, nodeIDs ...string) {
	t.Helper()
	for index := range spec.BusinessPools {
		if spec.BusinessPools[index].ID != poolID {
			continue
		}
		spec.BusinessPools[index].NodeIDs = append([]string(nil), nodeIDs...)
		return
	}
	t.Fatalf("business pool %s not found in test spec: %+v", poolID, spec.BusinessPools)
}

func (c *fakeProxyControl) Select(_ context.Context, selectorTag, outboundTag string) error {
	c.selectCalls = append(c.selectCalls, selectorTag+"="+outboundTag)
	c.selectSelector = selectorTag
	c.selectOutbound = outboundTag
	if c.selectErrors != nil {
		if err := c.selectErrors[selectorTag]; err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeProxyControl) Selected(_ context.Context, selectorTag string) (string, error) {
	if c.selectedLookup == nil {
		return "", fmt.Errorf("selector lookup is not configured")
	}
	return c.selectedLookup(selectorTag)
}

func (c *fakeProxyControl) Delay(ctx context.Context, outboundTag, testURL string, _ time.Duration) (selection.DelayResult, error) {
	c.delayMu.Lock()
	c.delayOutbound = outboundTag
	c.delayCalls = append(c.delayCalls, outboundTag)
	c.delayMu.Unlock()
	if wait := c.delayWait[outboundTag]; wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return selection.DelayResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	if err := c.delayErrors[outboundTag]; err != nil {
		return selection.DelayResult{}, err
	}
	if err := c.delayErrorsByURL[outboundTag+"|"+testURL]; err != nil {
		return selection.DelayResult{}, err
	}
	return selection.DelayResult{DelayMS: 123, URL: testURL}, nil
}

func TestFailoverProbeBatchUsesBoundedConcurrencyAndReturnsPartialResults(t *testing.T) {
	control := &fakeProxyControl{delayWait: map[string]time.Duration{}}
	nodes := map[string]struct{}{}
	nodeIDs := []string{"n1", "n2", "n3", "n4", "n5"}
	topology := state.AppliedTopology{}
	descriptor := newRuntimeTestCoreRegistry(t).List()[0]
	workerID := descriptor.WorkerID(1)
	candidates := make([]string, 0, len(nodeIDs))
	identities := map[string]smartnode.IdentitySource{}
	for _, nodeID := range nodeIDs {
		nodes[nodeID] = struct{}{}
		control.delayWait[nodeID] = 100 * time.Millisecond
		executorID := "exec-" + nodeID
		candidates = append(candidates, executorID)
		topology.ExecutionGraph.NodeExecutors = append(topology.ExecutionGraph.NodeExecutors, state.NodeExecutor{
			ID: executorID, SmartNode: nodeID, Revision: 1, Core: descriptor.ID, Worker: workerID,
		})
		identities[nodeID] = smartnode.IdentitySource{}
	}
	topology.ExecutionGraph.Workers = []state.WorkerTarget{{
		ID: workerID, Core: descriptor.ID,
		Control: state.WorkerControl{Kind: "clash", Listen: "127.0.0.1", Port: 9090},
		Routes: []state.WorkerRoute{{
			ID: "route-general-0", Pool: "general", Lane: 0, Selector: "route-general-0",
			Candidates: candidates, Selected: candidates[0],
		}},
		Inbounds: []state.WorkerInbound{
			{ID: "tproxy-general-0", Kind: "tproxy", Pool: "general", Route: "route-general-0", Lane: 0},
			{ID: "dns-general-0", Kind: "socks", Pool: "general", Route: "route-general-0", Lane: 0},
		},
	}}
	topology.ExecutionGraph.PoolBindings = []state.PoolBinding{{
		ID: "binding-general", Pool: "general",
		Target:     state.BindingTarget{Kind: "node", SmartNode: nodeIDs[0], Executor: candidates[0], Core: descriptor.ID, Worker: workerID},
		Generation: 1,
	}}
	manager, err := worker.NewManager(runtimeTestWorkerAdapter{core: descriptor.ID, control: control})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := manager.Prepare(context.Background(), "probe-batch", topology.ExecutionGraph, identities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	runner := schedulerFailoverRunner{api: nodesAPI{workers: manager}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	results, err := runner.probeBatch(ctx, topology, nodes, nil)
	if err == nil {
		t.Fatal("expected batch deadline after four concurrent probes")
	}
	if len(results) < 4 {
		t.Fatalf("completed probe samples were discarded: got %d results", len(results))
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("probe batch behaved serially: %s", elapsed)
	}
}

func TestWatchdogEligibilityIsolatesPoolsWithIncompleteProbeClosure(t *testing.T) {
	groups := []proxyGroupView{
		{ID: "complete", Selected: "a", Candidates: []string{"a", "b"}},
		{ID: "incomplete", Selected: "c", Candidates: []string{"c", "d"}},
	}
	probed := map[string]state.NodeHealth{
		"a": {Status: "failed"}, "b": {Status: "ok"},
		"c": {Status: "failed"},
	}
	eligible := watchdogEligiblePools(groups, probed)
	if !eligible["complete"] || eligible["incomplete"] {
		t.Fatalf("unexpected per-pool eligibility: %+v", eligible)
	}
}

func (c *fakeProxyControl) Connections(_ context.Context) (map[string]any, error) {
	c.connectionsCalled = true
	if c.connectionsPayload != nil {
		return c.connectionsPayload, nil
	}
	return map[string]any{"connections": []any{}, "downloadTotal": 0, "uploadTotal": 0}, nil
}

func (c *fakeProxyControl) CloseConnections(_ context.Context) error {
	c.closeConnectionsCalled = true
	return nil
}

func (c *fakeProxyControl) CloseConnection(_ context.Context, id string) error {
	c.closedConnectionIDs = append(c.closedConnectionIDs, id)
	return nil
}

func (c *fakeProxyControl) setRouteConnections(count int) {
	c.routeMu.Lock()
	c.routeConnections = count
	c.routeMu.Unlock()
}

func TestNodesHandlerReturnsNormalizedLastGoodNodes(t *testing.T) {
	server, store := newNodesTestServer(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "example.com", "server_port": 443},
		{"type": "naive", "tag": "bad", "server": "bad.example"},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("nodes status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	nodesPayload, _ := json.Marshal(data["nodes"])
	var nodes []struct {
		ID          string `json:"id"`
		Revision    int    `json:"revision"`
		DisplayName string `json:"display_name"`
		Identity    struct {
			Protocol string `json:"protocol"`
			Server   string `json:"server"`
			Port     int    `json:"port"`
		} `json:"identity"`
		Aliases []struct {
			ProviderID string `json:"provider_id"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(nodesPayload, &nodes); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected one usable SmartNode, got %+v", nodes)
	}
	if nodes[0].Aliases[0].ProviderID != "p1" || nodes[0].DisplayName != "node-1" || nodes[0].Revision != 1 {
		t.Fatalf("unexpected usable node: %+v", nodes[0])
	}
	if nodes[0].Identity.Protocol != "vmess" || nodes[0].Identity.Server != "example.com" || nodes[0].Identity.Port != 443 {
		t.Fatalf("expected server port in node view, got %+v", nodes[0])
	}
}

func TestSmartNodePublicViewUsesEmptyArrays(t *testing.T) {
	node := smartnode.SmartNode{
		ID: "empty-node",
		Capability: smartnode.CapabilityMatrix{
			runtimeCoreA: {Status: smartnode.CapabilitySupported},
		},
	}
	view := smartNodePublicView(node, nil, state.NodeHealth{}, true)
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"aliases", "referenced_by_pools"} {
		if string(data[field]) != "[]" {
			t.Fatalf("%s = %s, want []", field, data[field])
		}
	}
	if view.Capability[runtimeCoreA].Reasons == nil {
		t.Fatal("capability reasons must serialize as []")
	}
	if node.Capability[runtimeCoreA].Reasons != nil {
		t.Fatal("public view mutated the catalog capability")
	}
}

func TestSmartNodeDetailCoreChoiceAndDeepTest(t *testing.T) {
	server, store := newNodesTestServer(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: provider.TypeManual, Enabled: true}}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{{
		"type": "vless", "tag": "xhttp-node", "server": "node.example", "server_port": 443,
		"uuid": "plain-credential-leak", "transport": map[string]any{"type": "xhttp", "path": "/api"},
	}}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err = provider.ReconcileSmartNodes(spec, snapshots, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var nodeID string
	for id := range spec.SmartNodes {
		nodeID = id
	}
	spec.BusinessPools[1].NodeIDs = []string{nodeID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}

	detail := serve(t, server, http.MethodGet, "/api/nodes/"+nodeID, nil)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "xhttp-node") || strings.Contains(detail.Body.String(), "plain-credential-leak") {
		t.Fatalf("unexpected SmartNode detail: status=%d body=%s", detail.Code, detail.Body.String())
	}
	selectCoreB := serve(t, server, http.MethodPut, "/api/nodes/"+nodeID+"/core-choice", mustJSON(t, coreChoicePayload{
		Core: runtimeCoreB, OpID: "api-core-xray",
	}))
	if selectCoreB.Code != http.StatusOK {
		t.Fatalf("xray core choice status=%d body=%s", selectCoreB.Code, selectCoreB.Body.String())
	}
	saved, err := store.LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if saved.SmartNodes[nodeID].CoreChoice != runtimeCoreB {
		t.Fatalf("core choice not saved: %+v", saved.SmartNodes[nodeID])
	}
	selectDegraded := serve(t, server, http.MethodPut, "/api/nodes/"+nodeID+"/core-choice", mustJSON(t, coreChoicePayload{
		Core: runtimeCoreA, OpID: "api-core-singbox",
	}))
	if selectDegraded.Code != http.StatusConflict || !strings.Contains(selectDegraded.Body.String(), "confirm_degraded_required") {
		t.Fatalf("degraded core was not gated: status=%d body=%s", selectDegraded.Code, selectDegraded.Body.String())
	}
	deep := serve(t, server, http.MethodPost, "/api/nodes/"+nodeID+"/deep-test", nil)
	if deep.Code != http.StatusNotImplemented || !strings.Contains(deep.Body.String(), "deep_test_not_implemented") {
		t.Fatalf("deep test placeholder mismatch: status=%d body=%s", deep.Code, deep.Body.String())
	}
	for index := range saved.BusinessPools {
		saved.BusinessPools[index].NodeIDs = nil
	}
	if _, err := store.SaveSpec(saved); err != nil {
		t.Fatal(err)
	}
	unreferenced := serve(t, server, http.MethodPut, "/api/nodes/"+nodeID+"/core-choice", mustJSON(t, coreChoicePayload{
		Core: runtimeCoreB, OpID: "api-core-unreferenced",
	}))
	if unreferenced.Code != http.StatusOK {
		t.Fatalf("unreferenced core choice status=%d body=%s", unreferenced.Code, unreferenced.Body.String())
	}
	pools, ok := decodeEnvelopeData(t, unreferenced.Body.Bytes())["affected_pools"].([]any)
	if !ok || len(pools) != 0 {
		t.Fatalf("unreferenced core choice must return affected_pools: [], got %s", unreferenced.Body.String())
	}
}

func TestNodesHandlerReadsCurrentSpecWithoutStaleProviderCache(t *testing.T) {
	server, store := newNodesTestServer(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "trojan", "tag": "node-1", "server": "example.com"},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	if got := readNodeCount(t, server); got != 1 {
		t.Fatalf("node count before delete = %d", got)
	}

	spec.Providers = nil
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save empty spec: %v", err)
	}
	if got := readNodeCount(t, server); got != 0 {
		t.Fatalf("node count after provider delete = %d", got)
	}
}

func readNodeCount(t *testing.T, server *Server) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("nodes status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	nodesPayload, _ := json.Marshal(data["nodes"])
	var nodes []provider.NodeView
	if err := json.Unmarshal(nodesPayload, &nodes); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	return len(nodes)
}

func TestProxyGroupsDefaultSelectAndNodeDelay(t *testing.T) {
	defer selection.SetSelectorRebindDelaysForTest(nil)()
	control := &fakeProxyControl{}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "trojan", "tag": "node-1", "server": "example.com", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build node views: %v", err)
	}
	nodeID := nodes[0].ID
	spec.BusinessPools[0].NodeIDs = []string{nodeID}
	spec.BusinessPools[0].Policy.Cutover = state.CutoverStrict
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save pool membership: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ProxySelection: map[string]string{"general": "Direct"}}); err != nil {
		t.Fatalf("save initial running selection: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	groupsResp := serve(t, server, http.MethodGet, "/api/proxy-groups", nil)
	if groupsResp.Code != http.StatusOK {
		t.Fatalf("proxy groups status = %d body=%s", groupsResp.Code, groupsResp.Body.String())
	}
	groupsData := decodeEnvelopeData(t, groupsResp.Body.Bytes())
	groups := groupsData["groups"].([]any)
	if len(groups) != 5 || groups[0].(map[string]any)["pool"] != "general" {
		t.Fatalf("unexpected default proxy group: %+v", groupsData)
	}

	control.workerConnections = []worker.Connection{
		{ID: "sticky-general", Chains: []string{"Direct", "Pool_general"}},
		{ID: "other-ai", Chains: []string{"node-x", "Pool_ai"}},
	}
	selectResp := serve(t, server, http.MethodPost, "/api/proxy-groups/general/select", mustJSON(t, cutoverPayloadForPool(t, store, "general", nodeID, "manual-select-general")))
	if selectResp.Code != http.StatusOK {
		t.Fatalf("select status = %d body=%s", selectResp.Code, selectResp.Body.String())
	}
	if result := decodeEnvelopeData(t, selectResp.Body.Bytes()); result["result"] != "cutover_complete" || result["verified"] != true {
		t.Fatalf("strict cutover did not complete: %+v", result)
	}
	if control.selectSelector != "Pool_general" || control.selectOutbound != nodeID {
		t.Fatalf("proxy control not called correctly: %+v", control)
	}
	if control.closeConnectionsCalled {
		t.Fatal("proxy selection should not close all existing connections")
	}
	if len(control.closedConnectionIDs) != 0 {
		t.Fatalf("Direct source must not invoke a Worker-wide connection close: %+v", control.closedConnectionIDs)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != nodeID {
		t.Fatalf("selection not persisted: %+v", runtimeState.ProxySelection)
	}
	failoverState, err := state.NewFailoverStateStore(server.cfg.Layout).Load()
	if err != nil {
		t.Fatalf("load failover state: %v", err)
	}
	if failoverState.Pools["general"].Current != nodeID || failoverState.Pools["general"].LastReason != "manual_select" {
		t.Fatalf("manual selection not reflected in failover state: %+v", failoverState.Pools["general"])
	}

	delayResp := serve(t, server, http.MethodPost, "/api/nodes/"+nodeID+"/delay", mustJSON(t, nodeDelayPayload{}))
	if delayResp.Code != http.StatusOK {
		t.Fatalf("delay status = %d body=%s", delayResp.Code, delayResp.Body.String())
	}
	if control.delayOutbound != nodeID {
		t.Fatalf("delay should call proxy control, got %+v", control)
	}
	runtimeState, err = store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime after delay: %v", err)
	}
	health := runtimeState.NodeHealth[nodeID]
	if health.Status != "ok" || health.DelayMS != 123 || health.TestURL == "" || health.Context.Method != "worker_route_http_204" || health.Context.Worker == "" {
		t.Fatalf("health not recorded: %+v", health)
	}
}

func TestProxyGroupsExposeCutoverContractFields(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFailoverPoolTestServerWithCutover(t, control, state.CutoverStrict)
	now := time.Now().UTC()
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	runtimeState.NodeHealth = map[string]state.NodeHealth{
		nodes[0].ID: {Status: "ok", DelayMS: 42, UpdatedAt: &now},
	}
	if err := store.SaveRuntime(runtimeState); err != nil {
		t.Fatal(err)
	}
	cooldown := now.Add(time.Minute)
	failoverStore := state.NewFailoverStateStore(server.cfg.Layout)
	failoverState, err := failoverStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	failoverState.Locks["general"] = state.FailoverLock{Reason: "maintenance", LockedAt: now}
	poolState := failoverState.Pools["general"]
	poolState.PoolID = "general"
	poolState.CooldownUntil = &cooldown
	failoverState.Pools["general"] = poolState
	if err := failoverStore.Save(failoverState); err != nil {
		t.Fatal(err)
	}

	resp := serve(t, server, http.MethodGet, "/api/proxy-groups", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("proxy groups status=%d body=%s", resp.Code, resp.Body.String())
	}
	group := findGroupMap(t, decodeEnvelopeData(t, resp.Body.Bytes())["groups"].([]any), "general")
	binding := group["binding"].(map[string]any)
	if binding["pool"] != "general" || binding["generation"] != float64(1) {
		t.Fatalf("binding contract fields missing: %+v", binding)
	}
	if target := binding["target"].(map[string]any); target["smart_node"] != nodes[0].ID {
		t.Fatalf("binding target mismatch: %+v", target)
	}
	candidates := group["candidates"].([]any)
	var candidate map[string]any
	for _, raw := range candidates {
		item := raw.(map[string]any)
		if item["smart_node"] == nodes[0].ID {
			candidate = item
			break
		}
	}
	if candidate == nil || candidate["core"] != string(runtimeCoreA) || candidate["health"] != "ok" || candidate["delay_ms"] != float64(42) {
		t.Fatalf("candidate contract fields missing: %+v", candidates)
	}
	if lock := group["lock"].(map[string]any); lock["reason"] != "maintenance" {
		t.Fatalf("lock contract mismatch: %+v", lock)
	}
	if group["cooldown_until"] == nil || group["failover_enabled"] != true {
		t.Fatalf("failover contract fields missing: %+v", group)
	}
}

func TestProxyGroupSelectReturnsSelectionConflict(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFailoverPoolTestServerWithCutover(t, control, state.CutoverStrict)
	payload := cutoverPayloadForPool(t, store, "general", nodes[1].ID, "selection-conflict")
	payload.Source.Generation++

	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/general/select", mustJSON(t, payload))
	if resp.Code != http.StatusConflict {
		t.Fatalf("selection conflict status=%d body=%s", resp.Code, resp.Body.String())
	}
	code, details := decodeEnvelopeError(t, resp.Body.Bytes())
	if code != "selection_conflict" || details["pool"] != "general" {
		t.Fatalf("selection conflict payload mismatch: code=%s details=%+v", code, details)
	}
	actual := details["actual"].(map[string]any)
	if actual["smart_node"] != nodes[0].ID || actual["generation"] != float64(1) {
		t.Fatalf("selection conflict actual Binding mismatch: %+v", actual)
	}
}

func TestProxyGroupSelectReturnsRequiresConfigApply(t *testing.T) {
	control := &fakeProxyControl{}
	appliedServer, store, nodes := newFailoverPoolTestServerWithCutover(t, control, state.CutoverStrict)
	server := newUnconfiguredCutoverTestServer(t, appliedServer.cfg.Layout, store, control)
	payload := cutoverPayloadForPool(t, store, "general", nodes[1].ID, "requires-config-apply")

	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/general/select", mustJSON(t, payload))
	if resp.Code != http.StatusConflict {
		t.Fatalf("requires config apply status=%d body=%s", resp.Code, resp.Body.String())
	}
	code, details := decodeEnvelopeError(t, resp.Body.Bytes())
	if code != "requires_config_apply" || details["pool"] != "general" || details["target"] != nodes[1].ID {
		t.Fatalf("requires config apply payload mismatch: code=%s details=%+v", code, details)
	}
}

func TestProxyGroupSelectReturnsCutoverFailureDetails(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFailoverPoolTestServerWithCutover(t, control, state.CutoverStrict)
	control.selectErrors = map[string]error{"Pool_general": fmt.Errorf("route control unavailable")}
	payload := cutoverPayloadForPool(t, store, "general", nodes[1].ID, "cutover-failure")

	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/general/select", mustJSON(t, payload))
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("cutover failure status=%d body=%s", resp.Code, resp.Body.String())
	}
	code, details := decodeEnvelopeError(t, resp.Body.Bytes())
	if code != "cutover_failed" || details["stage"] != "select_target_executor" || details["rolled_back"] != true || details["blocked"] != false {
		t.Fatalf("cutover failure payload mismatch: code=%s details=%+v", code, details)
	}
	payload = cutoverPayloadForPool(t, store, "general", nodes[1].ID, "cutover-after-failure")
	if payload.Source.SmartNode != nodes[0].ID || payload.Source.Generation != 1 {
		t.Fatalf("failed cutover changed durable Binding: %+v", payload.Source)
	}
}

func TestRuntimeStatusReportsGracefulDrainAndGeneration(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFailoverPoolTestServerWithCutover(t, control, state.CutoverGraceful)
	control.setRouteConnections(3)
	payload := cutoverPayloadForPool(t, store, "general", nodes[1].ID, "graceful-status")

	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/general/select", mustJSON(t, payload))
	if resp.Code != http.StatusOK {
		t.Fatalf("graceful cutover status=%d body=%s", resp.Code, resp.Body.String())
	}
	result := decodeEnvelopeData(t, resp.Body.Bytes())
	if result["result"] != "committed" || result["draining"].(map[string]any)["remaining"] != float64(3) {
		t.Fatalf("graceful cutover result mismatch: %+v", result)
	}

	statusResp := serve(t, server, http.MethodGet, "/api/runtime/status", nil)
	if statusResp.Code != http.StatusOK {
		t.Fatalf("runtime status=%d body=%s", statusResp.Code, statusResp.Body.String())
	}
	status := decodeEnvelopeData(t, statusResp.Body.Bytes())
	binding := findBindingMap(t, status["bindings"].([]any), "general")
	if binding["generation"] != float64(payload.Source.Generation+1) || binding["target"].(map[string]any)["smart_node"] != nodes[1].ID {
		t.Fatalf("runtime Binding did not advance: %+v", binding)
	}
	drain := findDrainMap(t, status["drains"].([]any), "general")
	if drain["old_generation"] != float64(payload.Source.Generation) || drain["remaining"] != float64(3) || drain["deadline"] == nil {
		t.Fatalf("runtime drain mismatch: %+v", drain)
	}

	control.setRouteConnections(0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		statusResp = serve(t, server, http.MethodGet, "/api/runtime/status", nil)
		status = decodeEnvelopeData(t, statusResp.Body.Bytes())
		if len(status["drains"].([]any)) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("graceful drain did not finish after connections reached zero")
}

func TestProxyGroupsGETAndPOSTUseSameAppliedTopologyAfterSavedRefresh(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider", Type: provider.TypeManual, Enabled: true}}
	oldRaw := []map[string]any{{"type": "vmess", "tag": "old", "server": "old.example"}}
	newRaw := []map[string]any{{"type": "vmess", "tag": "new", "server": "new.example"}}
	oldID := provider.NormalizeProviderOutbounds(spec.Providers[0], oldRaw)[0].ID
	newID := provider.NormalizeProviderOutbounds(spec.Providers[0], newRaw)[0].ID
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{oldID}}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", oldRaw); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 2, TopologyHash: "old", GatewayMode: state.GatewayModeOn, ProxySelection: map[string]string{"general": oldID}}); err != nil {
		t.Fatal(err)
	}
	captureAppliedTopology(t, server, store, spec)
	saved := spec
	saved.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: []string{newID}}}
	if _, err := store.SaveSpec(saved); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", newRaw); err != nil {
		t.Fatal(err)
	}

	resp := serve(t, server, http.MethodGet, "/api/proxy-groups", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", resp.Code, resp.Body.String())
	}
	group := decodeEnvelopeData(t, resp.Body.Bytes())["groups"].([]any)[0].(map[string]any)
	candidates := group["candidates"].([]any)
	if !containsAnyString(candidates, oldID) || containsAnyString(candidates, newID) {
		t.Fatalf("GET leaked saved topology: %+v", group)
	}
	if got := serve(t, server, http.MethodPost, "/api/proxy-groups/general/select", mustJSON(t, cutoverPayloadForPool(t, store, "general", oldID, "select-applied-old"))); got.Code != http.StatusOK {
		t.Fatalf("applied candidate rejected: %d %s", got.Code, got.Body.String())
	}
	if got := serve(t, server, http.MethodPost, "/api/proxy-groups/general/select", mustJSON(t, cutoverPayloadForPool(t, store, "general", newID, "select-unapplied-new"))); got.Code != http.StatusBadRequest {
		t.Fatalf("saved-only candidate accepted: %d %s", got.Code, got.Body.String())
	}
}

func TestNodeDelayPreservesProxySelection(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "trojan", "tag": "node-1", "server": "example.com", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build node views: %v", err)
	}
	nodeID := nodes[0].ID
	spec.BusinessPools[0].NodeIDs = []string{nodeID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save node pool membership: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": "Direct"},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	delayResp := serve(t, server, http.MethodPost, "/api/nodes/"+nodeID+"/delay", mustJSON(t, nodeDelayPayload{}))
	if delayResp.Code != http.StatusOK {
		t.Fatalf("delay status = %d body=%s", delayResp.Code, delayResp.Body.String())
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != "Direct" || runtimeState.NodeHealth[nodeID].Status != "ok" {
		t.Fatalf("node delay should merge health without replacing selection: %+v", runtimeState)
	}
}

func TestProxyGroupRejectsNonCandidate(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", Policy: state.TemplatePoolPolicy("general")},
		{ID: "ai", Name: "AI", Role: "ai", Policy: state.TemplatePoolPolicy("ai")},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)
	payload := cutoverPayloadForPool(t, store, "ai", "missing", "select-missing")
	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/ai/select", mustJSON(t, payload))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("select missing candidate status = %d body=%s", resp.Code, resp.Body.String())
	}
	if control.selectSelector != "" {
		t.Fatalf("proxy control should not be called: %+v", control)
	}
}

func TestProxyGroupsExposeSensitiveFailClosedFallback(t *testing.T) {
	server, store := newNodesTestServer(t)
	spec := state.DefaultSpecState()
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", Policy: state.TemplatePoolPolicy("general")},
		{ID: "ai", Name: "AI", Role: "ai", Policy: state.TemplatePoolPolicy("ai")},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodGet, "/api/proxy-groups", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("proxy groups status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	groups := data["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("unexpected groups: %+v", data)
	}
	group := findGroupMap(t, groups, "ai")
	binding := group["binding"].(map[string]any)
	target := binding["target"].(map[string]any)
	if target["kind"] != "block" {
		t.Fatalf("sensitive empty group should expose Block fail-closed fallback: %+v", group)
	}
	candidates := group["candidates"].([]any)
	if len(candidates) != 1 || !containsAnyString(candidates, "Block") {
		t.Fatalf("unexpected candidates: %+v", candidates)
	}
}

func TestProxyGroupsUseExplicitPoolMembershipAndPolicyFallback(t *testing.T) {
	server, store := newNodesTestServer(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
		Roles:   []string{"ai", "crypto"},
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "trojan", "tag": "role-node", "server": "example.com", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build node views: %v", err)
	}
	nodeID := nodes[0].ID
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", Policy: state.TemplatePoolPolicy("general")},
		{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{nodeID}, Policy: state.TemplatePoolPolicy("ai")},
		{ID: "crypto", Name: "Crypto", Role: "crypto", NodeIDs: []string{nodeID}, Policy: state.TemplatePoolPolicy("crypto")},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save explicit pool membership: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodGet, "/api/proxy-groups", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("proxy groups status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	groups := data["groups"].([]any)
	ai := findGroupMap(t, groups, "ai")
	if !containsAnyString(ai["candidates"].([]any), nodeID) || !containsAnyString(ai["candidates"].([]any), "Block") {
		t.Fatalf("ai group should expose role node and Block fallback: %+v", ai)
	}
	crypto := findGroupMap(t, groups, "crypto")
	if !containsAnyString(crypto["candidates"].([]any), nodeID) || !containsAnyString(crypto["candidates"].([]any), "Block") {
		t.Fatalf("crypto group should expose role node and Block fallback: %+v", crypto)
	}
}

func TestProxyGroupsExposeDirectFallbackForGeneralPools(t *testing.T) {
	server, store := newNodesTestServer(t)
	spec := state.DefaultSpecState()
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", Policy: state.TemplatePoolPolicy("general")}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodGet, "/api/proxy-groups", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("proxy groups status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	groups := data["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("unexpected groups: %+v", data)
	}
	group := groups[0].(map[string]any)
	binding := group["binding"].(map[string]any)
	if binding["target"].(map[string]any)["kind"] != "direct" {
		t.Fatalf("general empty group should expose Direct fallback: %+v", group)
	}
	candidates := group["candidates"].([]any)
	if len(candidates) != 1 || !containsAnyString(candidates, "Direct") {
		t.Fatalf("unexpected candidates: %+v", candidates)
	}
}

func TestProxyGroupsCapCandidatesToRuntimeSelectorLimit(t *testing.T) {
	server, store := newNodesTestServer(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	var outbounds []map[string]any
	for i := 0; i < 12; i++ {
		tag := fmt.Sprintf("node-%02d", i)
		outbounds = append(outbounds, map[string]any{"type": "vmess", "tag": tag, "server": fmt.Sprintf("node-%02d.example", i), "server_port": 443})
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", outbounds); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build nodes: %v", err)
	}
	var poolNodes []string
	for _, node := range nodes {
		poolNodes = append(poolNodes, node.ID)
	}
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", NodeIDs: poolNodes}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save pool nodes: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodGet, "/api/proxy-groups", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("proxy groups status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	group := data["groups"].([]any)[0].(map[string]any)
	candidates := group["candidates"].([]any)
	if len(candidates) != maxProxyGroupCandidates+1 {
		t.Fatalf("general pool should expose 10 nodes plus Direct, got %+v", candidates)
	}
	if !containsAnyString(candidates[maxProxyGroupCandidates:], "Direct") {
		t.Fatalf("Direct should be fallback after capped node candidates: %+v", candidates)
	}
	for _, candidate := range candidates {
		if containsAnyString([]any{candidate}, poolNodes[10]) || containsAnyString([]any{candidate}, poolNodes[11]) {
			t.Fatalf("candidate beyond sing-box selector cap should not be exposed: %+v", candidates)
		}
	}
}

func TestProxyGroupFailoverSwitchesToHealthyCandidate(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general"}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "one.example", "server_port": 443},
		{"type": "vmess", "tag": "node-2", "server": "two.example", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build nodes: %v", err)
	}
	spec.BusinessPools[0].NodeIDs = []string{nodes[0].ID, nodes[1].ID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save pool nodes: %v", err)
	}
	now := time.Now().UTC()
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": nodes[0].ID},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &now},
			nodes[1].ID: {Status: "ok", UpdatedAt: &now},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/run", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("failover status = %d body=%s", resp.Code, resp.Body.String())
	}
	if control.selectSelector != "Pool_general" || control.selectOutbound != nodes[1].ID {
		t.Fatalf("failover select not called correctly: %+v", control)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != nodes[1].ID {
		t.Fatalf("failover selection not persisted: %+v", runtimeState.ProxySelection)
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if len(data["applied"].([]any)) != 1 {
		t.Fatalf("failover response should include applied decision: %+v", data)
	}
	events, err := state.NewFailoverEventStore(server.cfg.Layout).List()
	if err != nil {
		t.Fatalf("list failover events: %v", err)
	}
	if len(events) != 1 || events[0].PoolID != "general" || events[0].From != nodes[0].ID || events[0].To != nodes[1].ID || events[0].Kind != "switch" || events[0].Source != "manual" {
		t.Fatalf("failover event not persisted correctly: %+v", events)
	}

	eventsResp := serve(t, server, http.MethodGet, "/api/proxy-groups/failover/events", nil)
	if eventsResp.Code != http.StatusOK {
		t.Fatalf("failover events status = %d body=%s", eventsResp.Code, eventsResp.Body.String())
	}
	eventsData := decodeEnvelopeData(t, eventsResp.Body.Bytes())
	if len(eventsData["events"].([]any)) != 1 {
		t.Fatalf("failover events endpoint should return persisted event: %+v", eventsData)
	}
}

func TestProxyGroupFailoverFailsClosedToBlock(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", Policy: state.TemplatePoolPolicy("general")},
		{ID: "ai", Name: "AI", Role: "ai", Policy: state.TemplatePoolPolicy("ai")},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ProxySelection: map[string]string{"ai": "Block"}}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/run", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("failover status = %d body=%s", resp.Code, resp.Body.String())
	}
	if control.selectSelector != "" {
		t.Fatalf("already fail-closed pool should not reselect Block: %+v", control)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["ai"] != "Block" {
		t.Fatalf("fail-closed selection not persisted: %+v", runtimeState.ProxySelection)
	}
}

func TestProxyGroupFailoverSecondRunDoesNotDuplicateNoopEvent(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general"}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "one.example", "server_port": 443},
		{"type": "vmess", "tag": "node-2", "server": "two.example", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build nodes: %v", err)
	}
	spec.BusinessPools[0].NodeIDs = []string{nodes[0].ID, nodes[1].ID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save pool nodes: %v", err)
	}
	now := time.Now().UTC()
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": nodes[0].ID},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &now},
			nodes[1].ID: {Status: "ok", UpdatedAt: &now},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	first := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/run", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first failover status = %d body=%s", first.Code, first.Body.String())
	}
	second := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/run", nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second failover status = %d body=%s", second.Code, second.Body.String())
	}
	events, err := state.NewFailoverEventStore(server.cfg.Layout).List()
	if err != nil {
		t.Fatalf("list failover events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("noop failover should not duplicate events: %+v", events)
	}
}

func TestProxyGroupFailoverPreservesRuntimeFields(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general"}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "one.example", "server_port": 443},
		{"type": "vmess", "tag": "node-2", "server": "two.example", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build nodes: %v", err)
	}
	spec.BusinessPools[0].NodeIDs = []string{nodes[0].ID, nodes[1].ID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save pool nodes: %v", err)
	}
	now := time.Now().UTC()
	nextRun := now.Add(time.Hour)
	if err := store.SaveRuntime(state.RuntimeState{
		Scheduler: map[string]state.SchedulerTaskRuntime{
			"node-delay-test": {NextRun: &nextRun},
		},
		ProviderLogs: []state.ProviderRefreshLog{{Time: now, Scope: "all", Status: "ok"}},
		ProxySelection: map[string]string{
			"general": nodes[0].ID,
		},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &now},
			nodes[1].ID: {Status: "ok", UpdatedAt: &now},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/run", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("failover status = %d body=%s", resp.Code, resp.Body.String())
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != nodes[1].ID {
		t.Fatalf("selection not updated: %+v", runtimeState.ProxySelection)
	}
	if runtimeState.Scheduler["node-delay-test"].NextRun == nil || len(runtimeState.ProviderLogs) != 1 || runtimeState.NodeHealth[nodes[0].ID].Status != "failed" {
		t.Fatalf("failover should preserve other runtime fields: %+v", runtimeState)
	}
}

func TestProxyGroupFailoverPersistsEarlierAppliedDecisionWhenLaterSelectFails(t *testing.T) {
	control := &fakeProxyControl{selectErrors: map[string]error{"Pool_streaming": fmt.Errorf("selector failed")}}
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general"},
		{ID: "streaming", Name: "Streaming", Role: "streaming"},
	}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "one.example", "server_port": 443},
		{"type": "vmess", "tag": "node-2", "server": "two.example", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build nodes: %v", err)
	}
	spec.BusinessPools[0].NodeIDs = []string{nodes[0].ID, nodes[1].ID}
	spec.BusinessPools[1].NodeIDs = []string{nodes[0].ID, nodes[1].ID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save pool nodes: %v", err)
	}
	now := time.Now().UTC()
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{
			"general":   nodes[0].ID,
			"streaming": nodes[0].ID,
		},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &now},
			nodes[1].ID: {Status: "ok", UpdatedAt: &now},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/run", nil)
	if resp.Code != http.StatusMultiStatus || !strings.Contains(resp.Body.String(), `"status":"degraded"`) || !strings.Contains(resp.Body.String(), `"pool_id":"streaming"`) {
		t.Fatalf("failover should report partial success details, status=%d body=%s", resp.Code, resp.Body.String())
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != nodes[1].ID || runtimeState.ProxySelection["streaming"] != nodes[0].ID {
		t.Fatalf("earlier applied decision should persist and failed later one should not: %+v", runtimeState.ProxySelection)
	}
	events, err := state.NewFailoverEventStore(server.cfg.Layout).List()
	if err != nil {
		t.Fatalf("list failover events: %v", err)
	}
	if len(events) != 1 || events[0].PoolID != "general" || events[0].To != nodes[1].ID {
		t.Fatalf("earlier applied event should persist: %+v", events)
	}
	failoverState, err := state.NewFailoverStateStore(server.cfg.Layout).Load()
	if err != nil {
		t.Fatalf("load failover state: %v", err)
	}
	if pool := failoverState.Pools["general"]; pool.Current != nodes[1].ID {
		t.Fatalf("successful pool failover state should advance: %+v", pool)
	}
	if pool := failoverState.Pools["streaming"]; pool.Current != "" && pool.Current != nodes[0].ID {
		t.Fatalf("failed pool failover Current should stay at pre-txn value %q, got %+v", nodes[0].ID, pool)
	}
	if pool := failoverState.Pools["streaming"]; pool.Previous == nodes[0].ID || pool.LastKind != "" && pool.Current == nodes[1].ID {
		t.Fatalf("failed pool failover metadata should not look applied: %+v", pool)
	}
}

func TestFailoverResponseSanitizesFailureDetails(t *testing.T) {
	result := failoverRunResult{Failures: []failoverFailure{{
		PoolID: "general", Stage: "cutover", Error: "fetch https://user:pass@example.com/sub?token=plain password=hunter2",
	}}}
	safe := sanitizeFailoverRunResult(result)
	encoded := string(mustJSON(t, safe))
	for _, secret := range []string{"user:pass", "/sub?token=plain", "hunter2"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("failover response leaked %q: %s", secret, encoded)
		}
	}
}

func TestFailoverStateDisableAndLockBlockWatchdog(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFailoverPoolTestServer(t, control)
	now := time.Now().UTC()
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": nodes[0].ID},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &now},
			nodes[1].ID: {Status: "ok", UpdatedAt: &now},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	disabled := false
	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/enable", mustJSON(t, failoverEnabledPayload{Enabled: &disabled}))
	if resp.Code != http.StatusOK {
		t.Fatalf("disable failover status = %d body=%s", resp.Code, resp.Body.String())
	}
	result, err := NewSchedulerFailoverRunner(server.cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("watchdog disabled run: %v", err)
	}
	if result.Decisions != 0 || control.selectSelector != "" {
		t.Fatalf("disabled failover should not make decisions or select: result=%+v control=%+v", result, control)
	}
	enabled := true
	resp = serve(t, server, http.MethodPost, "/api/proxy-groups/failover/enable", mustJSON(t, failoverEnabledPayload{Enabled: &enabled}))
	if resp.Code != http.StatusOK {
		t.Fatalf("enable failover status = %d body=%s", resp.Code, resp.Body.String())
	}
	resp = serve(t, server, http.MethodPost, "/api/proxy-groups/failover/lock", mustJSON(t, failoverLockPayload{PoolID: "general", Reason: "maintenance"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("lock failover status = %d body=%s", resp.Code, resp.Body.String())
	}
	result, err = NewSchedulerFailoverRunner(server.cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("watchdog locked run: %v", err)
	}
	if result.Applied != 0 || control.selectSelector != "" {
		t.Fatalf("locked failover should not select: result=%+v control=%+v", result, control)
	}
}

// B26: a Pool on its no_candidate fallback binds nothing, so the Worker hosting
// its candidates is installed but stopped. Probing a candidate must start that
// Worker instead of failing with a refused connection forever.
func TestNodeDelayStartsStoppedWorkerForFallbackPool(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFallbackPoolTestServer(t, control)
	workerID := appliedWorkerForNode(t, store, nodes[0].ID)
	if server.cfg.Workers.IsRunning(workerID) {
		t.Fatalf("precondition: fallback pool must leave Worker %s stopped", workerID)
	}
	resp := serve(t, server, http.MethodPost, "/api/nodes/"+nodes[0].ID+"/delay", mustJSON(t, nodeDelayPayload{}))
	if resp.Code != http.StatusOK {
		t.Fatalf("delay status = %d body=%s", resp.Code, resp.Body.String())
	}
	if !server.cfg.Workers.IsRunning(workerID) {
		t.Fatalf("probe did not start Worker %s: %+v", workerID, server.cfg.Workers.Statuses())
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if health := runtimeState.NodeHealth[nodes[0].ID]; health.Status != "ok" || health.DelayMS != 123 || health.Context.Worker != workerID {
		t.Fatalf("health after on-demand Worker start: %+v", health)
	}
}

func TestFailoverWatchdogRecoversPoolFromFallbackByStartingWorker(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFallbackPoolTestServer(t, control)
	workerID := appliedWorkerForNode(t, store, nodes[0].ID)
	result, err := NewSchedulerFailoverRunner(server.cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("watchdog run: %v", err)
	}
	if result.Applied != 1 || result.Failed != 0 || control.selectSelector != "Pool_general" {
		t.Fatalf("watchdog did not recover the pool from its fallback: result=%+v control=%+v statuses=%+v", result, control, server.cfg.Workers.Statuses())
	}
	if !server.cfg.Workers.IsRunning(workerID) {
		t.Fatalf("recovered pool's Worker %s is not running", workerID)
	}
	topology, err := store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	if selected := topology.ProxySelection["general"]; selected != nodes[0].ID && selected != nodes[1].ID {
		t.Fatalf("pool did not return to a node after recovery: %+v", topology.ProxySelection)
	}
	failoverState, err := state.NewFailoverStateStore(server.cfg.Layout).Load()
	if err != nil {
		t.Fatal(err)
	}
	if pool := failoverState.Pools["general"]; pool.LastReason != "healthy_candidate_recovered" {
		t.Fatalf("recovery was not recorded: %+v", pool)
	}
}

// newFallbackPoolTestServer applies a two-node `general` Pool whose durable
// selection is already its `direct` fallback, the state V3 inherits when V2's
// watchdog failed the Pool over before an upgrade.
func newFallbackPoolTestServer(t *testing.T, control *fakeProxyControl) (*Server, *state.Store, []provider.NodeView) {
	t.Helper()
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "p1", Name: "Provider 1", Type: provider.TypeManual, Enabled: true}}
	policy := state.TemplatePoolPolicy("general")
	policy.NoCandidate = state.NoCandidateDirect
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", Policy: policy}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "one.example", "server_port": 443},
		{"type": "vmess", "tag": "node-2", "server": "two.example", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build nodes: %v", err)
	}
	spec.BusinessPools[0].NodeIDs = []string{nodes[0].ID, nodes[1].ID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save pool nodes: %v", err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ProxySelection: map[string]string{"general": "Direct"}}); err != nil {
		t.Fatalf("save fallback selection: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)
	return server, store, nodes
}

func appliedWorkerForNode(t *testing.T, store *state.Store, nodeID string) string {
	t.Helper()
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	for _, executor := range applied.ExecutionGraph.NodeExecutors {
		if executor.SmartNode == nodeID {
			return executor.Worker
		}
	}
	t.Fatalf("node %s has no applied executor: %+v", nodeID, applied.ExecutionGraph.NodeExecutors)
	return ""
}

func TestFailoverWatchdogSkipsStaleHealthSamples(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFailoverPoolTestServer(t, control)
	old := time.Now().UTC().Add(-failoverHealthTTL - time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": nodes[0].ID},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &old},
			nodes[1].ID: {Status: "ok", UpdatedAt: &old},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	result, err := NewSchedulerFailoverRunner(server.cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("watchdog stale health run: %v", err)
	}
	if result.Applied != 0 || control.selectSelector != "" {
		t.Fatalf("stale health should not trigger failover: result=%+v control=%+v", result, control)
	}
}

func TestFailoverWatchdogRequiresConsecutiveCurrentFailures(t *testing.T) {
	control := &fakeProxyControl{delayErrors: map[string]error{}}
	server, store, nodes := newFailoverPoolTestServer(t, control)
	control.delayErrors[nodes[0].ID] = fmt.Errorf("probe timeout")
	if err := store.SaveRuntime(state.RuntimeState{ProxySelection: map[string]string{"general": nodes[0].ID}}); err != nil {
		t.Fatal(err)
	}
	runner := NewSchedulerFailoverRunner(server.cfg)
	first, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Applied != 0 || control.selectSelector != "" {
		t.Fatalf("single probe failure should not switch: result=%+v control=%+v", first, control)
	}
	second, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Applied != 1 || control.selectSelector != "Pool_general" || control.selectOutbound != nodes[1].ID {
		t.Fatalf("second consecutive failure should switch: result=%+v control=%+v", second, control)
	}
}

func TestWatchdogUsesAppliedTopologyOnly(t *testing.T) {
	control := &fakeProxyControl{delayErrors: map[string]error{}}
	server, store, appliedNodes := newFailoverPoolTestServer(t, control)
	if err := store.SaveRuntime(state.RuntimeState{ProxySelection: map[string]string{"general": appliedNodes[0].ID}}); err != nil {
		t.Fatal(err)
	}

	saved := state.DefaultSpecState()
	saved.Providers = []state.ProviderSpec{{ID: "p2", Name: "Unapplied", Type: provider.TypeManual, Enabled: true}}
	saved.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general"}}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p2", []map[string]any{{
		"type": "vmess", "tag": "new-node", "server": "new.example", "server_port": 443,
	}}); err != nil {
		t.Fatal(err)
	}
	savedNodes, err := provider.BuildNodeViews(saved, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil || len(savedNodes) != 1 {
		t.Fatalf("build unapplied nodes: nodes=%+v err=%v", savedNodes, err)
	}
	saved.BusinessPools[0].NodeIDs = []string{savedNodes[0].ID}
	if _, err := store.SaveSpec(saved); err != nil {
		t.Fatal(err)
	}

	runner := NewSchedulerFailoverRunner(server.cfg)
	if _, err := runner.refreshWatchdogHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !containsString(control.delayCalls, appliedNodes[0].ID) {
		t.Fatalf("watchdog did not probe applied node: %+v", control.delayCalls)
	}
	if containsString(control.delayCalls, savedNodes[0].ID) {
		t.Fatalf("unapplied node entered watchdog candidates: %+v", control.delayCalls)
	}
}

func TestFailoverWatchdogContinuesDecisionAfterPartialProbeTimeout(t *testing.T) {
	control := &fakeProxyControl{delayErrors: map[string]error{}, delayWait: map[string]time.Duration{}}
	server, store, nodes := newFailoverPoolTestServer(t, control)
	control.delayErrors[nodes[0].ID] = fmt.Errorf("current failed")
	control.delayWait[nodes[1].ID] = 200 * time.Millisecond
	now := time.Now().UTC()
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": nodes[0].ID},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &now, TestURL: defaultNodeDelayURL, ConsecutiveFailures: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	oldBudget := failoverProbeBudget
	failoverProbeBudget = 50 * time.Millisecond
	t.Cleanup(func() { failoverProbeBudget = oldBudget })
	result, err := NewSchedulerFailoverRunner(server.cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("partial probe timeout should be reported as degraded result, not abort: %v", err)
	}
	if result.Decisions == 0 || result.Failed == 0 || !strings.Contains(result.Error, "health probe degraded") {
		t.Fatalf("watchdog did not continue decision after partial timeout: %+v", result)
	}
}

func TestFailoverRevertUsesPreviousTarget(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFailoverPoolTestServer(t, control)
	now := time.Now().UTC()
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": nodes[0].ID},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &now},
			nodes[1].ID: {Status: "ok", UpdatedAt: &now},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	first := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/run", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("failover status = %d body=%s", first.Code, first.Body.String())
	}
	revert := serve(t, server, http.MethodPost, "/api/proxy-groups/failover/revert", mustJSON(t, failoverRevertPayload{PoolID: "general"}))
	if revert.Code != http.StatusOK {
		t.Fatalf("revert status = %d body=%s", revert.Code, revert.Body.String())
	}
	if control.selectSelector != "Pool_general" || control.selectOutbound != nodes[0].ID {
		t.Fatalf("revert should select previous node: %+v", control)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != nodes[0].ID {
		t.Fatalf("revert selection not persisted: %+v", runtimeState.ProxySelection)
	}
	events, err := state.NewFailoverEventStore(server.cfg.Layout).List()
	if err != nil {
		t.Fatalf("list failover events: %v", err)
	}
	if len(events) != 2 || events[1].Reason != "manual_revert" {
		t.Fatalf("revert event not persisted: %+v", events)
	}
}

func newNodesTestServer(t *testing.T) (*Server, *state.Store) {
	return newNodesTestServerWithControl(t, nil)
}

func newFailoverPoolTestServer(t *testing.T, control *fakeProxyControl) (*Server, *state.Store, []provider.NodeView) {
	return newFailoverPoolTestServerWithCutover(t, control, "")
}

func newFailoverPoolTestServerWithCutover(t *testing.T, control *fakeProxyControl, cutover string) (*Server, *state.Store, []provider.NodeView) {
	t.Helper()
	server, store := newNodesTestServerWithControl(t, control)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	policy := state.TemplatePoolPolicy("general")
	if cutover != "" {
		policy.Cutover = cutover
	}
	if policy.Cutover == state.CutoverGraceful {
		policy.GracefulDrainSeconds = 5
	}
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general", Policy: policy}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if _, err := provider.NewLastGoodStore(server.cfg.Layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "one.example", "server_port": 443},
		{"type": "vmess", "tag": "node-2", "server": "two.example", "server_port": 443},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	nodes, err := provider.BuildNodeViews(spec, provider.NewLastGoodStore(server.cfg.Layout))
	if err != nil {
		t.Fatalf("build nodes: %v", err)
	}
	spec.BusinessPools[0].NodeIDs = []string{nodes[0].ID, nodes[1].ID}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save pool nodes: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)
	return server, store, nodes
}

func newUnconfiguredCutoverTestServer(t *testing.T, layout state.Layout, store *state.Store, control *fakeProxyControl) *Server {
	t.Helper()
	locks := lockset.New()
	owners := netgate.NewOwnerRegistry("smartgate")
	commitManager := commitlane.New(locks, layout, owners)
	workers, err := worker.NewManager(
		runtimeTestWorkerAdapter{core: runtimeCoreA, control: control},
		runtimeTestWorkerAdapter{core: runtimeCoreB, control: control},
	)
	if err != nil {
		t.Fatal(err)
	}
	fabricManager := fabric.NewManager(owners, runtimeTestFabricRunner{}).WithConnectionTracker(fabric.NoopConnectionTracker{})
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: locks, CommitLane: commitManager,
		Store: store, Workers: workers, Fabric: fabricManager, ProxyControl: control,
		Status: NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	t.Cleanup(func() {
		_ = fabricManager.Stop(context.Background())
		_ = workers.StopAll(context.Background())
	})
	return server
}

func captureAppliedTopology(t *testing.T, server *Server, store *state.Store, spec state.SpecState) {
	t.Helper()
	layout := server.cfg.Layout
	lastGood, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(layout))
	if err != nil {
		t.Fatalf("load applied last-good: %v", err)
	}
	spec = state.NormalizeSpecState(spec)
	spec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{"direct": {}, "block": {}}, spec.BusinessPools)
	spec, _, err = provider.ReconcileSmartNodes(spec, lastGood, time.Now().UTC())
	if err != nil {
		t.Fatalf("reconcile applied SmartNodes: %v", err)
	}
	for id, node := range spec.SmartNodes {
		if smartnode.ValidCore(node.CoreChoice) {
			continue
		}
		if capability, _ := node.Capability.ForCore(runtimeCoreA); capability.Status != smartnode.CapabilityUnsupportedByCore {
			node.CoreChoice = runtimeCoreA
		} else {
			node.CoreChoice = runtimeCoreB
		}
		spec.SmartNodes[id] = node
	}
	spec.Fabric.DNS.Listen = freeRuntimeDNSListen(t)
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime for applied capture: %v", err)
	}
	if runtimeState.ConfigEpoch <= 0 {
		runtimeState.ConfigEpoch = 1
	}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, lastGood)
	if err != nil {
		t.Fatalf("hash applied topology: %v", err)
	}
	runtimeState.TopologyHash = topologyHash
	selections := map[string]string{}
	poolSelections := map[string]state.PoolSelection{}
	for _, pool := range state.EffectiveBusinessPools(spec.BusinessPools) {
		selected := runtimeState.ProxySelection[pool.ID]
		if selected == "" && len(pool.NodeIDs) > 0 {
			selected = pool.NodeIDs[0]
		}
		if selected == "" {
			switch pool.Policy.NoCandidate {
			case state.NoCandidateDirect:
				selected = "Direct"
			default:
				selected = "Block"
			}
		}
		selections[pool.ID] = selected
		item := state.PoolSelection{Generation: 1, BoundEpoch: runtimeState.ConfigEpoch, BoundTopologyHash: topologyHash}
		switch selected {
		case "Direct":
			item.Target = state.NoCandidateDirect
		case "Block":
			item.Target = state.NoCandidateBlock
		default:
			item.Target = "node"
			item.SmartNode = selected
		}
		poolSelections[pool.ID] = item
	}
	plan := planner.BuildExecutionGraph(spec, poolSelections, state.ExecutionGraph{}, time.Now().UTC())
	if !plan.ReadyToApply {
		t.Fatalf("build applied ExecutionGraph: %+v", plan.Issues)
	}
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: runtimeState.ConfigEpoch, TopologyHash: topologyHash,
		GatewayMode: spec.Settings.GatewayMode, ProxySelection: selections, ExecutionGraph: plan.Graph,
	}, spec, lastGood)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatalf("save applied topology: %v", err)
	}
	if err := store.SaveSelectionState(state.SelectionStateForApplied(applied, selections)); err != nil {
		t.Fatalf("save applied selection: %v", err)
	}
	runtimeState.ProxySelection = selections
	if err := store.SaveRuntime(runtimeState); err != nil {
		t.Fatalf("save applied runtime: %v", err)
	}
	nodes, err := provider.BuildNodeViewsFromSnapshots(spec, lastGood)
	if err != nil {
		t.Fatalf("build protected outbound views: %v", err)
	}
	identities := map[string]smartnode.IdentitySource{}
	for _, node := range nodes {
		if !node.Excluded && node.ID != "" {
			identities[node.ID] = smartnode.IdentitySource{RawTag: node.Tag, Identity: node.Identity, References: node.References}
		}
	}
	if server.cfg.Workers != nil && server.cfg.Fabric != nil {
		preparedWorkers, err := server.cfg.Workers.Prepare(context.Background(), fmt.Sprintf("runtime-test-workers-%d", time.Now().UnixNano()), plan.Graph, identities)
		if err != nil {
			t.Fatalf("prepare applied workers: %v", err)
		}
		if _, err := server.cfg.Workers.Apply(context.Background(), preparedWorkers); err != nil {
			t.Fatalf("apply workers: %v", err)
		}
		preparedFabric, err := server.cfg.Fabric.Prepare(context.Background(), fmt.Sprintf("runtime-test-fabric-%d", time.Now().UnixNano()), spec, plan.Graph.Fabric)
		if err != nil {
			t.Fatalf("prepare applied Fabric: %v", err)
		}
		if _, err := server.cfg.Fabric.Apply(context.Background(), preparedFabric); err != nil {
			t.Fatalf("apply Fabric: %v", err)
		}
	}
	if fake, ok := server.cfg.ProxyControl.(*fakeProxyControl); ok {
		fake.cutoverReady = true
	}
}

func newNodesTestServerWithControl(t *testing.T, control selection.ProxyController) (*Server, *state.Store) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	commitManager := commitlane.New(locks, layout, owners)
	var fakeControl *fakeProxyControl
	if fake, ok := control.(*fakeProxyControl); ok {
		fakeControl = fake
	}
	workers, err := worker.NewManager(
		runtimeTestWorkerAdapter{core: runtimeCoreA, control: fakeControl},
		runtimeTestWorkerAdapter{core: runtimeCoreB, control: fakeControl},
	)
	if err != nil {
		t.Fatalf("new worker manager: %v", err)
	}
	fabricManager := fabric.NewManager(owners, runtimeTestFabricRunner{}).WithConnectionTracker(fabric.NoopConnectionTracker{})
	if fake, ok := control.(*fakeProxyControl); ok {
		attachFakeSelectorLookup(fake, store)
	}
	server := NewServer(Config{
		ListenAddr:   "127.0.0.1:3000",
		Layout:       layout,
		Locks:        locks,
		CommitLane:   commitManager,
		Store:        store,
		Workers:      workers,
		Fabric:       fabricManager,
		CoreRegistry: newRuntimeTestCoreRegistry(t),
		ProxyControl: control,
		Status:       NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	t.Cleanup(func() {
		_ = fabricManager.Stop(context.Background())
		_ = workers.StopAll(context.Background())
	})
	return server, store
}

func freeRuntimeDNSListen(t *testing.T) string {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := packet.LocalAddr().String()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		_ = packet.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		_ = packet.Close()
		t.Fatal(err)
	}
	if err := packet.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func cutoverPayloadForPool(t *testing.T, store *state.Store, poolID, target, opID string) proxySelectPayload {
	t.Helper()
	topology, err := store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := rebuildLiveExecutionGraph(topology, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range graph.PoolBindings {
		if binding.Pool == poolID {
			return proxySelectPayload{
				TargetSmartNode: target, Source: cutoverSourceForBinding(binding),
				ExpectedConfigEpoch: topology.ConfigEpoch, OpID: opID,
			}
		}
	}
	t.Fatalf("pool %s has no Binding", poolID)
	return proxySelectPayload{}
}

func attachFakeSelectorLookup(fake *fakeProxyControl, store *state.Store) {
	fake.selectedLookup = func(selector string) (string, error) {
		runtimeState, err := store.LoadRuntime()
		if err != nil {
			return "", err
		}
		poolID := strings.TrimPrefix(selector, "Pool_")
		if selector == "Proxy" {
			poolID = "proxy"
		}
		selected := runtimeState.ProxySelection[poolID]
		if selected == "" {
			return "", fmt.Errorf("selector %s has no current target", selector)
		}
		return selected, nil
	}
}

func TestProxyMutationUsesSameStoreRuntimeNetOrderAsConfigTxn(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	locks := lockset.New()
	configSession := locks.Session("config-order-test")
	releaseStore, err := configSession.Acquire(lockset.LevelStore)
	if err != nil {
		t.Fatal(err)
	}
	releaseRuntime, err := configSession.Acquire(lockset.LevelRuntime)
	if err != nil {
		releaseStore()
		t.Fatal(err)
	}
	started := make(chan struct{})
	proxyDone := make(chan error, 1)
	go func() {
		close(started)
		release, err := lockProxyMutation(locks, layout, "proxy-order-test")
		if err == nil {
			release()
		}
		proxyDone <- err
	}()
	<-started
	netDone := make(chan error, 1)
	var releaseNet func()
	go func() {
		var err error
		releaseNet, err = configSession.Acquire(lockset.LevelNet)
		netDone <- err
	}()
	select {
	case err := <-netDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy mutation held Net while waiting for Store (ABBA deadlock)")
	}
	releaseNet()
	releaseRuntime()
	releaseStore()
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy mutation did not resume after config locks released")
	}
}

func TestFailoverMetadataMutationsWaitForConfigStoreLock(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		body  []byte
		check func(t *testing.T, got state.FailoverState)
	}{
		{
			name: "enable", path: "/api/proxy-groups/failover/enable", body: mustJSON(t, failoverEnabledPayload{Enabled: boolPtr(false)}),
			check: func(t *testing.T, got state.FailoverState) {
				if got.Enabled {
					t.Fatal("enable mutation was not persisted")
				}
			},
		},
		{
			name: "lock", path: "/api/proxy-groups/failover/lock", body: mustJSON(t, failoverLockPayload{PoolID: "general", Reason: "maintenance"}),
			check: func(t *testing.T, got state.FailoverState) {
				if got.Locks["general"].Reason != "maintenance" {
					t.Fatalf("lock mutation was not persisted: %+v", got.Locks)
				}
			},
		},
		{
			name: "unlock", path: "/api/proxy-groups/failover/unlock", body: mustJSON(t, failoverLockPayload{PoolID: "general"}),
			check: func(t *testing.T, got state.FailoverState) {
				if _, ok := got.Locks["general"]; ok {
					t.Fatalf("unlock mutation was not persisted: %+v", got.Locks)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _ := newNodesTestServerWithControl(t, nil)
			failoverStore := state.NewFailoverStateStore(server.cfg.Layout)
			initial, err := failoverStore.Load()
			if err != nil {
				t.Fatal(err)
			}
			initial.Enabled = true
			initial.Locks["general"] = state.FailoverLock{Reason: "existing", LockedAt: time.Now().UTC()}
			if err := failoverStore.Save(initial); err != nil {
				t.Fatal(err)
			}

			configSession := server.cfg.Locks.Session("config-transaction-holds-store")
			releaseStore, err := configSession.Acquire(lockset.LevelStore)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- serve(t, server, http.MethodPost, tt.path, tt.body)
			}()
			select {
			case resp := <-done:
				releaseStore()
				t.Fatalf("mutation bypassed config Store lock: status=%d body=%s", resp.Code, resp.Body.String())
			case <-time.After(50 * time.Millisecond):
			}
			releaseStore()
			select {
			case resp := <-done:
				if resp.Code != http.StatusOK {
					t.Fatalf("mutation status=%d body=%s", resp.Code, resp.Body.String())
				}
			case <-time.After(time.Second):
				t.Fatal("mutation did not resume after config Store lock released")
			}
			got, err := failoverStore.Load()
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, got)
		})
	}
}

func boolPtr(value bool) *bool { return &value }

func findGroupMap(t *testing.T, groups []any, id string) map[string]any {
	t.Helper()
	for _, raw := range groups {
		group := raw.(map[string]any)
		if group["pool"] == id {
			return group
		}
	}
	t.Fatalf("missing group %s in %+v", id, groups)
	return nil
}

func findBindingMap(t *testing.T, bindings []any, pool string) map[string]any {
	t.Helper()
	for _, raw := range bindings {
		binding := raw.(map[string]any)
		if binding["pool"] == pool {
			return binding
		}
	}
	t.Fatalf("missing Binding for pool %s in %+v", pool, bindings)
	return nil
}

func findDrainMap(t *testing.T, drains []any, pool string) map[string]any {
	t.Helper()
	for _, raw := range drains {
		drain := raw.(map[string]any)
		if drain["pool"] == pool {
			return drain
		}
	}
	t.Fatalf("missing drain for pool %s in %+v", pool, drains)
	return nil
}

func decodeEnvelopeError(t *testing.T, body []byte) (string, map[string]any) {
	t.Helper()
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, string(body))
	}
	if env.OK {
		t.Fatalf("expected error envelope: %+v", env)
	}
	payload, ok := env.Error.(map[string]any)
	if !ok {
		t.Fatalf("error payload is %T: %+v", env.Error, env.Error)
	}
	details, _ := payload["details"].(map[string]any)
	return payload["code"].(string), details
}

func containsAnyString(items []any, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
		if object, ok := item.(map[string]any); ok && object["smart_node"] == want {
			return true
		}
	}
	return false
}

func TestFailoverWatchdogAlternateProbeKeepsCurrent(t *testing.T) {
	control := &fakeProxyControl{delayErrorsByURL: map[string]error{}}
	server, store, nodes := newFailoverPoolTestServer(t, control)
	control.delayErrorsByURL[nodes[0].ID+"|"+defaultNodeDelayURL] = fmt.Errorf("primary timed out")
	if err := store.SaveRuntime(state.RuntimeState{ProxySelection: map[string]string{"general": nodes[0].ID}}); err != nil {
		t.Fatal(err)
	}
	result, err := NewSchedulerFailoverRunner(server.cfg).Run(context.Background())
	if err != nil || result.Applied != 0 || control.selectSelector != "" {
		t.Fatalf("alternate success should preserve current node: result=%+v err=%v", result, err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	health := runtimeState.NodeHealth[nodes[0].ID]
	if health.WatchdogStatus != "ok" || health.ConsecutiveFailures != 0 || health.TestURL != failoverConfirmURL {
		t.Fatalf("alternate probe did not confirm current node: %+v", health)
	}
}

func TestFailoverWatchdogKeepsCurrentWithoutHealthyReplacement(t *testing.T) {
	control := &fakeProxyControl{delayErrors: map[string]error{}}
	server, store, nodes := newFailoverPoolTestServer(t, control)
	for _, node := range nodes {
		control.delayErrors[node.ID] = fmt.Errorf("probe timeout")
	}
	now := time.Now().UTC()
	if err := store.SaveRuntime(state.RuntimeState{
		ProxySelection: map[string]string{"general": nodes[0].ID},
		NodeHealth: map[string]state.NodeHealth{nodes[0].ID: {
			Status: "failed", WatchdogStatus: "failed", UpdatedAt: &now, WatchdogUpdatedAt: &now, ConsecutiveFailures: 1,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := NewSchedulerFailoverRunner(server.cfg).Run(context.Background())
	if err != nil || result.Applied != 0 || control.selectSelector != "" {
		t.Fatalf("no healthy replacement should preserve current node: result=%+v err=%v", result, err)
	}
}
