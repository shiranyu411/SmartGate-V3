package commitlane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"smartgate/internal/coredesc"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

var commitCoreA, commitCoreB = func() (smartnode.CoreID, smartnode.CoreID) {
	primary, secondary := commitTestDescriptors()
	return primary.ID, secondary.ID
}()

type cutoverTestFaults struct {
	mu                       sync.Mutex
	routeSelections          []worker.BindingPath
	verifyCalls              map[string]int
	selectionVerifyCalls     int
	liveRouteSelections      map[string]string
	failVerifyAt             map[string]int
	armFabricFailureOnVerify map[string]bool
	failNextFabric           bool
	starts                   []string
	stops                    []string
	fabricCalls              int
	failCloseConnections     bool
	failStopWorker           bool
	stopEntered              chan struct{}
	stopRelease              <-chan struct{}
}

type cutoverTestDrainStore struct {
	delegate    cutoverDrainStore
	mu          sync.Mutex
	failSave    bool
	saveFailure chan struct{}
}

func (store *cutoverTestDrainStore) List() ([]state.CutoverDrainState, error) {
	return store.delegate.List()
}

func (store *cutoverTestDrainStore) Save(drain state.CutoverDrainState) error {
	store.mu.Lock()
	fail := store.failSave
	signal := store.saveFailure
	store.mu.Unlock()
	if fail {
		if signal != nil {
			select {
			case signal <- struct{}{}:
			default:
			}
		}
		return fmt.Errorf("injected durable drain save failure")
	}
	return store.delegate.Save(drain)
}

func (store *cutoverTestDrainStore) Remove(pool string) error {
	return store.delegate.Remove(pool)
}

func (store *cutoverTestDrainStore) setFailSave(fail bool) {
	store.mu.Lock()
	store.failSave = fail
	store.mu.Unlock()
}

func newCutoverTestFaults() *cutoverTestFaults {
	return &cutoverTestFaults{
		verifyCalls: map[string]int{}, failVerifyAt: map[string]int{}, armFabricFailureOnVerify: map[string]bool{},
	}
}

func (faults *cutoverTestFaults) resetRuntimeCalls() {
	faults.mu.Lock()
	faults.routeSelections = nil
	faults.verifyCalls = map[string]int{}
	faults.selectionVerifyCalls = 0
	faults.starts = nil
	faults.stops = nil
	faults.fabricCalls = 0
	faults.mu.Unlock()
}

type cutoverTestAdapter struct {
	core   smartnode.CoreID
	faults *cutoverTestFaults
}

func (adapter cutoverTestAdapter) Core() smartnode.CoreID { return adapter.core }

func (adapter cutoverTestAdapter) Descriptor() coredesc.Descriptor {
	return coredesc.Descriptor{ID: adapter.core}
}

func (adapter cutoverTestAdapter) Prepare(_ context.Context, opID string, request worker.CompileRequest) (worker.Candidate, error) {
	return worker.Candidate{
		OpID: opID, WorkerID: request.Worker.ID, Core: adapter.core,
		Path: "/tmp/" + request.Worker.ID, CheckOK: true,
	}, nil
}

func (cutoverTestAdapter) Verify(candidate worker.Candidate) error {
	if !candidate.CheckOK {
		return fmt.Errorf("candidate was not checked")
	}
	return nil
}

func (cutoverTestAdapter) Install(_ context.Context, candidate worker.Candidate) (worker.Activation, error) {
	return worker.Activation{WorkerID: candidate.WorkerID}, nil
}

func (cutoverTestAdapter) Activate(_ context.Context, candidate worker.Candidate) (worker.Activation, error) {
	return worker.Activation{WorkerID: candidate.WorkerID, Restarted: true}, nil
}

func (adapter cutoverTestAdapter) Start(_ context.Context, target state.WorkerTarget) (worker.Activation, error) {
	adapter.faults.mu.Lock()
	adapter.faults.starts = append(adapter.faults.starts, target.ID)
	adapter.faults.mu.Unlock()
	return worker.Activation{WorkerID: target.ID, Restarted: true}, nil
}

func (adapter cutoverTestAdapter) Backup(_ string, workerID string) (worker.Backup, error) {
	return worker.Backup{WorkerID: workerID, Core: adapter.core, Existed: true}, nil
}

func (cutoverTestAdapter) Restore(_ context.Context, backup worker.Backup) (worker.Activation, error) {
	return worker.Activation{WorkerID: backup.WorkerID, Restarted: backup.WasActive}, nil
}

func (adapter cutoverTestAdapter) Stop(ctx context.Context, workerID string) error {
	adapter.faults.mu.Lock()
	adapter.faults.stops = append(adapter.faults.stops, workerID)
	fail := adapter.faults.failStopWorker
	entered := adapter.faults.stopEntered
	release := adapter.faults.stopRelease
	adapter.faults.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail {
		return fmt.Errorf("injected worker stop failure")
	}
	return nil
}

func (cutoverTestAdapter) Probe(context.Context, string) error    { return nil }
func (cutoverTestAdapter) RemoveCandidate(worker.Candidate) error { return nil }

