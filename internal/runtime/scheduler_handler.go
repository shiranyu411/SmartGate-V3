package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/opid"
	"smartgate/internal/scheduler"
	"smartgate/internal/state"
)

type schedulerAPI struct {
	store  *state.Store
	locks  *lockset.LockSet
	runner *scheduler.Runner
}

type schedulerRunNowPayload struct {
	Task    string `json:"task"`
	Confirm bool   `json:"confirm,omitempty"`
}

type schedulerRunTaskPayload struct {
	Confirm bool `json:"confirm,omitempty"`
}

type schedulerTaskPatch struct {
	ID       *string        `json:"id,omitempty"`
	Name     *string        `json:"name,omitempty"`
	Type     *string        `json:"type,omitempty"`
	Enabled  *bool          `json:"enabled,omitempty"`
	Schedule *string        `json:"schedule,omitempty"`
	Spec     map[string]any `json:"spec,omitempty"`
}

type schedulerTaskView struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Enabled    bool           `json:"enabled"`
	Schedule   string         `json:"schedule,omitempty"`
	TimeZone   string         `json:"time_zone,omitempty"`
	Spec       map[string]any `json:"spec,omitempty"`
	Running    bool           `json:"running"`
	LastRun    *time.Time     `json:"last_run,omitempty"`
	LastStatus string         `json:"last_status,omitempty"`
	LastOutput string         `json:"last_output,omitempty"`
	NextRun    *time.Time     `json:"next_run,omitempty"`
}

func registerSchedulerHandlers(mux *http.ServeMux, store *state.Store, locks *lockset.LockSet, runner *scheduler.Runner) {
	api := schedulerAPI{store: store, locks: locks, runner: runner}
	mux.HandleFunc("/api/scheduler/run-now", api.runNowLegacy)
	mux.HandleFunc("/api/scheduler", api.tasks)
	mux.HandleFunc("/api/scheduler/", api.task)
}

func (api schedulerAPI) runNowLegacy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if api.runner == nil {
		writeAPIError(w, http.StatusNotImplemented, "scheduler runner is not configured")
		return
	}
	var payload schedulerRunNowPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if !schedulerTaskRunConfirmed(w, payload.Task, payload.Confirm) {
		return
	}
	result, err := api.runner.RunTask(r.Context(), state.SchedulerTaskSpec{Type: payload.Task})
	if err != nil {
		api.writeRunError(w, err)
		return
	}
	writeAPIData(w, http.StatusAccepted, safeSchedulerResult(result))
}

func (api schedulerAPI) tasks(w http.ResponseWriter, r *http.Request) {
	if api.store == nil {
		writeAPIError(w, http.StatusNotImplemented, "scheduler store is not configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		api.listTasks(w, r)
	case http.MethodPost:
		api.createTask(w, r)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api schedulerAPI) task(w http.ResponseWriter, r *http.Request) {
	if api.store == nil {
		writeAPIError(w, http.StatusNotImplemented, "scheduler store is not configured")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/scheduler/")
	id, action, hasAction := strings.Cut(path, "/")
	if id == "" {
		writeAPIError(w, http.StatusNotFound, "scheduler task not found")
		return
	}
	switch {
	case hasAction && action == "run" && r.Method == http.MethodPost:
		api.runTask(w, r, id)
	case hasAction:
		writeAPIError(w, http.StatusNotFound, "scheduler task action not found")
	case r.Method == http.MethodPut:
		api.updateTask(w, r, id)
	case r.Method == http.MethodDelete:
		api.deleteTask(w, r, id)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api schedulerAPI) listTasks(w http.ResponseWriter, _ *http.Request) {
	release, err := api.lockStoreRuntime("scheduler-read")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer release()
	spec, runtimeState, err := api.loadState()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]any{"tasks": api.taskViews(spec.Scheduler, runtimeState)})
}

func (api schedulerAPI) createTask(w http.ResponseWriter, r *http.Request) {
	var payload state.SchedulerTaskSpec
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	task, err := normalizeSchedulerTask(payload, "")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	release, err := api.lockStoreRuntime("scheduler-create")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer release()
	spec, runtimeState, err := api.loadState()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if schedulerTaskIndex(spec.Scheduler, task.ID) >= 0 {
		writeAPIError(w, http.StatusConflict, "scheduler task already exists")
		return
	}
	spec.Scheduler = append(spec.Scheduler, task)
	api.setNextRun(&runtimeState, task, time.Now().UTC())
	if _, err := api.store.SaveSpec(spec); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := api.store.SaveRuntime(runtimeState); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusCreated, api.taskView(task, runtimeState))
}

func (api schedulerAPI) updateTask(w http.ResponseWriter, r *http.Request, id string) {
	var payload schedulerTaskPatch
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	release, err := api.lockStoreRuntime("scheduler-update")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer release()
	spec, runtimeState, err := api.loadState()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := schedulerTaskIndex(spec.Scheduler, id)
	if idx < 0 {
		writeAPIError(w, http.StatusNotFound, "scheduler task not found")
		return
	}
	task, err := patchSchedulerTask(spec.Scheduler[idx], payload, id)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	spec.Scheduler[idx] = task
	api.setNextRun(&runtimeState, task, time.Now().UTC())
	if _, err := api.store.SaveSpec(spec); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := api.store.SaveRuntime(runtimeState); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, api.taskView(task, runtimeState))
}

