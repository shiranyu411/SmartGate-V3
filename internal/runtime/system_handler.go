package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/coredesc"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/scheduler"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

const (
	defaultSystemLogLimit = 100
	maxSystemLogLimit     = 500
	systemWorkerControl   = "/usr/libexec/smartgate-worker-control"
)

type systemAPI struct {
	networkRepair  func(context.Context) error
	workers        *worker.Manager
	fabric         *fabric.Manager
	layout         interface{ DataDirPath() string }
	runtimeDir     string
	runner         netgate.Runner
	statusProvider func() Status
	commit         *commitlane.Manager
	recovery       *commitlane.RecoveryOrchestrator
	store          *state.Store
	coreRegistry   *coredesc.Registry
}

type systemActionPayload struct {
	Confirm bool `json:"confirm"`
}

type systemStatusView struct {
	Runtime   Status              `json:"runtime"`
	Memory    *systemMemoryView   `json:"memory,omitempty"`
	Disks     []systemDiskView    `json:"disks"`
	Services  []systemServiceView `json:"services"`
	Processes []systemProcessView `json:"processes"`
	Warnings  []string            `json:"warnings"`
	CheckedAt time.Time           `json:"checked_at"`
}

type systemMemoryView struct {
	TotalBytes     uint64 `json:"total_bytes"`
	FreeBytes      uint64 `json:"free_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
}

type systemDiskView struct {
	Path        string  `json:"path"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type systemServiceView struct {
	Name   string              `json:"name"`
	Status string              `json:"status"`
	Result systemCommandResult `json:"result"`
}

type systemProcessView struct {
	Name    string              `json:"name"`
	Running bool                `json:"running"`
	PIDs    []string            `json:"pids"`
	Result  systemCommandResult `json:"result"`
}

type systemCommandResult struct {
	OK     bool     `json:"ok"`
	Argv   []string `json:"argv"`
	Output string   `json:"output,omitempty"`
	Error  string   `json:"error,omitempty"`
}

type systemLogsView struct {
	Source string    `json:"source"`
	Limit  int       `json:"limit"`
	Lines  []string  `json:"lines"`
	Result string    `json:"result,omitempty"`
	Time   time.Time `json:"time"`
}

type systemActionResult struct {
	Action  string              `json:"action"`
	Service string              `json:"service,omitempty"`
	Status  string              `json:"status"`
	Result  systemCommandResult `json:"result"`
}

type systemServiceSpec struct {
	Name             string
	StatusCommand    []string
	ActionCommands   map[string][]string
	DataPlaneService bool
	ProcessName      string
}

type systemProcessSpec struct {
	Name    string
	Command []string
	Worker  bool
}

type dataDirProvider struct {
	dataDir string
}

func (p dataDirProvider) DataDirPath() string { return p.dataDir }

func NewSchedulerServiceRestarter(cfg Config) schedulerServiceRestarter {
	runner := cfg.SystemRunner
	if runner == nil {
		runner = cfg.NetGateRunner
	}
	return schedulerServiceRestarter{api: systemAPI{runner: runner, commit: cfg.CommitLane, recovery: cfg.Recovery, store: cfg.Store, coreRegistry: cfg.CoreRegistry}}
}

func NewSchedulerRouterRebooter(cfg Config) schedulerRouterRebooter {
	runner := cfg.SystemRunner
	if runner == nil {
		runner = cfg.NetGateRunner
	}
	return schedulerRouterRebooter{api: systemAPI{runner: runner, commit: cfg.CommitLane}}
}

type schedulerServiceRestarter struct {
	api systemAPI
}

type schedulerRouterRebooter struct {
	api systemAPI
}

func (r schedulerServiceRestarter) Restart(ctx context.Context, service string) (scheduler.ServiceRestartResult, error) {
	spec, command, err := r.api.serviceActionCommand(service, "restart")
	if err != nil {
		return scheduler.ServiceRestartResult{}, fmt.Errorf("%w: %v", scheduler.ErrTaskNotRunnable, err)
	}
	result := r.api.runServiceMutation(ctx, fmt.Sprintf("system-%s-restart-%d", spec.Name, time.Now().UnixNano()), "system_service_restart", spec, command)
	status := "ok"
	if !result.OK {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = "service restart failed"
		}
		status = "failed"
		return scheduler.ServiceRestartResult{
			Service: spec.Name,
			Action:  "restart",
			Status:  status,
			Argv:    result.Argv,
			Output:  result.Output,
		}, runtimeHTTPError{code: http.StatusBadGateway, err: fmt.Errorf("%s", errMsg)}
	}
	return scheduler.ServiceRestartResult{
		Service: spec.Name,
		Action:  "restart",
		Status:  status,
		Argv:    result.Argv,
		Output:  result.Output,
	}, nil
}