func (adapter cutoverTestAdapter) SelectRoute(_ context.Context, path worker.BindingPath) error {
	adapter.faults.mu.Lock()
	adapter.faults.routeSelections = append(adapter.faults.routeSelections, path)
	if adapter.faults.liveRouteSelections != nil {
		adapter.faults.liveRouteSelections[path.Route.ID] = path.Executor.ID
	}
	adapter.faults.mu.Unlock()
	return nil
}

func (adapter cutoverTestAdapter) VerifyRoute(_ context.Context, path worker.BindingPath) error {
	key := path.Route.Pool + ":" + path.Executor.SmartNode
	adapter.faults.mu.Lock()
	adapter.faults.verifyCalls[key]++
	call := adapter.faults.verifyCalls[key]
	failAt := adapter.faults.failVerifyAt[key]
	if failAt > 0 && call == failAt && adapter.faults.armFabricFailureOnVerify[key] {
		adapter.faults.failNextFabric = true
	}
	adapter.faults.mu.Unlock()
	if failAt > 0 && call == failAt {
		return fmt.Errorf("verify route failed for %s", key)
	}
	return nil
}

// VerifyRouteSelection models the local selector check; VerifyRoute above is
// the end-to-end probe through the upstream node.
func (adapter cutoverTestAdapter) VerifyRouteSelection(_ context.Context, path worker.BindingPath) error {
	adapter.faults.mu.Lock()
	defer adapter.faults.mu.Unlock()
	adapter.faults.selectionVerifyCalls++
	if adapter.faults.liveRouteSelections != nil && adapter.faults.liveRouteSelections[path.Route.ID] != path.Executor.ID {
		return fmt.Errorf("route %s lost its durable target", path.Route.ID)
	}
	return nil
}

func (cutoverTestAdapter) DelayRoute(_ context.Context, _ worker.BindingPath, testURL string, _ time.Duration) (worker.DelayResult, error) {
	return worker.DelayResult{URL: testURL}, nil
}

func (cutoverTestAdapter) CountRouteConnections(context.Context, worker.BindingPath) (int, error) {
	return 0, nil
}

func (cutoverTestAdapter) ListRouteConnections(context.Context, worker.BindingPath) ([]worker.Connection, error) {
	return nil, nil
}

func (cutoverTestAdapter) CloseRouteConnection(context.Context, worker.BindingPath, string) error {
	return nil
}

func (cutoverTestAdapter) CloseRouteConnections(context.Context, worker.BindingPath) (int, error) {
	return 0, nil
}

type cutoverTestFabricRunner struct {
	faults *cutoverTestFaults
}

func (runner cutoverTestFabricRunner) Run(_ context.Context, _ []string) (string, error) {
	runner.faults.mu.Lock()
	runner.faults.fabricCalls++
	if runner.faults.failNextFabric {
		runner.faults.failNextFabric = false
		runner.faults.mu.Unlock()
		return "", fmt.Errorf("injected Fabric failure")
	}
	runner.faults.mu.Unlock()
	return "ok", nil
}

type cutoverTestConnections struct {
	mu         sync.Mutex
	counts     map[string]int
	closeCalls []state.PoolDispatchGraph
	failClose  bool
}

func newCutoverTestConnections() *cutoverTestConnections {
	return &cutoverTestConnections{counts: map[string]int{}}
}

func (tracker *cutoverTestConnections) key(dispatch state.PoolDispatchGraph) string {
	return fmt.Sprintf("%s:%d", dispatch.Pool, dispatch.Generation)
}

func (tracker *cutoverTestConnections) Count(_ context.Context, dispatch state.PoolDispatchGraph) (int, error) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.counts[tracker.key(dispatch)], nil
}

func (tracker *cutoverTestConnections) Close(_ context.Context, dispatch state.PoolDispatchGraph) (int, error) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.failClose {
		return 0, fmt.Errorf("injected connection close failure")
	}
	key := tracker.key(dispatch)
	closed := tracker.counts[key]
	tracker.counts[key] = 0
	tracker.closeCalls = append(tracker.closeCalls, dispatch)
	return closed, nil
}

func (tracker *cutoverTestConnections) set(pool string, generation uint64, count int) {
	tracker.mu.Lock()
	tracker.counts[fmt.Sprintf("%s:%d", pool, generation)] = count
	tracker.mu.Unlock()
}

type cutoverTestFixture struct {
	txn         *SelectorTxn
	store       *state.Store
	layout      state.Layout
	workers     *worker.Manager
	fabric      *fabric.Manager
	faults      *cutoverTestFaults
	connections *cutoverTestConnections
	nodes       map[string]smartnode.SmartNode
	bindings    map[string]state.PoolBinding
}

