package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/rescue"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

func TestDataPlaneSupervisorWorkerFailureEntersRescueWithConfiguredReason(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	store := state.NewStore(layout)
	rescueManager := rescue.New(layout.RescueStatusPath(), commitlane.New(lockset.New(), layout, owners), owners)
	const rescueReason = "data_plane_unavailable"
	supervisor := dataPlaneSupervisor{
		store: store, rescuer: rescueManager, rescueReason: rescueReason,
	}

	if err := supervisor.HandleWorkerFailure(context.Background(), worker.Failure{WorkerID: "w_test", Err: errors.New("worker crashed")}); err == nil {
		t.Fatal("worker failure without a recovery controller should report recovery failure")
	}
	status, err := rescueManager.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Reason != rescueReason {
		t.Fatalf("rescue reason=%q want %q", status.Reason, rescueReason)
	}
	observation, err := store.LoadDataPlaneObservation()
	if err != nil {
		t.Fatal(err)
	}
	if observation.RunningGatewayMode != state.GatewayModeSafe || observation.Planes[state.DataPlaneGateway].Running || observation.Workers["w_test"].Running {
		t.Fatalf("worker failure did not publish safe stopped observation: %+v", observation)
	}
}

func TestDataPlaneSupervisorFabricFailureEntersRescue(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	store := state.NewStore(layout)
	rescueManager := rescue.New(layout.RescueStatusPath(), commitlane.New(lockset.New(), layout, owners), owners)
	supervisor := dataPlaneSupervisor{
		store: store, rescuer: rescueManager, rescueReason: "data_plane_unavailable",
	}

	err := supervisor.HandleFabricFailure(context.Background(), errors.New("fabric DNS stopped"))
	if err == nil || !strings.Contains(err.Error(), "fabric DNS stopped") {
		t.Fatalf("fabric failure cause was not returned: %v", err)
	}
	status, err := rescueManager.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Reason != supervisor.rescueReason {
		t.Fatalf("rescue reason=%q want %q", status.Reason, supervisor.rescueReason)
	}
	observation, err := store.LoadDataPlaneObservation()
	if err != nil {
		t.Fatal(err)
	}
	if observation.RunningGatewayMode != state.GatewayModeSafe || observation.Planes[state.DataPlaneGateway].Running {
		t.Fatalf("fabric failure did not publish safe stopped observation: %+v", observation)
	}
}

func TestDataPlaneSupervisorRestartsMissingFabricThenReconciles(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	owners := netgate.NewOwnerRegistry("smartgate")
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOn
	graph := supervisorTestFabricGraph(freeSupervisorTestListen(t))
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{
		GatewayMode: state.GatewayModeOn, ExecutionGraph: state.ExecutionGraph{Fabric: graph},
	}, spec, nil)); err != nil {
		t.Fatal(err)
	}
	manager := fabric.NewManager(owners, supervisorTestRunner{}).WithConnectionTracker(fabric.NoopConnectionTracker{})
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	reconciler := &supervisorTestReconciler{}
	supervisor := dataPlaneSupervisor{
		store: store, commit: commitlane.New(lockset.New(), layout, owners), fabric: manager, reconciler: reconciler,
	}
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !supervisor.restartWait || reconciler.calls != 0 {
		t.Fatalf("first missing-plane poll did not open a restart window: supervisor=%+v reconciles=%d", supervisor, reconciler.calls)
	}
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if supervisor.restartWait || !supervisor.initialized || reconciler.calls != 1 {
		t.Fatalf("healthy restarted plane did not reconcile once: supervisor=%+v reconciles=%d", supervisor, reconciler.calls)
	}
	observation, err := store.LoadDataPlaneObservation()
	if err != nil {
		t.Fatal(err)
	}
	if observation.RunningGatewayMode != state.GatewayModeOn || !observation.Planes[state.DataPlaneGateway].Running {
		t.Fatalf("restarted plane was not observed healthy: %+v", observation)
	}
}

