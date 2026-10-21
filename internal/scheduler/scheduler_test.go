package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type fakeStore struct {
	spec     state.SpecState
	topology state.AppliedTopology
	runtime  state.RuntimeState
}

func (s fakeStore) LoadSpec() (state.SpecState, error) {
	return s.spec, nil
}

func (s fakeStore) LoadAppliedTopology() (state.AppliedTopology, error) {
	if s.topology.GatewayMode == "" {
		s.topology.GatewayMode = s.spec.Settings.GatewayMode
	}
	return s.topology, nil
}

func (s *fakeStore) LoadRuntime() (state.RuntimeState, error) {
	return s.runtime, nil
}

func (s *fakeStore) SaveRuntime(runtime state.RuntimeState) error {
	s.runtime = runtime
	return nil
}

func (s *fakeStore) MergeNodeHealth(health map[string]state.NodeHealth) error {
	if s.runtime.NodeHealth == nil {
		s.runtime.NodeHealth = map[string]state.NodeHealth{}
	}
	for nodeID, item := range health {
		s.runtime.NodeHealth[nodeID] = item
	}
	return nil
}

type fakeDelayTester struct {
	calls []string
	err   error
}

func (t *fakeDelayTester) Delay(_ context.Context, outboundTag, testURL string, _ time.Duration) (DelayResult, error) {
	t.calls = append(t.calls, outboundTag)
	if t.err != nil {
		return DelayResult{}, t.err
	}
	return DelayResult{DelayMS: 77, URL: testURL}, nil
}

func schedulerTestAppliedTopology(t *testing.T, layout state.Layout, spec state.SpecState) state.AppliedTopology {
	t.Helper()
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(context.Background(), provider.NewLastGoodStore(layout))
	if err != nil {
		t.Fatalf("load test last-good: %v", err)
	}
	return state.AppliedTopology{GatewayMode: spec.Settings.GatewayMode, Spec: spec, LastGood: snapshots}
}

type fakeBackupCreator struct {
	calls int
	opID  string
	kind  string
}

func (c *fakeBackupCreator) Create(_ context.Context, opID, kind string) (BackupResult, error) {
	c.calls++
	c.opID = opID
	c.kind = kind
	return BackupResult{
		ID:        kind + "-1",
		Path:      "/etc/smartgate/backups/" + kind + "-1.tar.gz",
		Size:      1234,
		SHA256:    "abc123",
		CreatedAt: time.Now().UTC(),
	}, nil
}

type fakeConfigApplier struct {
	calls int
	opID  string
}

func (a *fakeConfigApplier) Apply(_ context.Context, opID string) (ConfigApplyResult, error) {
	a.calls++
	a.opID = opID
	return ConfigApplyResult{
		DryRunID:          opID,
		Status:            "applied",
		ConfigEpoch:       2,
		TopologyHash:      "hash-2",
		WorkerConfigPaths: map[string]string{"worker-a": "/etc/smartgate/workers/worker-a/config.json"},
		OpenWrtApplied:    true,
		OpenWrtSteps:      5,
	}, nil
}

type fakeServiceRestarter struct {
	calls   int
	service string
}

func (r *fakeServiceRestarter) Restart(_ context.Context, service string) (ServiceRestartResult, error) {
	r.calls++
	r.service = service
	return ServiceRestartResult{
		Service: service,
		Action:  "restart",
		Status:  "ok",
		Argv:    []string{"/etc/init.d/" + service, "restart"},
		Output:  "restarted",
	}, nil
}

type fakeRouterRebooter struct {
	calls int
}

func (r *fakeRouterRebooter) Reboot(_ context.Context) (RouterRebootResult, error) {
	r.calls++
	return RouterRebootResult{
		Action: "reboot",
		Status: "ok",
		Argv:   []string{"reboot"},
		Output: "rebooting",
	}, nil
}

type blockingFetcher struct {
	started chan struct{}
	release chan struct{}
}

