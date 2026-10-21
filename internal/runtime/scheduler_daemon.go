package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"smartgate/internal/lockset"
	"smartgate/internal/provider"
	"smartgate/internal/scheduler"
	"smartgate/internal/state"
)

const defaultSchedulerPollInterval = time.Minute
const schedulerClaimLease = 15 * time.Minute

type schedulerDaemon struct {
	api      schedulerAPI
	interval time.Duration
}

type dueSchedulerTask struct {
	task state.SchedulerTaskSpec
}

func newSchedulerDaemon(store *state.Store, locks *lockset.LockSet, runner *scheduler.Runner) *schedulerDaemon {
	return &schedulerDaemon{
		api:      schedulerAPI{store: store, locks: locks, runner: runner},
		interval: defaultSchedulerPollInterval,
	}
}

func (d *schedulerDaemon) Start(ctx context.Context) func() {
	runCtx, cancel := context.WithCancel(ctx)
	go d.loop(runCtx)
	return cancel
}

func (d *schedulerDaemon) loop(ctx context.Context) {
	interval := d.interval
	if interval <= 0 {
		interval = defaultSchedulerPollInterval
	}
	if err := d.RunDue(ctx, time.Now().UTC()); err != nil {
		log.Printf("scheduler initial poll failed: %v", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := d.RunDue(ctx, now.UTC()); err != nil {
				log.Printf("scheduler poll failed: %v", err)
			}
		}
	}
}

func (d *schedulerDaemon) RunDue(ctx context.Context, now time.Time) error {
	for {
		due, err := d.claimDue(now)
		if err != nil {
			return err
		}
		if len(due) == 0 {
			return nil
		}
		item := due[0]
		if err := ctx.Err(); err != nil {
			return err
		}
		d.runOne(ctx, item.task)
	}
}

func (d *schedulerDaemon) claimDue(now time.Time) ([]dueSchedulerTask, error) {
	release, err := d.api.lockStoreRuntime("scheduler-daemon-claim")
	if err != nil {
		return nil, err
	}
	defer release()
	spec, runtimeState, err := d.api.loadState()
	if err != nil {
		return nil, err
	}
	var due []dueSchedulerTask
	changed := false
	for _, task := range spec.Scheduler {
		if !task.Enabled || task.Schedule == "" {
			continue
		}
		record := runtimeState.Scheduler[task.ID]
		legacyRunning := record.LastStatus == "running" && record.ClaimedAt == nil && record.ClaimDeadline == nil
		if record.ClaimDeadline != nil && record.ClaimDeadline.After(now) {
			continue
		}
		if record.ClaimedAt != nil || record.ClaimDeadline != nil || record.LastStatus == "running" {
			record.ClaimedAt = nil
			record.ClaimDeadline = nil
			if record.LastStatus == "running" {
				record.LastStatus = "queued"
				record.LastOutput = "上次计划执行超时，已重新排队"
			}
			if legacyRunning {
				retryAt := now.UTC()
				record.NextRun = &retryAt
			}
			runtimeState.Scheduler[task.ID] = record
			changed = true
		}
		nextRun := record.NextRun
		if nextRun == nil {
			nextRun = nextRunPointer(task, now)
			record.NextRun = nextRun
			runtimeState.Scheduler[task.ID] = record
			changed = true
		}
		if nextRun == nil || nextRun.After(now) {
			continue
		}
		record.LastStatus = "running"
		record.LastOutput = "计划任务已开始执行"
		claimedAt := now.UTC()
		deadline := claimedAt.Add(schedulerClaimLease)
		record.ClaimedAt = &claimedAt
		record.ClaimDeadline = &deadline
		if task.Type == scheduler.TaskRebootRouter {
			// A successful reboot can terminate this process before completion is
			// persisted. Advance before dispatch so recovery cannot create a
			// repeated reboot loop.
			record.NextRun = nextRunPointer(task, now)
		}
		runtimeState.Scheduler[task.ID] = record
		changed = true
		due = append(due, dueSchedulerTask{task: task})
		break
	}
	if changed {
		if err := d.api.store.SaveRuntime(runtimeState); err != nil {
			return nil, err
		}
	}
	return due, nil
}