func (r schedulerRouterRebooter) Reboot(ctx context.Context) (scheduler.RouterRebootResult, error) {
	result := r.api.runMutation(ctx, fmt.Sprintf("system-reboot-%d", time.Now().UnixNano()), "system_router_reboot", []string{"reboot"})
	status := "ok"
	if !result.OK {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = "router reboot failed"
		}
		status = "failed"
		return scheduler.RouterRebootResult{
			Action: "reboot",
			Status: status,
			Argv:   result.Argv,
			Output: result.Output,
		}, runtimeHTTPError{code: http.StatusBadGateway, err: fmt.Errorf("%s", errMsg)}
	}
	return scheduler.RouterRebootResult{
		Action: "reboot",
		Status: status,
		Argv:   result.Argv,
		Output: result.Output,
	}, nil
}

func registerSystemHandlers(mux *http.ServeMux, cfg Config, statusProvider func() Status) {
	runner := cfg.SystemRunner
	if runner == nil {
		runner = cfg.NetGateRunner
	}
	api := systemAPI{
		layout:         dataDirProvider{dataDir: cfg.Layout.DataDir},
		runtimeDir:     cfg.Layout.RuntimeDir,
		runner:         runner,
		statusProvider: statusProvider,
		commit:         cfg.CommitLane,
		recovery:       cfg.Recovery,
		store:          cfg.Store,
		coreRegistry:   cfg.CoreRegistry,
		workers:        cfg.Workers, fabric: cfg.Fabric,
	}
	if cfg.ConfigTxn != nil {
		api.networkRepair = cfg.ConfigTxn.ReconcileNetwork
	}
	mux.HandleFunc("/api/system/network/diagnostics", api.networkDiagnostics)
	mux.HandleFunc("/api/system/network/repair", api.repairNetwork)
	mux.HandleFunc("/api/system/status", api.status)
	mux.HandleFunc("/api/system/logs", api.logs)
	mux.HandleFunc("/api/system/service/", api.serviceAction)
	mux.HandleFunc("/api/system/reboot", api.reboot)
	mux.HandleFunc("/api/system/dns/flush", api.flushDNS)
}

func (api systemAPI) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	view := systemStatusView{
		Runtime:   api.statusProvider(),
		Disks:     api.diskViews(),
		Services:  api.serviceViews(r.Context()),
		Processes: api.processViews(r.Context()),
		Warnings:  []string{},
		CheckedAt: time.Now().UTC(),
	}
	memory, err := readSystemMemory("/proc/meminfo")
	if err != nil {
		view.Warnings = append(view.Warnings, "内存信息暂不可用")
	} else {
		view.Memory = &memory
	}
	writeAPIData(w, http.StatusOK, view)
}

func (api systemAPI) logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	if source == "" {
		source = "system"
	}
	limit := parseSystemLogLimit(r.URL.Query().Get("limit"))
	result := api.run(r.Context(), []string{"logread", "-l", strconv.Itoa(limit)})
	if !result.OK {
		writeAPICommandError(w, http.StatusBadGateway, result.Error, result)
		return
	}
	lines := splitSystemLogLines(result.Output, source, limit)
	for index := range lines {
		lines[index] = sanitizeAPIMessage(lines[index])
	}
	writeAPIData(w, http.StatusOK, systemLogsView{
		Source: source,
		Limit:  limit,
		Lines:  lines,
		Result: "ok",
		Time:   time.Now().UTC(),
	})
}