func TestDataPlaneSupervisorRequiresSecondFailureAfterSuccessfulRestart(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOn
	graph := supervisorTestFabricGraph(freeSupervisorTestListen(t))
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{
		GatewayMode: state.GatewayModeOn, ExecutionGraph: state.ExecutionGraph{Fabric: graph},
	}, spec, nil)); err != nil {
		t.Fatal(err)
	}
	manager := fabric.NewManager(owners, supervisorTestRunner{}).WithConnectionTracker(fabric.NoopConnectionTracker{})
	rescueManager := rescue.New(layout.RescueStatusPath(), commitlane.New(lockset.New(), layout, owners), owners)
	supervisor := dataPlaneSupervisor{
		store: store, commit: commitlane.New(lockset.New(), layout, owners), fabric: manager, reconciler: &supervisorTestReconciler{},
		rescuer: rescueManager, rescueReason: "data_plane_unavailable",
	}
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active, err := rescueManager.Active(); err != nil || active {
		t.Fatalf("first failure entered rescue: active=%v err=%v", active, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := supervisor.pollFabric(context.Background())
	var failure *fabricRestartFailure
	if !errors.As(err, &failure) {
		t.Fatalf("second failure did not become a confirmed restart failure: %v", err)
	}
	if active, activeErr := rescueManager.Active(); activeErr != nil || active {
		t.Fatalf("poll entered rescue before the failure handler: active=%v err=%v", active, activeErr)
	}
	if err := supervisor.HandleFabricFailure(context.Background(), err); err == nil {
		t.Fatal("confirmed failure cause was not returned")
	}
	if active, activeErr := rescueManager.Active(); activeErr != nil || !active {
		t.Fatalf("confirmed failure did not enter rescue: active=%v err=%v", active, activeErr)
	}
}

func TestDataPlaneSupervisorStopsUnexpectedWorkersAndFabricInSafeMode(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeSafe
	graph := supervisorTestFabricGraph(freeSupervisorTestListen(t))
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{
		GatewayMode: state.GatewayModeSafe, ExecutionGraph: state.ExecutionGraph{Fabric: graph},
	}, spec, nil)); err != nil {
		t.Fatal(err)
	}
	workers, err := worker.NewManager(runtimeTestWorkerAdapter{core: runtimeCoreA})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := runtimeTestWorkerDescriptors[runtimeCoreA]
	workerID := descriptor.WorkerID(1)
	preparedWorkers := worker.Prepared{
		OpID: "supervisor-safe-window", Candidates: map[string]worker.Candidate{
			workerID: {OpID: "supervisor-safe-window", WorkerID: workerID, Core: runtimeCoreA, CheckOK: true},
		},
		Targets: []state.WorkerTarget{{ID: workerID, Core: runtimeCoreA}}, ActiveTargets: []string{workerID},
	}
	if _, err := workers.Apply(context.Background(), preparedWorkers); err != nil {
		t.Fatal(err)
	}
	manager := fabric.NewManager(owners, supervisorTestRunner{}).WithConnectionTracker(fabric.NoopConnectionTracker{})
	preparedFabric, err := manager.Prepare(context.Background(), "supervisor-safe-fabric", spec, graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), preparedFabric); err != nil {
		t.Fatal(err)
	}
	supervisor := dataPlaneSupervisor{
		store: store, commit: commitlane.New(lockset.New(), layout, owners), workers: workers, fabric: manager,
	}
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, ok := workers.Status(workerID); !ok || status.State != worker.StateStopped {
		t.Fatalf("unexpected worker was not stopped: %+v", status)
	}
	if _, active := manager.Current(); active {
		t.Fatal("unexpected Fabric plane was not stopped")
	}
	observation, err := store.LoadDataPlaneObservation()
	if err != nil {
		t.Fatal(err)
	}
	if observation.Planes[state.DataPlaneGateway].Running || observation.Workers[workerID].Running {
		t.Fatalf("safe-mode observation still reports data-plane activity: %+v", observation)
	}
}

type supervisorTestRunner struct{}

func (supervisorTestRunner) Run(context.Context, []string) (string, error) { return "", nil }

// driftingFabricRunner simulates a host whose owner table can vanish behind
// the daemon's back (firewall flush) and, optionally, come back once the
// reconcile script is executed again.
type driftingFabricRunner struct {
	mu                sync.Mutex
	tableLoaded       bool
	healOnReconcile   bool
	comments          []string
	reconcileRuns     int
	tableListings     int
	otherCommands     int
	lastReconcileSeen int
}

