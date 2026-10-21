package runtime

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"smartgate/internal/fabric"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type runtimeConnectionTracker struct {
	mu           sync.Mutex
	connections  map[string][]fabric.TrackedConnection
	closedScopes []string
	closedOneIDs []string
}

func newRuntimeConnectionTracker() *runtimeConnectionTracker {
	return &runtimeConnectionTracker{connections: map[string][]fabric.TrackedConnection{}}
}

func (tracker *runtimeConnectionTracker) Count(_ context.Context, dispatch state.PoolDispatchGraph) (int, error) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return len(tracker.connections[connectionDispatchKey(dispatch)]), nil
}

func (tracker *runtimeConnectionTracker) Close(_ context.Context, dispatch state.PoolDispatchGraph) (int, error) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	key := connectionDispatchKey(dispatch)
	closed := len(tracker.connections[key])
	tracker.connections[key] = nil
	tracker.closedScopes = append(tracker.closedScopes, key)
	return closed, nil
}

func (tracker *runtimeConnectionTracker) List(_ context.Context, dispatch state.PoolDispatchGraph) ([]fabric.TrackedConnection, error) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return append([]fabric.TrackedConnection(nil), tracker.connections[connectionDispatchKey(dispatch)]...), nil
}

func (tracker *runtimeConnectionTracker) CloseOne(_ context.Context, dispatch state.PoolDispatchGraph, connection fabric.TrackedConnection) (int, error) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	key := connectionDispatchKey(dispatch)
	items := tracker.connections[key]
	for index, item := range items {
		if item.ID != connection.ID {
			continue
		}
		tracker.connections[key] = append(items[:index], items[index+1:]...)
		tracker.closedOneIDs = append(tracker.closedOneIDs, connection.ID)
		return 1, nil
	}
	return 0, nil
}

func (tracker *runtimeConnectionTracker) set(dispatch state.PoolDispatchGraph, connections ...fabric.TrackedConnection) {
	tracker.mu.Lock()
	tracker.connections[connectionDispatchKey(dispatch)] = append([]fabric.TrackedConnection(nil), connections...)
	tracker.mu.Unlock()
}

func connectionDispatchKey(dispatch state.PoolDispatchGraph) string {
	return fmt.Sprintf("%s:%d", dispatch.Pool, dispatch.Generation)
}

func TestConnectionsHandlerRequiresAppliedObservation(t *testing.T) {
	server, _ := newNodesTestServer(t)
	resp := serve(t, server, http.MethodGet, "/api/connections", nil)
	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("connections without applied observation status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestConnectionsHandlerUsesEmptyChainsForKernelOnlyFlows(t *testing.T) {
	server, store, _ := newFailoverPoolTestServerWithCutover(t, &fakeProxyControl{}, state.CutoverGraceful)
	tracker := newRuntimeConnectionTracker()
	server.cfg.Fabric.WithConnectionTracker(tracker)
	scope := currentConnectionScope(t, server, store, "general")
	tracker.set(scope.dispatch, fabric.TrackedConnection{
		ID: "kernel-only", Protocol: "udp", SourceIP: "192.0.2.10", DestinationIP: "198.51.100.20",
		SourcePort: "43210", DestinationPort: "443",
	})
	resp := serve(t, server, http.MethodGet, "/api/connections", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("connections status = %d", resp.Code)
	}
	items := decodeEnvelopeData(t, resp.Body.Bytes())["connections"].([]any)
	if len(items) != 1 {
		t.Fatalf("connections = %+v", items)
	}
	chains, ok := items[0].(map[string]any)["chains"].([]any)
	if !ok || len(chains) != 0 {
		t.Fatalf("kernel-only chains must be [], got %+v", items[0])
	}
}

func TestConnectionsHandlerDoesNotFallbackToGlobalProxyControl(t *testing.T) {
	control := &fakeProxyControl{
		connectionsPayload: map[string]any{
			"downloadTotal": 1024,
			"uploadTotal":   512,
			"connections": []any{
				map[string]any{"id": "conn-1", "metadata": map[string]any{"host": "example.com"}},
			},
		},
	}
	server, _ := newNodesTestServerWithControl(t, control)

	resp := serve(t, server, http.MethodGet, "/api/connections", nil)
	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("connections without applied topology status = %d body=%s", resp.Code, resp.Body.String())
	}
	if control.connectionsCalled {
		t.Fatalf("connections should not call global proxy control")
	}

	closeResp := serve(t, server, http.MethodPost, "/api/connections/close", mustJSON(t, connectionClosePayload{}))
	if closeResp.Code != http.StatusBadRequest {
		t.Fatalf("close without confirm status = %d body=%s", closeResp.Code, closeResp.Body.String())
	}
	if control.closeConnectionsCalled {
		t.Fatalf("close should not be called without confirmation")
	}

	closeResp = serve(t, server, http.MethodPost, "/api/connections/close", mustJSON(t, connectionClosePayload{Confirm: true}))
	if closeResp.Code != http.StatusNotImplemented {
		t.Fatalf("close status = %d body=%s", closeResp.Code, closeResp.Body.String())
	}
	if control.closeConnectionsCalled {
		t.Fatalf("close should not call global proxy control")
	}
}