func (api systemAPI) serviceAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/system/service/")
	service, action, ok := strings.Cut(path, "/")
	if !ok || service == "" || action == "" {
		writeAPIError(w, http.StatusNotFound, "service action not found")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	spec, command, err := api.serviceActionCommand(service, action)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !api.confirmed(w, r) {
		return
	}
	result := api.runServiceMutation(r.Context(), fmt.Sprintf("system-%s-%s-%d", spec.Name, action, time.Now().UnixNano()), "system_service_action", spec, command)
	actionResult := systemActionResult{Action: action, Service: spec.Name, Status: "ok", Result: result}
	if systemCommandIsDevNoop(result) {
		actionResult.Status = "stub"
	}
	if !result.OK {
		actionResult.Status = "failed"
		writeAPICommandError(w, http.StatusBadGateway, result.Error, actionResult)
		return
	}
	writeAPIData(w, http.StatusAccepted, actionResult)
}

func (api systemAPI) reboot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !api.confirmed(w, r) {
		return
	}
	result := api.runMutation(r.Context(), fmt.Sprintf("system-reboot-%d", time.Now().UnixNano()), "system_router_reboot", []string{"reboot"})
	actionResult := systemActionResult{Action: "reboot", Status: "ok", Result: result}
	if systemCommandIsDevNoop(result) {
		actionResult.Status = "stub"
	}
	if !result.OK {
		actionResult.Status = "failed"
		writeAPICommandError(w, http.StatusBadGateway, result.Error, actionResult)
		return
	}
	writeAPIData(w, http.StatusAccepted, actionResult)
}

func (api systemAPI) flushDNS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !api.confirmed(w, r) {
		return
	}
	result := api.runMutation(r.Context(), fmt.Sprintf("system-dns-flush-%d", time.Now().UnixNano()), "system_dns_flush", []string{"/etc/init.d/dnsmasq", "restart"})
	actionResult := systemActionResult{Action: "flush_dns", Service: "dnsmasq", Status: "ok", Result: result}
	if systemCommandIsDevNoop(result) {
		actionResult.Status = "stub"
	}
	if !result.OK {
		actionResult.Status = "failed"
		writeAPICommandError(w, http.StatusBadGateway, result.Error, actionResult)
		return
	}
	writeAPIData(w, http.StatusAccepted, actionResult)
}

func (api systemAPI) confirmed(w http.ResponseWriter, r *http.Request) bool {
	var payload systemActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	if !payload.Confirm {
		writeAPIError(w, http.StatusBadRequest, "confirm must be true")
		return false
	}
	return true
}

func (api systemAPI) serviceViews(ctx context.Context) []systemServiceView {
	specs := api.serviceSpecs()
	out := make([]systemServiceView, 0, len(specs))
	for _, spec := range specs {
		result := api.run(ctx, spec.StatusCommand)
		status := "running"
		if !result.OK {
			status = "not_running"
		} else if systemCommandIsDevNoop(result) {
			status = "stub"
		}
		out = append(out, systemServiceView{Name: spec.Name, Status: status, Result: sanitizeSystemCommandResult(result)})
	}
	return out
}

func (api systemAPI) processViews(ctx context.Context) []systemProcessView {
	specs := api.processSpecs()
	out := make([]systemProcessView, 0, len(specs))
	for _, spec := range specs {
		result := api.run(ctx, spec.Command)
		pids := []string{}
		if !spec.Worker && !systemCommandIsDevNoop(result) {
			pids = strings.Fields(result.Output)
		}
		if pids == nil || systemCommandIsDevNoop(result) {
			pids = []string{}
		}
		running := result.OK && (len(pids) > 0 || spec.Worker)
		out = append(out, systemProcessView{Name: spec.Name, Running: running, PIDs: pids, Result: sanitizeSystemCommandResult(result)})
	}
	return out
}

func (api systemAPI) diskViews() []systemDiskView {
	paths := uniqueSystemPaths([]string{"/", api.layout.DataDirPath(), api.runtimeDir})
	out := make([]systemDiskView, 0, len(paths))
	for _, path := range paths {
		if view, err := systemDiskUsage(path); err == nil {
			out = append(out, view)
		}
	}
	return out
}