func (d *schedulerDaemon) runOne(ctx context.Context, task state.SchedulerTaskSpec) {
	if d.api.runner == nil {
		if _, err := d.api.recordTaskRun(task.ID, "failed", "计划任务执行器未配置", time.Now().UTC()); err != nil {
			log.Printf("scheduler task %s failed to persist missing-runner status: %v", task.ID, err)
		}
		return
	}
	result, err := d.api.runner.RunTask(ctx, task)
	at := result.EndedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if err != nil {
		status := runStatusFromError(err)
		if errors.Is(err, scheduler.ErrTaskRunning) {
			status = "running"
		}
		if _, recordErr := d.api.recordTaskRun(task.ID, status, schedulerRunErrorOutput(task.Type, err), at); recordErr != nil {
			log.Printf("scheduler task %s failed to persist %s status after %v: %v", task.ID, status, err, recordErr)
		}
		return
	}
	output := schedulerRunOutput(result)
	status := schedulerRunStatus(result)
	if _, err := d.api.recordTaskRun(task.ID, status, output, at); err != nil {
		log.Printf("scheduler task %s failed to persist %s status: %v", task.ID, status, err)
	}
}

func schedulerRunOutput(result scheduler.Result) string {
	output := schedulerTaskText(result.Task) + "已完成"
	if result.Refresh != nil {
		ok, failed := providerRefreshCounts(result.Refresh.Results)
		output = fmt.Sprintf("订阅刷新完成：共 %d 个，成功 %d 个，失败 %d 个", len(result.Refresh.Results), ok, failed)
	}
	if result.DelayTest != nil {
		output = fmt.Sprintf("延迟测试完成：共 %d 个，已测 %d 个，成功 %d 个，失败 %d 个，未完成 %d 个", result.DelayTest.Total, result.DelayTest.Tested, result.DelayTest.OK, result.DelayTest.Failed, result.DelayTest.Unknown)
	}
	if result.Backup != nil {
		output = fmt.Sprintf("备份完成：备份 ID %s，大小 %d 字节", result.Backup.ID, result.Backup.Size)
	}
	if result.Config != nil {
		output = fmt.Sprintf("配置应用完成：config_epoch %d，OpenWrt 步骤 %d 个", result.Config.ConfigEpoch, result.Config.OpenWrtSteps)
	}
	if result.Service != nil {
		output = fmt.Sprintf("%s 服务重启完成", result.Service.Service)
	}
	if result.Router != nil {
		output = "路由器重启命令已执行"
	}
	if result.Failover != nil {
		output = fmt.Sprintf("故障切换检查完成：决策 %d 个，已应用 %d 个，失败 %d 个，事件 %d 条", result.Failover.Decisions, result.Failover.Applied, result.Failover.Failed, result.Failover.Events)
	}
	return sanitizeAPIMessage(output)
}

func schedulerRunErrorOutput(taskType string, err error) string {
	label := schedulerTaskText(taskType)
	switch runStatusFromError(err) {
	case "running":
		return label + "正在执行"
	case "unsupported":
		return "当前版本暂不支持" + label
	case "locked":
		return "当前网关状态不允许执行" + label
	default:
		if errors.Is(err, context.DeadlineExceeded) {
			return label + "执行超时"
		}
		if errors.Is(err, context.Canceled) {
			return label + "已取消"
		}
		return label + "执行失败"
	}
}

func schedulerTaskText(taskType string) string {
	switch taskType {
	case scheduler.TaskProviderRefresh:
		return "订阅刷新"
	case scheduler.TaskDelayTest:
		return "延迟测试"
	case scheduler.TaskFailoverWatchdog:
		return "故障切换检查"
	case scheduler.TaskBackup:
		return "备份"
	case scheduler.TaskApplyConfig:
		return "配置应用"
	case scheduler.TaskRestartService:
		return "服务重启"
	case scheduler.TaskRebootRouter:
		return "路由器重启"
	case scheduler.TaskCustomShell:
		return "自定义脚本"
	default:
		return "计划任务"
	}
}