func TestConnectionsHandlerUsesLiveAppliedGenerationFiltersAndMergesTuple(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, _ := newFailoverPoolTestServerWithCutover(t, control, state.CutoverGraceful)
	tracker := newRuntimeConnectionTracker()
	server.cfg.Fabric.WithConnectionTracker(tracker)
	scope := currentConnectionScope(t, server, store, "general")
	if scope.path == nil {
		t.Fatal("general Binding path is missing")
	}
	tracked := fabric.TrackedConnection{
		ID: "kernel-one", Protocol: "tcp", State: "ESTABLISHED",
		SourceIP: "192.0.2.10", DestinationIP: "198.51.100.20", SourcePort: "43210", DestinationPort: "443",
		ReplySourceIP: "198.51.100.20", ReplyDestIP: "192.0.2.10", ReplySourcePort: "443", ReplyDestPort: "43210",
		Upload: 120, Download: 900,
	}
	tracker.set(scope.dispatch, tracked)
	control.workerConnections = []worker.Connection{{
		ID: "worker-one",
		Metadata: map[string]any{
			"network": "tcp", "sourceIP": "192.0.2.10", "destinationIP": "198.51.100.20",
			"sourcePort": "43210", "destinationPort": "443", "host": "example.com",
		},
		Upload: 120, Download: 900, Chains: []string{scope.path.Route.Selector},
	}}

	resp := serve(t, server, http.MethodGet, "/api/connections?pool=general&worker="+scope.path.Worker.ID, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("connections status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if control.connectionsCalled {
		t.Fatalf("connections should not call global proxy control")
	}
	if got := data["download_total"].(float64); got != 900 {
		t.Fatalf("download_total = %v", got)
	}
	if got := data["upload_total"].(float64); got != 120 {
		t.Fatalf("upload_total = %v", got)
	}
	items := data["connections"].([]any)
	if len(items) != 1 {
		t.Fatalf("worker and conntrack tuple were not merged: %+v", data)
	}
	item := items[0].(map[string]any)
	if item["id"] != "worker-one" || item["pool"] != "general" || item["generation"] != float64(scope.generation) {
		t.Fatalf("live Binding metadata mismatch: %+v", item)
	}
	if item["worker"] != scope.path.Worker.ID || item["smart_node"] != scope.path.Executor.SmartNode || item["core"] != string(scope.path.Worker.Core) {
		t.Fatalf("worker metadata mismatch: %+v", item)
	}
	if item["stale_generation"] != false || item["metadata"].(map[string]any)["host"] != "example.com" {
		t.Fatalf("merged connection mismatch: %+v", item)
	}

	filtered := serve(t, server, http.MethodGet, "/api/connections?pool=missing", nil)
	if filtered.Code != http.StatusOK || len(decodeEnvelopeData(t, filtered.Body.Bytes())["connections"].([]any)) != 0 {
		t.Fatalf("pool filter mismatch: %d %s", filtered.Code, filtered.Body.String())
	}
}

func TestMergeObservedConnectionsKeepsCurrentAndStaleGenerationsDistinct(t *testing.T) {
	tuple := map[string]any{
		"network": "tcp", "sourceIP": "192.0.2.10", "destinationIP": "198.51.100.20",
		"sourcePort": "43210", "destinationPort": "443",
	}
	tracked := []observedConnection{
		{view: connectionView{ID: "old", Pool: "general", Generation: 1, StaleGeneration: true, Metadata: tuple}},
		{view: connectionView{ID: "current-kernel", Pool: "general", Generation: 2, Metadata: tuple}},
	}
	worker := []observedConnection{{
		view:               connectionView{ID: "current-worker", Pool: "general", Generation: 2, Metadata: tuple},
		workerConnectionID: "current-worker",
	}}
	merged := mergeObservedConnections(worker, tracked)
	if len(merged) != 2 {
		t.Fatalf("merged = %+v", merged)
	}
	if merged[0].view.ID != "old" || merged[1].view.ID != "current-worker" || merged[1].workerConnectionID != "current-worker" {
		t.Fatalf("generation-aware merge mismatch: %+v", merged)
	}
}

func TestConnectionsHandlerExposesGracefulDrainAsStaleGeneration(t *testing.T) {
	control := &fakeProxyControl{}
	server, store, nodes := newFailoverPoolTestServerWithCutover(t, control, state.CutoverGraceful)
	tracker := newRuntimeConnectionTracker()
	server.cfg.Fabric.WithConnectionTracker(tracker)
	oldScope := currentConnectionScope(t, server, store, "general")
	tracker.set(oldScope.dispatch, testTrackedConnection("old-flow", "192.0.2.11", "443"))
	control.setRouteConnections(1)
	payload := cutoverPayloadForPool(t, store, "general", nodes[1].ID, "connections-graceful-drain")
	resp := serve(t, server, http.MethodPost, "/api/proxy-groups/general/select", mustJSON(t, payload))
	if resp.Code != http.StatusOK {
		t.Fatalf("graceful cutover status=%d body=%s", resp.Code, resp.Body.String())
	}
	currentScope := currentConnectionScope(t, server, store, "general")
	if currentScope.generation != oldScope.generation+1 {
		t.Fatalf("generation did not advance: old=%d current=%d", oldScope.generation, currentScope.generation)
	}
	tracker.set(currentScope.dispatch, testTrackedConnection("current-flow", "192.0.2.12", "8443"))

	connectionsResp := serve(t, server, http.MethodGet, "/api/connections?pool=general", nil)
	if connectionsResp.Code != http.StatusOK {
		t.Fatalf("connections status=%d body=%s", connectionsResp.Code, connectionsResp.Body.String())
	}
	items := decodeEnvelopeData(t, connectionsResp.Body.Bytes())["connections"].([]any)
	if len(items) != 2 {
		t.Fatalf("current and draining generations not both visible: %+v", items)
	}
	seen := map[uint64]bool{}
	for _, raw := range items {
		item := raw.(map[string]any)
		generation := uint64(item["generation"].(float64))
		seen[generation] = item["stale_generation"].(bool)
	}
	if seen[oldScope.generation] != true || seen[currentScope.generation] != false {
		t.Fatalf("stale generation flags mismatch: %+v", seen)
	}

	control.setRouteConnections(0)
	tracker.set(oldScope.dispatch)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, drains := server.cfg.SelectorTxn.Progress()
		if len(drains) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("graceful drain did not finish")
}

func TestConnectionsHandlerClosesSingleBindingAndAllScopes(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		control := &fakeProxyControl{}
		server, store, _ := newFailoverPoolTestServerWithCutover(t, control, state.CutoverStrict)
		tracker := newRuntimeConnectionTracker()
		server.cfg.Fabric.WithConnectionTracker(tracker)
		scope := currentConnectionScope(t, server, store, "general")
		tracked := testTrackedConnection("single-kernel", "192.0.2.21", "443")
		tracker.set(scope.dispatch, tracked)
		control.workerConnections = workerConnectionRecords(t, scope, "single-worker", tracked)

		resp := serve(t, server, http.MethodPost, "/api/connections/close", mustJSON(t, connectionClosePayload{Scope: "single", ID: "single-worker"}))
		if resp.Code != http.StatusAccepted {
			t.Fatalf("single close status=%d body=%s", resp.Code, resp.Body.String())
		}
		data := decodeEnvelopeData(t, resp.Body.Bytes())
		if data["closed"] != float64(1) || data["closed_exact"] != true || len(control.closedConnectionIDs) != 1 || len(tracker.closedOneIDs) != 1 {
			t.Fatalf("single close mismatch: data=%+v control=%+v tracker=%+v", data, control.closedConnectionIDs, tracker.closedOneIDs)
		}
	})

	t.Run("binding", func(t *testing.T) {
		control := &fakeProxyControl{}
		server, store, _ := newFailoverPoolTestServerWithCutover(t, control, state.CutoverStrict)
		tracker := newRuntimeConnectionTracker()
		server.cfg.Fabric.WithConnectionTracker(tracker)
		scope := currentConnectionScope(t, server, store, "general")
		tracked := testTrackedConnection("binding-kernel", "192.0.2.22", "443")
		tracker.set(scope.dispatch, tracked)
		control.workerConnections = workerConnectionRecords(t, scope, "binding-worker", tracked)

		unconfirmed := serve(t, server, http.MethodPost, "/api/connections/close", mustJSON(t, connectionClosePayload{Scope: "binding", Pool: "general"}))
		if unconfirmed.Code != http.StatusBadRequest {
			t.Fatalf("unconfirmed binding close=%d body=%s", unconfirmed.Code, unconfirmed.Body.String())
		}
		resp := serve(t, server, http.MethodPost, "/api/connections/close", mustJSON(t, connectionClosePayload{Scope: "binding", Pool: "general", Confirm: true}))
		if resp.Code != http.StatusAccepted {
			t.Fatalf("binding close status=%d body=%s", resp.Code, resp.Body.String())
		}
		data := decodeEnvelopeData(t, resp.Body.Bytes())
		if data["closed"] != float64(1) || data["closed_exact"] != true || len(tracker.closedScopes) != 1 {
			t.Fatalf("binding close mismatch: data=%+v tracker=%+v", data, tracker.closedScopes)
		}
		missing := serve(t, server, http.MethodPost, "/api/connections/close", mustJSON(t, connectionClosePayload{Scope: "binding", Pool: "missing", Confirm: true}))
		if missing.Code != http.StatusNotFound {
			t.Fatalf("unknown binding close=%d body=%s", missing.Code, missing.Body.String())
		}
	})

	t.Run("all", func(t *testing.T) {
		control := &fakeProxyControl{}
		server, store, _ := newFailoverPoolTestServerWithCutover(t, control, state.CutoverStrict)
		tracker := newRuntimeConnectionTracker()
		server.cfg.Fabric.WithConnectionTracker(tracker)
		scope := currentConnectionScope(t, server, store, "general")
		tracker.set(scope.dispatch,
			testTrackedConnection("all-one", "192.0.2.31", "443"),
			testTrackedConnection("all-two", "192.0.2.32", "8443"),
		)
		resp := serve(t, server, http.MethodPost, "/api/connections/close", mustJSON(t, connectionClosePayload{Scope: "all", Confirm: true}))
		if resp.Code != http.StatusAccepted {
			t.Fatalf("all close status=%d body=%s", resp.Code, resp.Body.String())
		}
		data := decodeEnvelopeData(t, resp.Body.Bytes())
		if data["closed"] != float64(2) || data["closed_exact"] != true {
			t.Fatalf("all close mismatch: %+v", data)
		}
	})
}