func TestCutoverTxnStrictCompletesAndOnlyAdvancesTargetPool(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreB, commitCoreB)
	source := fixture.bindings["ai"]
	general := fixture.bindings["general"]
	fixture.connections.set("ai", source.Generation, 3)

	result, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "strict-ai", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "cutover_complete" || !result.Verified || result.ClosedConnections != 3 || result.Binding.Generation != source.Generation+1 {
		t.Fatalf("unexpected strict result: %+v", result)
	}
	topology, err := fixture.store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	if topology.ProxySelection["ai"] != fixture.nodes["ai-target"].ID || topology.Selections["ai"].Generation != source.Generation+1 {
		t.Fatalf("strict selection was not persisted: %+v", topology.Selections["ai"])
	}
	graph, err := buildCutoverGraph(mustAppliedState(t, fixture.store), topology.Selections, mustAppliedState(t, fixture.store).ExecutionGraph, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	gotGeneral, _ := bindingForPool(graph, "general")
	if gotGeneral.ID != general.ID || gotGeneral.Generation != general.Generation || gotGeneral.Target != general.Target {
		t.Fatalf("AI cutover changed General Binding: before=%+v after=%+v", general, gotGeneral)
	}
	if cutoverJournalExists(fixture.layout, "strict-ai") {
		t.Fatal("strict cutover journal remained")
	}
}

func TestCutoverTxnGracefulCommitsAndDrainsOldGeneration(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreA, commitCoreA)
	source := fixture.bindings["ai"]
	fixture.connections.set("ai", source.Generation, 2)

	result, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "graceful-ai", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "committed" || result.Draining == nil || result.Draining.OldGeneration != source.Generation || result.Draining.Remaining != 2 {
		t.Fatalf("unexpected graceful result: %+v", result)
	}
	_, drains := fixture.txn.Progress()
	if len(drains) != 1 || drains[0].Pool != "ai" || drains[0].Remaining != 2 {
		t.Fatalf("graceful drain was not tracked: %+v", drains)
	}
	fixture.connections.set("ai", source.Generation, 0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, drains = fixture.txn.Progress()
		if len(drains) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("graceful drain did not finish: %+v", drains)
}

func TestCutoverTxnDrainCloseFailureRemainsFailedAndBlocksPool(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreA, commitCoreA)
	withFastDrainRetry(t, fixture.txn)
	source := fixture.bindings["ai"]
	fixture.connections.set("ai", source.Generation, 1)
	fixture.connections.failClose = true

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "drain-close-failure", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	drain := waitForDrainStatus(t, fixture.txn, "ai", state.CutoverDrainStatusFailed)
	if drain.LastError == "" || drain.RetryCount != drainRetryLimit {
		t.Fatalf("failed drain metadata is incomplete: %+v", drain)
	}
	persisted, err := state.NewCutoverDrainStore(fixture.layout).List()
	if err != nil || len(persisted) != 1 || persisted[0].Status != state.CutoverDrainStatusFailed {
		t.Fatalf("failed drain was not durable: drains=%+v err=%v", persisted, err)
	}
	topology, err := fixture.store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "blocked-by-failed-drain", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-source"].ID,
		Source: CutoverSource{SmartNode: fixture.nodes["ai-target"].ID, Generation: topology.Selections["ai"].Generation}, ExpectedConfigEpoch: 7,
	})
	var conflict CutoverConflictError
	if !errors.As(err, &conflict) || conflict.OldGeneration != source.Generation {
		t.Fatalf("failed drain did not block a later cutover: %v", err)
	}
}

func TestCutoverTxnDrainWorkerStopFailureRemainsFailed(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreB, commitCoreA)
	withFastDrainRetry(t, fixture.txn)
	source := fixture.bindings["ai"]
	fixture.connections.set("ai", source.Generation, 1)

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "drain-stop-failure", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.faults.mu.Lock()
	fixture.faults.failStopWorker = true
	fixture.faults.mu.Unlock()
	fixture.connections.set("ai", source.Generation, 0)
	drain := waitForDrainStatus(t, fixture.txn, "ai", state.CutoverDrainStatusFailed)
	if !strings.Contains(drain.LastError, "worker stop") {
		t.Fatalf("worker stop failure was not reported: %+v", drain)
	}
}

func TestCutoverTxnFailedDrainRetriesAutomatically(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreB, commitCoreA)
	withFastDrainRetry(t, fixture.txn)
	withFastFailedDrainRetry(t, fixture.txn)
	source := fixture.bindings["ai"]
	fixture.connections.set("ai", source.Generation, 0)
	fixture.faults.mu.Lock()
	fixture.faults.failStopWorker = true
	fixture.faults.mu.Unlock()

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "drain-auto-retry", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	drain := waitForDrainStatus(t, fixture.txn, "ai", state.CutoverDrainStatusFailed)
	if !strings.Contains(drain.LastError, "worker stop") {
		t.Fatalf("worker stop failure was not reported: %+v", drain)
	}
	fixture.faults.mu.Lock()
	fixture.faults.failStopWorker = false
	fixture.faults.mu.Unlock()

	waitForNoDrain(t, fixture.txn, "ai")
	persisted, err := state.NewCutoverDrainStore(fixture.layout).List()
	if err != nil || len(persisted) != 0 {
		t.Fatalf("automatic retry left durable drain: %+v err=%v", persisted, err)
	}
}