func (api systemAPI) run(ctx context.Context, argv []string) systemCommandResult {
	runner := api.runner
	if runner == nil {
		runner = netgate.ShellRunner{}
	}
	output, err := runner.Run(ctx, argv)
	result := systemCommandResult{OK: err == nil, Argv: append([]string(nil), argv...), Output: strings.TrimSpace(output)}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func (api systemAPI) runMutation(ctx context.Context, opID, kind string, argv []string) systemCommandResult {
	return api.runMutationWithPost(ctx, opID, kind, argv, nil)
}

func (api systemAPI) runServiceMutation(ctx context.Context, opID, kind string, spec systemServiceSpec, argv []string) systemCommandResult {
	if !spec.DataPlaneService {
		return api.runMutation(ctx, opID, kind, argv)
	}
	if api.store == nil {
		return systemCommandResult{OK: false, Argv: append([]string(nil), argv...), Error: spec.Name + " applied state is not configured"}
	}
	if api.commit == nil {
		runtimeServiceMu.Lock()
		defer runtimeServiceMu.Unlock()
		return api.runDataPlaneServiceMutationLocked(ctx, spec, argv)
	}
	var result systemCommandResult
	_, err := api.commit.WithCommit(ctx, commitlane.CommitRequest{OpID: opID, Kind: kind, StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime, lockset.LevelNet} {
			release, err := session.Acquire(level)
			if err != nil {
				return err
			}
			defer release()
		}
		result = api.runDataPlaneServiceMutationLocked(ctx, spec, argv)
		if !result.OK {
			return fmt.Errorf("%s", result.Error)
		}
		return nil
	})
	if err != nil {
		if len(result.Argv) == 0 {
			result.Argv = append([]string(nil), argv...)
		}
		result.OK = false
		result.Error = err.Error()
	}
	return result
}

func (api systemAPI) runDataPlaneServiceMutationLocked(ctx context.Context, spec systemServiceSpec, requested []string) systemCommandResult {
	if spec.ProcessName == "" {
		return systemCommandResult{OK: false, Argv: append([]string(nil), requested...), Error: spec.Name + " process name is not registered"}
	}
	applied, err := api.store.LoadAppliedState()
	if err != nil {
		return systemCommandResult{OK: false, Argv: append([]string(nil), requested...), Error: err.Error()}
	}
	modeCaptured := applied.SchemaVersion != 0 && applied.SpecCaptured
	mode := ""
	command := append([]string(nil), requested...)
	if modeCaptured {
		mode = state.EffectiveGatewayMode(applied.GatewayMode)
		if (mode == state.GatewayModeOn || mode == state.GatewayModeGlobal) && len(requested) > 0 && requested[len(requested)-1] == "stop" {
			return systemCommandResult{OK: false, Argv: append([]string(nil), requested...), Error: "cannot stop " + spec.Name + " while transparent proxy mode is applied; apply safe or off mode instead"}
		}
		if mode == state.GatewayModeSafe || mode == state.GatewayModeOff {
			command = []string{"/etc/init.d/" + spec.Name, "stop"}
		}
	}
	result := api.run(ctx, command)
	if !result.OK {
		return result
	}
	if modeCaptured && (mode == state.GatewayModeSafe || mode == state.GatewayModeOff) {
		status := api.run(ctx, []string{"pidof", spec.ProcessName})
		if status.OK && len(strings.Fields(status.Output)) != 0 {
			_ = api.store.SaveDataPlaneObservation(state.DataPlaneObservationForGateway("", true))
			result.OK = false
			result.Error = spec.ProcessName + " remains running after stop"
			return result
		}
		if err := api.store.SaveDataPlaneObservation(state.DataPlaneObservationForGateway(mode, false)); err != nil {
			result.OK = false
			result.Error = err.Error()
			return result
		}
	}
	if modeCaptured && (mode == state.GatewayModeOn || mode == state.GatewayModeGlobal) {
		if api.recovery == nil {
			result.OK = false
			result.Error = spec.Name + " restart reconciliation is not configured"
			return result
		}
		if err := api.recovery.ReconcileLiveSelectionsLocked(ctx); err != nil {
			result.OK = false
			result.Error = fmt.Sprintf("reconcile durable selector state: %v; service left running with prepared durable defaults and data-plane supervisor enabled", err)
			return result
		}
		if err := api.store.SaveDataPlaneObservation(state.DataPlaneObservationForGateway(mode, true)); err != nil {
			result.OK = false
			result.Error = err.Error()
			return result
		}
	}
	if modeCaptured {
		if err := api.updateRunningGatewayMode(mode); err != nil {
			result.OK = false
			result.Error = err.Error()
		}
	}
	return result
}

func (api systemAPI) updateRunningGatewayMode(mode string) error {
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		return err
	}
	runtimeState.GatewayMode = mode
	return api.store.SaveRuntime(runtimeState)
}