func (api schedulerAPI) deleteTask(w http.ResponseWriter, _ *http.Request, id string) {
	release, err := api.lockStoreRuntime("scheduler-delete")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer release()
	spec, runtimeState, err := api.loadState()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := schedulerTaskIndex(spec.Scheduler, id)
	if idx < 0 {
		writeAPIError(w, http.StatusNotFound, "scheduler task not found")
		return
	}
	deleted := spec.Scheduler[idx]
	spec.Scheduler = append(spec.Scheduler[:idx], spec.Scheduler[idx+1:]...)
	delete(runtimeState.Scheduler, id)
	if _, err := api.store.SaveSpec(spec); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := api.store.SaveRuntime(runtimeState); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]any{"deleted": deleted.ID})
}

func (api schedulerAPI) runTask(w http.ResponseWriter, r *http.Request, id string) {
	task, err := api.loadTask(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	var payload schedulerRunTaskPayload
	if r.Body != nil && r.Body != http.NoBody {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
	}
	if !schedulerTaskRunConfirmed(w, task.Type, payload.Confirm) {
		return
	}
	if api.runner == nil {
		api.recordTaskRun(id, "failed", "计划任务执行器未配置", time.Now().UTC())
		writeAPIError(w, http.StatusNotImplemented, "scheduler runner is not configured")
		return
	}
	result, err := api.runner.RunTask(r.Context(), task)
	if err != nil {
		status := runStatusFromError(err)
		api.recordTaskRun(id, status, schedulerRunErrorOutput(task.Type, err), time.Now().UTC())
		api.writeRunError(w, err)
		return
	}
	output := schedulerRunOutput(result)
	at := result.EndedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	runtimeState, recordErr := api.recordTaskRun(id, schedulerRunStatus(result), output, at)
	if recordErr != nil {
		writeAPIError(w, http.StatusInternalServerError, recordErr.Error())
		return
	}
	writeAPIData(w, http.StatusAccepted, map[string]any{
		"result": safeSchedulerResult(result),
		"task":   api.taskView(task, runtimeState),
	})
}

func safeSchedulerResult(result scheduler.Result) scheduler.Result {
	safe := result
	if safe.Refresh != nil {
		refresh := safeRefreshResult(*safe.Refresh)
		safe.Refresh = &refresh
	}
	if safe.Failover != nil {
		failover := *safe.Failover
		failover.Error = sanitizeAPIMessage(failover.Error)
		safe.Failover = &failover
	}
	return safe
}

func schedulerTaskRunConfirmed(w http.ResponseWriter, taskType string, confirmed bool) bool {
	normalized, err := scheduler.NormalizeTaskType(taskType)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if normalized != scheduler.TaskRebootRouter || confirmed {
		return true
	}
	msg := "confirm must be true for reboot_router"
	writeAPIError(w, http.StatusBadRequest, msg)
	return false
}

func (api schedulerAPI) loadTask(id string) (state.SchedulerTaskSpec, error) {
	release, err := api.lockStoreRuntime("scheduler-run-read")
	if err != nil {
		return state.SchedulerTaskSpec{}, err
	}
	defer release()
	spec, _, err := api.loadState()
	if err != nil {
		return state.SchedulerTaskSpec{}, err
	}
	idx := schedulerTaskIndex(spec.Scheduler, id)
	if idx < 0 {
		return state.SchedulerTaskSpec{}, fmt.Errorf("scheduler task not found")
	}
	return spec.Scheduler[idx], nil
}

func (api schedulerAPI) recordTaskRun(id, status, output string, at time.Time) (state.RuntimeState, error) {
	release, err := api.lockStoreRuntime("scheduler-run-write")
	if err != nil {
		return state.RuntimeState{}, err
	}
	defer release()
	spec, runtimeState, err := api.loadState()
	if err != nil {
		return state.RuntimeState{}, err
	}
	if runtimeState.Scheduler == nil {
		runtimeState.Scheduler = map[string]state.SchedulerTaskRuntime{}
	}
	taskIndex := schedulerTaskIndex(spec.Scheduler, id)
	if taskIndex < 0 {
		// The task was deleted while its previous run was in flight. Do not
		// resurrect a runtime record based on the stale claimed definition.
		delete(runtimeState.Scheduler, id)
		if err := api.store.SaveRuntime(runtimeState); err != nil {
			return state.RuntimeState{}, err
		}
		return runtimeState, nil
	}
	task := spec.Scheduler[taskIndex]
	record := runtimeState.Scheduler[id]
	record.LastRun = &at
	record.LastStatus = status
	record.LastOutput = sanitizeAPIMessage(output)
	record.NextRun = nextRunPointer(task, at)
	record.ClaimedAt = nil
	record.ClaimDeadline = nil
	runtimeState.Scheduler[id] = record
	if err := api.store.SaveRuntime(runtimeState); err != nil {
		return state.RuntimeState{}, err
	}
	return runtimeState, nil
}

func (api schedulerAPI) loadState() (state.SpecState, state.RuntimeState, error) {
	spec, err := api.store.LoadSpec()
	if err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	if runtimeState.Scheduler == nil {
		runtimeState.Scheduler = map[string]state.SchedulerTaskRuntime{}
	}
	return spec, runtimeState, nil
}

func (api schedulerAPI) lockStoreRuntime(name string) (func(), error) {
	if api.locks == nil {
		return func() {}, nil
	}
	session := api.locks.Session(name)
	releaseStore, err := session.Acquire(lockset.LevelStore)
	if err != nil {
		return nil, err
	}
	releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
	if err != nil {
		releaseStore()
		return nil, err
	}
	return func() {
		releaseRuntime()
		releaseStore()
	}, nil
}

func (api schedulerAPI) taskViews(tasks []state.SchedulerTaskSpec, runtimeState state.RuntimeState) []schedulerTaskView {
	out := make([]schedulerTaskView, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, api.taskView(task, runtimeState))
	}
	return out
}