func TestCutoverTxnBackgroundRetryIsVisibleInMemory(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreB, commitCoreA)
	withFastDrainRetry(t, fixture.txn)
	fixture.txn.drainTiming.failedRetryInterval = 100 * time.Millisecond
	fixture.txn.drainTiming.attemptLimit = time.Second
	source := fixture.bindings["ai"]
	fixture.connections.set("ai", source.Generation, 0)
	fixture.faults.mu.Lock()
	fixture.faults.failStopWorker = true
	fixture.faults.mu.Unlock()

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "drain-retry-visible", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForDrainStatus(t, fixture.txn, "ai", state.CutoverDrainStatusFailed)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		fixture.faults.mu.Lock()
		fixture.faults.failStopWorker = false
		fixture.faults.mu.Unlock()
		releaseOnce.Do(func() { close(release) })
	})
	fixture.faults.mu.Lock()
	fixture.faults.stopEntered = entered
	fixture.faults.stopRelease = release
	fixture.faults.mu.Unlock()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("background retry did not start")
	}
	retrying := waitForDrainStatus(t, fixture.txn, "ai", state.CutoverDrainStatusRetrying)
	if !strings.Contains(retrying.LastError, "worker stop") {
		t.Fatalf("background retry lost the previous failure context: %+v", retrying)
	}
	persisted, err := state.NewCutoverDrainStore(fixture.layout).List()
	if err != nil || len(persisted) != 1 || persisted[0].Status != state.CutoverDrainStatusFailed {
		t.Fatalf("background retry changed durable failed state: %+v err=%v", persisted, err)
	}
	fixture.faults.mu.Lock()
	fixture.faults.failStopWorker = false
	fixture.faults.mu.Unlock()
	releaseOnce.Do(func() { close(release) })
	waitForNoDrain(t, fixture.txn, "ai")
}

func TestCutoverTxnPersistFailureKeepsAutomaticRetryScheduled(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreB, commitCoreA)
	withFastDrainRetry(t, fixture.txn)
	fixture.txn.drainTiming.failedRetryInterval = 100 * time.Millisecond
	drains := &cutoverTestDrainStore{delegate: fixture.txn.drains, saveFailure: make(chan struct{}, 1)}
	fixture.txn.drains = drains
	source := fixture.bindings["ai"]
	fixture.connections.set("ai", source.Generation, 0)
	fixture.faults.mu.Lock()
	fixture.faults.failStopWorker = true
	fixture.faults.mu.Unlock()
	t.Cleanup(func() {
		drains.setFailSave(false)
		fixture.faults.mu.Lock()
		fixture.faults.failStopWorker = false
		fixture.faults.mu.Unlock()
	})

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "drain-persist-retry", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForDrainStatus(t, fixture.txn, "ai", state.CutoverDrainStatusFailed)
	drains.setFailSave(true)

	select {
	case <-drains.saveFailure:
	case <-time.After(3 * time.Second):
		t.Fatal("background retry did not encounter the injected persistence failure")
	}
	waitForDrainError(t, fixture.txn, "ai", "persist drain")
	drains.setFailSave(false)
	fixture.faults.mu.Lock()
	fixture.faults.failStopWorker = false
	fixture.faults.mu.Unlock()
	waitForNoDrain(t, fixture.txn, "ai")
}

func TestCutoverTxnFailedDrainRetryDoesNotRewriteUnchangedState(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreB, commitCoreA)
	withFastDrainRetry(t, fixture.txn)
	source := fixture.bindings["ai"]
	fixture.connections.set("ai", source.Generation, 0)
	fixture.faults.mu.Lock()
	fixture.faults.failStopWorker = true
	fixture.faults.mu.Unlock()

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "drain-retry-no-rewrite", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	drain := waitForDrainStatus(t, fixture.txn, "ai", state.CutoverDrainStatusFailed)
	before, err := os.Stat(fixture.layout.CutoverDrainsPath())
	if err != nil {
		t.Fatal(err)
	}

	_, retry := fixture.txn.retryFailedDrain(drain)
	if !retry {
		t.Fatal("persistent cleanup failure did not request another background retry")
	}
	after, err := os.Stat(fixture.layout.CutoverDrainsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("unchanged failed drain retry rewrote its durable file")
	}
}