func (api systemAPI) runMutationWithPost(ctx context.Context, opID, kind string, argv []string, post func() error) systemCommandResult {
	if api.commit == nil {
		runtimeServiceMu.Lock()
		defer runtimeServiceMu.Unlock()
		result := api.run(ctx, argv)
		if result.OK && post != nil {
			if err := post(); err != nil {
				result.OK = false
				result.Error = err.Error()
			}
		}
		return result
	}
	var result systemCommandResult
	_, err := api.commit.WithCommit(ctx, commitlane.CommitRequest{OpID: opID, Kind: kind, StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
		if post != nil {
			for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelRuntime} {
				release, err := session.Acquire(level)
				if err != nil {
					return err
				}
				defer release()
			}
		}
		releaseNet, err := session.Acquire(lockset.LevelNet)
		if err != nil {
			return err
		}
		defer releaseNet()
		result = api.run(ctx, argv)
		if !result.OK {
			return fmt.Errorf("%s", result.Error)
		}
		if post != nil {
			return post()
		}
		return nil
	})
	if err != nil {
		if len(result.Argv) == 0 {
			result = systemCommandResult{Argv: append([]string(nil), argv...)}
		}
		result.OK = false
		result.Error = err.Error()
	}
	return result
}

func (api systemAPI) serviceActionCommand(service, action string) (systemServiceSpec, []string, error) {
	service = strings.ToLower(strings.TrimSpace(service))
	action = strings.ToLower(strings.TrimSpace(action))
	if service == "" {
		return systemServiceSpec{}, nil, fmt.Errorf("unsupported service %q", service)
	}
	for _, spec := range api.serviceSpecs() {
		if spec.Name != service {
			continue
		}
		command, ok := spec.ActionCommands[action]
		if !ok {
			return systemServiceSpec{}, nil, fmt.Errorf("unsupported service action %q", action)
		}
		return spec, append([]string(nil), command...), nil
	}
	return systemServiceSpec{}, nil, fmt.Errorf("unsupported service %q", service)
}

func (api systemAPI) serviceSpecs() []systemServiceSpec {
	specs := []systemServiceSpec{
		initServiceSpec("dnsmasq", false, "dnsmasq"),
		initServiceSpec("network", false, ""),
		initServiceSpec("smartgate", false, "smartgate"),
	}
	for _, descriptor := range api.coreDescriptors() {
		if descriptor.Process.ServiceUnit == "" {
			continue
		}
		specs = append(specs, initServiceSpec(descriptor.Process.ServiceUnit, true, descriptor.Process.ProcessName))
	}
	for _, worker := range api.appliedWorkers() {
		specs = append(specs, workerServiceSpec(worker.ID))
	}
	return uniqueSystemServiceSpecs(specs)
}

func (api systemAPI) processSpecs() []systemProcessSpec {
	specs := []systemProcessSpec{
		pidofProcessSpec("dnsmasq"),
		pidofProcessSpec("smartgate"),
	}
	for _, descriptor := range api.coreDescriptors() {
		processName := descriptor.Process.ProcessName
		if processName != "" {
			specs = append(specs, pidofProcessSpec(processName))
		}
	}
	for _, worker := range api.appliedWorkers() {
		specs = append(specs, systemProcessSpec{
			Name: worker.ID, Command: []string{systemWorkerControl, "status", worker.ID}, Worker: true,
		})
	}
	return uniqueSystemProcessSpecs(specs)
}

func initServiceSpec(name string, dataPlane bool, processName string) systemServiceSpec {
	return systemServiceSpec{
		Name:          name,
		StatusCommand: []string{"/etc/init.d/" + name, "status"},
		ActionCommands: map[string][]string{
			"restart": {"/etc/init.d/" + name, "restart"},
			"reload":  {"/etc/init.d/" + name, "reload"},
		},
		DataPlaneService: dataPlane,
		ProcessName:      processName,
	}
}

func workerServiceSpec(workerID string) systemServiceSpec {
	return systemServiceSpec{
		Name:          workerID,
		StatusCommand: []string{systemWorkerControl, "status", workerID},
		ActionCommands: map[string][]string{
			"restart": {systemWorkerControl, "restart", workerID},
		},
	}
}

func pidofProcessSpec(name string) systemProcessSpec {
	return systemProcessSpec{Name: name, Command: []string{"pidof", name}}
}

