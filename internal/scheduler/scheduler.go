package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"smartgate/internal/provider"
	"smartgate/internal/state"
)

var (
	ErrDelayUnavailable = errors.New("node measurement is unavailable")
	ErrTaskRunning      = errors.New("scheduler task is already running")
	ErrGatewayOff       = errors.New("gateway mode blocks this scheduler task")
	ErrUnknownTask      = errors.New("unknown scheduler task")
	ErrTaskNotRunnable  = errors.New("scheduler task is not executable yet")
	ErrInvalidSchedule  = errors.New("invalid scheduler schedule")
)

const (
	TaskProviderRefresh  = state.SchedulerTaskProviderRefresh
	TaskApplyConfig      = "apply_config"
	TaskBackup           = "backup"
	TaskRestartService   = state.SchedulerTaskRestartService
	TaskRebootRouter     = "reboot_router"
	TaskDelayTest        = "delay_test"
	TaskFailoverWatchdog = "failover_watchdog"
	TaskCustomShell      = "custom_shell"
)

type Store interface {
	LoadSpec() (state.SpecState, error)
	LoadAppliedTopology() (state.AppliedTopology, error)
}

type RuntimeStore interface {
	LoadRuntime() (state.RuntimeState, error)
	SaveRuntime(state.RuntimeState) error
	MergeNodeHealth(map[string]state.NodeHealth) error
}

type DelayTester interface {
	Delay(ctx context.Context, outboundTag, testURL string, timeout time.Duration) (DelayResult, error)
}

type BatchDelayTester interface {
	Begin(context.Context, state.AppliedTopology, []provider.NodeView) (DelayTester, func(), error)
}

type BackupCreator interface {
	Create(ctx context.Context, opID, kind string) (BackupResult, error)
}

type ConfigApplier interface {
	Apply(ctx context.Context, opID string) (ConfigApplyResult, error)
}

type ServiceRestarter interface {
	Restart(ctx context.Context, service string) (ServiceRestartResult, error)
}

type RouterRebooter interface {
	Reboot(ctx context.Context) (RouterRebootResult, error)
}

type FailoverRunner interface {
	Run(ctx context.Context) (FailoverResult, error)
}

type DelayResult struct {
	DelayMS int
	URL     string
	Context state.NodeHealthContext
}