func TestSanitizeDrainErrorRedactsPathsAndSecrets(t *testing.T) {
	message := sanitizeDrainError(fmt.Errorf(`open /etc/smartgate/cutover-drains.json: fetch https://user:pass@example.com/sub?token=plain password=hunter2`))
	for _, secret := range []string{"/etc/smartgate/cutover-drains.json", "user:pass", "/sub?token=plain", "hunter2"} {
		if strings.Contains(message, secret) {
			t.Fatalf("drain error leaked %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "[REDACTED_PATH]") || !strings.Contains(message, "password=[REDACTED]") {
		t.Fatalf("drain error redaction markers missing: %s", message)
	}
	if !strings.Contains(message, "https://example.com") {
		t.Fatalf("drain error redaction corrupted safe URL identity: %s", message)
	}
}

func TestCutoverTxnDrainLockTimeoutRemainsFailed(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreA, commitCoreA)
	withFastDrainRetry(t, fixture.txn)
	source := fixture.bindings["ai"]
	fixture.connections.set("ai", source.Generation, 1)

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "drain-lock-failure", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	holder := fixture.txn.commit.locks.Session("hold-drain-lock")
	release, err := holder.Acquire(lockset.LevelCommitLane)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fixture.connections.set("ai", source.Generation, 0)
	drain := waitForDrainStatus(t, fixture.txn, "ai", state.CutoverDrainStatusFailed)
	if !strings.Contains(drain.LastError, "deadline exceeded") {
		t.Fatalf("lock timeout was not reported: %+v", drain)
	}
}

func TestCutoverTxnRecoversDurableDrainAndRetriesCleanup(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreA, commitCoreA)
	withFastDrainRetry(t, fixture.txn)
	source := fixture.bindings["ai"]
	dispatch, ok := dispatchForPoolGraph(mustAppliedState(t, fixture.store).ExecutionGraph.Fabric, "ai")
	if !ok {
		t.Fatal("missing source dispatch")
	}
	if err := state.NewCutoverDrainStore(fixture.layout).Save(state.CutoverDrainState{
		Pool: "ai", OldGeneration: source.Generation, Remaining: 1, Deadline: time.Now().Add(-time.Second),
		Status: state.CutoverDrainStatusFailed, LastError: "previous cleanup failure", RetryCount: drainRetryLimit,
		Binding: source, Dispatch: dispatch,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.connections.set("ai", source.Generation, 1)
	if err := fixture.txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, drains := fixture.txn.Progress()
	if len(drains) != 1 || drains[0].Status != state.CutoverDrainStatusFailed {
		t.Fatalf("durable drain was not restored before retry: %+v", drains)
	}
	fixture.txn.ResumeDrains()
	waitForNoDrain(t, fixture.txn, "ai")
	persisted, err := state.NewCutoverDrainStore(fixture.layout).List()
	if err != nil || len(persisted) != 0 {
		t.Fatalf("successful recovery left durable drain: %+v err=%v", persisted, err)
	}
}

func TestCutoverTxnRecoversGracefulDrainFromCommittedJournal(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverGraceful, commitCoreA, commitCoreA)
	sourceState, err := fixture.txn.loadState()
	if err != nil {
		t.Fatal(err)
	}
	sourceBinding := fixture.bindings["ai"]
	pool, _ := businessPoolByID(sourceState.Applied.Spec, "ai")
	targetSelections, targetSelection, err := targetSelectionsForCutover(
		sourceState.Selection.Selections, sourceState.Applied, pool, sourceBinding, fixture.nodes["ai-target"].ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	targetGraph, err := buildCutoverGraph(sourceState.Applied, targetSelections, sourceState.Applied.ExecutionGraph, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	targetBinding, _ := bindingForPool(targetGraph, "ai")
	targetState := cutoverTargetState(sourceState, CutoverRequest{PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID}, pool, targetSelection, targetBinding)
	deadline := time.Now().Add(time.Minute)
	journal := state.TransactionJournal{
		OpID: "recover-graceful-journal", Kind: selectorTransactionKind, Phase: state.TransactionCommitted,
		Source: sourceState, Target: targetState, PoolID: "ai", CutoverPolicy: state.CutoverGraceful,
		SourceBinding: &sourceBinding, TargetBinding: &targetBinding, DrainDeadline: &deadline,
	}
	if err := state.NewTransactionStore(fixture.layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	if err := fixture.txn.persistState(targetState); err != nil {
		t.Fatal(err)
	}
	if err := fixture.txn.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, err := state.NewCutoverDrainStore(fixture.layout).List()
	if err != nil || len(persisted) != 1 || persisted[0].Pool != "ai" || persisted[0].Status != state.CutoverDrainStatusFailed {
		t.Fatalf("committed journal did not rebuild drain: %+v err=%v", persisted, err)
	}
	_, drains := fixture.txn.Progress()
	if len(drains) != 1 || drains[0].OldGeneration != sourceBinding.Generation {
		t.Fatalf("rebuilt drain was not restored to tracker: %+v", drains)
	}
	if cutoverJournalExists(fixture.layout, journal.OpID) {
		t.Fatal("committed graceful journal remained after drain recovery")
	}
}

func TestCutoverTxnRejectsFullSourceCASWithoutMutation(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreB, commitCoreB)
	source := fixture.bindings["ai"]
	before, err := fixture.store.LoadSelectionState()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "stale-ai", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation + 1}, ExpectedConfigEpoch: 7,
	})
	var conflict SelectionConflictError
	if !errors.As(err, &conflict) || conflict.Actual.Generation != source.Generation {
		t.Fatalf("expected full source conflict, got %v", err)
	}
	after, loadErr := fixture.store.LoadSelectionState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if after.Selections["ai"] != before.Selections["ai"] || cutoverJournalExists(fixture.layout, "stale-ai") {
		t.Fatalf("stale CAS mutated durable state: before=%+v after=%+v", before.Selections["ai"], after.Selections["ai"])
	}
}