func (f blockingFetcher) Fetch(_ context.Context, req provider.FetchRequest) (provider.FetchResponse, error) {
	close(f.started)
	<-f.release
	return provider.FetchResponse{StatusCode: http.StatusOK, Body: []byte(`[{"type":"vmess","tag":"node-1","server":"example.com"}]`)}, nil
}

type fakeFetcher struct {
	calls int
	body  []byte
}

func (f *fakeFetcher) Fetch(_ context.Context, _ provider.FetchRequest) (provider.FetchResponse, error) {
	f.calls++
	return provider.FetchResponse{StatusCode: http.StatusOK, Body: f.body}, nil
}

func TestRunNowRejectsConcurrentSameTask(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	fetcher := blockingFetcher{started: make(chan struct{}), release: make(chan struct{})}
	lastGood := provider.NewLastGoodStore(testLayout(t))
	runner := NewRunner(fakeStore{spec: spec}, provider.NewRefreshService(lastGood, fetcher))
	errCh := make(chan error, 1)
	go func() {
		_, err := runner.RunNow(context.Background(), TaskProviderRefresh)
		errCh <- err
	}()
	<-fetcher.started

	_, err := runner.RunNow(context.Background(), TaskProviderRefresh)
	if !errors.Is(err, ErrTaskRunning) {
		t.Fatalf("expected ErrTaskRunning, got %v", err)
	}
	close(fetcher.release)
	if err := <-errCh; err != nil {
		t.Fatalf("first run should finish: %v", err)
	}
}

type fakeFailoverRunner struct {
	calls int
}

func (r *fakeFailoverRunner) Run(_ context.Context) (FailoverResult, error) {
	r.calls++
	return FailoverResult{Decisions: 2, Applied: 1, Events: 1}, nil
}

func TestRunNowProviderRefreshBlockedWhenGatewayOff(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = "off"
	runner := NewRunner(fakeStore{spec: spec}, provider.NewRefreshService(provider.NewLastGoodStore(testLayout(t)), nil))
	_, err := runner.RunNow(context.Background(), TaskProviderRefresh)
	if !errors.Is(err, ErrGatewayOff) {
		t.Fatalf("expected ErrGatewayOff, got %v", err)
	}
}

func TestRunNowProviderRefreshAllowedWhenGatewaySafe(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeSafe
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      provider.TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: provider.FetchDirect,
	}}
	fetcher := &fakeFetcher{body: []byte(`[{"type":"vmess","tag":"node-1","server":"example.com"}]`)}
	runner := NewRunner(fakeStore{spec: spec}, provider.NewRefreshService(provider.NewLastGoodStore(testLayout(t)), fetcher))

	if _, err := runner.RunNow(context.Background(), TaskProviderRefresh); err != nil {
		t.Fatalf("safe mode should allow provider refresh, got %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected one fetch in safe mode, got %d", fetcher.calls)
	}
}