func (api systemAPI) coreDescriptors() []coredesc.Descriptor {
	if api.coreRegistry == nil {
		return nil
	}
	return api.coreRegistry.List()
}

func (api systemAPI) appliedWorkers() []state.WorkerTarget {
	if api.store == nil {
		return nil
	}
	applied, err := api.store.LoadAppliedState()
	if err != nil || applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return nil
	}
	workers := append([]state.WorkerTarget(nil), applied.ExecutionGraph.Workers...)
	sort.Slice(workers, func(left, right int) bool {
		return workers[left].ID < workers[right].ID
	})
	return workers
}

func uniqueSystemServiceSpecs(specs []systemServiceSpec) []systemServiceSpec {
	byName := make(map[string]systemServiceSpec, len(specs))
	for _, spec := range specs {
		if spec.Name == "" {
			continue
		}
		byName[spec.Name] = spec
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]systemServiceSpec, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

func uniqueSystemProcessSpecs(specs []systemProcessSpec) []systemProcessSpec {
	byName := make(map[string]systemProcessSpec, len(specs))
	for _, spec := range specs {
		if spec.Name == "" {
			continue
		}
		byName[spec.Name] = spec
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]systemProcessSpec, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

func parseSystemLogLimit(raw string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return defaultSystemLogLimit
	}
	if limit > maxSystemLogLimit {
		return maxSystemLogLimit
	}
	return limit
}

func splitSystemLogLines(output, source string, limit int) []string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	lines := []string{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if source != "system" && !strings.Contains(strings.ToLower(line), source) {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
}

func readSystemMemory(path string) (systemMemoryView, error) {
	data, err := syscallOpen(path)
	if err != nil {
		return systemMemoryView{}, err
	}
	defer data.Close()
	values := map[string]uint64{}
	scanner := bufio.NewScanner(data)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		values[key] = value * 1024
	}
	if err := scanner.Err(); err != nil {
		return systemMemoryView{}, err
	}
	total := values["MemTotal"]
	free := values["MemFree"]
	available := values["MemAvailable"]
	if total == 0 {
		return systemMemoryView{}, fmt.Errorf("MemTotal missing")
	}
	if available == 0 {
		available = free
	}
	var used uint64
	if available < total {
		used = total - available
	}
	return systemMemoryView{TotalBytes: total, FreeBytes: free, AvailableBytes: available, UsedBytes: used}, nil
}

type closeReader interface {
	Read([]byte) (int, error)
	Close() error
}

var syscallOpen = func(path string) (closeReader, error) {
	return os.Open(path)
}

func systemDiskUsage(path string) (systemDiskView, error) {
	if path == "" {
		path = "/"
	}
	clean := filepath.Clean(path)
	var stat syscall.Statfs_t
	if err := syscall.Statfs(clean, &stat); err != nil {
		return systemDiskView{}, err
	}
	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	free := stat.Bavail * blockSize
	used := total - free
	var usedPercent float64
	if total > 0 {
		usedPercent = float64(used) / float64(total) * 100
	}
	return systemDiskView{Path: clean, TotalBytes: total, FreeBytes: free, UsedBytes: used, UsedPercent: usedPercent}, nil
}

func uniqueSystemPaths(paths []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func writeAPICommandError(w http.ResponseWriter, code int, msg string, data any) {
	writeAPIErrorCode(w, code, apiErrorCodeForStatus(code), msg, sanitizeSystemCommandDetails(data))
}

func sanitizeSystemCommandDetails(data any) any {
	switch value := data.(type) {
	case systemCommandResult:
		return sanitizeSystemCommandResult(value)
	case systemActionResult:
		value.Result = sanitizeSystemCommandResult(value.Result)
		return value
	default:
		return nil
	}
}

func sanitizeSystemCommandResult(result systemCommandResult) systemCommandResult {
	result.Argv = sanitizeCommandArgv(result.Argv)
	result.Output = sanitizeAPIMessage(result.Output)
	result.Error = sanitizeAPIMessage(result.Error)
	return result
}

func sanitizeCommandArgv(argv []string) []string {
	if argv == nil {
		return nil
	}
	out := append([]string(nil), argv...)
	for index, arg := range out {
		out[index] = sanitizeAPIMessage(arg)
	}
	return out
}

func systemCommandIsDevNoop(result systemCommandResult) bool {
	return strings.HasPrefix(result.Output, "smartgate dev no-op:")
}