func TestCutoverTxnCrossCoreStartsTargetWithoutChangingGeneral(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreA, commitCoreB)
	source := fixture.bindings["ai"]
	general := fixture.bindings["general"]

	result, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "cross-core-ai", PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Binding.Target.Core != commitCoreB || !fixture.workers.IsRunning(result.Binding.Target.Worker) || !fixture.workers.IsRunning(general.Target.Worker) {
		t.Fatalf("cross-core workers are wrong: result=%+v statuses=%+v", result, fixture.workers.Statuses())
	}
	fixture.faults.mu.Lock()
	starts := append([]string(nil), fixture.faults.starts...)
	fixture.faults.mu.Unlock()
	if len(starts) != 1 || starts[0] != result.Binding.Target.Worker {
		t.Fatalf("inactive target worker was not started exactly once: %+v", starts)
	}
	topology, err := fixture.store.LoadAppliedTopology()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := buildCutoverGraph(mustAppliedState(t, fixture.store), topology.Selections, mustAppliedState(t, fixture.store).ExecutionGraph, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	gotGeneral, _ := bindingForPool(graph, "general")
	if gotGeneral.ID != general.ID || gotGeneral.Generation != general.Generation || gotGeneral.Target != general.Target {
		t.Fatalf("cross-core AI cutover changed General: before=%+v after=%+v", general, gotGeneral)
	}
}

func TestSelectorTxnEnsureWorkerRunningStartsInstalledWorkerOnce(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreA, commitCoreB)
	target := fixture.nodes["ai-target"]
	var workerID string
	for _, executor := range mustAppliedState(t, fixture.store).ExecutionGraph.NodeExecutors {
		if executor.SmartNode == target.ID {
			workerID = executor.Worker
		}
	}
	if workerID == "" || fixture.workers.IsRunning(workerID) {
		t.Fatalf("precondition: unbound target worker %q must be installed but stopped", workerID)
	}
	started, err := fixture.txn.EnsureWorkerRunning(context.Background(), "probe-start-1", workerID)
	if err != nil || !started {
		t.Fatalf("first ensure: started=%v err=%v", started, err)
	}
	started, err = fixture.txn.EnsureWorkerRunning(context.Background(), "probe-start-2", workerID)
	if err != nil || started {
		t.Fatalf("second ensure must be a no-op: started=%v err=%v", started, err)
	}
	fixture.faults.mu.Lock()
	starts := append([]string(nil), fixture.faults.starts...)
	fixture.faults.mu.Unlock()
	if len(starts) != 1 || starts[0] != workerID || !fixture.workers.IsRunning(workerID) {
		t.Fatalf("worker was not started exactly once: starts=%v running=%v", starts, fixture.workers.IsRunning(workerID))
	}
}

func TestCutoverTxnVerificationFailureRollsBack(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreB, commitCoreB)
	source := fixture.bindings["ai"]
	targetID := fixture.nodes["ai-target"].ID
	fixture.faults.failVerifyAt["ai:"+targetID] = 2

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "verify-rollback", PoolID: "ai", TargetSmartNode: targetID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	var cutoverErr *CutoverError
	if !errors.As(err, &cutoverErr) || !cutoverErr.RolledBack || cutoverErr.Blocked || cutoverErr.Stage != "verify_new_path" {
		t.Fatalf("expected verified rollback, got %v", err)
	}
	topology, loadErr := fixture.store.LoadAppliedTopology()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if topology.ProxySelection["ai"] != source.Target.SmartNode || topology.Selections["ai"].Generation != source.Generation {
		t.Fatalf("rollback did not restore source: %+v", topology.Selections["ai"])
	}
	if cutoverJournalExists(fixture.layout, "verify-rollback") {
		t.Fatal("rolled-back cutover journal remained")
	}
}

func TestCutoverTxnCompensationFailureForcesPoolBlock(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreB, commitCoreB)
	source := fixture.bindings["ai"]
	targetID := fixture.nodes["ai-target"].ID
	key := "ai:" + targetID
	fixture.faults.failVerifyAt[key] = 2
	fixture.faults.armFabricFailureOnVerify[key] = true

	_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
		OpID: "verify-block", PoolID: "ai", TargetSmartNode: targetID,
		Source: CutoverSource{SmartNode: source.Target.SmartNode, Generation: source.Generation}, ExpectedConfigEpoch: 7,
	})
	var cutoverErr *CutoverError
	if !errors.As(err, &cutoverErr) || cutoverErr.RolledBack || !cutoverErr.Blocked || cutoverErr.Stage != "verify_new_path" {
		t.Fatalf("expected forced Block after compensation failure, got %v", err)
	}
	topology, loadErr := fixture.store.LoadAppliedTopology()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	selection := topology.Selections["ai"]
	if topology.ProxySelection["ai"] != "Block" || selection.Target != state.NoCandidateBlock || selection.Generation != source.Generation+2 {
		t.Fatalf("failed compensation did not force a new Block generation: %+v", selection)
	}
	if cutoverJournalExists(fixture.layout, "verify-block") {
		t.Fatal("forced-Block journal remained")
	}
}