type BackupResult struct {
	ID        string    `json:"id"`
	Path      string    `json:"-"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

type ConfigApplyResult struct {
	DryRunID          string            `json:"dryrun_id"`
	Status            string            `json:"status"`
	ConfigEpoch       int64             `json:"config_epoch"`
	TopologyHash      string            `json:"topology_hash"`
	WorkerConfigPaths map[string]string `json:"-"`
	OpenWrtApplied    bool              `json:"openwrt_applied"`
	OpenWrtSteps      int               `json:"openwrt_steps"`
	Warnings          []string          `json:"warnings,omitempty"`
}

func (result ConfigApplyResult) ConfigPathForWorker(workerID string) string {
	return result.WorkerConfigPaths[workerID]
}

type ServiceRestartResult struct {
	Service string   `json:"service"`
	Action  string   `json:"action"`
	Status  string   `json:"status"`
	Argv    []string `json:"argv,omitempty"`
	Output  string   `json:"-"`
}

type RouterRebootResult struct {
	Action string   `json:"action"`
	Status string   `json:"status"`
	Argv   []string `json:"argv,omitempty"`
	Output string   `json:"-"`
}

type FailoverResult struct {
	Decisions int    `json:"decisions"`
	Applied   int    `json:"applied"`
	Events    int    `json:"events"`
	Failed    int    `json:"failed"`
	Error     string `json:"error,omitempty"`
}

type Result struct {
	Task      string                  `json:"task"`
	StartedAt time.Time               `json:"started_at"`
	EndedAt   time.Time               `json:"ended_at"`
	Refresh   *provider.RefreshResult `json:"refresh,omitempty"`
	DelayTest *DelayTestResult        `json:"delay_test,omitempty"`
	Backup    *BackupResult           `json:"backup,omitempty"`
	Config    *ConfigApplyResult      `json:"config,omitempty"`
	Service   *ServiceRestartResult   `json:"service,omitempty"`
	Router    *RouterRebootResult     `json:"router,omitempty"`
	Failover  *FailoverResult         `json:"failover,omitempty"`
}

type DelayTestResult struct {
	Total   int `json:"total"`
	Tested  int `json:"tested"`
	OK      int `json:"ok"`
	Failed  int `json:"failed"`
	Unknown int `json:"unknown"`
}

type Runner struct {
	store        Store
	runtimeStore RuntimeStore
	layout       state.Layout
	refresher    *provider.RefreshService
	delayTester  DelayTester
	backup       BackupCreator
	config       ConfigApplier
	restarter    ServiceRestarter
	rebooter     RouterRebooter
	failover     FailoverRunner
	mu           sync.Mutex
	running      map[string]time.Time
}

func NewRunner(store Store, refresher *provider.RefreshService) *Runner {
	return &Runner{store: store, refresher: refresher, running: map[string]time.Time{}}
}

func (r *Runner) WithDelayTester(layout state.Layout, runtimeStore RuntimeStore, tester DelayTester) *Runner {
	r.layout = layout
	r.runtimeStore = runtimeStore
	r.delayTester = tester
	return r
}

func (r *Runner) WithBackupCreator(creator BackupCreator) *Runner {
	r.backup = creator
	return r
}

func (r *Runner) WithConfigApplier(applier ConfigApplier) *Runner {
	r.config = applier
	return r
}

func (r *Runner) WithServiceRestarter(restarter ServiceRestarter) *Runner {
	r.restarter = restarter
	return r
}

func (r *Runner) WithRouterRebooter(rebooter RouterRebooter) *Runner {
	r.rebooter = rebooter
	return r
}

func (r *Runner) WithFailoverRunner(failover FailoverRunner) *Runner {
	r.failover = failover
	return r
}

func (r *Runner) RunNow(ctx context.Context, task string) (Result, error) {
	return r.RunTask(ctx, state.SchedulerTaskSpec{Type: task})
}

func (r *Runner) RunTask(ctx context.Context, task state.SchedulerTaskSpec) (Result, error) {
	task = state.NormalizeSchedulerTaskSpec(task)
	normalized, err := NormalizeTaskType(task.Type)
	if err != nil {
		return Result{}, err
	}
	task.Type = normalized
	if normalized != TaskProviderRefresh && normalized != TaskDelayTest && normalized != TaskBackup && normalized != TaskApplyConfig && normalized != TaskRestartService && normalized != TaskRebootRouter && normalized != TaskFailoverWatchdog {
		return Result{}, fmt.Errorf("%w: %s", ErrTaskNotRunnable, normalized)
	}
	service := ""
	if normalized == TaskRestartService {
		service, err = RestartServiceName(task.Spec)
		if err != nil {
			return Result{}, err
		}
	}
	startedAt, release, err := r.tryStart(taskRunKey(task.Type, service))
	if err != nil {
		return Result{}, err
	}
	defer release()

	result := Result{Task: normalized, StartedAt: startedAt}
	if normalized == TaskDelayTest {
		delayResult, err := r.runDelayTest(ctx)
		result.EndedAt = time.Now().UTC()
		result.DelayTest = &delayResult
		return result, err
	}
	if normalized == TaskBackup {
		backupResult, err := r.runBackup(ctx)
		result.EndedAt = time.Now().UTC()
		result.Backup = &backupResult
		return result, err
	}
	if normalized == TaskApplyConfig {
		configResult, err := r.runConfigApply(ctx)
		result.EndedAt = time.Now().UTC()
		result.Config = &configResult
		return result, err
	}
	if normalized == TaskRestartService {
		serviceResult, err := r.runRestartService(ctx, service)
		result.EndedAt = time.Now().UTC()
		result.Service = &serviceResult
		return result, err
	}
	if normalized == TaskRebootRouter {
		rebootResult, err := r.runRebootRouter(ctx)
		result.EndedAt = time.Now().UTC()
		result.Router = &rebootResult
		return result, err
	}
	if normalized == TaskFailoverWatchdog {
		topology, err := r.store.LoadAppliedTopology()
		if err != nil {
			return result, err
		}
		if gatewayModeBlocksRuntimeTask(topology.GatewayMode) {
			return result, ErrGatewayOff
		}
		failoverResult, err := r.runFailover(ctx)
		result.EndedAt = time.Now().UTC()
		result.Failover = &failoverResult
		return result, err
	}
	spec, err := r.store.LoadSpec()
	if err != nil {
		return result, err
	}
	if gatewayModeBlocksProviderRefresh(spec.Settings.GatewayMode) {
		return result, ErrGatewayOff
	}
	refresh, err := r.refresher.RefreshAll(ctx, spec)
	if err != nil {
		return result, err
	}
	result.EndedAt = time.Now().UTC()
	result.Refresh = &refresh
	return result, nil
}

func gatewayModeBlocksProviderRefresh(mode string) bool {
	return state.EffectiveGatewayMode(mode) == state.GatewayModeOff
}

func gatewayModeBlocksRuntimeTask(mode string) bool {
	switch state.EffectiveGatewayMode(mode) {
	case state.GatewayModeSafe, state.GatewayModeOff:
		return true
	default:
		return false
	}
}

func (r *Runner) runBackup(ctx context.Context) (BackupResult, error) {
	if r.backup == nil {
		return BackupResult{}, fmt.Errorf("%w: %s", ErrTaskNotRunnable, TaskBackup)
	}
	opID := fmt.Sprintf("scheduler-backup-%d", time.Now().UTC().UnixNano())
	return r.backup.Create(ctx, opID, "scheduled")
}

func (r *Runner) runConfigApply(ctx context.Context) (ConfigApplyResult, error) {
	if r.config == nil {
		return ConfigApplyResult{}, fmt.Errorf("%w: %s", ErrTaskNotRunnable, TaskApplyConfig)
	}
	opID := fmt.Sprintf("scheduler-apply-%d", time.Now().UTC().UnixNano())
	return r.config.Apply(ctx, opID)
}

func (r *Runner) runRestartService(ctx context.Context, service string) (ServiceRestartResult, error) {
	if r.restarter == nil {
		return ServiceRestartResult{}, fmt.Errorf("%w: %s", ErrTaskNotRunnable, TaskRestartService)
	}
	return r.restarter.Restart(ctx, service)
}

func (r *Runner) runRebootRouter(ctx context.Context) (RouterRebootResult, error) {
	if r.rebooter == nil {
		return RouterRebootResult{}, fmt.Errorf("%w: %s", ErrTaskNotRunnable, TaskRebootRouter)
	}
	return r.rebooter.Reboot(ctx)
}

func (r *Runner) runFailover(ctx context.Context) (FailoverResult, error) {
	if r.failover == nil {
		return FailoverResult{}, fmt.Errorf("%w: %s", ErrTaskNotRunnable, TaskFailoverWatchdog)
	}
	return r.failover.Run(ctx)
}

func (r *Runner) runDelayTest(ctx context.Context) (DelayTestResult, error) {
	if r.delayTester == nil || r.runtimeStore == nil || r.layout.DataDir == "" {
		return DelayTestResult{}, fmt.Errorf("%w: %s", ErrTaskNotRunnable, TaskDelayTest)
	}
	topology, err := r.store.LoadAppliedTopology()
	if err != nil {
		return DelayTestResult{}, err
	}
	if gatewayModeBlocksRuntimeTask(topology.GatewayMode) {
		return DelayTestResult{}, ErrGatewayOff
	}
	nodes, err := provider.BuildNodeViewsFromSnapshots(topology.Spec, topology.LastGood)
	if err != nil {
		return DelayTestResult{}, err
	}
	contexts := map[string]state.NodeHealthContext{}
	for id, node := range topology.Spec.SmartNodes {
		contexts[id] = state.NodeHealthContext{Revision: node.Revision, Core: node.CoreChoice, Method: "unknown"}
	}
	for _, executor := range topology.ExecutionGraph.NodeExecutors {
		contexts[executor.SmartNode] = state.NodeHealthContext{Revision: executor.Revision, Core: executor.Core, Worker: executor.Worker, Method: "unknown"}
	}
	roundCtx, cancelRound := context.WithTimeout(ctx, delayTestRoundTimeout)
	defer cancelRound()
	tester := r.delayTester
	if batchTester, ok := tester.(BatchDelayTester); ok {
		batch, closeBatch, err := batchTester.Begin(roundCtx, topology, nodes)
		if err != nil {
			return DelayTestResult{}, err
		}
		defer closeBatch()
		tester = batch
	}
	return r.measureNodesInRound(ctx, roundCtx, nodes, contexts, tester)
}

const delayTestConcurrency = 24
const delayTestLaunchInterval = 50 * time.Millisecond
const delayTestRoundTimeout = 28 * time.Second

// A shared launch clock spreads handshakes out. A round is bounded independently
// of the caller; external cancellation still leaves untested nodes untouched.
func (r *Runner) measureNodes(ctx context.Context, nodes []provider.NodeView, contexts map[string]state.NodeHealthContext) (DelayTestResult, error) {
	roundCtx, cancelRound := context.WithTimeout(ctx, delayTestRoundTimeout)
	defer cancelRound()
	return r.measureNodesInRound(ctx, roundCtx, nodes, contexts, r.delayTester)
}

func (r *Runner) measureNodesInRound(ctx, roundCtx context.Context, nodes []provider.NodeView, contexts map[string]state.NodeHealthContext, tester DelayTester) (DelayTestResult, error) {
	jobs := make(chan string, len(nodes))
	eligible := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if !node.Excluded {
			jobs <- node.ID
			eligible = append(eligible, node.ID)
		}
	}
	close(jobs)
	type measurement struct {
		id     string
		health state.NodeHealth
	}
	results := make(chan measurement, len(nodes))
	clock := time.NewTicker(delayTestLaunchInterval)
	defer clock.Stop()
	var workers sync.WaitGroup
	for i := 0; i < delayTestConcurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				select {
				case <-roundCtx.Done():
					return
				case <-clock.C:
				}
				if roundCtx.Err() != nil {
					return
				}
				started := time.Now().UTC()
				probeCtx, cancel := context.WithTimeout(roundCtx, defaultDelayTestTimeout)
				delay, err := tester.Delay(probeCtx, id, defaultDelayTestURL, defaultDelayTestTimeout)
				cancel()
				if ctx.Err() != nil {
					return
				}
				ended := time.Now().UTC()
				health := state.NodeHealth{LastTest: &started, UpdatedAt: &ended, TestURL: defaultDelayTestURL, Context: delay.Context}
				if health.Context.Revision == 0 {
					health.Context = contexts[id]
				}
				if roundCtx.Err() != nil {
					health.Status, health.Error = "unknown", "delay-test round exceeded 28 seconds"
				} else if errors.Is(err, ErrDelayUnavailable) {
					health.Status, health.Error = "unknown", err.Error()
				} else if err != nil {
					health.Status, health.Error = "failed", err.Error()
				} else {
					health.Status, health.DelayMS = "ok", delay.DelayMS
					if delay.URL != "" {
						health.TestURL = delay.URL
					}
				}
				results <- measurement{id, health}
			}
		}()
	}
	workers.Wait()
	close(results)
	nodeHealth := map[string]state.NodeHealth{}
	result := DelayTestResult{Total: len(eligible)}
	for item := range results {
		if item.health.Status == "unknown" {
			result.Unknown++
		} else if item.health.Status == "ok" {
			result.Tested++
			result.OK++
		} else {
			result.Tested++
			result.Failed++
		}
		nodeHealth[item.id] = item.health
	}
	if ctx.Err() == nil && roundCtx.Err() != nil {
		now := time.Now().UTC()
		for _, id := range eligible {
			if _, measured := nodeHealth[id]; measured {
				continue
			}
			nodeHealth[id] = state.NodeHealth{UpdatedAt: &now, TestURL: defaultDelayTestURL, Context: contexts[id], Status: "unknown", Error: "delay-test round exceeded 28 seconds"}
			result.Unknown++
		}
	}
	if err := r.runtimeStore.MergeNodeHealth(nodeHealth); err != nil {
		return result, err
	}
	return result, ctx.Err()
}

func (r *Runner) IsRunning(task string) bool {
	task, err := NormalizeTaskType(task)
	if err != nil {
		return false
	}
	return r.isRunning(task)
}

func (r *Runner) IsRunningTask(task state.SchedulerTaskSpec) bool {
	task = state.NormalizeSchedulerTaskSpec(task)
	task.Type, _ = NormalizeTaskType(task.Type)
	if task.Type == "" {
		return false
	}
	service := ""
	if task.Type == TaskRestartService {
		var err error
		service, err = RestartServiceName(task.Spec)
		if err != nil {
			return false
		}
	}
	return r.isRunning(taskRunKey(task.Type, service))
}

func (r *Runner) isRunning(task string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.running[task]
	return ok
}

func (r *Runner) tryStart(task string) (time.Time, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.running[task]; ok {
		return time.Time{}, nil, ErrTaskRunning
	}
	startedAt := time.Now().UTC()
	r.running[task] = startedAt
	return startedAt, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.running, task)
	}, nil
}

func NormalizeTaskType(task string) (string, error) {
	task = strings.TrimSpace(strings.ToLower(task))
	switch task {
	case state.SchedulerTaskProviderRefreshLegacy:
		return TaskProviderRefresh, nil
	case legacyRestartServiceTaskType():
		return TaskRestartService, nil
	case TaskProviderRefresh, TaskApplyConfig, TaskBackup, TaskRestartService, TaskRebootRouter, TaskDelayTest, TaskFailoverWatchdog, TaskCustomShell:
		return task, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnknownTask, task)
	}
}

func legacyRestartServiceTaskType() string {
	return "restart_singbox"
}

func KnownTaskTypes() []string {
	return []string{TaskProviderRefresh, TaskApplyConfig, TaskBackup, TaskRestartService, TaskRebootRouter, TaskDelayTest, TaskFailoverWatchdog, TaskCustomShell}
}

func IsRunnableTask(task string) bool {
	normalized, err := NormalizeTaskType(task)
	return err == nil && (normalized == TaskProviderRefresh || normalized == TaskDelayTest || normalized == TaskBackup || normalized == TaskApplyConfig || normalized == TaskRestartService || normalized == TaskRebootRouter || normalized == TaskFailoverWatchdog)
}

func RestartServiceName(spec map[string]any) (string, error) {
	raw, ok := spec[state.SchedulerTaskRestartServiceSpecKey]
	if !ok {
		return "", fmt.Errorf("%w: %s requires spec.%s", ErrTaskNotRunnable, TaskRestartService, state.SchedulerTaskRestartServiceSpecKey)
	}
	service, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s spec.%s must be a string", ErrTaskNotRunnable, TaskRestartService, state.SchedulerTaskRestartServiceSpecKey)
	}
	service = strings.TrimSpace(service)
	if service == "" {
		return "", fmt.Errorf("%w: %s spec.%s is empty", ErrTaskNotRunnable, TaskRestartService, state.SchedulerTaskRestartServiceSpecKey)
	}
	return service, nil
}

func taskRunKey(taskType, service string) string {
	if taskType != TaskRestartService {
		return taskType
	}
	return taskType + ":" + service
}

const (
	defaultDelayTestURL     = "http://www.gstatic.com/generate_204"
	defaultDelayTestTimeout = 5 * time.Second
	CronTimeZone            = "Asia/Shanghai"
)

var cronLocation = time.FixedZone(CronTimeZone, 8*60*60)

func NextRun(schedule string, from time.Time) (time.Time, error) {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return time.Time{}, nil
	}
	if strings.HasPrefix(schedule, "@every ") {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(schedule, "@every ")))
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("%w: %q", ErrInvalidSchedule, schedule)
		}
		return from.UTC().Add(d), nil
	}
	return nextCronRun(schedule, from)
}

type cronSpec struct {
	minutes     cronField
	hours       cronField
	daysOfMonth cronField
	months      cronField
	daysOfWeek  cronField
}

type cronField struct {
	any    bool
	values map[int]struct{}
}

func nextCronRun(schedule string, from time.Time) (time.Time, error) {
	spec, err := parseCron(schedule)
	if err != nil {
		return time.Time{}, err
	}
	candidate := from.In(cronLocation).Truncate(time.Minute).Add(time.Minute)
	deadline := candidate.AddDate(5, 0, 0)
	for candidate.Before(deadline) {
		if spec.matches(candidate) {
			return candidate.UTC(), nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("%w: no matching time within five years", ErrInvalidSchedule)
}

func parseCron(schedule string) (cronSpec, error) {
	parts := strings.Fields(schedule)
	if len(parts) != 5 {
		return cronSpec{}, fmt.Errorf("%w: cron must have five fields", ErrInvalidSchedule)
	}
	minutes, err := parseCronField(parts[0], 0, 59, false)
	if err != nil {
		return cronSpec{}, err
	}
	hours, err := parseCronField(parts[1], 0, 23, false)
	if err != nil {
		return cronSpec{}, err
	}
	daysOfMonth, err := parseCronField(parts[2], 1, 31, false)
	if err != nil {
		return cronSpec{}, err
	}
	months, err := parseCronField(parts[3], 1, 12, false)
	if err != nil {
		return cronSpec{}, err
	}
	daysOfWeek, err := parseCronField(parts[4], 0, 7, true)
	if err != nil {
		return cronSpec{}, err
	}
	return cronSpec{
		minutes:     minutes,
		hours:       hours,
		daysOfMonth: daysOfMonth,
		months:      months,
		daysOfWeek:  daysOfWeek,
	}, nil
}

func parseCronField(expr string, min, max int, sundaySeven bool) (cronField, error) {
	field := cronField{values: map[int]struct{}{}}
	if expr == "*" {
		field.any = true
		for i := min; i <= max; i++ {
			if sundaySeven && i == 7 {
				field.values[0] = struct{}{}
				continue
			}
			field.values[i] = struct{}{}
		}
		return field, nil
	}
	for _, item := range strings.Split(expr, ",") {
		if err := addCronItem(field.values, item, min, max, sundaySeven); err != nil {
			return cronField{}, err
		}
	}
	return field, nil
}

func addCronItem(values map[int]struct{}, item string, min, max int, sundaySeven bool) error {
	if item == "" {
		return fmt.Errorf("%w: empty cron field item", ErrInvalidSchedule)
	}
	step := 1
	base := item
	if before, after, ok := strings.Cut(item, "/"); ok {
		base = before
		parsed, err := strconv.Atoi(after)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("%w: invalid cron step %q", ErrInvalidSchedule, item)
		}
		step = parsed
	}
	start, end := min, max
	switch {
	case base == "*":
	case strings.Contains(base, "-"):
		left, right, _ := strings.Cut(base, "-")
		parsedStart, err := parseCronNumber(left, min, max, sundaySeven)
		if err != nil {
			return err
		}
		parsedEnd, err := parseCronNumber(right, min, max, sundaySeven)
		if err != nil {
			return err
		}
		start, end = parsedStart, parsedEnd
	default:
		parsed, err := parseCronNumber(base, min, max, sundaySeven)
		if err != nil {
			return err
		}
		start, end = parsed, parsed
	}
	if start > end {
		return fmt.Errorf("%w: descending cron range %q", ErrInvalidSchedule, item)
	}
	for i := start; i <= end; i += step {
		if sundaySeven && i == 7 {
			values[0] = struct{}{}
			continue
		}
		values[i] = struct{}{}
	}
	return nil
}

func parseCronNumber(raw string, min, max int, sundaySeven bool) (int, error) {
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid cron number %q", ErrInvalidSchedule, raw)
	}
	if sundaySeven && parsed == 7 {
		return parsed, nil
	}
	if parsed < min || parsed > max {
		return 0, fmt.Errorf("%w: cron value %d outside %d-%d", ErrInvalidSchedule, parsed, min, max)
	}
	return parsed, nil
}

func (s cronSpec) matches(t time.Time) bool {
	if !s.minutes.matches(t.Minute()) || !s.hours.matches(t.Hour()) || !s.months.matches(int(t.Month())) {
		return false
	}
	dom := s.daysOfMonth.matches(t.Day())
	dow := s.daysOfWeek.matches(int(t.Weekday()))
	switch {
	case s.daysOfMonth.any && s.daysOfWeek.any:
		return true
	case s.daysOfMonth.any:
		return dow
	case s.daysOfWeek.any:
		return dom
	default:
		return dom || dow
	}
}

func (f cronField) matches(value int) bool {
	if f.any {
		return true
	}
	_, ok := f.values[value]
	return ok
}
