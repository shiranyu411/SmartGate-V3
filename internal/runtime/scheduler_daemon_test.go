package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/scheduler"
	"smartgate/internal/state"
)

func TestSchedulerDaemonRunsDueProviderRefresh(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	commitMgr := commitlane.New(locks, layout, netgate.NewOwnerRegistry("smartgate"))
	fetcher := &schedulerFetcher{}
	refresher := provider.NewRefreshService(commitlane.NewLastGoodTxn(commitMgr, provider.NewLastGoodStore(layout)), fetcher)
	runner := scheduler.NewRunner(store, refresher)
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	spec := state.DefaultSpecState()
	spec.Scheduler = []state.SchedulerTaskSpec{{
		ID:       "provider-auto-refresh",
		Name:     "Provider auto refresh",
		Type:     scheduler.TaskProviderRefresh,
		Enabled:  true,
		Schedule: "@every 1h",
	}}
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
	due := now.Add(-time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{
		Scheduler: map[string]state.SchedulerTaskRuntime{
			"provider-auto-refresh": {NextRun: &due},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	daemon := newSchedulerDaemon(store, locks, runner)
	if err := daemon.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected provider refresh, got %d calls", fetcher.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["provider-auto-refresh"]
	if record.LastStatus != "ok" || record.LastRun == nil || record.NextRun == nil || !record.NextRun.After(*record.LastRun) {
		t.Fatalf("unexpected runtime record: %+v", record)
	}
}

func TestSchedulerRunStatusReportsPartialProviderFailureAsDegraded(t *testing.T) {
	result := scheduler.Result{Refresh: &provider.RefreshResult{Results: []provider.RefreshStepResult{
		{ProviderID: "ok", Status: "ok"},
		{ProviderID: "failed", Status: "failed", Error: "timeout"},
	}}}
	if got := schedulerRunStatus(result); got != "degraded" {
		t.Fatalf("partial provider refresh status=%q, want degraded", got)
	}
	if output := schedulerRunOutput(result); !strings.Contains(output, "成功 1 个") || !strings.Contains(output, "失败 1 个") {
		t.Fatalf("partial provider refresh output is ambiguous: %q", output)
	}
}

func TestSchedulerRunStatusReportsPartialFailoverAsDegraded(t *testing.T) {
	result := scheduler.Result{Task: scheduler.TaskFailoverWatchdog, Failover: &scheduler.FailoverResult{
		Decisions: 2, Applied: 1, Failed: 1, Error: "streaming selector failed",
	}}
	if got := schedulerRunStatus(result); got != "degraded" {
		t.Fatalf("partial failover status=%q, want degraded", got)
	}
	output := schedulerRunOutput(result)
	if !strings.Contains(output, "已应用 1 个") || !strings.Contains(output, "失败 1 个") || strings.Contains(output, "streaming selector failed") {
		t.Fatalf("partial failover output is ambiguous: %q", output)
	}
}

func TestSchedulerOutputLocalizesLegacyRecords(t *testing.T) {
	legacy := "provider_refresh completed: providers=3 ok=2 failed=1"
	if got := schedulerOutputText(scheduler.TaskProviderRefresh, "degraded", legacy); got != "订阅刷新完成：共 3 个，成功 2 个，失败 1 个" {
		t.Fatalf("legacy output=%q", got)
	}
	if got := schedulerOutputText(scheduler.TaskBackup, "failed", "open /tmp/backup: permission denied"); got != "备份执行失败" {
		t.Fatalf("legacy failure output=%q", got)
	}
}

func TestSchedulerDaemonInitializesMissingNextRun(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	runner := scheduler.NewRunner(store, nil)
	spec := state.DefaultSpecState()
	spec.Scheduler = []state.SchedulerTaskSpec{{
		ID:       "node-delay-test",
		Name:     "Node delay test",
		Type:     scheduler.TaskDelayTest,
		Enabled:  true,
		Schedule: "@every 1h",
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	daemon := newSchedulerDaemon(store, locks, runner)
	if err := daemon.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["node-delay-test"]
	if record.NextRun == nil || !record.NextRun.After(now) || record.LastRun != nil {
		t.Fatalf("expected next_run initialization only, got %+v", record)
	}
}

func TestSchedulerDaemonExpiredClaimRequeuesWithoutSkippingDueRun(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	locks := lockset.New()
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	task := state.SchedulerTaskSpec{ID: "recover-me", Type: scheduler.TaskDelayTest, Enabled: true, Schedule: "@every 1h"}
	spec := state.DefaultSpecState()
	spec.Scheduler = []state.SchedulerTaskSpec{task}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	due := now.Add(-time.Minute)
	expiredClaim := now.Add(-2 * schedulerClaimLease)
	expiredDeadline := now.Add(-schedulerClaimLease)
	if err := store.SaveRuntime(state.RuntimeState{Scheduler: map[string]state.SchedulerTaskRuntime{
		task.ID: {NextRun: &due, LastStatus: "running", ClaimedAt: &expiredClaim, ClaimDeadline: &expiredDeadline},
	}}); err != nil {
		t.Fatal(err)
	}
	daemon := newSchedulerDaemon(store, locks, scheduler.NewRunner(store, nil))
	dueTasks, err := daemon.claimDue(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(dueTasks) != 1 || dueTasks[0].task.ID != task.ID {
		t.Fatalf("expired claimed task was not requeued: %+v", dueTasks)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeState.Scheduler[task.ID]
	if record.NextRun == nil || !record.NextRun.Equal(due) {
		t.Fatalf("claim advanced next run before completion: %+v", record)
	}
	if record.ClaimDeadline == nil || !record.ClaimDeadline.After(now) {
		t.Fatalf("requeued task did not receive a fresh lease: %+v", record)
	}
}

func TestSchedulerDaemonLegacyRunningWithFutureNextRunIsRequeued(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	task := state.SchedulerTaskSpec{ID: "legacy", Type: scheduler.TaskDelayTest, Enabled: true, Schedule: "@every 1h"}
	spec := state.DefaultSpecState()
	spec.Scheduler = []state.SchedulerTaskSpec{task}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	future := now.Add(time.Hour)
	if err := store.SaveRuntime(state.RuntimeState{Scheduler: map[string]state.SchedulerTaskRuntime{task.ID: {NextRun: &future, LastStatus: "running"}}}); err != nil {
		t.Fatal(err)
	}
	due, err := newSchedulerDaemon(store, lockset.New(), scheduler.NewRunner(store, nil)).claimDue(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("legacy running task was skipped: %+v", due)
	}
}

func TestSchedulerDaemonRebootAdvancesScheduleBeforeDispatch(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	task := state.SchedulerTaskSpec{ID: "reboot", Type: scheduler.TaskRebootRouter, Enabled: true, Schedule: "@every 1h"}
	spec := state.DefaultSpecState()
	spec.Scheduler = []state.SchedulerTaskSpec{task}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	dueAt := now.Add(-time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{Scheduler: map[string]state.SchedulerTaskRuntime{task.ID: {NextRun: &dueAt}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := newSchedulerDaemon(store, lockset.New(), scheduler.NewRunner(store, nil)).claimDue(now); err != nil {
		t.Fatal(err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeState.Scheduler[task.ID]
	if record.NextRun == nil || !record.NextRun.After(now) {
		t.Fatalf("reboot schedule was not advanced before dispatch: %+v", record)
	}
}

func TestServerDeploymentGateDisablesAutomaticScheduler(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000", Layout: layout, Locks: lockset.New(), Store: store,
		Scheduler: scheduler.NewRunner(store, nil), SchedulerDisabled: true,
		Status: NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	if server.scheduler != nil {
		t.Fatal("deployment gate should suppress automatic scheduler daemon")
	}
}

func TestSchedulerDaemonRunsDueBackup(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	commitMgr := commitlane.New(locks, layout, netgate.NewOwnerRegistry("smartgate"))
	backupTxn := commitlane.NewBackupTxn(commitMgr, store, state.NewSnapshotStore(layout), layout)
	runner := scheduler.NewRunner(store, nil).WithBackupCreator(testSchedulerBackupCreator{txn: backupTxn})
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	spec := state.DefaultSpecState()
	spec.Scheduler = []state.SchedulerTaskSpec{{
		ID:       "daily-backup",
		Name:     "Daily backup",
		Type:     scheduler.TaskBackup,
		Enabled:  true,
		Schedule: "0 5 * * *",
	}}
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	due := now.Add(-time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{
		Scheduler: map[string]state.SchedulerTaskRuntime{
			"daily-backup": {NextRun: &due},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}

	daemon := newSchedulerDaemon(store, locks, runner)
	if err := daemon.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	archives, err := filepath.Glob(filepath.Join(layout.BackupsDir(), "scheduled-*.tar.gz"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("expected one scheduled backup archive, got %+v", archives)
	}
	info, err := os.Stat(archives[0])
	if err != nil || info.Size() == 0 {
		t.Fatalf("backup archive not written: info=%+v err=%v", info, err)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["daily-backup"]
	if record.LastStatus != "ok" || record.LastRun == nil || record.NextRun == nil || !strings.Contains(record.LastOutput, "备份 ID") {
		t.Fatalf("unexpected runtime record: %+v", record)
	}
}

func TestSchedulerDaemonRunsDueApplyConfig(t *testing.T) {
	server, store, netGateRunner := newSchedulerConfigTestServer(t)
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	spec := state.DefaultSpecState()
	spec.Scheduler = []state.SchedulerTaskSpec{{
		ID:       "apply-now",
		Name:     "Apply now",
		Type:     scheduler.TaskApplyConfig,
		Enabled:  true,
		Schedule: "@every 1h",
	}}
	configureRuntimeGraphReadyManualProvider(t, server.cfg.Layout, &spec, "manual")
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	due := now.Add(-time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{
		Scheduler: map[string]state.SchedulerTaskRuntime{
			"apply-now": {NextRun: &due},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	if err := server.scheduler.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	if netGateRunner.calls == 0 {
		t.Fatalf("expected graph/Fabric apply, openwrt=%d", netGateRunner.calls)
	}
	appliedState, err := store.LoadAppliedState()
	if err != nil {
		t.Fatalf("load applied state: %v", err)
	}
	if len(appliedState.ExecutionGraph.Workers) == 0 || !server.cfg.Workers.IsRunning(appliedState.ExecutionGraph.Workers[0].ID) {
		t.Fatalf("scheduled apply did not activate graph worker: %+v", appliedState.ExecutionGraph.Workers)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["apply-now"]
	if record.LastStatus != "ok" || record.LastRun == nil || record.NextRun == nil || !strings.Contains(record.LastOutput, "OpenWrt 步骤") {
		t.Fatalf("unexpected runtime record: %+v", record)
	}
}

func TestSchedulerDaemonRunsDueServiceRestart(t *testing.T) {
	serviceName := state.SchedulerTaskRestartDefaultService()
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}): {output: "restarted"},
	}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{
		Scheduler: map[string]state.SchedulerTaskRuntime{
			"nightly-service-restart": {NextRun: &due},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	idx := schedulerTaskIndex(spec.Scheduler, "nightly-service-restart")
	if idx < 0 {
		t.Fatalf("default restart task missing: %+v", spec.Scheduler)
	}
	spec.Scheduler[idx].Enabled = true
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	if err := server.scheduler.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	if len(systemRunner.calls) != 1 || systemTestKey(systemRunner.calls[0]) != systemTestKey([]string{"/etc/init.d/" + serviceName, "restart"}) {
		t.Fatalf("unexpected system runner calls: %+v", systemRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["nightly-service-restart"]
	if record.LastStatus != "ok" || record.LastRun == nil || record.NextRun == nil || !strings.Contains(record.LastOutput, serviceName+" 服务重启完成") {
		t.Fatalf("unexpected runtime record: %+v", record)
	}
}

func TestSchedulerDaemonDoesNotRunDisabledRouterReboot(t *testing.T) {
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{
		Scheduler: map[string]state.SchedulerTaskRuntime{
			"weekly-router-reboot": {NextRun: &due},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}

	if err := server.scheduler.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	if len(systemRunner.calls) != 0 {
		t.Fatalf("disabled reboot should not run: %+v", systemRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["weekly-router-reboot"]
	if record.LastRun != nil || record.LastStatus != "" {
		t.Fatalf("disabled reboot should not record run: %+v", record)
	}
}

func TestSchedulerDaemonInitializesEnabledRouterRebootWithoutImmediateRun(t *testing.T) {
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	idx := schedulerTaskIndex(spec.Scheduler, "weekly-router-reboot")
	if idx < 0 {
		t.Fatalf("default reboot task missing: %+v", spec.Scheduler)
	}
	spec.Scheduler[idx].Enabled = true
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	if err := server.scheduler.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	if len(systemRunner.calls) != 0 {
		t.Fatalf("missing next_run should initialize, not reboot immediately: %+v", systemRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["weekly-router-reboot"]
	if record.NextRun == nil || !record.NextRun.After(now) || record.LastRun != nil {
		t.Fatalf("expected future next_run only, got %+v", record)
	}
}

func TestSchedulerDaemonRunsDueRouterReboot(t *testing.T) {
	systemRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"reboot"}): {output: "rebooting"},
	}}
	server, store := newSchedulerSystemTestServer(t, systemRunner)
	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{
		Scheduler: map[string]state.SchedulerTaskRuntime{
			"weekly-router-reboot": {NextRun: &due},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	idx := schedulerTaskIndex(spec.Scheduler, "weekly-router-reboot")
	if idx < 0 {
		t.Fatalf("default reboot task missing: %+v", spec.Scheduler)
	}
	spec.Scheduler[idx].Enabled = true
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	if err := server.scheduler.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	if len(systemRunner.calls) != 1 || systemTestKey(systemRunner.calls[0]) != systemTestKey([]string{"reboot"}) {
		t.Fatalf("unexpected system runner calls: %+v", systemRunner.calls)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	record := runtimeState.Scheduler["weekly-router-reboot"]
	if record.LastStatus != "ok" || record.LastRun == nil || record.NextRun == nil || !strings.Contains(record.LastOutput, "路由器重启命令已执行") {
		t.Fatalf("unexpected runtime record: %+v", record)
	}
}

func TestSchedulerDaemonRunsDueFailoverWatchdog(t *testing.T) {
	control := &fakeProxyControl{}
	server, store := newSchedulerTestServerWithControl(t, &schedulerFetcher{}, control)
	now := time.Now().UTC()
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "p1",
		Name:    "Provider 1",
		Type:    provider.TypeManual,
		Enabled: true,
	}}
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "general", Name: "General", Role: "general"}}
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
	control.delayErrors = map[string]error{nodes[0].ID: fmt.Errorf("probe failed")}
	idx := schedulerTaskIndex(spec.Scheduler, "failover-watchdog")
	if idx < 0 {
		t.Fatalf("default failover task missing: %+v", spec.Scheduler)
	}
	spec.Scheduler[idx].Enabled = true
	if _, err := store.SaveSpec(spec); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	due := now.Add(-time.Minute)
	if err := store.SaveRuntime(state.RuntimeState{
		Scheduler: map[string]state.SchedulerTaskRuntime{
			"failover-watchdog": {NextRun: &due},
		},
		ProxySelection: map[string]string{"general": nodes[0].ID},
		NodeHealth: map[string]state.NodeHealth{
			nodes[0].ID: {Status: "failed", UpdatedAt: &now, TestURL: defaultNodeDelayURL, ConsecutiveFailures: 1},
			nodes[1].ID: {Status: "ok", UpdatedAt: &now},
		},
	}); err != nil {
		t.Fatalf("save runtime: %v", err)
	}
	captureAppliedTopology(t, server, store, spec)

	if err := server.scheduler.RunDue(context.Background(), now); err != nil {
		t.Fatalf("run due: %v", err)
	}
	if control.selectSelector != "Pool_general" || control.selectOutbound != nodes[1].ID {
		t.Fatalf("failover watchdog select not called correctly: %+v", control)
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtimeState.ProxySelection["general"] != nodes[1].ID {
		t.Fatalf("failover watchdog selection not persisted: %+v", runtimeState.ProxySelection)
	}
	record := runtimeState.Scheduler["failover-watchdog"]
	if record.LastStatus != "ok" || record.LastRun == nil || record.NextRun == nil || !strings.Contains(record.LastOutput, "已应用 1 个") {
		t.Fatalf("unexpected failover watchdog record: %+v", record)
	}
	events, err := state.NewFailoverEventStore(server.cfg.Layout).List()
	if err != nil {
		t.Fatalf("list failover events: %v", err)
	}
	if len(events) != 1 || events[0].Source != "watchdog" || events[0].PoolID != "general" || events[0].To != nodes[1].ID {
		t.Fatalf("watchdog event not persisted correctly: %+v", events)
	}
}