func TestCutoverTxnRecoversApplyingAndCommittedJournals(t *testing.T) {
	for _, phase := range []state.TransactionPhase{state.TransactionApplying, state.TransactionCommitted} {
		t.Run(string(phase), func(t *testing.T) {
			fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreB, commitCoreB)
			sourceState, err := fixture.txn.loadState()
			if err != nil {
				t.Fatal(err)
			}
			sourceBinding := fixture.bindings["ai"]
			pool, _ := businessPoolByID(sourceState.Applied.Spec, "ai")
			targetSelections, targetSelection, err := targetSelectionsForCutover(
				sourceState.Selection.Selections, sourceState.Applied, pool, sourceBinding, fixture.nodes["ai-target"].ID,
			)
			if err != nil {
				t.Fatal(err)
			}
			targetGraph, err := buildCutoverGraph(sourceState.Applied, targetSelections, sourceState.Applied.ExecutionGraph, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			targetBinding, _ := bindingForPool(targetGraph, "ai")
			targetState := cutoverTargetState(sourceState, CutoverRequest{PoolID: "ai", TargetSmartNode: fixture.nodes["ai-target"].ID}, pool, targetSelection, targetBinding)
			journal := state.TransactionJournal{
				OpID: "recover-" + string(phase), Kind: selectorTransactionKind, Phase: phase,
				Source: sourceState, Target: targetState, PoolID: "ai", CutoverPolicy: state.CutoverStrict,
				SourceBinding: &sourceBinding, TargetBinding: &targetBinding,
			}
			if err := state.NewTransactionStore(fixture.layout).Save(journal); err != nil {
				t.Fatal(err)
			}
			if err := fixture.txn.persistState(targetState); err != nil {
				t.Fatal(err)
			}
			if err := fixture.txn.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			topology, err := fixture.store.LoadAppliedTopology()
			if err != nil {
				t.Fatal(err)
			}
			want := sourceBinding.Target.SmartNode
			wantGeneration := sourceBinding.Generation
			if phase == state.TransactionCommitted {
				want = targetBinding.Target.SmartNode
				wantGeneration = targetBinding.Generation
			}
			if topology.ProxySelection["ai"] != want || topology.Selections["ai"].Generation != wantGeneration {
				t.Fatalf("%s recovery chose wrong direction: %+v", phase, topology.Selections["ai"])
			}
			if cutoverJournalExists(fixture.layout, journal.OpID) {
				t.Fatalf("%s recovery left journal", phase)
			}
		})
	}
}