func schedulerOutputText(taskType, status, output string) string {
	output = sanitizeAPIMessage(output)
	if output == "" || containsHanText(output) {
		return output
	}
	fields := legacySchedulerOutputFields(output)
	switch taskType {
	case scheduler.TaskProviderRefresh:
		if fields["providers"] != "" {
			return fmt.Sprintf("订阅刷新完成：共 %s 个，成功 %s 个，失败 %s 个", fields["providers"], fields["ok"], fields["failed"])
		}
	case scheduler.TaskDelayTest:
		if fields["tested"] != "" {
			return fmt.Sprintf("延迟测试完成：共测试 %s 个，成功 %s 个，失败 %s 个", fields["tested"], fields["ok"], fields["failed"])
		}
	case scheduler.TaskBackup:
		if fields["backup"] != "" {
			return fmt.Sprintf("备份完成：备份 ID %s，大小 %s 字节", fields["backup"], fields["size"])
		}
	case scheduler.TaskApplyConfig:
		if fields["epoch"] != "" {
			return fmt.Sprintf("配置应用完成：config_epoch %s，OpenWrt 步骤 %s 个", fields["epoch"], fields["openwrt_steps"])
		}
	case scheduler.TaskRestartService:
		if fields["service"] != "" {
			return fields["service"] + " 服务重启完成"
		}
	case scheduler.TaskRebootRouter:
		if fields["action"] == "reboot" {
			return "路由器重启命令已执行"
		}
	case scheduler.TaskFailoverWatchdog:
		if fields["decisions"] != "" {
			return fmt.Sprintf("故障切换检查完成：决策 %s 个，已应用 %s 个，失败 %s 个，事件 %s 条", fields["decisions"], fields["applied"], fields["failed"], fields["events"])
		}
	}
	switch status {
	case "ok":
		return schedulerTaskText(taskType) + "已完成"
	case "degraded":
		return schedulerTaskText(taskType) + "部分完成"
	case "running":
		return schedulerTaskText(taskType) + "正在执行"
	case "queued":
		return schedulerTaskText(taskType) + "已重新排队"
	case "unsupported":
		return "当前版本暂不支持" + schedulerTaskText(taskType)
	case "locked":
		return "当前网关状态不允许执行" + schedulerTaskText(taskType)
	default:
		return schedulerTaskText(taskType) + "执行失败"
	}
}

func legacySchedulerOutputFields(output string) map[string]string {
	fields := map[string]string{}
	cleaned := strings.NewReplacer(":", " ", ",", " ").Replace(output)
	for _, field := range strings.Fields(cleaned) {
		key, value, ok := strings.Cut(field, "=")
		if ok && key != "" && value != "" {
			fields[key] = value
		}
	}
	return fields
}

func containsHanText(value string) bool {
	return strings.IndexFunc(value, func(character rune) bool {
		return unicode.Is(unicode.Han, character)
	}) >= 0
}

func schedulerRunStatus(result scheduler.Result) string {
	if result.Refresh != nil {
		ok, failed := providerRefreshCounts(result.Refresh.Results)
		if failed > 0 {
			if ok > 0 {
				return "degraded"
			}
			return "failed"
		}
	}
	if result.DelayTest != nil && result.DelayTest.Failed+result.DelayTest.Unknown > 0 {
		if result.DelayTest.OK > 0 {
			return "degraded"
		}
		return "failed"
	}
	if result.Failover != nil && result.Failover.Failed > 0 {
		if result.Failover.Applied > 0 {
			return "degraded"
		}
		return "failed"
	}
	return "ok"
}

func providerRefreshCounts(results []provider.RefreshStepResult) (ok, failed int) {
	for _, result := range results {
		switch result.Status {
		case "ok", "unchanged":
			ok++
		case "failed":
			failed++
		}
	}
	return ok, failed
}