func (api schedulerAPI) taskView(task state.SchedulerTaskSpec, runtimeState state.RuntimeState) schedulerTaskView {
	record := runtimeState.Scheduler[task.ID]
	nextRun := record.NextRun
	if nextRun == nil {
		nextRun = nextRunPointer(task, time.Now().UTC())
	}
	running := false
	if api.runner != nil {
		running = api.runner.IsRunningTask(task)
	}
	return schedulerTaskView{
		ID:         task.ID,
		Name:       task.Name,
		Type:       task.Type,
		Enabled:    task.Enabled,
		Schedule:   task.Schedule,
		TimeZone:   scheduler.CronTimeZone,
		Spec:       schedulerTaskSpecView(task.Spec),
		Running:    running,
		LastRun:    record.LastRun,
		LastStatus: record.LastStatus,
		LastOutput: schedulerOutputText(task.Type, record.LastStatus, record.LastOutput),
		NextRun:    nextRun,
	}
}

func schedulerTaskSpecView(spec map[string]any) map[string]any {
	if spec == nil {
		return nil
	}
	view := make(map[string]any, len(spec))
	for key, value := range spec {
		if sensitiveDisplayPath(key) {
			view[key] = "[REDACTED]"
			continue
		}
		switch item := value.(type) {
		case string:
			view[key] = sanitizeAPIMessage(item)
		case bool, float64, float32, int, int64, int32, uint, uint64, uint32, json.Number, nil:
			view[key] = item
		default:
			view[key] = "[REDACTED]"
		}
	}
	return view
}