func (runner *driftingFabricRunner) Run(_ context.Context, argv []string) (string, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	joined := strings.Join(argv, " ")
	switch {
	case strings.HasPrefix(joined, "nft -t list table"):
		runner.tableListings++
		if !runner.tableLoaded {
			return "", errors.New("Error: No such file or directory")
		}
		var table strings.Builder
		for _, comment := range runner.comments {
			table.WriteString("comment \"" + comment + "\"\n")
		}
		return table.String(), nil
	case len(argv) > 4 && argv[4] == fabric.FabricReconcilePath:
		runner.reconcileRuns++
		if runner.healOnReconcile {
			runner.tableLoaded = true
		}
		return "", nil
	}
	runner.otherCommands++
	return "", nil
}

func (runner *driftingFabricRunner) setTable(loaded bool, resources []netgate.OwnedResource) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.tableLoaded = loaded
	runner.comments = runner.comments[:0]
	for _, resource := range resources {
		runner.comments = append(runner.comments, resource.Comment)
	}
}

func (runner *driftingFabricRunner) reconciles() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.reconcileRuns
}

func newDriftingSupervisorFixture(t *testing.T, heal bool) (*dataPlaneSupervisor, *driftingFabricRunner, *fabric.Manager, *rescue.Manager) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOn
	graph := supervisorTestFabricGraph(freeSupervisorTestListen(t))
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{
		GatewayMode: state.GatewayModeOn, ExecutionGraph: state.ExecutionGraph{Fabric: graph},
	}, spec, nil)); err != nil {
		t.Fatal(err)
	}
	runner := &driftingFabricRunner{tableLoaded: true, healOnReconcile: heal}
	manager := fabric.NewManager(owners, runner).WithConnectionTracker(fabric.NoopConnectionTracker{})
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	prepared, err := manager.Prepare(context.Background(), "supervisor-drift", spec, graph)
	if err != nil {
		t.Fatal(err)
	}
	runner.setTable(true, prepared.Plan.Resources)
	if _, err := manager.Apply(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	rescueManager := rescue.New(layout.RescueStatusPath(), commitlane.New(lockset.New(), layout, owners), owners)
	supervisor := &dataPlaneSupervisor{
		store: store, commit: commitlane.New(lockset.New(), layout, owners), fabric: manager, reconciler: &supervisorTestReconciler{},
		rescuer: rescueManager, rescueReason: "data_plane_unavailable", verifyHost: true,
	}
	return supervisor, runner, manager, rescueManager
}

func TestDataPlaneSupervisorReassertsOwnerTableAfterHostFlush(t *testing.T) {
	supervisor, runner, manager, rescueManager := newDriftingSupervisorFixture(t, true)
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatal(err)
	}
	applyRuns := runner.reconciles()
	if applyRuns != 1 || supervisor.reasserts != 0 || supervisor.reassertWait {
		t.Fatalf("healthy host must not be reasserted: reconciles=%d supervisor=%+v", applyRuns, supervisor)
	}

	prepared, _ := manager.Current()
	runner.setTable(false, prepared.Plan.Resources)
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatalf("drift poll must reassert instead of failing: %v", err)
	}
	if runner.reconciles() != applyRuns+1 || supervisor.reasserts != 1 || !supervisor.reassertWait {
		t.Fatalf("flushed owner table was not reasserted: reconciles=%d supervisor=%+v", runner.reconciles(), supervisor)
	}
	if _, active := manager.Current(); !active {
		t.Fatal("reassert must not stop the embedded DNS plane")
	}
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if supervisor.reassertWait || runner.reconciles() != applyRuns+1 {
		t.Fatalf("healed host was reasserted again or window not closed: reconciles=%d supervisor=%+v", runner.reconciles(), supervisor)
	}
	if active, err := rescueManager.Active(); err != nil || active {
		t.Fatalf("successful reassert entered rescue: active=%v err=%v", active, err)
	}
}

func TestDataPlaneSupervisorEscalatesPersistentHostDriftToRescue(t *testing.T) {
	supervisor, runner, manager, rescueManager := newDriftingSupervisorFixture(t, false)
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatal(err)
	}
	prepared, _ := manager.Current()
	runner.setTable(false, prepared.Plan.Resources)
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatalf("first drift poll must attempt a reassert: %v", err)
	}
	err := supervisor.pollFabric(context.Background())
	var failure *fabricRestartFailure
	if !errors.As(err, &failure) {
		t.Fatalf("drift surviving a reassert was not confirmed as a failure: %v", err)
	}
	if err := supervisor.HandleFabricFailure(context.Background(), err); err == nil {
		t.Fatal("confirmed drift cause was not returned")
	}
	if active, activeErr := rescueManager.Active(); activeErr != nil || !active {
		t.Fatalf("confirmed drift did not enter rescue: active=%v err=%v", active, activeErr)
	}
}

