package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

type fakeAdapter struct {
	core      smartnode.CoreID
	workerID  string
	version   string
	mu        sync.Mutex
	prepared  []string
	installed []string
	activated []string
	stopped   []string
	probeErr  error
	probeHook func()
}

func (adapter *fakeAdapter) Core() smartnode.CoreID { return adapter.core }

func (adapter *fakeAdapter) Descriptor() Descriptor {
	return Descriptor{ID: adapter.core, Version: adapter.version, Naming: DescriptorNaming{LegacyWorkerAlias: adapter.workerID}}
}

func (adapter *fakeAdapter) Prepare(_ context.Context, opID string, request CompileRequest) (Candidate, error) {
	adapter.mu.Lock()
	adapter.prepared = append(adapter.prepared, request.Worker.ID)
	adapter.mu.Unlock()
	return Candidate{OpID: opID, WorkerID: request.Worker.ID, Core: adapter.core, Path: "/tmp/" + request.Worker.ID, CheckOK: true}, nil
}

func (adapter *fakeAdapter) Verify(candidate Candidate) error {
	if !candidate.CheckOK {
		return errors.New("not checked")
	}
	return nil
}

func (adapter *fakeAdapter) Activate(_ context.Context, candidate Candidate) (Activation, error) {
	adapter.mu.Lock()
	adapter.activated = append(adapter.activated, candidate.WorkerID)
	adapter.mu.Unlock()
	return Activation{WorkerID: candidate.WorkerID, Restarted: true}, nil
}

func (adapter *fakeAdapter) Install(_ context.Context, candidate Candidate) (Activation, error) {
	adapter.mu.Lock()
	adapter.installed = append(adapter.installed, candidate.WorkerID)
	adapter.mu.Unlock()
	return Activation{WorkerID: candidate.WorkerID}, nil
}

func (adapter *fakeAdapter) Start(_ context.Context, target state.WorkerTarget) (Activation, error) {
	adapter.mu.Lock()
	adapter.activated = append(adapter.activated, target.ID)
	adapter.mu.Unlock()
	return Activation{WorkerID: target.ID, Restarted: true}, nil
}

func (adapter *fakeAdapter) Backup(_ string, workerID string) (Backup, error) {
	return Backup{WorkerID: workerID, Core: adapter.core, Existed: true}, nil
}

func (adapter *fakeAdapter) Restore(_ context.Context, backup Backup) (Activation, error) {
	return Activation{WorkerID: backup.WorkerID, Restarted: true}, nil
}

func (adapter *fakeAdapter) Stop(_ context.Context, workerID string) error {
	adapter.mu.Lock()
	adapter.stopped = append(adapter.stopped, workerID)
	adapter.mu.Unlock()
	return nil
}