func TestRunNowDelayTestPersistsNodeHealth(t *testing.T) {
	layout := testLayout(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	if _, err := provider.NewLastGoodStore(layout).Save("p1", []map[string]any{
		{"type": "vmess", "tag": "node-1", "server": "example.com", "server_port": 443},
		{"type": "naive", "tag": "bad", "server": "bad.example"},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	store := &fakeStore{spec: spec, topology: schedulerTestAppliedTopology(t, layout, spec)}
	tester := &fakeDelayTester{}
	runner := NewRunner(store, nil).WithDelayTester(layout, store, tester)

	result, err := runner.RunNow(context.Background(), TaskDelayTest)
	if err != nil {
		t.Fatalf("delay test: %v", err)
	}
	if result.DelayTest == nil || result.DelayTest.Tested != 1 || result.DelayTest.OK != 1 {
		t.Fatalf("unexpected delay result: %+v", result.DelayTest)
	}
	if len(tester.calls) != 1 {
		t.Fatalf("delay tester calls = %+v", tester.calls)
	}
	for nodeID, health := range store.runtime.NodeHealth {
		if health.Status != "ok" || health.DelayMS != 77 || health.TestURL == "" || health.LastTest == nil {
			t.Fatalf("health for %s not persisted: %+v", nodeID, health)
		}
		return
	}
	t.Fatalf("node health not persisted")
}

func TestRunNowDelayTestRecordsFailures(t *testing.T) {
	layout := testLayout(t)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	if _, err := provider.NewLastGoodStore(layout).Save("p1", []map[string]any{
		{"type": "trojan", "tag": "node-1", "server": "example.com"},
	}); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	store := &fakeStore{spec: spec, topology: schedulerTestAppliedTopology(t, layout, spec)}
	runner := NewRunner(store, nil).WithDelayTester(layout, store, &fakeDelayTester{err: fmt.Errorf("timeout")})

	result, err := runner.RunNow(context.Background(), TaskDelayTest)
	if err != nil {
		t.Fatalf("delay test should persist failures without failing task: %v", err)
	}
	if result.DelayTest == nil || result.DelayTest.Failed != 1 {
		t.Fatalf("unexpected delay result: %+v", result.DelayTest)
	}
	for _, health := range store.runtime.NodeHealth {
		if health.Status != "failed" || health.Error == "" {
			t.Fatalf("failure health not persisted: %+v", health)
		}
		return
	}
	t.Fatalf("node health not persisted")
}

func TestRunNowBackupUsesCreator(t *testing.T) {
	creator := &fakeBackupCreator{}
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil).WithBackupCreator(creator)

	result, err := runner.RunNow(context.Background(), TaskBackup)
	if err != nil {
		t.Fatalf("backup run: %v", err)
	}
	if creator.calls != 1 || creator.kind != "scheduled" || creator.opID == "" {
		t.Fatalf("backup creator not called correctly: %+v", creator)
	}
	if result.Backup == nil || result.Backup.ID != "scheduled-1" || result.Backup.Size == 0 {
		t.Fatalf("backup result not returned: %+v", result.Backup)
	}
}

func TestRunNowBackupWithoutCreatorNotRunnable(t *testing.T) {
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil)
	_, err := runner.RunNow(context.Background(), TaskBackup)
	if !errors.Is(err, ErrTaskNotRunnable) {
		t.Fatalf("expected ErrTaskNotRunnable, got %v", err)
	}
}

func TestRunNowConfigApplyUsesApplier(t *testing.T) {
	applier := &fakeConfigApplier{}
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil).WithConfigApplier(applier)

	result, err := runner.RunNow(context.Background(), TaskApplyConfig)
	if err != nil {
		t.Fatalf("config apply run: %v", err)
	}
	if applier.calls != 1 || applier.opID == "" {
		t.Fatalf("config applier not called correctly: %+v", applier)
	}
	if result.Config == nil || result.Config.ConfigEpoch != 2 || result.Config.OpenWrtSteps != 5 {
		t.Fatalf("config apply result not returned: %+v", result.Config)
	}
	if got := result.Config.ConfigPathForWorker("worker-a"); got != "/etc/smartgate/workers/worker-a/config.json" {
		t.Fatalf("worker config path = %q", got)
	}
}

func TestRunNowConfigApplyWithoutApplierNotRunnable(t *testing.T) {
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil)
	_, err := runner.RunNow(context.Background(), TaskApplyConfig)
	if !errors.Is(err, ErrTaskNotRunnable) {
		t.Fatalf("expected ErrTaskNotRunnable, got %v", err)
	}
}

func TestRunNowLegacyRestartAliasUsesRestarter(t *testing.T) {
	restarter := &fakeServiceRestarter{}
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil).WithServiceRestarter(restarter)
	legacyService := state.SchedulerTaskRestartDefaultService()

	result, err := runner.RunNow(context.Background(), legacyRestartServiceTaskType())
	if err != nil {
		t.Fatalf("restart sing-box run: %v", err)
	}
	if restarter.calls != 1 || restarter.service != legacyService {
		t.Fatalf("restarter not called correctly: %+v", restarter)
	}
	if result.Service == nil || result.Service.Service != legacyService || result.Service.Action != "restart" {
		t.Fatalf("service restart result not returned: %+v", result.Service)
	}
	if result.Task != TaskRestartService {
		t.Fatalf("legacy restart alias should return canonical task type, got %q", result.Task)
	}
	if result.Refresh != nil {
		t.Fatalf("restart_singbox should not fall through to provider refresh: %+v", result.Refresh)
	}
}