func TestDataPlaneSupervisorSkipsHostVerificationWhenDisabled(t *testing.T) {
	supervisor, runner, manager, _ := newDriftingSupervisorFixture(t, true)
	supervisor.verifyHost = false
	prepared, _ := manager.Current()
	runner.setTable(false, prepared.Plan.Resources)
	if err := supervisor.pollFabric(context.Background()); err != nil {
		t.Fatal(err)
	}
	if supervisor.reasserts != 0 || runner.reconciles() != 1 {
		t.Fatalf("dev runtime without host verification reasserted: reconciles=%d supervisor=%+v", runner.reconciles(), supervisor)
	}
}

type supervisorTestReconciler struct {
	calls int
	err   error
}

type supervisorTestWorkerRestarter struct {
	calls int
	err   error
	stale bool
}

func (r *supervisorTestWorkerRestarter) RestartFailed(context.Context, string) (bool, error) {
	r.calls++
	return !r.stale, r.err
}

func TestSupervisorWorkerRestartPreservesStateAndBoundsRepeatedCrashes(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOn
	applied := state.AppliedStateWithTarget(state.AppliedState{GatewayMode: state.GatewayModeOn, ConfigEpoch: 42}, spec, nil)
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	restarter, reconciler := &supervisorTestWorkerRestarter{}, &supervisorTestReconciler{}
	supervisor := dataPlaneSupervisor{store: store, commit: commitlane.New(lockset.New(), layout, owners), workerRestarter: restarter, reconciler: reconciler}
	failure := worker.Failure{WorkerID: "worker-test"}
	if recovered, err := supervisor.recoverWorker(context.Background(), failure); !recovered || err != nil {
		t.Fatalf("recovered=%v err=%v", recovered, err)
	}
	if restarter.calls != 1 || reconciler.calls != 1 {
		t.Fatal("restart did not reconcile durable selections")
	}
	observed, _ := store.LoadDataPlaneObservation()
	after, _ := store.LoadAppliedState()
	if !observed.Workers[failure.WorkerID].Running || after.ConfigEpoch != 42 {
		t.Fatal("recovery did not preserve applied epoch")
	}
	if recovered, err := supervisor.recoverWorker(context.Background(), failure); recovered || err != nil || restarter.calls != 1 {
		t.Fatalf("repeat crash bypassed failure policy: %v %v", recovered, err)
	}
	supervisor.workerRestarts[failure.WorkerID] = time.Now().Add(-2 * time.Minute)
	restarter.err = errors.New("persistent failure")
	if recovered, err := supervisor.recoverWorker(context.Background(), failure); recovered || err == nil {
		t.Fatal("failed restart must use fallback")
	}
	if reconciler.calls != 1 {
		t.Fatal("reconciled failed process")
	}
}

func TestSupervisorIgnoresQueuedWorkerFailureWhenDisabled(t *testing.T) {
	for _, mode := range []string{state.GatewayModeOff, state.GatewayModeSafe} {
		t.Run(mode, func(t *testing.T) {
			layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
			if err := layout.Ensure(); err != nil {
				t.Fatal(err)
			}
			store := state.NewStore(layout)
			spec := state.DefaultSpecState()
			spec.Settings.GatewayMode = mode
			if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{GatewayMode: mode}, spec, nil)); err != nil {
				t.Fatal(err)
			}
			restarter := &supervisorTestWorkerRestarter{}
			supervisor := dataPlaneSupervisor{store: store, commit: commitlane.New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")), workerRestarter: restarter, reconciler: &supervisorTestReconciler{}}
			if handled, err := supervisor.recoverWorker(context.Background(), worker.Failure{WorkerID: "worker-test"}); !handled || err != nil || restarter.calls != 0 {
				t.Fatalf("disabled gateway restarted worker: %v %v", handled, err)
			}
		})
	}
}

func (reconciler *supervisorTestReconciler) ReconcileLiveSelectionsLocked(context.Context) error {
	reconciler.calls++
	return reconciler.err
}