func (adapter *fakeAdapter) Probe(context.Context, string) error {
	adapter.mu.Lock()
	err, hook := adapter.probeErr, adapter.probeHook
	adapter.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func TestManagerRestartsFailedWorkerWithoutChangingConfiguredBindings(t *testing.T) {
	primary, secondary := managerTestAdapters()
	manager, _ := NewManager(primary, secondary)
	graph, identities := managerTestGraph(t, true)
	prepared, err := manager.Prepare(context.Background(), "restart-failed", graph, identities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	primary.probeErr = errors.New("exited")
	manager.probe(context.Background())
	before := manager.configured
	if restarted, err := manager.RestartFailed(context.Background(), primary.workerID); err != nil || !restarted {
		t.Fatalf("restart=%v err=%v", restarted, err)
	}
	if !manager.IsReady(primary.workerID) || len(primary.activated) != 2 || len(primary.stopped) != 1 || len(secondary.activated) != 1 {
		t.Fatalf("restart crossed worker boundary: primary=%+v secondary=%+v", primary, secondary)
	}
	if len(manager.configured.Bindings) != len(before.Bindings) {
		t.Fatal("restart changed bindings")
	}
	for i := range before.Bindings {
		if manager.configured.Bindings[i].Generation != before.Bindings[i].Generation {
			t.Fatal("restart changed generation")
		}
	}
	if restarted, err := manager.RestartFailed(context.Background(), primary.workerID); err != nil || restarted {
		t.Fatalf("running process restarted again: %v %v", restarted, err)
	}
}

func TestManagerDiscardsProbeFailureAfterLifecycleChange(t *testing.T) {
	primary, secondary := managerTestAdapters()
	manager, _ := NewManager(primary, secondary)
	graph, identities := managerTestGraph(t, false)
	prepared, err := manager.Prepare(context.Background(), "stale-probe", graph, identities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	primary.probeErr = errors.New("old process exited")
	primary.probeHook = func() { manager.markStopped(graph.Workers[0]) }
	manager.probe(context.Background())
	if status, _ := manager.Status(primary.workerID); status.State != StateStopped {
		t.Fatalf("stale probe changed stopped state: %+v", status)
	}
}

func (adapter *fakeAdapter) RemoveCandidate(Candidate) error { return nil }

func (adapter *fakeAdapter) SelectRoute(context.Context, BindingPath) error { return nil }
func (adapter *fakeAdapter) VerifyRoute(context.Context, BindingPath) error { return nil }
func (adapter *fakeAdapter) DelayRoute(_ context.Context, _ BindingPath, testURL string, _ time.Duration) (DelayResult, error) {
	return DelayResult{DelayMS: 77, URL: testURL}, nil
}
func (adapter *fakeAdapter) CountRouteConnections(context.Context, BindingPath) (int, error) {
	return 0, nil
}
func (adapter *fakeAdapter) ListRouteConnections(context.Context, BindingPath) ([]Connection, error) {
	return nil, nil
}
func (adapter *fakeAdapter) CloseRouteConnection(context.Context, BindingPath, string) error {
	return nil
}
func (adapter *fakeAdapter) CloseRouteConnections(context.Context, BindingPath) (int, error) {
	return 0, nil
}

func managerTestAdapters() (*fakeAdapter, *fakeAdapter) {
	return &fakeAdapter{core: smartnode.CoreID("core-a"), workerID: "worker-a"}, &fakeAdapter{core: smartnode.CoreID("core-b"), workerID: "worker-b"}
}

func TestManagerInitialStoppedStatusIncludesDescriptorVersion(t *testing.T) {
	adapter := &fakeAdapter{core: smartnode.CoreID("core-a"), workerID: "worker-a", version: "1.2.3"}
	manager, err := NewManager(adapter)
	if err != nil {
		t.Fatal(err)
	}
	status, ok := manager.Status(adapter.workerID)
	if !ok || status.State != StateStopped || status.Version != adapter.version {
		t.Fatalf("initial worker status = %+v, exists=%v", status, ok)
	}
}

func TestManagerStartsOnlyBoundWorkers(t *testing.T) {
	primary, secondary := managerTestAdapters()
	manager, err := NewManager(primary, secondary)
	if err != nil {
		t.Fatal(err)
	}
	graph, identities := managerTestGraph(t, false)
	prepared, err := manager.Prepare(context.Background(), "workers-on-demand", graph, identities)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Candidates) != 2 || prepared.Candidates[primary.workerID].WorkerID != primary.workerID || prepared.Candidates[secondary.workerID].WorkerID != secondary.workerID {
		t.Fatalf("unexpected candidates: %+v", prepared.Candidates)
	}
	if _, err := manager.Apply(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if len(primary.activated) != 1 || len(secondary.activated) != 0 || len(secondary.installed) != 1 {
		t.Fatalf("on-demand activation failed: primary=%v secondary=%v installed=%v", primary.activated, secondary.activated, secondary.installed)
	}
}

func TestManagerKeepsWorkerFailurePoolLocal(t *testing.T) {
	primary, secondary := managerTestAdapters()
	manager, err := NewManager(primary, secondary)
	if err != nil {
		t.Fatal(err)
	}
	manager.probeInterval = 10 * time.Millisecond
	failures := make(chan Failure, 1)
	manager.WithFailureHandler(func(_ context.Context, failure Failure) { failures <- failure })
	graph, identities := managerTestGraph(t, true)
	prepared, err := manager.Prepare(context.Background(), "dual-workers", graph, identities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	secondary.mu.Lock()
	secondary.probeErr = errors.New("secondary worker exited")
	secondary.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	stop := manager.StartSupervisor(ctx)
	defer func() { stop(); cancel() }()
	select {
	case failure := <-failures:
		if failure.WorkerID != secondary.workerID || len(failure.Pools) != 1 || failure.Pools[0] != "ai" {
			t.Fatalf("failure escaped worker boundary: %+v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for isolated worker failure")
	}
	statuses := manager.Statuses()
	if statusByID(statuses, primary.workerID).State != StateRunning || statusByID(statuses, secondary.workerID).State != StateFailed {
		t.Fatalf("unexpected states after secondary failure: %+v", statuses)
	}
}

func TestManagerStopAllStopsBothWorkers(t *testing.T) {
	primary, secondary := managerTestAdapters()
	manager, err := NewManager(primary, secondary)
	if err != nil {
		t.Fatal(err)
	}
	graph, identities := managerTestGraph(t, true)
	prepared, err := manager.Prepare(context.Background(), "rescue-workers", graph, identities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := manager.StopAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(primary.stopped) != 1 || len(secondary.stopped) != 1 {
		t.Fatalf("rescue did not stop both workers: primary=%v secondary=%v", primary.stopped, secondary.stopped)
	}
}

func TestManagerStopAllStopsKnownWorkersWithoutActiveLedger(t *testing.T) {
	primary, secondary := managerTestAdapters()
	manager, err := NewManager(primary, secondary)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StopAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(primary.stopped) != 1 || primary.stopped[0] != primary.workerID || len(secondary.stopped) != 1 || secondary.stopped[0] != secondary.workerID {
		t.Fatalf("rescue did not stop known workers after restart: primary=%v secondary=%v", primary.stopped, secondary.stopped)
	}
}

func TestManagerRegistersThirdAdapterWithUniqueWorkerID(t *testing.T) {
	primary, secondary := managerTestAdapters()
	third := &fakeAdapter{core: "core-c"}
	manager, err := NewManager(primary, secondary, third)
	if err != nil {
		t.Fatal(err)
	}
	statuses := manager.Statuses()
	thirdWorkerID := third.Descriptor().WorkerID(1)
	if len(statuses) != 3 || statusByID(statuses, thirdWorkerID).Core != third.core {
		t.Fatalf("third adapter worker status collided: %+v", statuses)
	}
	checkpoint, err := manager.Checkpoint("three-adapters")
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Backups) != 3 || checkpoint.Backups[thirdWorkerID].Core != third.core {
		t.Fatalf("third adapter checkpoint collided: %+v", checkpoint.Backups)
	}
	if _, err := manager.workerID("unregistered"); err == nil {
		t.Fatal("unregistered core resolved to a worker ID")
	}
}

func TestManagerRollbackRestoresActiveWorkerLedger(t *testing.T) {
	primary, secondary := managerTestAdapters()
	manager, err := NewManager(primary, secondary)
	if err != nil {
		t.Fatal(err)
	}
	baseGraph, identities := managerTestGraph(t, false)
	basePrepared, err := manager.Prepare(context.Background(), "rollback-base", baseGraph, identities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), basePrepared); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := manager.Checkpoint("rollback-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	nextGraph, nextIdentities := managerTestGraph(t, true)
	nextPrepared, err := manager.Prepare(context.Background(), "rollback-target", nextGraph, nextIdentities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), nextPrepared); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rollback(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.active[primary.workerID]; !ok {
		t.Fatal("restored primary worker is missing from active ledger")
	}
	if _, ok := manager.active[secondary.workerID]; ok {
		t.Fatal("rolled-back secondary worker remains active")
	}
	statuses := manager.Statuses()
	primaryStatus := statusByID(statuses, primary.workerID)
	secondaryStatus := statusByID(statuses, secondary.workerID)
	if primaryStatus.State != StateRunning || len(primaryStatus.Pools) != 1 || primaryStatus.Pools[0] != "general" {
		t.Fatalf("restored primary status does not match checkpoint: %+v", primaryStatus)
	}
	if secondaryStatus.State != StateStopped || len(secondaryStatus.Pools) != 0 || len(secondaryStatus.Executors) != 1 || secondaryStatus.Executors[0].ID != "exec-ai" {
		t.Fatalf("rolled-back secondary installed configuration was not restored: %+v", secondaryStatus)
	}
}

func managerTestGraph(t *testing.T, includeSecondary bool) (state.ExecutionGraph, map[string]smartnode.IdentitySource) {
	t.Helper()
	primary, secondary := managerTestAdapters()
	graph := state.ExecutionGraph{
		Workers: []state.WorkerTarget{
			{
				ID: primary.workerID, Core: primary.core,
				Routes: []state.WorkerRoute{{ID: "route-general", Pool: "general", Selector: "route-general", Candidates: []string{"exec-general"}, Selected: "exec-general"}},
				Inbounds: []state.WorkerInbound{
					{ID: "sb-general", Kind: "tproxy", Pool: "general", Route: "route-general", Binding: "bind-general", Generation: 1, Port: 16000},
					{ID: "sb-general-dns", Kind: "socks", Pool: "general", Route: "route-general", Binding: "bind-general", Generation: 1, Listen: "127.0.0.1", Port: 18000},
				},
			},
			{
				ID: secondary.workerID, Core: secondary.core,
				Routes: []state.WorkerRoute{{ID: "route-ai", Pool: "ai", Selector: "route-ai", Candidates: []string{"exec-ai"}, Selected: "exec-ai"}},
				Inbounds: []state.WorkerInbound{
					{ID: "xr-ai", Kind: "tproxy", Pool: "ai", Route: "route-ai", Binding: "bind-ai", Generation: 1, Port: 17000},
					{ID: "xr-ai-dns", Kind: "socks", Pool: "ai", Route: "route-ai", Binding: "bind-ai", Generation: 1, Listen: "127.0.0.1", Port: 19000},
				},
			},
		},
		NodeExecutors: []state.NodeExecutor{
			{ID: "exec-general", SmartNode: "node-general", Revision: 1, Core: primary.core, Worker: primary.workerID},
			{ID: "exec-ai", SmartNode: "node-ai", Revision: 1, Core: secondary.core, Worker: secondary.workerID},
		},
		PoolBindings: []state.PoolBinding{{
			ID: "bind-general", Pool: "general", Generation: 1,
			Target: state.BindingTarget{Kind: "node", SmartNode: "node-general", Executor: "exec-general", Core: primary.core, Worker: primary.workerID},
		}},
	}
	if includeSecondary {
		graph.PoolBindings = append(graph.PoolBindings, state.PoolBinding{
			ID: "bind-ai", Pool: "ai", Generation: 1,
			Target: state.BindingTarget{Kind: "node", SmartNode: "node-ai", Executor: "exec-ai", Core: secondary.core, Worker: secondary.workerID},
		})
	}
	outbounds := map[string]map[string]any{
		"node-general": {"type": "vless", "tag": "general", "server": "general.example", "server_port": 443, "uuid": "general-secret"},
		"node-ai":      {"type": "vless", "tag": "ai", "server": "ai.example", "server_port": 443, "uuid": "ai-secret"},
	}
	identities := make(map[string]smartnode.IdentitySource, len(outbounds))
	for nodeID, outbound := range outbounds {
		source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{ProviderID: "p1", RawTag: outbound["tag"].(string), Outbound: outbound})
		if err != nil {
			t.Fatal(err)
		}
		identities[nodeID] = source
	}
	return graph, identities
}

func statusByID(statuses []Status, workerID string) Status {
	for _, status := range statuses {
		if status.ID == workerID {
			return status
		}
	}
	return Status{}
}