func TestRunTaskRestartServiceUsesSpecService(t *testing.T) {
	restarter := &fakeServiceRestarter{}
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil).WithServiceRestarter(restarter)

	result, err := runner.RunTask(context.Background(), state.SchedulerTaskSpec{
		Type: TaskRestartService,
		Spec: map[string]any{state.SchedulerTaskRestartServiceSpecKey: "smartgate"},
	})
	if err != nil {
		t.Fatalf("restart service run: %v", err)
	}
	if restarter.calls != 1 || restarter.service != "smartgate" {
		t.Fatalf("restarter not called with spec service: %+v", restarter)
	}
	if result.Service == nil || result.Service.Service != "smartgate" {
		t.Fatalf("service result not returned: %+v", result.Service)
	}
}

func TestRunTaskRestartServiceWithoutServiceNotRunnable(t *testing.T) {
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil).WithServiceRestarter(&fakeServiceRestarter{})
	_, err := runner.RunTask(context.Background(), state.SchedulerTaskSpec{Type: TaskRestartService})
	if !errors.Is(err, ErrTaskNotRunnable) {
		t.Fatalf("expected ErrTaskNotRunnable, got %v", err)
	}
}

func TestRunNowLegacyRestartAliasWithoutRestarterNotRunnable(t *testing.T) {
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil)
	_, err := runner.RunNow(context.Background(), legacyRestartServiceTaskType())
	if !errors.Is(err, ErrTaskNotRunnable) {
		t.Fatalf("expected ErrTaskNotRunnable, got %v", err)
	}
}

func TestNormalizeTaskTypeAliases(t *testing.T) {
	normalized, err := NormalizeTaskType("refresh_provider")
	if err != nil || normalized != TaskProviderRefresh {
		t.Fatalf("refresh alias normalized to %q err=%v", normalized, err)
	}
	normalized, err = NormalizeTaskType(legacyRestartServiceTaskType())
	if err != nil || normalized != TaskRestartService {
		t.Fatalf("restart alias normalized to %q err=%v", normalized, err)
	}
	for _, task := range KnownTaskTypes() {
		if task == legacyRestartServiceTaskType() {
			t.Fatalf("known task types must expose canonical values only: %+v", KnownTaskTypes())
		}
	}
}

func TestRunNowRebootRouterUsesRebooter(t *testing.T) {
	rebooter := &fakeRouterRebooter{}
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil).WithRouterRebooter(rebooter)

	result, err := runner.RunNow(context.Background(), TaskRebootRouter)
	if err != nil {
		t.Fatalf("reboot router run: %v", err)
	}
	if rebooter.calls != 1 {
		t.Fatalf("rebooter not called correctly: %+v", rebooter)
	}
	if result.Router == nil || result.Router.Action != "reboot" || result.Router.Status != "ok" {
		t.Fatalf("router reboot result not returned: %+v", result.Router)
	}
	if result.Refresh != nil {
		t.Fatalf("reboot_router should not fall through to provider refresh: %+v", result.Refresh)
	}
}

func TestRunNowRebootRouterWithoutRebooterNotRunnable(t *testing.T) {
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil)
	_, err := runner.RunNow(context.Background(), TaskRebootRouter)
	if !errors.Is(err, ErrTaskNotRunnable) {
		t.Fatalf("expected ErrTaskNotRunnable, got %v", err)
	}
}

func TestRunNowFailoverWatchdogUsesRunner(t *testing.T) {
	failover := &fakeFailoverRunner{}
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil).WithFailoverRunner(failover)

	result, err := runner.RunNow(context.Background(), TaskFailoverWatchdog)
	if err != nil {
		t.Fatalf("failover watchdog run: %v", err)
	}
	if failover.calls != 1 {
		t.Fatalf("failover runner not called correctly: %+v", failover)
	}
	if result.Failover == nil || result.Failover.Decisions != 2 || result.Failover.Applied != 1 || result.Failover.Events != 1 {
		t.Fatalf("failover result not returned: %+v", result.Failover)
	}
	if result.Refresh != nil {
		t.Fatalf("failover_watchdog should not fall through to provider refresh: %+v", result.Refresh)
	}
}