func freeSupervisorTestListen(t *testing.T) string {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := packet.LocalAddr().String()
	if err := packet.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func supervisorTestFabricGraph(listen string) state.FabricGraph {
	return state.FabricGraph{
		Takeover: state.FabricTakeoverGraph{Enabled: true, TCP: true, UDP: true, IPv4: true, IPv6: true},
		DNS: state.FabricDNSGraph{
			Listen: listen, RedirectLAN53: true, RejectHTTPSRecords: true, BlockDoT853: true,
			PoolSets: map[string]state.FabricPoolSets{
				fabric.TargetDirect: {IPv4: "direct_4", IPv6: "direct_6"},
				fabric.TargetBlock:  {IPv4: "block_4", IPv6: "block_6"},
				"general":           {IPv4: "general_4", IPv6: "general_6"},
			},
		},
		FallbackPool: "general",
		Dispatch: []state.PoolDispatchGraph{{
			Pool: "general", Binding: "binding-general", Generation: 1, Mark: 0x50010001,
			MarkMask: 0xffffffff, Table: 1001, Port: 16000, DNSPort: 18000, Target: fabric.TargetDirect,
		}},
	}
}

func TestDataPlaneSupervisorPollFabricRequiresDependencies(t *testing.T) {
	if err := (*dataPlaneSupervisor)(nil).pollFabric(context.Background()); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("nil supervisor did not fail loud: %v", err)
	}
	if err := (&dataPlaneSupervisor{}).pollFabric(context.Background()); err == nil || !strings.Contains(err.Error(), "store is required") {
		t.Fatalf("missing store did not fail loud: %v", err)
	}
	layout := state.Layout{}
	store := state.NewStore(layout)
	if err := (&dataPlaneSupervisor{store: store}).pollFabric(context.Background()); err == nil || !strings.Contains(err.Error(), "CommitLane is required") {
		t.Fatalf("missing CommitLane did not fail loud: %v", err)
	}
	commit := commitlane.New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate"))
	if err := (&dataPlaneSupervisor{store: store, commit: commit}).pollFabric(context.Background()); err == nil || !strings.Contains(err.Error(), "Fabric manager is required") {
		t.Fatalf("missing Fabric manager did not fail loud: %v", err)
	}
}

func TestDataPlaneSupervisorSerializesPollWithCommitLockOrder(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	locks := lockset.New()
	owners := netgate.NewOwnerRegistry("smartgate")
	store := state.NewStore(layout)
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeSafe
	if err := store.SaveAppliedState(state.AppliedStateWithTarget(state.AppliedState{GatewayMode: state.GatewayModeSafe}, spec, nil)); err != nil {
		t.Fatal(err)
	}
	supervisor := dataPlaneSupervisor{
		store: store, commit: commitlane.New(locks, layout, owners),
		fabric: fabric.NewManager(owners, supervisorTestRunner{}),
	}
	session := locks.Session("hold-store-before-supervisor")
	release, err := session.Acquire(lockset.LevelStore)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- supervisor.pollFabric(context.Background()) }()
	select {
	case err := <-done:
		release()
		t.Fatalf("supervisor bypassed the Store lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not resume after the Store lock was released")
	}
}

func TestProbeFabricTCPUsesWildcardLoopback(t *testing.T) {
	listener, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
		close(accepted)
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := probeFabricTCP(context.Background(), net.JoinHostPort("::", fmt.Sprint(port))); err != nil {
		t.Fatal(err)
	}
	<-accepted
}

func TestDataPlaneSupervisorRequiresRescueReason(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	owners := netgate.NewOwnerRegistry("smartgate")
	supervisor := dataPlaneSupervisor{
		store:   state.NewStore(layout),
		rescuer: rescue.New(layout.RescueStatusPath(), commitlane.New(lockset.New(), layout, owners), owners),
	}

	err := supervisor.HandleWorkerFailure(context.Background(), worker.Failure{WorkerID: "w_test", Err: errors.New("worker crashed")})
	if err == nil || !strings.Contains(err.Error(), "data-plane rescue reason is required") {
		t.Fatalf("missing rescue reason was not a hard failure: %v", err)
	}
	active, activeErr := supervisor.rescuer.Active()
	if activeErr != nil {
		t.Fatal(activeErr)
	}
	if active {
		t.Fatal("rescue entered without a configured reason")
	}
}