func newCutoverTestFixture(t *testing.T, cutoverPolicy string, sourceCore, targetCore smartnode.CoreID) *cutoverTestFixture {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	faults := newCutoverTestFaults()
	connections := newCutoverTestConnections()
	owners := netgate.NewOwnerRegistry("smartgate")
	workers, err := worker.NewManager(
		cutoverTestAdapter{core: commitCoreA, faults: faults},
		cutoverTestAdapter{core: commitCoreB, faults: faults},
	)
	if err != nil {
		t.Fatal(err)
	}
	fabricManager := fabric.NewManager(owners, cutoverTestFabricRunner{faults: faults}).WithConnectionTracker(connections)
	store := state.NewStore(layout)
	commit := New(lockset.New(), layout, owners)
	txn := NewCutoverTxn(commit, store, workers, fabricManager)

	nodes := map[string]smartnode.SmartNode{
		"general":   cutoverTestNode(t, "general", commitCoreA),
		"ai-source": cutoverTestNode(t, "ai-source", sourceCore),
		"ai-target": cutoverTestNode(t, "ai-target", targetCore),
	}
	spec := state.DefaultSpecState()
	spec.SmartNodes = map[string]smartnode.SmartNode{}
	for _, node := range nodes {
		spec.SmartNodes[node.ID] = node
	}
	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", NodeIDs: []string{nodes["general"].ID}, Policy: state.PoolPolicy{Cutover: state.CutoverGraceful, GracefulDrainSeconds: 1, NoCandidate: state.NoCandidateDirect, AutoFailover: true}},
		{ID: "ai", Name: "AI", Role: "ai", NodeIDs: []string{nodes["ai-source"].ID, nodes["ai-target"].ID}, Policy: state.PoolPolicy{Cutover: cutoverPolicy, GracefulDrainSeconds: 1, NoCandidate: state.NoCandidateBlock, AutoFailover: true}},
	}
	spec.Rules = state.RulesFromLegacyGroups(map[string]state.RuleSpec{"direct": {}, "block": {}}, spec.BusinessPools)
	spec.Fabric.DNS.Listen = cutoverTestDNSListen(t)
	selections := map[string]state.PoolSelection{
		"general": {SmartNode: nodes["general"].ID, Target: "node", Generation: 4, BoundEpoch: 7, BoundTopologyHash: "cutover-topology"},
		"ai":      {SmartNode: nodes["ai-source"].ID, Target: "node", Generation: 9, BoundEpoch: 7, BoundTopologyHash: "cutover-topology"},
	}
	plan := planner.BuildExecutionGraph(spec, selections, state.ExecutionGraph{}, time.Now().UTC())
	if !plan.ReadyToApply {
		t.Fatalf("build fixture graph: %+v", plan.Issues)
	}
	selected := map[string]string{"general": nodes["general"].ID, "ai": nodes["ai-source"].ID}
	applied := state.AppliedStateWithTarget(state.AppliedState{
		ConfigEpoch: 7, TopologyHash: "cutover-topology", GatewayMode: state.GatewayModeOn,
		ProxySelection: selected, ExecutionGraph: plan.Graph,
	}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	selectionState := state.SelectionStateForApplied(applied, selected)
	selectionState.Selections = selections
	if err := store.SaveSelectionState(selectionState); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRuntime(state.RuntimeState{ConfigEpoch: 7, TopologyHash: "cutover-topology", ProxySelection: selected}); err != nil {
		t.Fatal(err)
	}
	if err := state.NewFailoverStateStore(layout).Save(state.DefaultFailoverState()); err != nil {
		t.Fatal(err)
	}
	identities := map[string]smartnode.IdentitySource{}
	for _, node := range nodes {
		outbound := map[string]any{
			"type": "vless", "tag": node.ID, "server": node.Identity.Server,
			"server_port": node.Identity.Port, "uuid": "test-secret",
		}
		source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{ProviderID: "cutover", RawTag: node.ID, Outbound: outbound})
		if err != nil {
			t.Fatal(err)
		}
		identities[node.ID] = source
	}
	preparedWorkers, err := workers.Prepare(context.Background(), "cutover-fixture-workers", plan.Graph, identities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workers.Apply(context.Background(), preparedWorkers); err != nil {
		t.Fatal(err)
	}
	preparedFabric, err := fabricManager.Prepare(context.Background(), "cutover-fixture-fabric", spec, plan.Graph.Fabric)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fabricManager.Apply(context.Background(), preparedFabric); err != nil {
		t.Fatal(err)
	}
	faults.resetRuntimeCalls()
	bindings := map[string]state.PoolBinding{}
	for _, binding := range plan.Graph.PoolBindings {
		bindings[binding.Pool] = binding
	}
	t.Cleanup(func() {
		_ = fabricManager.Stop(context.Background())
		_ = workers.StopAll(context.Background())
	})
	return &cutoverTestFixture{
		txn: txn, store: store, layout: layout, workers: workers, fabric: fabricManager,
		faults: faults, connections: connections, nodes: nodes, bindings: bindings,
	}
}

func cutoverTestNode(t *testing.T, tag string, core smartnode.CoreID) smartnode.SmartNode {
	t.Helper()
	node, err := smartnode.Normalize(smartnode.RawNode{
		ProviderID: "cutover-test", RawTag: tag,
		Outbound: map[string]any{
			"type": "vless", "tag": tag, "server": tag + ".example", "server_port": 443, "uuid": tag + "-secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	node.CoreChoice = core
	return node
}

func cutoverTestDNSListen(t *testing.T) string {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := packet.LocalAddr().String()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		if closeErr := packet.Close(); closeErr != nil {
			t.Fatalf("%v; close UDP probe: %v", err, closeErr)
		}
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := packet.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func mustAppliedState(t *testing.T, store *state.Store) state.AppliedState {
	t.Helper()
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

func withFastDrainRetry(t *testing.T, txn *SelectorTxn) {
	t.Helper()
	txn.drainTiming = drainTiming{
		pollInterval:        5 * time.Millisecond,
		retryBase:           5 * time.Millisecond,
		attemptLimit:        25 * time.Millisecond,
		failedRetryInterval: time.Hour,
	}
}

func withFastFailedDrainRetry(t *testing.T, txn *SelectorTxn) {
	t.Helper()
	txn.drainTiming.failedRetryInterval = 5 * time.Millisecond
}

func waitForDrainStatus(t *testing.T, txn *SelectorTxn, pool, status string) DrainProgress {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, drains := txn.Progress()
		for _, drain := range drains {
			if drain.Pool == pool && drain.Status == status {
				return drain
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, drains := txn.Progress()
	t.Fatalf("drain %s did not reach %s: %+v", pool, status, drains)
	return DrainProgress{}
}

func waitForDrainError(t *testing.T, txn *SelectorTxn, pool, message string) DrainProgress {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, drains := txn.Progress()
		for _, drain := range drains {
			if drain.Pool == pool && strings.Contains(drain.LastError, message) {
				return drain
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("drain %s did not report error containing %q", pool, message)
	return DrainProgress{}
}

func waitForNoDrain(t *testing.T, txn *SelectorTxn, pool string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, drains := txn.Progress()
		found := false
		for _, drain := range drains {
			found = found || drain.Pool == pool
		}
		if !found {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, drains := txn.Progress()
	t.Fatalf("drain %s did not finish: %+v", pool, drains)
}