func (api schedulerAPI) setNextRun(runtimeState *state.RuntimeState, task state.SchedulerTaskSpec, from time.Time) {
	if runtimeState.Scheduler == nil {
		runtimeState.Scheduler = map[string]state.SchedulerTaskRuntime{}
	}
	record := runtimeState.Scheduler[task.ID]
	record.NextRun = nextRunPointer(task, from)
	runtimeState.Scheduler[task.ID] = record
}

func normalizeSchedulerTask(task state.SchedulerTaskSpec, idFromPath string) (state.SchedulerTaskSpec, error) {
	if idFromPath != "" {
		if task.ID != "" && task.ID != idFromPath {
			return state.SchedulerTaskSpec{}, fmt.Errorf("task id cannot differ from path id")
		}
		task.ID = idFromPath
	}
	if task.ID == "" {
		task.ID = fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	cleanID, err := opid.Clean(task.ID)
	if err != nil {
		return state.SchedulerTaskSpec{}, err
	}
	task.ID = cleanID
	task = state.NormalizeSchedulerTaskSpec(task)
	task.Type, err = scheduler.NormalizeTaskType(task.Type)
	if err != nil {
		return state.SchedulerTaskSpec{}, err
	}
	task.Name = strings.TrimSpace(task.Name)
	if task.Name == "" {
		task.Name = defaultSchedulerTaskName(task.Type)
	}
	task.Schedule = strings.TrimSpace(task.Schedule)
	if task.Schedule != "" {
		if _, err := scheduler.NextRun(task.Schedule, time.Now().UTC()); err != nil {
			return state.SchedulerTaskSpec{}, err
		}
	}
	return task, nil
}

func patchSchedulerTask(existing state.SchedulerTaskSpec, patch schedulerTaskPatch, idFromPath string) (state.SchedulerTaskSpec, error) {
	task := existing
	if patch.ID != nil {
		task.ID = strings.TrimSpace(*patch.ID)
	}
	if patch.Name != nil {
		task.Name = *patch.Name
	}
	if patch.Type != nil {
		task.Type = *patch.Type
	}
	if patch.Enabled != nil {
		task.Enabled = *patch.Enabled
	}
	if patch.Schedule != nil {
		task.Schedule = *patch.Schedule
	}
	if patch.Spec != nil {
		task.Spec = patch.Spec
	}
	return normalizeSchedulerTask(task, idFromPath)
}

func defaultSchedulerTaskName(taskType string) string {
	switch taskType {
	case scheduler.TaskProviderRefresh:
		return "Provider refresh"
	case scheduler.TaskApplyConfig:
		return "Apply config"
	case scheduler.TaskBackup:
		return "Backup"
	case scheduler.TaskRestartService:
		return "Restart service"
	case scheduler.TaskRebootRouter:
		return "Reboot router"
	case scheduler.TaskDelayTest:
		return "Delay test"
	case scheduler.TaskFailoverWatchdog:
		return "Failover watchdog"
	case scheduler.TaskCustomShell:
		return "Custom shell"
	default:
		return taskType
	}
}

func nextRunPointer(task state.SchedulerTaskSpec, from time.Time) *time.Time {
	if !task.Enabled || task.Schedule == "" {
		return nil
	}
	nextRun, err := scheduler.NextRun(task.Schedule, from)
	if err != nil || nextRun.IsZero() {
		return nil
	}
	return &nextRun
}

func schedulerTaskIndex(tasks []state.SchedulerTaskSpec, id string) int {
	for i, task := range tasks {
		if task.ID == id {
			return i
		}
	}
	return -1
}

func runStatusFromError(err error) string {
	switch {
	case errors.Is(err, scheduler.ErrTaskRunning):
		return "running"
	case errors.Is(err, scheduler.ErrTaskNotRunnable):
		return "unsupported"
	case errors.Is(err, scheduler.ErrGatewayOff), errors.Is(err, commitlane.ErrRescueActive):
		return "locked"
	default:
		return "failed"
	}
}

func (api schedulerAPI) writeRunError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, scheduler.ErrTaskRunning):
		code = http.StatusConflict
	case errors.Is(err, scheduler.ErrTaskNotRunnable):
		code = http.StatusNotImplemented
	case errors.Is(err, scheduler.ErrGatewayOff), errors.Is(err, commitlane.ErrRescueActive):
		code = http.StatusLocked
	default:
		if runtimeCode, ok := runtimeHTTPErrorStatus(err); ok {
			code = runtimeCode
		}
	}
	writeAPIError(w, code, err.Error())
}