func TestConnectionsHandlerRejectsUnknownSingleID(t *testing.T) {
	control := &fakeProxyControl{}
	server, _, _ := newFailoverPoolTestServerWithCutover(t, control, state.CutoverStrict)
	resp := serve(t, server, http.MethodPost, "/api/connections/close", mustJSON(t, connectionClosePayload{Scope: "single", ID: "missing"}))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("unknown single close=%d body=%s", resp.Code, resp.Body.String())
	}
}

func currentConnectionScope(t *testing.T, server *Server, store *state.Store, pool string) connectionScope {
	t.Helper()
	topology, err := store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	api := connectionsAPI{store: store, workers: server.cfg.Workers, fabric: server.cfg.Fabric, selector: server.cfg.SelectorTxn}
	scopes, err := api.scopes(topology, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range scopes {
		if scope.pool == pool && !scope.stale {
			return scope
		}
	}
	t.Fatalf("current connection scope for %s not found", pool)
	return connectionScope{}
}

func testTrackedConnection(id, sourceIP, destinationPort string) fabric.TrackedConnection {
	return fabric.TrackedConnection{
		ID: id, Protocol: "tcp", State: "ESTABLISHED", SourceIP: sourceIP, DestinationIP: "198.51.100.20",
		SourcePort: "43210", DestinationPort: destinationPort, ReplySourceIP: "198.51.100.20", ReplyDestIP: sourceIP,
		ReplySourcePort: destinationPort, ReplyDestPort: "43210", Upload: 120, Download: 900,
	}
}

func workerConnectionRecords(t *testing.T, scope connectionScope, id string, tracked fabric.TrackedConnection) []worker.Connection {
	t.Helper()
	if scope.path == nil {
		t.Fatal("Binding path is missing")
	}
	return []worker.Connection{{
		ID: id,
		Metadata: map[string]any{
			"network": tracked.Protocol, "sourceIP": tracked.SourceIP, "destinationIP": tracked.DestinationIP,
			"sourcePort": tracked.SourcePort, "destinationPort": tracked.DestinationPort,
		},
		Upload: float64(tracked.Upload), Download: float64(tracked.Download), Chains: []string{scope.path.Route.Selector},
	}}
}