func TestRunNowFailoverWatchdogWithoutRunnerNotRunnable(t *testing.T) {
	runner := NewRunner(fakeStore{spec: state.DefaultSpecState()}, nil)
	_, err := runner.RunNow(context.Background(), TaskFailoverWatchdog)
	if !errors.Is(err, ErrTaskNotRunnable) {
		t.Fatalf("expected ErrTaskNotRunnable, got %v", err)
	}
}

func TestRunNowFailoverWatchdogBlockedWhenGatewayOff(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = "off"
	failover := &fakeFailoverRunner{}
	runner := NewRunner(fakeStore{spec: spec}, nil).WithFailoverRunner(failover)

	_, err := runner.RunNow(context.Background(), TaskFailoverWatchdog)
	if !errors.Is(err, ErrGatewayOff) {
		t.Fatalf("expected ErrGatewayOff, got %v", err)
	}
	if failover.calls != 0 {
		t.Fatalf("failover runner should not be called when gateway is off: %+v", failover)
	}
}

func TestRunNowFailoverWatchdogBlockedWhenGatewaySafe(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeSafe
	failover := &fakeFailoverRunner{}
	runner := NewRunner(fakeStore{spec: spec}, nil).WithFailoverRunner(failover)

	_, err := runner.RunNow(context.Background(), TaskFailoverWatchdog)
	if !errors.Is(err, ErrGatewayOff) {
		t.Fatalf("expected ErrGatewayOff, got %v", err)
	}
	if failover.calls != 0 {
		t.Fatalf("failover runner should not be called in safe mode: %+v", failover)
	}
}

func TestRunNowFailoverWatchdogUsesAppliedModeWhenSavedModeIsOff(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = state.GatewayModeOff
	failover := &fakeFailoverRunner{}
	runner := NewRunner(fakeStore{
		spec:     spec,
		topology: state.AppliedTopology{GatewayMode: state.GatewayModeOn},
	}, nil).WithFailoverRunner(failover)

	if _, err := runner.RunNow(context.Background(), TaskFailoverWatchdog); err != nil {
		t.Fatalf("saved off must not block applied-on watchdog: %v", err)
	}
	if failover.calls != 1 {
		t.Fatalf("applied-on watchdog was not run: %+v", failover)
	}
}

func TestNextRunEvery(t *testing.T) {
	from := time.Date(2026, 6, 25, 10, 15, 0, 0, time.UTC)
	next, err := NextRun("@every 4h", from)
	if err != nil {
		t.Fatalf("next run: %v", err)
	}
	want := time.Date(2026, 6, 25, 14, 15, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next run = %s want %s", next, want)
	}
}

func TestNextRunCronDayOfMonthOrDayOfWeek(t *testing.T) {
	from := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	next, err := NextRun("0 9 25 * 5", from)
	if err != nil {
		t.Fatalf("next run: %v", err)
	}
	want := time.Date(2026, 6, 25, 1, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next run = %s want %s", next, want)
	}
}

func TestNextRunCronUsesAsiaShanghaiWallClock(t *testing.T) {
	from := time.Date(2026, 6, 24, 20, 0, 0, 0, time.UTC)
	next, err := NextRun("0 5 * * *", from)
	if err != nil {
		t.Fatalf("next run: %v", err)
	}
	want := time.Date(2026, 6, 24, 21, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next run = %s want %s (05:00 %s)", next, want, CronTimeZone)
	}
}

func TestNextRunCronStep(t *testing.T) {
	from := time.Date(2026, 6, 25, 10, 7, 0, 0, time.UTC)
	next, err := NextRun("*/15 * * * *", from)
	if err != nil {
		t.Fatalf("next run: %v", err)
	}
	want := time.Date(2026, 6, 25, 10, 15, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next run = %s want %s", next, want)
	}
}

func testLayout(t *testing.T) state.Layout {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	return layout
}
