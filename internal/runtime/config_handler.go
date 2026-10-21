package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/configapply"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/opid"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/redact"
	"smartgate/internal/scheduler"
	"smartgate/internal/selection"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type configAPI struct {
	store          *state.Store
	layout         state.Layout
	locks          *lockset.LockSet
	owners         *netgate.OwnerRegistry
	netGateRunner  netgate.Runner
	proxyControl   selection.ProxyController
	statusProvider func() Status
	configTxn      *commitlane.ConfigTxn
}

type configDryRunPayload struct {
	OpID string `json:"op_id"`
}

type configApplyPayload struct {
	DryRunID    string `json:"dryrun_id"`
	Description string `json:"description,omitempty"`
}

type gatewayModePayload struct {
	Mode string `json:"mode"`
}

type configStateView struct {
	Dirty              bool   `json:"dirty"`
	SpecHash           string `json:"spec_hash"`
	TopologyHash       string `json:"topology_hash"`
	ConfigEpoch        int64  `json:"config_epoch"`
	GatewayMode        string `json:"gateway_mode"`
	SavedGatewayMode   string `json:"saved_gateway_mode"`
	AppliedGatewayMode string `json:"applied_gateway_mode,omitempty"`
	RunningGatewayMode string `json:"running_gateway_mode,omitempty"`
}

type configDryRunRecord struct {
	ID                   string                         `json:"id"`
	CreatedAt            time.Time                      `json:"created_at"`
	SpecHash             string                         `json:"spec_hash"`
	TopologyHash         string                         `json:"topology_hash,omitempty"`
	PreparedTopologyHash string                         `json:"prepared_topology_hash,omitempty"`
	ConfigEpoch          int64                          `json:"config_epoch"`
	GatewayMode          string                         `json:"gateway_mode"`
	RuntimeAction        string                         `json:"runtime_action"`
	CoreCandidate        *configCoreCandidate           `json:"core_candidate,omitempty"`
	OpenWrtPlan          *netgate.DryRunPlan            `json:"openwrt_plan,omitempty"`
	ExecutionGraph       state.ExecutionGraph           `json:"execution_graph"`
	PreviousGraph        state.ExecutionGraph           `json:"previous_execution_graph"`
	GraphHash            string                         `json:"graph_hash,omitempty"`
	GraphIssues          []planner.GraphIssue           `json:"graph_issues,omitempty"`
	WorkerPrepared       *worker.Prepared               `json:"worker_prepared,omitempty"`
	FabricPrepared       *fabric.PreparedFabric         `json:"fabric_prepared,omitempty"`
	Changes              []configChangeView             `json:"changes,omitempty"`
	ChangesComplete      bool                           `json:"changes_complete"`
	ChangesOmitted       int                            `json:"changes_omitted,omitempty"`
	ProxySelection       map[string]string              `json:"proxy_selection,omitempty"`
	SelectionCaptured    bool                           `json:"selection_captured,omitempty"`
	Selections           map[string]state.PoolSelection `json:"selections,omitempty"`
	LastGood             []state.LastGoodSnapshot       `json:"last_good,omitempty"`
}

type configCoreCandidate struct {
	Core         string    `json:"core"`
	OpID         string    `json:"op_id"`
	Path         string    `json:"path"`
	Size         int       `json:"size"`
	ConfigSHA256 string    `json:"config_sha256"`
	CreatedAt    time.Time `json:"created_at"`
	SpecHash     string    `json:"spec_hash,omitempty"`
	ConfigEpoch  int64     `json:"config_epoch,omitempty"`
	TopologyHash string    `json:"topology_hash,omitempty"`
	CheckLog     string    `json:"check_log,omitempty"`
	CheckOK      bool      `json:"check_ok"`
}

type configDryRunView struct {
	ID                    string                `json:"id"`
	CreatedAt             time.Time             `json:"created_at"`
	SpecHash              string                `json:"spec_hash"`
	TopologyHash          string                `json:"topology_hash,omitempty"`
	ConfigEpoch           int64                 `json:"config_epoch"`
	GatewayMode           string                `json:"gateway_mode"`
	RuntimeAction         string                `json:"runtime_action"`
	Changes               []configChangeView    `json:"changes"`
	ChangesComplete       bool                  `json:"changes_complete"`
	ChangesOmitted        int                   `json:"changes_omitted,omitempty"`
	ExecutionGraphSummary executionGraphSummary `json:"execution_graph_summary"`
}

func configDryRunResponse(record configDryRunRecord) configDryRunView {
	return configDryRunView{ID: record.ID, CreatedAt: record.CreatedAt, SpecHash: record.SpecHash,
		TopologyHash: record.TopologyHash, ConfigEpoch: record.ConfigEpoch, GatewayMode: record.GatewayMode,
		RuntimeAction: record.RuntimeAction, Changes: record.Changes, ChangesComplete: record.ChangesComplete,
		ChangesOmitted: record.ChangesOmitted, ExecutionGraphSummary: summarizeExecutionGraph(record)}
}

func (record *configDryRunRecord) UnmarshalJSON(body []byte) error {
	type recordAlias configDryRunRecord
	var decoded recordAlias
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	*record = configDryRunRecord(decoded)
	return nil
}

func (record configDryRunRecord) candidatePath() string {
	if record.CoreCandidate == nil {
		return ""
	}
	return record.CoreCandidate.Path
}

func (record *configDryRunRecord) setPreparedArtifacts(prepared configapply.Prepared) {
	plan := prepared.OpenWrtPlan
	record.OpenWrtPlan = &plan
	record.PreparedTopologyHash = prepared.TopologyHash
	record.ExecutionGraph = prepared.ExecutionGraph
	record.PreviousGraph = prepared.Target.PreviousGraph
	record.LastGood = prepared.Target.LastGood
	record.GraphHash = prepared.GraphHash
	record.GraphIssues = append([]planner.GraphIssue(nil), prepared.GraphIssues...)
	record.WorkerPrepared = prepared.Workers
	record.FabricPrepared = prepared.Fabric
}

func graphIssueChanges(issues []planner.GraphIssue) []configChangeView {
	out := make([]configChangeView, 0, len(issues))
	for _, issue := range issues {
		if issue.Code == "rule_conflict" || (issue.Code == "fabric_rules_invalid" && strings.Contains(issue.Message, "multiple owners")) {
			continue
		}
		category := "executor"
		if strings.HasPrefix(issue.Code, "fabric_") {
			category = "fabric"
		}
		impact := "Execution Graph cannot be applied until this issue is resolved"
		if !issue.Blocking {
			impact = "Execution remains available with degraded semantics"
		}
		if issue.Code == "pool_selection_removed" {
			impact = "原选节点已从 Pool 移除；确认应用后切换到剩余成员或空池策略，当前连接在应用前保持不变"
		}
		out = append(out, configChangeView{
			Category: category, Title: issue.Message, Before: issue.SmartNode, After: issue.Pool,
			Impact: impact, Blocking: issue.Blocking,
		})
	}
	return out
}

func appliedWithRuntimeSelection(applied state.AppliedState, selection map[string]string) state.AppliedState {
	applied.ProxySelection = cloneSelectionMap(selection)
	return applied
}

type configChangeView struct {
	Category string `json:"category"`
	Title    string `json:"title"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
	Impact   string `json:"impact"`
	Blocking bool   `json:"blocking,omitempty"`
}

type executionGraphSummary struct {
	Workers      map[string][]string  `json:"workers"`
	Executors    map[string]int       `json:"executors"`
	PoolBindings []poolBindingSummary `json:"pool_bindings"`
}

type poolBindingSummary struct {
	Pool       string `json:"pool"`
	To         string `json:"to"`
	Core       string `json:"core,omitempty"`
	Worker     string `json:"worker,omitempty"`
	Generation uint64 `json:"generation"`
}

func summarizeExecutionGraph(record configDryRunRecord) executionGraphSummary {
	summary := executionGraphSummary{
		Workers:      map[string][]string{"create": {}, "keep": {}, "stop": {}},
		Executors:    map[string]int{"create": 0, "reuse": 0, "release": 0},
		PoolBindings: []poolBindingSummary{},
	}
	previousWorkers := make(map[string]struct{}, len(record.PreviousGraph.Workers))
	for _, target := range record.PreviousGraph.Workers {
		previousWorkers[target.ID] = struct{}{}
	}
	targetWorkers := make(map[string]struct{}, len(record.ExecutionGraph.Workers))
	for _, target := range record.ExecutionGraph.Workers {
		targetWorkers[target.ID] = struct{}{}
		bucket := "create"
		if _, ok := previousWorkers[target.ID]; ok {
			bucket = "keep"
		}
		summary.Workers[bucket] = append(summary.Workers[bucket], target.ID)
	}
	for _, target := range record.PreviousGraph.Workers {
		if _, ok := targetWorkers[target.ID]; !ok {
			summary.Workers["stop"] = append(summary.Workers["stop"], target.ID)
		}
	}
	previousExecutors := make(map[string]struct{}, len(record.PreviousGraph.NodeExecutors))
	for _, executor := range record.PreviousGraph.NodeExecutors {
		previousExecutors[executor.ID] = struct{}{}
	}
	targetExecutors := make(map[string]struct{}, len(record.ExecutionGraph.NodeExecutors))
	for _, executor := range record.ExecutionGraph.NodeExecutors {
		targetExecutors[executor.ID] = struct{}{}
		if _, ok := previousExecutors[executor.ID]; ok {
			summary.Executors["reuse"]++
		} else {
			summary.Executors["create"]++
		}
	}
	for _, executor := range record.PreviousGraph.NodeExecutors {
		if _, ok := targetExecutors[executor.ID]; !ok {
			summary.Executors["release"]++
		}
	}
	for _, binding := range record.ExecutionGraph.PoolBindings {
		to := binding.Target.SmartNode
		if to == "" {
			to = binding.Target.Kind
		}
		summary.PoolBindings = append(summary.PoolBindings, poolBindingSummary{
			Pool: binding.Pool, To: to, Core: string(binding.Target.Core), Worker: binding.Target.Worker, Generation: binding.Generation,
		})
	}
	for _, values := range summary.Workers {
		sort.Strings(values)
	}
	return summary
}

func cloneSelectionMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func clonePoolSelectionMap(in map[string]state.PoolSelection) map[string]state.PoolSelection {
	if in == nil {
		return nil
	}
	out := make(map[string]state.PoolSelection, len(in))
	for pool, selection := range in {
		out[pool] = selection
	}
	return out
}
func selectionMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

const (
	configSoftLimitBytes    = 512 * 1024
	overlayWarnFreeBytes    = 5 * 1024 * 1024
	overlayRejectFreeBytes  = 2 * 1024 * 1024
	dryRunResponseHardLimit = 4 * 1024 * 1024
	dryRunChangesBudget     = 3 * 1024 * 1024
	dryRunFieldLimit        = 16 * 1024
	persistedDryRunMaxBytes = 16 * 1024 * 1024
)

var configDiskUsage = systemDiskUsage
var configApplyTimeout = 30 * time.Second

// runtimeServiceMu serializes operations that mutate the active proxy service.
var runtimeServiceMu sync.Mutex

type configResourceGuardView struct {
	Enabled        bool     `json:"enabled"`
	OverlayPath    string   `json:"overlay_path,omitempty"`
	FreeBytes      uint64   `json:"free_bytes,omitempty"`
	CandidateBytes int      `json:"candidate_bytes"`
	SoftLimitBytes int      `json:"soft_limit_bytes"`
	HardLimitBytes int      `json:"hard_limit_bytes"`
	Warnings       []string `json:"warnings,omitempty"`
}

type configApplyResult struct {
	Status         string                  `json:"status"`
	ConfigEpoch    int64                   `json:"config_epoch"`
	TopologyHash   string                  `json:"topology_hash"`
	CoreResults    []configCoreApplyResult `json:"core_results,omitempty"`
	OpenWrt        netgate.ApplyResult     `json:"openwrt"`
	OpenWrtApplied bool                    `json:"openwrt_applied"`
	Workers        []configWorkerResult    `json:"workers"`
	FabricApplied  bool                    `json:"fabric_applied"`
	RuntimeAction  string                  `json:"runtime_action"`
	ResourceGuard  configResourceGuardView `json:"resource_guard"`
	Warnings       []string                `json:"warnings,omitempty"`
}

type configCoreApplyResult struct {
	Core       string   `json:"core"`
	Status     string   `json:"status"`
	Workers    []string `json:"workers,omitempty"`
	ConfigPath string   `json:"config_path,omitempty"`
	Log        string   `json:"log,omitempty"`
}

type configWorkerResult struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	ConfigPath string `json:"-"`
}

type configRollbackReport struct {
	Attempted         bool     `json:"attempted"`
	OK                bool     `json:"ok"`
	Stage             string   `json:"stage,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	WorkersRestored   bool     `json:"workers_restored"`
	FabricRestored    bool     `json:"fabric_restored"`
	BindingsRestored  bool     `json:"bindings_restored"`
	SelectionRestored bool     `json:"selection_restored"`
	Errors            []string `json:"errors,omitempty"`
}

type configApplyErrorData struct {
	Rollback *configRollbackReport `json:"rollback,omitempty"`
}

func NewSchedulerConfigApplier(cfg Config, statusProvider func() Status) schedulerConfigApplier {
	return schedulerConfigApplier{
		api: configAPI{
			store:          cfg.Store,
			layout:         cfg.Layout,
			locks:          cfg.Locks,
			owners:         cfg.Owners,
			netGateRunner:  cfg.NetGateRunner,
			proxyControl:   cfg.ProxyControl,
			statusProvider: statusProvider,
			configTxn:      configTxnForConfig(cfg),
		},
	}
}

type schedulerConfigApplier struct {
	api configAPI
}

func (a schedulerConfigApplier) Apply(ctx context.Context, opID string) (scheduler.ConfigApplyResult, error) {
	runtimeServiceMu.Lock()
	defer runtimeServiceMu.Unlock()

	record, err := a.api.createDryRunRecord(ctx, opID)
	if err != nil {
		return scheduler.ConfigApplyResult{}, err
	}
	result, err := a.api.applyDryRunRecord(ctx, record, false, true)
	if err != nil {
		if code, ok := configRuntimeErrorStatus(err); ok && code == http.StatusNotImplemented {
			return scheduler.ConfigApplyResult{}, fmt.Errorf("%w: %v", scheduler.ErrTaskNotRunnable, err)
		}
		return scheduler.ConfigApplyResult{}, err
	}
	return scheduler.ConfigApplyResult{
		DryRunID:          record.ID,
		Status:            result.Status,
		ConfigEpoch:       result.ConfigEpoch,
		TopologyHash:      result.TopologyHash,
		WorkerConfigPaths: workerConfigPathsFromApply(result.Workers),
		OpenWrtApplied:    result.OpenWrtApplied,
		OpenWrtSteps:      result.OpenWrt.Steps,
		Warnings:          result.Warnings,
	}, nil
}

func workerConfigPathsFromApply(workers []configWorkerResult) map[string]string {
	paths := map[string]string{}
	for _, activation := range workers {
		if activation.ID == "" || activation.ConfigPath == "" {
			continue
		}
		paths[activation.ID] = activation.ConfigPath
	}
	if len(paths) == 0 {
		return nil
	}
	return paths
}

func registerConfigHandlers(mux *http.ServeMux, cfg Config, statusProvider func() Status) {
	api := configAPI{
		store:          cfg.Store,
		layout:         cfg.Layout,
		locks:          cfg.Locks,
		owners:         cfg.Owners,
		netGateRunner:  cfg.NetGateRunner,
		proxyControl:   cfg.ProxyControl,
		statusProvider: statusProvider,
		configTxn:      configTxnForConfig(cfg),
	}
	mux.HandleFunc("/api/config/state", api.state)
	mux.HandleFunc("/api/config/dryrun", api.dryRun)
	mux.HandleFunc("/api/config/apply", api.apply)
	mux.HandleFunc("/api/config/discard", api.discard)
	mux.HandleFunc("/api/gateway/mode", api.gatewayMode)
	mux.HandleFunc("/api/overview", api.overview)
}

func (api configAPI) state(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	view, err := api.currentConfigState()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, view)
}

func (api configAPI) dryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeDryRunAPIError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	var payload configDryRunPayload
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&payload)
	}
	opID := payload.OpID
	if opID == "" {
		opID = fmt.Sprintf("config-%d", time.Now().UnixNano())
	}
	record, err := api.createDryRun(r.Context(), opID)
	if err != nil {
		var dryRunErr configDryRunError
		if errors.As(err, &dryRunErr) {
			apiCode := "worker_check_failed"
			var graphErr configapply.GraphValidationError
			if errors.As(dryRunErr.err, &graphErr) {
				apiCode = "execution_graph_invalid"
			}
			logConfigRequestRejected("dry-run", opID, dryRunErr.code, apiCode, dryRunErr.err)
			writeDryRunAPIErrorCode(w, dryRunErr.code, apiCode, dryRunErr.err.Error(), map[string]any{
				"dryrun": configDryRunResponse(dryRunErr.record),
				"legacy_error_codes": []string{
					apiErrorCodeForStatus(dryRunErr.code),
				},
			})
			return
		}
		logConfigRequestRejected("dry-run", opID, configDryRunErrorStatus(err), "", err)
		writeDryRunAPIError(w, configDryRunErrorStatus(err), err.Error(), nil)
		return
	}
	writeDryRunAPIData(w, http.StatusOK, configDryRunResponse(record))
}

// logConfigRequestRejected leaves a syslog trace for a rejected controlled
// dry-run or apply. Deployment tooling reaches these endpoints through
// uclient-fetch, which discards non-2xx bodies, so without this line a failed
// controlled reconcile is invisible after the fact.
func logConfigRequestRejected(kind, opID string, status int, apiCode string, err error) {
	message := "unknown error"
	if err != nil {
		message = redact.Message(err.Error())
	}
	if apiCode == "" {
		apiCode = apiErrorCodeForStatus(status)
	}
	log.Printf("config %s %s rejected: status=%d code=%s: %s", kind, opID, status, apiCode, message)
}

func (api configAPI) createDryRun(ctx context.Context, opID string) (configDryRunRecord, error) {
	runtimeServiceMu.Lock()
	defer runtimeServiceMu.Unlock()

	record, err := api.createDryRunRecord(ctx, opID)
	if err != nil {
		_ = api.cleanupDryRun(record)
		return record, err
	}
	enforceConfigDryRunResponseLimit(&record)
	if err := api.saveDryRun(record); err != nil {
		_ = api.cleanupDryRun(record)
		return configDryRunRecord{}, err
	}
	return record, nil
}

func enforceConfigDryRunResponseLimit(record *configDryRunRecord) {
	viewBytes, _ := json.Marshal(apiEnvelope{OK: true, Data: configDryRunResponse(*record)})
	if len(viewBytes)+1 > dryRunResponseHardLimit {
		record.ChangesComplete = false
		record.ChangesOmitted++
		record.Changes = []configChangeView{{Category: "摘要限制", Title: "预演响应超过设备安全上限", Impact: "本次预演禁止应用；请缩小变更范围"}}
	}
}

func (api configAPI) createDryRunRecord(ctx context.Context, opID string) (configDryRunRecord, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return configDryRunRecord{}, configApplyError{code: http.StatusBadRequest, err: err}
	}
	spec, runtimeState, err := api.loadSpecRuntime("config-dryrun")
	if err != nil {
		return configDryRunRecord{}, err
	}
	appliedState, err := api.loadAppliedState(spec.Settings.GatewayMode)
	if err != nil {
		return configDryRunRecord{}, err
	}
	specHash := provider.HashSpec(spec)
	currentLastGood, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(api.layout))
	if err != nil {
		var limitErr *provider.TopologyResourceLimitError
		if errors.As(err, &limitErr) {
			return configDryRunRecord{}, configApplyError{code: http.StatusRequestEntityTooLarge, err: err}
		}
		return configDryRunRecord{}, configApplyError{code: http.StatusInternalServerError, err: err}
	}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, currentLastGood)
	if err != nil {
		return configDryRunRecord{}, configApplyError{code: http.StatusInternalServerError, err: err}
	}
	gatewayMode := state.EffectiveGatewayMode(spec.Settings.GatewayMode)
	appliedView := appliedWithRuntimeSelection(appliedState, runtimeState.ProxySelection)
	record := configDryRunRecord{
		ID:             safeOpID,
		CreatedAt:      time.Now().UTC(),
		SpecHash:       specHash,
		TopologyHash:   topologyHash,
		ConfigEpoch:    appliedState.ConfigEpoch,
		GatewayMode:    gatewayMode,
		RuntimeAction:  runtimeActionForGatewayMode(gatewayMode),
		Changes:        summarizeConfigChanges(appliedView, spec, topologyHash, currentLastGood),
		ProxySelection: cloneSelectionMap(runtimeState.ProxySelection),
	}
	conflicts, conflictErr := state.RuleConflicts(spec)
	if conflictErr != nil {
		return record, configApplyError{code: http.StatusUnprocessableEntity, apiCode: "rules_invalid", err: conflictErr}
	}
	record.Changes = append(record.Changes, ruleConflictChanges(conflicts)...)
	record.Changes = append(record.Changes, gatewayModeDerivedChanges(appliedView, spec)...)
	if api.configTxn == nil {
		return record, configApplyError{code: http.StatusNotImplemented, err: fmt.Errorf("config transaction is not configured")}
	}
	var selections map[string]state.PoolSelection
	if appliedState.SchemaVersion != 0 && appliedState.SpecCaptured {
		durable, err := api.store.LoadSelectionState()
		if err != nil {
			return record, err
		}
		if durable.SchemaVersion == 0 {
			durable = state.SelectionStateForApplied(appliedState, runtimeState.ProxySelection)
		} else if !state.SelectionStateMatchesApplied(durable, appliedState) {
			return record, fmt.Errorf("durable selection does not match applied topology")
		}
		selections = clonePoolSelectionMap(durable.Selections)
		record.SelectionCaptured = true
	}
	record.Selections = clonePoolSelectionMap(selections)
	prepared, prepareErr := api.configTxn.Prepare(ctx, safeOpID, configapply.TargetState{
		Spec: spec, LastGood: currentLastGood, ProxySelection: cloneSelectionMap(record.ProxySelection),
		Selections: selections, PreviousGraph: appliedState.ExecutionGraph,
	}, appliedState.ConfigEpoch)
	record.setPreparedArtifacts(prepared)
	if record.OpenWrtPlan != nil {
		change := operationalChange(record.RuntimeAction, *record.OpenWrtPlan)
		if record.GraphHash != "" && gatewayModeUsesDataPlane(record.GatewayMode) {
			change.After = strings.ReplaceAll(change.After, "准备核心候选并切换数据面", "准备 Worker 后切换 Gateway Fabric")
		}
		record.Changes = append(record.Changes, change)
	}
	record.Changes = append(record.Changes, graphIssueChanges(record.GraphIssues)...)
	finalizeConfigChanges(&record)
	if prepareErr != nil {
		return record, configDryRunError{code: http.StatusUnprocessableEntity, record: record, err: prepareErr}
	}
	return record, nil
}

func operationalChange(action string, plan netgate.DryRunPlan) configChangeView {
	names := make([]string, 0, len(plan.Steps))
	destructive := 0
	for _, step := range plan.Steps {
		names = append(names, step.Name)
		if step.Destructive {
			destructive++
		}
	}
	actionText := map[string]string{"cleanup_safe_direct": "停止代理并清理 SmartGate 网络接管，保留直连", "cleanup_off": "停止代理并移除 SmartGate 网络接管", "apply_global_proxy": "准备核心候选并切换数据面，启用全局代理", "apply_transparent_proxy": "准备核心候选并切换数据面，启用透明分流"}[action]
	if actionText == "" {
		actionText = action
	}
	return configChangeView{Category: "实际执行", Title: "应用时将执行的运行操作", Before: "当前运行数据面", After: fmt.Sprintf("%s；OpenWrt步骤=%d（破坏性=%d）：%s", actionText, len(plan.Steps), destructive, strings.Join(names, "、")), Impact: "即使配置字段没有变化，确认应用仍会执行这次 reconcile 操作"}
}

func ruleConflictChanges(conflicts []state.RuleConflict) []configChangeView {
	out := make([]configChangeView, 0, len(conflicts))
	for _, conflict := range conflicts {
		out = append(out, configChangeView{
			Category: "rule-conflict",
			Title:    fmt.Sprintf("规则 %s %q 存在归属冲突", conflict.Kind, conflict.Value),
			Before:   strings.Join(conflict.Pools, ", "),
			After:    "请选择唯一 Pool",
			Impact:   "未解决冲突会使分类结果不确定，本次应用被阻断",
			Blocking: true,
		})
	}
	return out
}

func finalizeConfigChanges(record *configDryRunRecord) {
	critical, regular := []configChangeView{}, []configChangeView{}
	preOmitted := 0
	for _, change := range record.Changes {
		if change.Category == "摘要限制" {
			preOmitted++
			continue
		}
		if change.Blocking {
			critical = append(critical, change)
			continue
		}
		switch change.Category {
		case "实际执行", "选择器", "最终路由出口", "实际运行语义":
			critical = append(critical, change)
		default:
			regular = append(regular, change)
		}
	}
	ordered := append(critical, regular...)
	kept := make([]configChangeView, 0, len(ordered))
	used := 0
	for _, change := range ordered {
		encoded, _ := json.Marshal(change)
		if used+len(encoded) > dryRunChangesBudget {
			break
		}
		kept = append(kept, change)
		used += len(encoded)
	}
	record.ChangesOmitted = preOmitted + len(ordered) - len(kept)
	record.ChangesComplete = record.ChangesOmitted == 0
	if record.ChangesOmitted > 0 {
		kept = append(kept, configChangeView{Category: "摘要限制", Title: "变更摘要超过设备安全上限", After: fmt.Sprintf("省略 %d 项", record.ChangesOmitted), Impact: "为避免软路由内存压力，本次预演禁止应用；请拆分配置或缩小订阅后重新预演"})
	}
	record.Changes = kept
}

type configChangeCollector struct {
	changes []configChangeView
	used    int
	full    bool
}

func (c *configChangeCollector) add(change configChangeView) {
	if c.full {
		return
	}
	change.Before = limitDryRunField(change.Before)
	change.After = limitDryRunField(change.After)
	encoded, _ := json.Marshal(change)
	if c.used+len(encoded) > dryRunChangesBudget {
		c.full = true
		return
	}
	c.changes = append(c.changes, change)
	c.used += len(encoded)
}

func (c *configChangeCollector) emit(build func(func(configChangeView) bool)) {
	if c.full {
		return
	}
	build(func(change configChangeView) bool {
		c.add(change)
		return !c.full
	})
}

func (c *configChangeCollector) result() []configChangeView {
	if c.full {
		return append(c.changes, configChangeView{Category: "摘要限制", Title: "变更生成达到设备安全上限"})
	}
	return c.changes
}

func writeDryRunAPIData(w http.ResponseWriter, code int, data any) {
	writeDryRunAPIEnvelope(w, code, apiEnvelope{OK: true, Data: data})
}

func writeDryRunAPIError(w http.ResponseWriter, code int, message string, data any) {
	writeDryRunAPIErrorCode(w, code, apiErrorCodeForStatus(code), message, data)
}

func writeDryRunAPIErrorCode(w http.ResponseWriter, code int, apiCode string, message string, data any) {
	if apiCode == "" {
		apiCode = apiErrorCodeForStatus(code)
	}
	writeDryRunAPIEnvelope(w, code, apiEnvelope{OK: false, Error: apiErrorPayload{
		Code:    apiCode,
		Message: redact.MessageWithPaths(message),
		Details: data,
	}})
}

func writeDryRunAPIEnvelope(w http.ResponseWriter, code int, envelope apiEnvelope) {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		writeBoundedDryRunFallback(w, http.StatusInternalServerError, "encode dry-run response failed")
		return
	}
	if len(encoded)+1 > dryRunResponseHardLimit {
		writeBoundedDryRunFallback(w, http.StatusRequestEntityTooLarge, "dry-run response exceeds device safety limit; reduce the change scope")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(encoded, '\n'))
}

func writeBoundedDryRunFallback(w http.ResponseWriter, code int, message string) {
	encoded, _ := json.Marshal(apiEnvelope{OK: false, Error: apiErrorPayload{
		Code:    apiErrorCodeForStatus(code),
		Message: sanitizeAPIMessage(message),
	}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(encoded, '\n'))
}

func gatewayModeDerivedChanges(applied state.AppliedState, spec state.SpecState) []configChangeView {
	beforeDNS, beforeRoute := "尚未应用", "尚未应用"
	if applied.SpecCaptured {
		beforeDNS, beforeRoute = gatewayDerivedSemantics(applied.Spec)
	}
	afterDNS, afterRoute := gatewayDerivedSemantics(spec)
	out := []configChangeView{}
	if beforeDNS != afterDNS {
		out = append(out, configChangeView{Category: "实际运行语义", Title: "实际 DNS 行为", Before: beforeDNS, After: afterDNS, Impact: "这是应用后真正运行的 DNS final 与上游行为"})
	}
	if beforeRoute != afterRoute {
		out = append(out, configChangeView{Category: "实际运行语义", Title: "实际路由行为", Before: beforeRoute, After: afterRoute, Impact: "这是网关模式转换后真正参与运行的路由规则语义"})
	}
	return out
}

func gatewayDerivedSemantics(spec state.SpecState) (string, string) {
	mode := state.EffectiveGatewayMode(spec.Settings.GatewayMode)
	if mode == state.GatewayModeOff || mode == state.GatewayModeSafe {
		return "Worker DNS 停止，SmartGate DNS 劫持清理", "Worker 路由停止，SmartGate 网络接管清理"
	}
	dns := selection.EffectiveDNSForGatewayMode(spec.DNS, mode, true)
	savedDNS := state.EffectiveDNSSpec(spec.DNS)
	proxyServers := append([]string(nil), dns.ProxyServers...)
	localResolverNote := ""
	if !reflect.DeepEqual(savedDNS.DirectServers, dns.DirectServers) {
		localResolverNote = "；原始本地DNS仍保留为 dns-cn/节点域名解析=" + strings.Join(savedDNS.DirectServers, ", ")
	}
	if mode == state.GatewayModeGlobal {
		return "final=proxy；直连DNS=" + strings.Join(dns.DirectServers, ", ") + "；代理DNS=" + strings.Join(proxyServers, ", ") + localResolverNote, "direct 使用受保护默认值；geosite-cn/geoip-cn 直连远程集跳过；最终流量进入全局代理"
	}
	return "final=" + dns.Final + "；直连DNS=" + strings.Join(dns.DirectServers, ", ") + "；代理DNS=" + strings.Join(proxyServers, ", ") + localResolverNote, "使用保存的有效业务分流、本地规则与远程规则"
}

func summarizeConfigChanges(applied state.AppliedState, next state.SpecState, nextTopologyHash string, currentLastGood []state.LastGoodSnapshot) []configChangeView {
	return summarizeConfigChangesWithSelection(applied, next, nextTopologyHash, currentLastGood, applied.ProxySelection)
}

func summarizeConfigChangesWithSelection(applied state.AppliedState, next state.SpecState, nextTopologyHash string, currentLastGood []state.LastGoodSnapshot, targetSelection map[string]string) []configChangeView {
	collector := configChangeCollector{changes: make([]configChangeView, 0, 16)}
	summarizeConfigChangesInto(&collector, applied, next, nextTopologyHash, currentLastGood, targetSelection)
	return collector.result()
}

func summarizeConfigChangesInto(collector *configChangeCollector, applied state.AppliedState, next state.SpecState, nextTopologyHash string, currentLastGood []state.LastGoodSnapshot, targetSelection map[string]string) {
	before := applied.Spec
	before.DNS = state.EffectiveDNSSpec(before.DNS)
	next.DNS = state.EffectiveDNSSpec(next.DNS)
	before.BusinessPools = state.EffectiveBusinessPools(before.BusinessPools)
	next.BusinessPools = state.EffectiveBusinessPools(next.BusinessPools)
	before.Rules = state.EffectiveRuleCatalog(state.DesiredRuleCatalog(before))
	next.Rules = state.EffectiveRuleCatalog(state.DesiredRuleCatalog(next))
	before.RemoteRules = state.EffectiveRemoteRuleSettings(before.RemoteRules)
	next.RemoteRules = state.EffectiveRemoteRuleSettings(next.RemoteRules)
	add := func(category, title, oldValue, newValue, impact string) {
		collector.add(configChangeView{Category: category, Title: title, Before: oldValue, After: newValue, Impact: impact})
	}
	if !applied.SpecCaptured || before.Settings.GatewayMode != next.Settings.GatewayMode {
		oldMode := before.Settings.GatewayMode
		if !applied.SpecCaptured {
			oldMode = "尚未应用"
		}
		add("网关", "网关模式变化", oldMode, next.Settings.GatewayMode, "会改变流量是否经过代理及相关网络规则")
	}
	if !applied.SpecCaptured || before.Settings.DiscardProviderRules != next.Settings.DiscardProviderRules {
		add("保存设置", "订阅自带规则处理标记", boolText(before.Settings.DiscardProviderRules, applied.SpecCaptured), boolText(next.Settings.DiscardProviderRules, true), "当前版本仅保存此标记，尚未接入运行配置生成")
	}
	if !applied.SpecCaptured || before.Settings.OverlayGuardEnabled != next.Settings.OverlayGuardEnabled {
		add("存储保护", "Overlay 空间保护", boolText(before.Settings.OverlayGuardEnabled, applied.SpecCaptured), boolText(next.Settings.OverlayGuardEnabled, true), "决定应用前是否检查闪存剩余空间")
	}
	if !applied.SpecCaptured || before.Settings.ListenLAN != next.Settings.ListenLAN || before.Settings.AllowWAN != next.Settings.AllowWAN {
		add("保存设置", "当前版本未动态应用的监听设置", settingsPassiveSummary(before.Settings, applied.SpecCaptured), settingsPassiveSummary(next.Settings, true), "这些值会保存，但当前守护进程启动参数不会在本次 apply 中改变")
	}
	if !reflect.DeepEqual(before.DNS, next.DNS) {
		add("DNS", "DNS 上游与解析策略", dnsSummary(before.DNS, applied.SpecCaptured), dnsSummary(next.DNS, true), "DNS 上游或真实 IP 分类策略将重新生成")
	}
	collector.emit(func(yield func(configChangeView) bool) {
		providerChangesEmit(before.Providers, next.Providers, applied.SpecCaptured, yield)
	})
	collector.emit(func(yield func(configChangeView) bool) {
		for _, change := range orderChange("节点来源", "Provider 顺序", providerIDs(before.Providers), providerIDs(next.Providers), applied.SpecCaptured, "会改变节点合并顺序及默认候选优先级") {
			if !yield(change) {
				return
			}
		}
	})
	collector.emit(func(yield func(configChangeView) bool) {
		poolChangesEmit(before.BusinessPools, next.BusinessPools, applied.SpecCaptured, yield)
	})
	collector.emit(func(yield func(configChangeView) bool) {
		for _, change := range orderChange("业务分组", "业务池顺序", poolIDs(before.BusinessPools), poolIDs(next.BusinessPools), applied.SpecCaptured, "会改变顶层默认业务池与选择器排列") {
			if !yield(change) {
				return
			}
		}
	})
	collector.emit(func(yield func(configChangeView) bool) {
		ruleChangesEmit(before.Rules, next.Rules, applied.SpecCaptured, yield)
	})
	collector.emit(func(yield func(configChangeView) bool) {
		remoteRuleChangesEmit(before.RemoteRules, next.RemoteRules, applied.SpecCaptured, yield)
	})
	collector.emit(func(yield func(configChangeView) bool) {
		schedulerChangesEmit(before.Scheduler, next.Scheduler, applied.SpecCaptured, yield)
	})
	collector.emit(func(yield func(configChangeView) bool) {
		for _, change := range orderChange("计划任务", "计划任务顺序", schedulerIDs(before.Scheduler), schedulerIDs(next.Scheduler), applied.SpecCaptured, "会改变任务列表的稳定顺序") {
			if !yield(change) {
				return
			}
		}
	})
	if !collector.full && !topologyDisplayInputWithinBudget(applied.LastGood, currentLastGood) {
		collector.full = true
	}
	collector.emit(func(yield func(configChangeView) bool) {
		if err := lastGoodNodeChangesEmitStrict(applied.LastGood, currentLastGood, before, next, applied.SpecCaptured, yield); err != nil {
			panic(err)
		}
	})
	collector.emit(func(yield func(configChangeView) bool) {
		selectorChangesEmit(applied, next, currentLastGood, targetSelection, yield)
	})
	collector.emit(func(yield func(configChangeView) bool) {
		resolvedRouteTargetChangesEmit(applied, next, currentLastGood, targetSelection, yield)
	})
	collector.changes = applyGatewayModeSemantics(collector.changes, state.EffectiveGatewayMode(next.Settings.GatewayMode))
	if len(collector.changes) == 0 && !collector.full {
		if applied.TopologyHash != "" && nextTopologyHash != "" && applied.TopologyHash != nextTopologyHash {
			collector.add(configChangeView{Category: "运行拓扑", Title: "有效拓扑或规范化结果变化", Before: applied.TopologyHash[:min(12, len(applied.TopologyHash))], After: nextTopologyHash[:min(12, len(nextTopologyHash))], Impact: "未归入上述字段的规范化结果发生变化，请结合高级详情确认候选配置"})
		} else {
			collector.add(configChangeView{Category: "配置", Title: "没有实际配置变化", Before: "当前已应用配置", After: "保持不变", Impact: "应用不会改变业务配置"})
		}
	}
}

func resolvedRouteTargetChanges(applied state.AppliedState, next state.SpecState, currentLastGood []state.LastGoodSnapshot, targetSelection map[string]string) []configChangeView {
	out := []configChangeView{}
	resolvedRouteTargetChangesEmit(applied, next, currentLastGood, targetSelection, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}
func resolvedRouteTargetChangesEmit(applied state.AppliedState, next state.SpecState, currentLastGood []state.LastGoodSnapshot, targetSelection map[string]string, yield func(configChangeView) bool) {
	oldTargets := map[string]string{}
	if applied.SpecCaptured {
		oldTargets = resolvedRouteTargets(applied.Spec, applied.LastGood, applied.GatewayMode, applied.ProxySelection)
	}
	newTargets := resolvedRouteTargets(next, currentLastGood, next.Settings.GatewayMode, targetSelection)
	keys := make([]string, 0, len(oldTargets)+len(newTargets))
	seen := map[string]bool{}
	for key := range oldTargets {
		seen[key] = true
		keys = append(keys, key)
	}
	for key := range newTargets {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		before, oldOK := oldTargets[key]
		after, newOK := newTargets[key]
		if oldOK && newOK && before == after {
			continue
		}
		if !oldOK {
			before = "不存在"
		}
		if !newOK {
			after = "不存在"
		}
		if !yield(configChangeView{Category: "最终路由出口", Title: key, Before: before, After: after, Impact: "业务池 ID/角色变化会重新解析规则出口；缺失池会回退到顶层 Proxy"}) {
			return
		}
	}
}

func resolvedRouteTargets(spec state.SpecState, snapshots []state.LastGoodSnapshot, mode string, selected map[string]string) map[string]string {
	mode = state.EffectiveGatewayMode(mode)
	out := map[string]string{}
	if !gatewayModeUsesDataPlane(mode) {
		return out
	}
	nodes, err := provider.BuildNodeViewsFromSnapshots(spec, snapshots)
	if err != nil {
		return out
	}
	selectors := selection.BuildSelectorStates(nodes, spec.BusinessPools, mode, selection.DefaultMaxPoolCandidates, selected)
	pools := map[string]string{}
	for _, selector := range selectors {
		pools[selector.PoolID] = selector.Selector
		pools[selector.Role] = selector.Selector
	}
	proxyAvailable := len(selectors) > 0
	resolve := func(target string) string {
		switch target {
		case "direct", "Direct":
			return "Direct"
		case "block", "Block":
			return "Block"
		case "proxy", "Proxy":
			if proxyAvailable {
				return "Proxy"
			}
			return "Direct"
		case "generalproxy", "":
			if tag := pools["general"]; tag != "" {
				return tag
			}
			if proxyAvailable {
				return "Proxy"
			}
			return "Direct"
		}
		if tag := pools[target]; tag != "" {
			return tag
		}
		if poolID, ok := selection.PoolIDFromTag(target); ok {
			if tag := pools[poolID]; tag != "" {
				return tag
			}
		}
		if proxyAvailable {
			return "Proxy"
		}
		return "Direct"
	}
	rules := state.EffectiveRuleCatalog(spec.Rules)
	if mode == state.GatewayModeGlobal {
		filtered := make(state.RuleCatalog, 0, len(rules))
		for _, rule := range rules {
			if rule.PoolID != "direct" {
				filtered = append(filtered, rule)
			}
		}
		for _, rule := range state.DefaultRules() {
			if rule.PoolID == "direct" {
				filtered = append(filtered, rule)
			}
		}
		rules = filtered
	}
	localTargets := map[string]struct{}{}
	for _, rule := range rules {
		if !rule.Enabled || rule.OverriddenBy != "" || rule.Source == state.RuleSourceGeoDefault {
			continue
		}
		localTargets[rule.PoolID] = struct{}{}
	}
	for target := range localTargets {
		out["本地规则 "+target] = resolve(target)
	}
	remote := state.EffectiveRemoteRuleSettings(spec.RemoteRules)
	for _, item := range remote.Sets {
		enabled := item.Enabled
		if item.Kind == "adblock" {
			// Same three-state gate as enabledRemoteRuleSets: master + ad_block + item.
			// ad_block.sets no longer overrides the per-set enabled flag.
			enabled = remote.AdBlock.Enabled && item.Enabled
		}
		if !remote.Enabled || !enabled {
			continue
		}
		if mode == state.GatewayModeGlobal && item.Target == "direct" && (item.ID == "geosite-cn" || item.ID == "geoip-cn") {
			continue
		}
		target := item.Outbound
		if target == "" {
			target = item.Target
		}
		out["远程规则 "+item.ID] = resolve(target)
	}
	return out
}

func selectorChanges(applied state.AppliedState, next state.SpecState, currentLastGood []state.LastGoodSnapshot, targetSelection map[string]string) []configChangeView {
	out := []configChangeView{}
	selectorChangesEmit(applied, next, currentLastGood, targetSelection, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}
func selectorChangesEmit(applied state.AppliedState, next state.SpecState, currentLastGood []state.LastGoodSnapshot, targetSelection map[string]string, yield func(configChangeView) bool) {
	oldStates := []selection.SelectorState{}
	if applied.SpecCaptured && gatewayModeUsesDataPlane(applied.GatewayMode) {
		if nodes, err := provider.BuildNodeViewsFromSnapshots(applied.Spec, applied.LastGood); err == nil {
			oldStates = selection.BuildSelectorStates(nodes, applied.Spec.BusinessPools, applied.GatewayMode, selection.DefaultMaxPoolCandidates, applied.ProxySelection)
		}
	}
	newStates := []selection.SelectorState{}
	newMode := state.EffectiveGatewayMode(next.Settings.GatewayMode)
	if gatewayModeUsesDataPlane(newMode) {
		if nodes, err := provider.BuildNodeViewsFromSnapshots(next, currentLastGood); err == nil {
			newStates = selection.BuildSelectorStates(nodes, next.BusinessPools, newMode, selection.DefaultMaxPoolCandidates, targetSelection)
		}
	}
	oldByID := map[string]selection.SelectorState{}
	for _, item := range oldStates {
		oldByID[item.PoolID] = item
	}
	newByID := map[string]selection.SelectorState{}
	for _, item := range newStates {
		newByID[item.PoolID] = item
	}
	ids := make([]string, 0, len(oldByID)+len(newByID))
	seen := map[string]bool{}
	for id := range oldByID {
		seen[id] = true
		ids = append(ids, id)
	}
	for id := range newByID {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		oldState, oldOK := oldByID[id]
		newState, newOK := newByID[id]
		if oldOK && newOK && oldState.Selected == newState.Selected && reflect.DeepEqual(oldState.Candidates, newState.Candidates) {
			continue
		}
		beforeText, afterText, title := "不存在", "不存在", "业务池 "+id+" 选择器变化"
		if oldOK {
			beforeText = "选中=" + oldState.Selected + "；候选=" + strings.Join(oldState.Candidates, ", ")
		}
		if newOK {
			afterText = "选中=" + newState.Selected + "；候选=" + strings.Join(newState.Candidates, ", ")
		}
		if oldOK && newOK && oldState.Selected != newState.Selected {
			title = "业务池 " + id + " 将切换节点"
		}
		if !yield(configChangeView{Category: "选择器", Title: title, Before: beforeText, After: afterText, Impact: "应用会恢复仍有效的持久选择；失效时回退到新的默认候选"}) {
			return
		}
	}
}

func applyGatewayModeSemantics(changes []configChangeView, mode string) []configChangeView {
	for i := range changes {
		switch mode {
		case state.GatewayModeOff, state.GatewayModeSafe:
			switch changes[i].Category {
			case "DNS", "节点来源", "业务分组", "rule", "远程规则", "广告拦截", "远程规则集", "实际节点":
				changes[i].Impact = "本次仅保存；当前网关模式不会启动 Worker 数据面，切换到 on/global 后才参与运行"
			}
		case state.GatewayModeGlobal:
			if changes[i].Category == "rule" && strings.Contains(changes[i].After, "归属=direct") {
				changes[i].Impact = "本次保存，但 global 模式会使用受保护的默认 direct 规则；切换到 on 后使用该保存值"
			}
			if changes[i].Category == "远程规则集" && (strings.Contains(changes[i].Title, "geosite-cn") || strings.Contains(changes[i].Title, "geoip-cn")) {
				changes[i].Impact = "若该集合目标为 direct，global 模式会跳过；其他目标仍会立即参与运行"
			}
		}
	}
	return changes
}

func boolText(value, known bool) string {
	if !known {
		return "尚未应用"
	}
	if value {
		return "启用"
	}
	return "停用"
}
func dnsSummary(value state.DNSSpec, known bool) string {
	if !known {
		return "尚未应用"
	}
	return fmt.Sprintf("策略=%s；最终=%s；直连=%s；代理=%s；备用=%s；分类=真实IP+nft set", value.Strategy, value.Final, joinDryRunDisplay(value.DirectServers, ", "), joinDryRunDisplay(value.ProxyServers, ", "), joinDryRunDisplay(value.FallbackServers, ", "))
}
func structuredDisplaySummary(value any, known bool) string {
	if !known {
		return "尚未应用"
	}
	parts := []string{}
	used := 0
	var walk func(string, any)
	walk = func(path string, raw any) {
		if used >= dryRunFieldLimit {
			return
		}
		switch item := raw.(type) {
		case map[string]any:
			keys := make([]string, 0, len(item))
			for key := range item {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next := key
				if path != "" {
					next = path + "." + key
				}
				walk(next, item[key])
				if used >= dryRunFieldLimit {
					return
				}
			}
		case []any:
			for index, child := range item {
				walk(fmt.Sprintf("%s[%d]", path, index), child)
				if used >= dryRunFieldLimit {
					return
				}
			}
		default:
			appendDisplayPart(&parts, &used, path, structuredDisplayValue(path, item))
		}
	}
	walk("", value)
	return strings.Join(parts, "；")
}
func settingsPassiveSummary(value state.SettingsSpec, known bool) string {
	if !known {
		return "尚未应用"
	}
	return fmt.Sprintf("LAN监听=%s；允许WAN=%t", value.ListenLAN, value.AllowWAN)
}

func remoteRuleChanges(before, after state.RemoteRuleSettings, known bool) []configChangeView {
	out := []configChangeView{}
	remoteRuleChangesEmit(before, after, known, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}

func remoteRuleChangesEmit(before, after state.RemoteRuleSettings, known bool, yield func(configChangeView) bool) {
	globalBefore := fmt.Sprintf("启用=%t；格式=%s；下载出口=%s；更新=%s；最多启用=%d；总上限=%d；单集上限=%d；总量警告=%d；数量警告=%d", before.Enabled, before.DefaultFormat, before.DownloadDetour, before.UpdateInterval, before.MemoryGuard.MaxEnabledSets, before.MemoryGuard.MaxTotalBytes, before.MemoryGuard.MaxSetBytes, before.MemoryGuard.WarnTotalBytes, before.MemoryGuard.WarnEnabledSets)
	globalAfter := fmt.Sprintf("启用=%t；格式=%s；下载出口=%s；更新=%s；最多启用=%d；总上限=%d；单集上限=%d；总量警告=%d；数量警告=%d", after.Enabled, after.DefaultFormat, after.DownloadDetour, after.UpdateInterval, after.MemoryGuard.MaxEnabledSets, after.MemoryGuard.MaxTotalBytes, after.MemoryGuard.MaxSetBytes, after.MemoryGuard.WarnTotalBytes, after.MemoryGuard.WarnEnabledSets)
	if !known || globalBefore != globalAfter {
		if !known {
			globalBefore = "尚未应用"
		}
		if !yield(configChangeView{Category: "远程规则", Title: "全局下载与内存设置", Before: globalBefore, After: globalAfter, Impact: "会改变远程规则的启用、下载出口、刷新周期或内存限制"}) {
			return
		}
	}
	adBefore, adAfter := "尚未应用", fmt.Sprintf("启用=%t；模式=%s；规则集=%s", after.AdBlock.Enabled, after.AdBlock.Mode, strings.Join(after.AdBlock.Sets, ","))
	if known {
		adBefore = fmt.Sprintf("启用=%t；模式=%s；规则集=%s", before.AdBlock.Enabled, before.AdBlock.Mode, strings.Join(before.AdBlock.Sets, ","))
	}
	if !known || !reflect.DeepEqual(before.AdBlock, after.AdBlock) {
		if !yield(configChangeView{Category: "广告拦截", Title: "广告拦截设置", Before: adBefore, After: adAfter, Impact: "会改变广告规则集及拦截模式"}) {
			return
		}
	}
	old := map[string]state.RemoteRuleSet{}
	for _, item := range before.Sets {
		old[item.ID] = item
	}
	next := map[string]state.RemoteRuleSet{}
	for _, item := range after.Sets {
		next[item.ID] = item
	}
	if !entityChangesEmit("远程规则集", old, next, known, func(v state.RemoteRuleSet) string {
		return fmt.Sprintf("%s；启用=%t；种类=%s；目标=%s；出口=%s；格式=%s/%s；URL=%s；路径=%s；下载出口=%s；更新=%s；预计=%d；标签=%s；说明=%s", v.Name, v.Enabled, v.Kind, v.Target, v.Outbound, v.Format, v.Type, redactRemoteURL(v.URL), v.Path, v.DownloadDetour, v.UpdateInterval, v.EstimatedBytes, strings.Join(v.Tags, ","), v.Description)
	}, "会改变该远程规则集的来源、匹配类型或路由目标", yield) {
		return
	}
	setIDs := func(items []state.RemoteRuleSet) []string {
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		return ids
	}
	for _, change := range orderChange("远程规则集", "规则集匹配顺序", setIDs(before.Sets), setIDs(after.Sets), known, "会改变 first-match 路由规则的优先顺序") {
		if !yield(change) {
			return
		}
	}
}

func providerChanges(before, after []state.ProviderSpec, known bool) []configChangeView {
	out := []configChangeView{}
	providerChangesEmit(before, after, known, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}
func providerChangesEmit(before, after []state.ProviderSpec, known bool, yield func(configChangeView) bool) {
	old := map[string]state.ProviderSpec{}
	for _, item := range before {
		old[item.ID] = item
	}
	next := map[string]state.ProviderSpec{}
	for _, item := range after {
		next[item.ID] = item
	}
	entityChangesEmit("节点来源", old, next, known, func(v state.ProviderSpec) string {
		source := v.URL
		if v.Type == provider.TypeLocalFile {
			source = v.FilePath
		}
		if v.Type == provider.TypeManual {
			source = "手动节点"
		}
		return fmt.Sprintf("%s；类型=%s；启用=%t；获取=%s；地址=%s；Bootstrap=%s；优先级=%d；角色=%s；包含=%s；排除=%s；last-good=%s", v.Name, v.Type, v.Enabled, v.FetchMode, redactSource(firstNonEmpty(source)), redactSource(v.Bootstrap), v.Priority, strings.Join(v.Roles, ","), strings.Join(v.IncludeKeywords, ","), strings.Join(v.ExcludeKeywords, ","), v.LastGoodID)
	}, "会改变节点来源、可用节点或订阅获取方式", yield)
}
func poolChanges(before, after []state.BusinessPoolSpec, known bool) []configChangeView {
	out := []configChangeView{}
	poolChangesEmit(before, after, known, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}
func poolChangesEmit(before, after []state.BusinessPoolSpec, known bool, yield func(configChangeView) bool) {
	old := map[string]state.BusinessPoolSpec{}
	for _, item := range before {
		old[item.ID] = item
	}
	next := map[string]state.BusinessPoolSpec{}
	for _, item := range after {
		next[item.ID] = item
	}
	entityChangesEmit("业务分组", old, next, known, func(v state.BusinessPoolSpec) string {
		return fmt.Sprintf("%s（%s）；节点=%s", v.Name, v.Role, strings.Join(v.NodeIDs, ", "))
	}, "会改变该业务流量的候选节点和故障切换范围", yield)
}
func schedulerChanges(before, after []state.SchedulerTaskSpec, known bool) []configChangeView {
	out := []configChangeView{}
	schedulerChangesEmit(before, after, known, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}
func schedulerChangesEmit(before, after []state.SchedulerTaskSpec, known bool, yield func(configChangeView) bool) {
	old := map[string]state.SchedulerTaskSpec{}
	for _, item := range before {
		old[item.ID] = item
	}
	next := map[string]state.SchedulerTaskSpec{}
	for _, item := range after {
		next[item.ID] = item
	}
	entityChangesEmit("计划任务", old, next, known, func(v state.SchedulerTaskSpec) string {
		return fmt.Sprintf("%s；类型=%s；启用=%t；计划=%s；参数=%s", v.Name, v.Type, v.Enabled, v.Schedule, structuredDisplaySummary(v.Spec, true))
	}, "该任务设置保存时已经立即生效；本次 apply 只把它纳入 applied/回滚基线", yield)
}
func ruleChanges(before, after state.RuleCatalog, known bool) []configChangeView {
	out := []configChangeView{}
	ruleChangesEmit(before, after, known, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}
func ruleChangesEmit(before, after state.RuleCatalog, known bool, yield func(configChangeView) bool) {
	old := make(map[string]state.Rule, len(before))
	for _, rule := range before {
		old[rule.ID] = rule
	}
	next := make(map[string]state.Rule, len(after))
	for _, rule := range after {
		next[rule.ID] = rule
	}
	entityChangesEmit("rule", old, next, known, func(v state.Rule) string {
		return fmt.Sprintf("来源=%s；类型=%s；值=%s；归属=%s；启用=%t", v.Source, v.Kind, v.Value, v.PoolID, v.Enabled)
	}, "会改变匹配流量的路由目标", yield)
}
func entityChanges[T any](category string, before, after map[string]T, known bool, summary func(T) string, impact string) []configChangeView {
	out := []configChangeView{}
	entityChangesEmit(category, before, after, known, summary, impact, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}
func entityChangesEmit[T any](category string, before, after map[string]T, known bool, summary func(T) string, impact string, yield func(configChangeView) bool) bool {
	keys := make([]string, 0, len(before)+len(after))
	seen := map[string]bool{}
	for key := range before {
		seen[key] = true
		keys = append(keys, key)
	}
	for key := range after {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		oldValue, oldOK := before[key]
		newValue, newOK := after[key]
		if known && oldOK && newOK && reflect.DeepEqual(oldValue, newValue) {
			continue
		}
		title := "修改 " + key
		oldText, newText := "无", "无"
		if !known {
			oldText = "尚未应用"
		} else if oldOK {
			oldText = summary(oldValue)
		}
		if newOK {
			newText = summary(newValue)
		}
		if !oldOK || !known {
			title = "新增 " + key
		}
		if !newOK {
			title = "删除 " + key
		}
		change := configChangeView{Category: category, Title: title, Before: limitDryRunField(oldText), After: limitDryRunField(newText), Impact: impact}
		if !yield(change) {
			return false
		}
	}
	return true
}

func limitDryRunField(value string) string {
	if len(value) <= dryRunFieldLimit {
		return value
	}
	return value[:dryRunFieldLimit] + "…[TRUNCATED]"
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "未设置"
}

func orderChange(category, title string, before, after []string, known bool, impact string) []configChangeView {
	if known && reflect.DeepEqual(before, after) {
		return nil
	}
	oldText := "尚未应用"
	if known {
		oldText = joinDryRunDisplay(before, " → ")
	}
	return []configChangeView{{Category: category, Title: title, Before: oldText, After: joinDryRunDisplay(after, " → "), Impact: impact}}
}

func joinDryRunDisplay(values []string, separator string) string {
	var builder strings.Builder
	for index, value := range values {
		extra := len(value)
		if index > 0 {
			extra += len(separator)
		}
		if builder.Len()+extra > dryRunFieldLimit {
			builder.WriteString("…[TRUNCATED]")
			break
		}
		if index > 0 {
			builder.WriteString(separator)
		}
		builder.WriteString(value)
	}
	return builder.String()
}
func providerIDs(items []state.ProviderSpec) []string {
	out := make([]string, 0, len(items))
	for _, v := range items {
		out = append(out, v.ID)
	}
	return out
}
func poolIDs(items []state.BusinessPoolSpec) []string {
	out := make([]string, 0, len(items))
	for _, v := range items {
		out = append(out, v.ID)
	}
	return out
}
func schedulerIDs(items []state.SchedulerTaskSpec) []string {
	out := make([]string, 0, len(items))
	for _, v := range items {
		out = append(out, v.ID)
	}
	return out
}

func lastGoodNodeChanges(before, after []state.LastGoodSnapshot, beforeSpec, afterSpec state.SpecState, known bool) []configChangeView {
	out := []configChangeView{}
	if err := lastGoodNodeChangesEmitStrict(before, after, beforeSpec, afterSpec, known, func(change configChangeView) bool { out = append(out, change); return true }); err != nil {
		panic(err)
	}
	return out
}

func topologyDisplayInputWithinBudget(groups ...[]state.LastGoodSnapshot) bool {
	const maxNodes = 4096
	const maxScalarBytes = 1024 * 1024
	nodes, scalarBytes := 0, 0
	var measure func(any) bool
	measure = func(value any) bool {
		switch item := value.(type) {
		case map[string]any:
			for key, child := range item {
				scalarBytes += len(key)
				if scalarBytes > maxScalarBytes || !measure(child) {
					return false
				}
			}
		case []any:
			for _, child := range item {
				if !measure(child) {
					return false
				}
			}
		case string:
			scalarBytes += min(len(item), dryRunFieldLimit)
		default:
			scalarBytes += 16
		}
		return scalarBytes <= maxScalarBytes
	}
	for _, snapshots := range groups {
		for _, snapshot := range snapshots {
			values := make([]any, 0, len(snapshot.Identities))
			for _, source := range snapshot.Identities {
				values = append(values, source)
			}
			if len(values) == 0 {
				for _, outbound := range snapshot.Outbounds {
					values = append(values, outbound)
				}
			}
			for _, value := range values {
				nodes++
				if nodes > maxNodes || !measure(value) {
					return false
				}
			}
		}
	}
	return true
}

func lastGoodNodeChangesEmit(before, after []state.LastGoodSnapshot, beforeSpec, afterSpec state.SpecState, known bool, yield func(configChangeView) bool) {
	if err := lastGoodNodeChangesEmitStrict(before, after, beforeSpec, afterSpec, known, yield); err != nil {
		panic(err)
	}
}

func lastGoodNodeChangesEmitStrict(before, after []state.LastGoodSnapshot, beforeSpec, afterSpec state.SpecState, known bool, yield func(configChangeView) bool) error {
	if !topologyDisplayInputWithinBudget(before, after) {
		return nil
	}
	build := func(spec state.SpecState, snapshots []state.LastGoodSnapshot) (map[string]provider.NodeView, map[string][]string, error) {
		views, err := provider.BuildNodeViewsFromSnapshots(spec, snapshots)
		if err != nil {
			return nil, nil, err
		}
		nodes := map[string]provider.NodeView{}
		order := map[string][]string{}
		for _, node := range views {
			if node.Excluded {
				continue
			}
			nodes[node.ID] = node
			order[node.ProviderID] = append(order[node.ProviderID], node.ID)
		}
		return nodes, order, nil
	}
	oldNodes, oldOrder, err := build(beforeSpec, before)
	if err != nil {
		return err
	}
	newNodes, newOrder, err := build(afterSpec, after)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(oldNodes)+len(newNodes))
	seen := map[string]bool{}
	for key := range oldNodes {
		seen[key] = true
		keys = append(keys, key)
	}
	for key := range newNodes {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		oldNode, oldOK := oldNodes[key]
		newNode, newOK := newNodes[key]
		if known && oldOK && newOK && reflect.DeepEqual(oldNode.Identity, newNode.Identity) {
			continue
		}
		display := key
		if oldOK {
			display = oldNode.ProviderID + "/" + oldNode.Tag
		}
		if newOK {
			display = newNode.ProviderID + "/" + newNode.Tag
		}
		title, beforeText, afterText := "修改 "+display, effectiveNodeSummary(oldNode), effectiveNodeSummary(newNode)
		if !known || !oldOK {
			title, beforeText = "新增 "+display, "无"
		}
		if !newOK {
			title, afterText = "删除 "+display, "无"
		}
		change := configChangeView{Category: "实际节点", Title: title, Before: beforeText, After: afterText, Impact: "会改变 Worker 数据面中的实际可选节点或其连接参数"}
		if !yield(change) {
			return nil
		}
	}
	providers := map[string]bool{}
	for id := range oldOrder {
		providers[id] = true
	}
	for id := range newOrder {
		providers[id] = true
	}
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, change := range orderChange("实际节点", "节点顺序 "+id, oldOrder[id], newOrder[id], known, "会改变该 Provider 节点的默认候选优先级") {
			if !yield(change) {
				return nil
			}
		}
	}
	return nil
}
func effectiveNodeSummary(node provider.NodeView) string {
	body, err := json.Marshal(node.Identity)
	if err != nil {
		panic(err)
	}
	value := map[string]any{}
	if err := json.Unmarshal(body, &value); err != nil {
		panic(err)
	}
	return flattenOutbound(value)
}
func flattenOutbound(value map[string]any) string {
	parts := []string{}
	used := 0
	var walk func(string, any)
	walk = func(path string, raw any) {
		if used >= dryRunFieldLimit {
			return
		}
		switch item := raw.(type) {
		case map[string]any:
			keys := make([]string, 0, len(item))
			for key := range item {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next := key
				if path != "" {
					next = path + "." + key
				}
				walk(next, item[key])
			}
		case []any:
			for index, child := range item {
				walk(fmt.Sprintf("%s[%d]", path, index), child)
			}
		default:
			appendDisplayPart(&parts, &used, path, outboundDisplayValue(path, item))
		}
	}
	walk("", value)
	return strings.Join(parts, "；")
}

func appendDisplayPart(parts *[]string, used *int, path, value string) {
	remaining := dryRunFieldLimit - *used
	if len(*parts) > 0 {
		remaining -= len("；")
	}
	if remaining <= 0 {
		return
	}
	if len(path)+1 >= remaining {
		*parts = append(*parts, "…=[TRUNCATED]")
		*used = dryRunFieldLimit
		return
	}
	maxValue := remaining - len(path) - 1
	if len(value) > maxValue {
		suffix := "…[TRUNCATED]"
		if maxValue > len(suffix) {
			value = value[:maxValue-len(suffix)] + suffix
		} else {
			value = suffix[:maxValue]
		}
		*used = dryRunFieldLimit
	} else {
		*used += len(path) + 1 + len(value)
		if len(*parts) > 0 {
			*used += len("；")
		}
	}
	*parts = append(*parts, path+"="+value)
}

func displayPathKey(path string) string {
	key := path
	if dot := strings.LastIndex(key, "."); dot >= 0 {
		key = key[dot+1:]
	}
	if bracket := strings.Index(key, "["); bracket >= 0 {
		key = key[:bracket]
	}
	return strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(key))
}

func sensitiveDisplayPath(path string) bool {
	lower := strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(path))
	for _, marker := range []string{"password", "passwd", "secret", "private_key", "token", "auth", "uuid", "cookie", "psk", "credential", "header", "command", "args", "shell", "script"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func boundedDisplayScalar(value any) string {
	switch item := value.(type) {
	case string:
		if len(item) > dryRunFieldLimit {
			return item[:dryRunFieldLimit] + "…[TRUNCATED]"
		}
		return item
	case bool, float64, float32, int, int64, int32, uint, uint64, uint32, json.Number:
		return fmt.Sprint(item)
	case nil:
		return "null"
	default:
		return "[REDACTED]"
	}
}

func structuredDisplayValue(path string, value any) string {
	if sensitiveDisplayPath(path) {
		return "[REDACTED]"
	}
	safe := map[string]bool{
		"provider_id": true, "service": true, "action": true, "mode": true,
		"enabled": true, "timeout": true, "interval": true, "max_attempts": true,
	}
	if !safe[displayPathKey(path)] {
		return "[REDACTED]"
	}
	return boundedDisplayScalar(value)
}

func outboundDisplayValue(path string, value any) string {
	if sensitiveDisplayPath(path) {
		return "[REDACTED]"
	}
	safe := map[string]bool{
		"type": true, "tag": true, "server": true, "server_port": true,
		"network": true, "tls_enabled": true, "tls_server_name": true,
		"transport_type": true, "transport_path": true,
	}
	key := strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(path))
	if !safe[key] {
		return "[REDACTED]"
	}
	if strings.Contains(key, "path") {
		return "[REDACTED]"
	}
	return boundedDisplayScalar(value)
}
func redactSource(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return value
	}
	return parsed.Scheme + "://" + parsed.Host
}

func redactRemoteURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[REDACTED]"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (api configAPI) apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload configApplyPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if payload.DryRunID == "" {
		writeAPIError(w, http.StatusBadRequest, "dryrun_id is required")
		return
	}
	applyResult, err := api.applyDryRun(r.Context(), payload.DryRunID)
	if err != nil {
		var applyErr configApplyError
		apiCode := ""
		if errors.As(err, &applyErr) {
			apiCode = applyErr.apiCode
		}
		logConfigRequestRejected("apply", payload.DryRunID, configApplyErrorStatus(err), apiCode, err)
		if errors.As(err, &applyErr) && applyErr.apiCode != "" {
			writeAPIErrorCode(w, configApplyErrorStatus(err), applyErr.apiCode, applyErr.err.Error(), applyErr.data)
			return
		}
		if errors.As(err, &applyErr) && applyErr.data != nil {
			writeAPIErrorData(w, configApplyErrorStatus(err), err.Error(), applyErr.data)
			return
		}
		writeAPIError(w, configApplyErrorStatus(err), err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, applyResult)
}

func (api configAPI) applyDryRun(ctx context.Context, dryRunID string) (configApplyResult, error) {
	record, err := api.loadDryRunLocked("config-apply-load")
	if err != nil {
		return configApplyResult{}, configApplyError{code: http.StatusPreconditionRequired, err: err}
	}
	if record.ID != dryRunID {
		return configApplyResult{}, configApplyError{code: http.StatusConflict, err: fmt.Errorf("dryrun_id does not match latest dry-run")}
	}
	return api.applyDryRunRecord(ctx, record, true, false)
}

func (api configAPI) applyDryRunRecord(ctx context.Context, record configDryRunRecord, removeDryRun bool, automatic bool) (configApplyResult, error) {
	defer func() { _ = api.cleanupDryRun(record) }()
	if removeDryRun {
		defer func() { _ = api.removeDryRunIfIDLocked("config-apply-consume", record.ID) }()
	}
	if api.store == nil && !record.ChangesComplete {
		return configApplyResult{}, configApplyError{code: http.StatusPreconditionRequired, err: fmt.Errorf("dry-run readable change summary is incomplete; split the change and run dry-run again")}
	}
	spec, runtimeState, err := api.loadSpecRuntime("config-apply-read")
	if err != nil {
		return configApplyResult{}, err
	}
	conflicts, err := state.RuleConflicts(spec)
	if err != nil {
		return configApplyResult{}, configApplyError{code: http.StatusUnprocessableEntity, apiCode: "rules_invalid", err: err}
	}
	if len(conflicts) > 0 {
		return configApplyResult{}, configApplyError{
			code: http.StatusUnprocessableEntity, apiCode: "rule_conflict_unresolved",
			err: fmt.Errorf("unresolved rule conflicts must be resolved before apply"), data: map[string]any{"conflicts": conflicts},
		}
	}
	if !record.ChangesComplete {
		return configApplyResult{}, configApplyError{code: http.StatusPreconditionRequired, err: fmt.Errorf("dry-run readable change summary is incomplete; split the change and run dry-run again")}
	}
	if api.rescueActive() {
		return configApplyResult{}, configApplyError{code: http.StatusLocked, err: fmt.Errorf("rescue gate is active")}
	}
	appliedState, err := api.loadAppliedState(spec.Settings.GatewayMode)
	if err != nil {
		return configApplyResult{}, err
	}
	if appliedState.ConfigEpoch != record.ConfigEpoch {
		return configApplyResult{}, configApplyError{code: http.StatusConflict, err: fmt.Errorf("applied config epoch changed after dry-run; run dry-run again")}
	}
	if !selectionMapsEqual(runtimeState.ProxySelection, record.ProxySelection) {
		return configApplyResult{}, configApplyError{code: http.StatusConflict, err: fmt.Errorf("proxy selection changed after dry-run; run dry-run again")}
	}
	if automatic && !gatewayModeUsesDataPlane(spec.Settings.GatewayMode) {
		return configApplyResult{}, scheduler.ErrGatewayOff
	}
	specHash := provider.HashSpec(spec)
	if specHash != record.SpecHash {
		return configApplyResult{}, configApplyError{code: http.StatusConflict, err: fmt.Errorf("spec changed after dry-run; run dry-run again")}
	}
	lastGood, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(api.layout))
	if err != nil {
		var limitErr *provider.TopologyResourceLimitError
		if errors.As(err, &limitErr) {
			return configApplyResult{}, configApplyError{code: http.StatusRequestEntityTooLarge, err: err}
		}
		return configApplyResult{}, err
	}
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, lastGood)
	if err != nil {
		return configApplyResult{}, err
	}
	if topologyHash != record.TopologyHash {
		return configApplyResult{}, configApplyError{code: http.StatusConflict, err: fmt.Errorf("effective topology changed after dry-run; run dry-run again")}
	}
	gatewayMode := state.EffectiveGatewayMode(spec.Settings.GatewayMode)
	if gatewayMode != record.GatewayMode {
		return configApplyResult{}, configApplyError{code: http.StatusConflict, err: fmt.Errorf("gateway mode changed after dry-run; run dry-run again")}
	}
	if !gatewayModeUsesDataPlane(gatewayMode) {
		return api.applyPreparedTransaction(ctx, spec, lastGood, record, removeDryRun, configResourceGuardView{
			Enabled: false, SoftLimitBytes: configSoftLimitBytes, HardLimitBytes: worker.DefaultMaxCandidateBytes,
		})
	}
	if record.GraphHash == "" {
		return configApplyResult{}, configApplyError{code: http.StatusPreconditionRequired, err: fmt.Errorf("dry-run is missing execution graph")}
	}
	if record.WorkerPrepared == nil || record.FabricPrepared == nil {
		return configApplyResult{}, configApplyError{code: http.StatusPreconditionRequired, err: fmt.Errorf("dry-run is missing Worker or Gateway Fabric artifacts")}
	}
	resourceGuard, err := api.resourceGuardForRecord(spec, record)
	if err != nil {
		return configApplyResult{}, configApplyError{code: http.StatusInsufficientStorage, err: err}
	}
	return api.applyPreparedTransaction(ctx, spec, lastGood, record, removeDryRun, resourceGuard)
}

func configTxnForConfig(cfg Config) *commitlane.ConfigTxn {
	if cfg.ConfigTxn != nil {
		return cfg.ConfigTxn
	}
	if cfg.CommitLane == nil || cfg.Store == nil {
		return nil
	}
	engine := configapply.Engine{
		Owners:               cfg.Owners,
		WorkerCleanupTargets: cfg.WorkerCleanupTargets,
		NetGateRunner:        cfg.NetGateRunner,
	}
	if cfg.Workers != nil && cfg.Fabric != nil {
		engine.Workers = cfg.Workers
		engine.Fabric = cfg.Fabric
	}
	return commitlane.NewConfigTxn(cfg.CommitLane, cfg.Store, engine)
}

func (api configAPI) applyPreparedTransaction(ctx context.Context, spec state.SpecState, lastGood []state.LastGoodSnapshot, record configDryRunRecord, removeDryRun bool, guard configResourceGuardView) (configApplyResult, error) {
	if api.configTxn == nil {
		return configApplyResult{}, configApplyError{code: http.StatusNotImplemented, err: fmt.Errorf("config transaction is not configured")}
	}
	if record.ConfigEpoch > 0 && !record.SelectionCaptured {
		return configApplyResult{}, configApplyError{code: http.StatusPreconditionRequired, err: fmt.Errorf("dry-run is missing persisted Selection snapshot")}
	}
	loader, err := provider.NewSnapshotLastGoodLoader(lastGood)
	if err != nil {
		return configApplyResult{}, err
	}
	nodes, err := provider.BuildNodeViews(spec, loader)
	if err != nil {
		return configApplyResult{}, err
	}
	prepared := configapply.Prepared{
		OpID: record.ID, Target: configapply.TargetState{Spec: spec, LastGood: lastGood, ProxySelection: cloneSelectionMap(record.ProxySelection), Selections: clonePoolSelectionMap(record.Selections), PreviousGraph: record.PreviousGraph},
		SpecHash: record.SpecHash, TopologyHash: record.PreparedTopologyHash, GatewayMode: record.GatewayMode,
		ConfigEpoch: record.ConfigEpoch, Nodes: nodes,
		ExecutionGraph: record.ExecutionGraph, GraphHash: record.GraphHash,
		GraphIssues: append([]planner.GraphIssue(nil), record.GraphIssues...), Workers: record.WorkerPrepared, Fabric: record.FabricPrepared,
	}
	if len(record.LastGood) > 0 {
		prepared.Target.LastGood = record.LastGood
	}
	if prepared.TopologyHash == "" {
		prepared.TopologyHash = record.TopologyHash
	}
	if record.OpenWrtPlan == nil {
		return configApplyResult{}, configApplyError{code: http.StatusPreconditionRequired, err: fmt.Errorf("dry-run is missing OpenWrt plan")}
	}
	prepared.OpenWrtPlan = *record.OpenWrtPlan
	applyCtx, cancel := context.WithTimeout(ctx, configApplyTimeout)
	defer cancel()
	txnResult, err := api.configTxn.ApplyPrepared(applyCtx, commitlane.ConfigTxnApplyRequest{
		OpID: record.ID, Kind: "config_apply", Prepared: prepared,
		ExpectedSpecHash: record.SpecHash, ExpectedTopologyHash: record.TopologyHash,
		ExpectedSelection:  cloneSelectionMap(record.ProxySelection),
		ExpectedSelections: clonePoolSelectionMap(record.Selections),
		Selected:           cloneSelectionMap(record.ProxySelection),
	})
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, commitlane.ErrRescueActive) || errors.Is(err, commitlane.ErrRecoveryRequired) {
			code = http.StatusLocked
		} else if strings.Contains(err.Error(), "changed after dry-run") {
			code = http.StatusConflict
		} else if strings.Contains(err.Error(), "candidate") || strings.Contains(err.Error(), "prepared") || strings.Contains(err.Error(), "execution graph") {
			code = http.StatusPreconditionRequired
		}
		var data any
		var rollbackErr *commitlane.ConfigTxnRollbackError
		if errors.As(err, &rollbackErr) {
			var rollbackErrors []string
			if rollbackErr.RollbackError != nil {
				rollbackErrors = append(rollbackErrors, rollbackErr.RollbackError.Error())
			}
			data = configApplyErrorData{Rollback: &configRollbackReport{
				Attempted: true, OK: rollbackErr.OK(), Stage: "transaction_apply", Reason: err.Error(),
				WorkersRestored: rollbackErr.Result.WorkersRestored, FabricRestored: rollbackErr.Result.FabricRestored,
				BindingsRestored: rollbackErr.Result.BindingsRestored, SelectionRestored: rollbackErr.SelectionRestored,
				Errors: rollbackErrors,
			}}
		}
		return configApplyResult{}, configApplyError{code: code, err: err, data: data}
	}
	warnings := append([]string{}, guard.Warnings...)
	warnings = append(warnings, txnResult.Apply.Warnings...)
	workerResults := make([]configWorkerResult, 0, len(txnResult.Apply.Workers.Workers))
	for _, activation := range txnResult.Apply.Workers.Workers {
		workerResults = append(workerResults, configWorkerResult{ID: activation.WorkerID, Status: "running", ConfigPath: activation.ConfigPath})
	}
	return configApplyResult{
		Status: "applied", ConfigEpoch: txnResult.Applied.ConfigEpoch, TopologyHash: txnResult.Applied.TopologyHash,
		OpenWrt: safeNetgateApplyResult(txnResult.Apply.OpenWrt), OpenWrtApplied: true,
		CoreResults: coreApplyResultsFromWorkers(record.ExecutionGraph.Workers, workerResults),
		Workers:     workerResults, FabricApplied: txnResult.Apply.FabricApplied,
		RuntimeAction: runtimeActionForGatewayMode(record.GatewayMode), ResourceGuard: guard, Warnings: warnings,
	}, nil
}

func coreApplyResultsFromWorkers(targets []state.WorkerTarget, workers []configWorkerResult) []configCoreApplyResult {
	coreByWorker := make(map[string]string, len(targets))
	for _, target := range targets {
		if target.ID != "" && target.Core != "" {
			coreByWorker[target.ID] = string(target.Core)
		}
	}
	workersByCore := map[string][]string{}
	for _, result := range workers {
		core := coreByWorker[result.ID]
		if core == "" {
			continue
		}
		workersByCore[core] = append(workersByCore[core], result.ID)
	}
	cores := make([]string, 0, len(workersByCore))
	for core := range workersByCore {
		cores = append(cores, core)
	}
	sort.Strings(cores)
	out := make([]configCoreApplyResult, 0, len(cores))
	for _, core := range cores {
		ids := workersByCore[core]
		sort.Strings(ids)
		out = append(out, configCoreApplyResult{Core: core, Status: "running", Workers: ids})
	}
	return out
}

func safeNetgateApplyResult(result netgate.ApplyResult) netgate.ApplyResult {
	result.Logs = nil
	return result
}

type configApplyError struct {
	code    int
	apiCode string
	err     error
	data    any
}

type configDryRunError struct {
	code   int
	record configDryRunRecord
	err    error
}

func (e configApplyError) Error() string {
	return e.err.Error()
}

func (e configApplyError) Unwrap() error {
	return e.err
}

func (e configDryRunError) Error() string {
	return e.err.Error()
}

func (e configDryRunError) Unwrap() error {
	return e.err
}

func configApplyErrorStatus(err error) int {
	if code, ok := configRuntimeErrorStatus(err); ok {
		return code
	}
	return http.StatusInternalServerError
}

func configDryRunErrorStatus(err error) int {
	if code, ok := configRuntimeErrorStatus(err); ok {
		return code
	}
	return http.StatusInternalServerError
}

func configRuntimeErrorStatus(err error) (int, bool) {
	var applyErr configApplyError
	if err != nil && errors.As(err, &applyErr) {
		return applyErr.code, true
	}
	var dryRunErr configDryRunError
	if err != nil && errors.As(err, &dryRunErr) {
		return dryRunErr.code, true
	}
	return 0, false
}

func gatewayModeUsesDataPlane(mode string) bool {
	switch state.EffectiveGatewayMode(mode) {
	case state.GatewayModeOn, state.GatewayModeGlobal:
		return true
	default:
		return false
	}
}

func runtimeActionForGatewayMode(mode string) string {
	switch state.EffectiveGatewayMode(mode) {
	case state.GatewayModeSafe:
		return "cleanup_safe_direct"
	case state.GatewayModeOff:
		return "cleanup_off"
	case state.GatewayModeGlobal:
		return "apply_global_proxy"
	default:
		return "apply_transparent_proxy"
	}
}

func openWrtDryRunPlanForGatewayMode(opID string, owners *netgate.OwnerRegistry, workerCleanupTargets []netgate.WorkerCleanupTarget, mode string) (netgate.DryRunPlan, error) {
	if gatewayModeUsesDataPlane(mode) {
		return netgate.OpenWrtDryRunPlan(opID, owners)
	}
	return netgate.OpenWrtRescueCleanupDryRunPlan(opID, owners, workerCleanupTargets)
}

func (api configAPI) resourceGuardForRecord(spec state.SpecState, record configDryRunRecord) (configResourceGuardView, error) {
	count := 0
	total := 0
	if record.WorkerPrepared != nil {
		for workerID, candidate := range record.WorkerPrepared.Candidates {
			count++
			if candidate.Size > worker.DefaultMaxCandidateBytes {
				return configResourceGuardView{}, fmt.Errorf("worker %s candidate config too large: %d > %d", workerID, candidate.Size, worker.DefaultMaxCandidateBytes)
			}
			total += candidate.Size
		}
	}
	if count == 0 {
		count = 1
	}
	view := configResourceGuardView{
		Enabled: spec.Settings.OverlayGuardEnabled, CandidateBytes: total,
		SoftLimitBytes: configSoftLimitBytes, HardLimitBytes: worker.DefaultMaxCandidateBytes * count,
	}
	if total > view.HardLimitBytes {
		return view, fmt.Errorf("aggregate worker candidates too large: %d > %d", total, view.HardLimitBytes)
	}
	if total > configSoftLimitBytes {
		view.Warnings = append(view.Warnings, fmt.Sprintf("Worker 候选配置总大小超过建议值：%d > %d 字节", total, configSoftLimitBytes))
	}
	if !spec.Settings.OverlayGuardEnabled {
		return view, nil
	}
	disk, err := configDiskUsage(existingParent(api.layout.DataDir))
	if err != nil {
		return view, fmt.Errorf("overlay guard failed: %w", err)
	}
	view.OverlayPath = disk.Path
	view.FreeBytes = disk.FreeBytes
	if disk.FreeBytes < overlayRejectFreeBytes {
		return view, fmt.Errorf("overlay free space too low: %d < %d", disk.FreeBytes, overlayRejectFreeBytes)
	}
	if uint64(total) > disk.FreeBytes {
		return view, fmt.Errorf("worker candidates exceed overlay free space: %d > %d", total, disk.FreeBytes)
	}
	if disk.FreeBytes < overlayWarnFreeBytes {
		view.Warnings = append(view.Warnings, fmt.Sprintf("Overlay 可用空间偏低：%d < %d 字节", disk.FreeBytes, overlayWarnFreeBytes))
	}
	return view, nil
}

func existingParent(path string) string {
	if path == "" {
		return "."
	}
	clean := filepath.Clean(path)
	for {
		if _, err := os.Stat(clean); err == nil {
			return clean
		}
		parent := filepath.Dir(clean)
		if parent == clean {
			return clean
		}
		clean = parent
	}
}

func (api configAPI) discard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	runtimeServiceMu.Lock()
	defer runtimeServiceMu.Unlock()

	if err := api.removeDryRunLocked("config-discard"); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]string{"status": "discarded latest dry-run"})
}

func (api configAPI) gatewayMode(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		spec, _, err := api.loadSpecRuntime("gateway-mode-read")
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAPIData(w, http.StatusOK, map[string]string{"mode": state.EffectiveGatewayMode(spec.Settings.GatewayMode)})
	case http.MethodPost:
		runtimeServiceMu.Lock()
		defer runtimeServiceMu.Unlock()

		var payload gatewayModePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		if !state.ValidGatewayMode(payload.Mode) {
			writeAPIError(w, http.StatusBadRequest, "mode must be on, safe, off, or global")
			return
		}
		spec, runtimeState, err := api.updateGatewayMode(payload.Mode)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		view, err := api.configStateFrom(spec, runtimeState)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAPIData(w, http.StatusOK, view)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api configAPI) overview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	spec, runtimeState, err := api.loadSpecRuntime("overview")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	configState, err := api.configStateFrom(spec, runtimeState)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pendingSnapshots, pendingSnapshotsErr := provider.LoadLastGoodSnapshotsBounded(r.Context(), provider.NewLastGoodStore(api.layout))
	if pendingSnapshotsErr != nil {
		writeAPIError(w, http.StatusInternalServerError, pendingSnapshotsErr.Error())
		return
	}
	pendingNodes, pendingNodesErr := provider.BuildNodeViewsFromSnapshots(spec, pendingSnapshots)
	if pendingNodesErr != nil {
		writeAPIError(w, http.StatusBadRequest, pendingNodesErr.Error())
		return
	}
	pendingProvidersTotal, pendingProvidersEnabled, pendingNodesTotal, pendingNodesUsable, pendingPoolsTotal := overviewTopologyCounts(spec, pendingNodes)

	runningSpec := state.SpecState{}
	var runningNodes []provider.NodeView
	topology, topologyErr := api.store.LoadAppliedTopology()
	if topologyErr == nil {
		runningSpec = topology.Spec
		runningNodes, topologyErr = provider.BuildNodeViewsFromSnapshots(topology.Spec, topology.LastGood)
	}
	if topologyErr != nil {
		applied, appliedErr := api.store.LoadAppliedState()
		if appliedErr != nil {
			writeAPIError(w, http.StatusInternalServerError, appliedErr.Error())
			return
		}
		if applied.SchemaVersion != 0 && applied.SpecCaptured {
			writeAPIError(w, http.StatusInternalServerError, topologyErr.Error())
			return
		}
		runningSpec = state.SpecState{}
		runningNodes = nil
	}
	runningProvidersTotal, runningProvidersEnabled, runningNodesTotal, runningNodesUsable, runningPoolsTotal := overviewTopologyCounts(runningSpec, runningNodes)
	runtimeStatus := api.statusProvider()
	writeAPIData(w, http.StatusOK, map[string]any{
		"runtime":  runtimeStatus,
		"workers":  runtimeStatus.Workers,
		"bindings": runtimeStatus.Bindings,
		"gateway": map[string]any{
			"mode":         configState.RunningGatewayMode,
			"saved_mode":   configState.SavedGatewayMode,
			"applied_mode": configState.AppliedGatewayMode,
			"running_mode": configState.RunningGatewayMode,
			"dirty":        configState.Dirty,
		},
		"config": configState,
		"providers": map[string]int{
			"total":   runningProvidersTotal,
			"enabled": runningProvidersEnabled,
		},
		"nodes": map[string]int{
			"total":  runningNodesTotal,
			"usable": runningNodesUsable,
		},
		"business_pools": map[string]int{
			"total": runningPoolsTotal,
		},
		"pending": map[string]any{
			"dirty": configState.Dirty,
			"providers": map[string]int{
				"total": pendingProvidersTotal, "enabled": pendingProvidersEnabled,
			},
			"nodes": map[string]int{
				"total": pendingNodesTotal, "usable": pendingNodesUsable,
			},
			"business_pools": map[string]int{
				"total": pendingPoolsTotal,
			},
		},
		"snapshots": map[string]int{
			"pending": countFiles(api.layout.SnapshotsDir(), "pending-*.json"),
		},
	})
}

func overviewTopologyCounts(spec state.SpecState, nodes []provider.NodeView) (providersTotal, providersEnabled, nodesTotal, nodesUsable, poolsTotal int) {
	providersTotal = len(spec.Providers)
	for _, item := range spec.Providers {
		if item.Enabled {
			providersEnabled++
		}
	}
	nodesTotal = len(nodes)
	for _, item := range nodes {
		if !item.Excluded {
			nodesUsable++
		}
	}
	poolsTotal = len(spec.BusinessPools)
	return
}

func (api configAPI) currentConfigState() (configStateView, error) {
	spec, runtimeState, err := api.loadSpecRuntime("config-state")
	if err != nil {
		return configStateView{}, err
	}
	return api.configStateFrom(spec, runtimeState)
}

func (api configAPI) updateGatewayMode(mode string) (state.SpecState, state.RuntimeState, error) {
	release, err := api.lockStoreRuntime("gateway-mode-write")
	if err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	defer release()
	spec, err := api.store.LoadSpec()
	if err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	spec.Settings.GatewayMode = mode
	if _, err := api.store.SaveSpec(spec); err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	return spec, runtimeState, nil
}

func (api configAPI) rescueActive() bool {
	_, err := os.Stat(api.layout.RescueStatusPath())
	return err == nil
}

func (api configAPI) configStateFrom(spec state.SpecState, runtimeState state.RuntimeState) (configStateView, error) {
	specHash := provider.HashSpec(spec)
	topologyHash, err := provider.HashEffectiveTopology(context.Background(), spec, provider.NewLastGoodStore(api.layout))
	if err != nil {
		return configStateView{}, err
	}
	appliedState, err := api.loadAppliedState(spec.Settings.GatewayMode)
	if err != nil {
		return configStateView{}, err
	}
	savedMode := state.EffectiveGatewayMode(spec.Settings.GatewayMode)
	appliedMode := ""
	if appliedState.SchemaVersion != 0 && appliedState.SpecCaptured {
		appliedMode = state.EffectiveGatewayMode(appliedState.GatewayMode)
	}
	runningMode := ""
	observation, err := api.store.LoadDataPlaneObservation()
	if err != nil {
		return configStateView{}, err
	}
	runningMode = observation.RunningGatewayMode
	return configStateView{
		Dirty:              topologyHash != appliedState.TopologyHash,
		SpecHash:           specHash,
		TopologyHash:       appliedState.TopologyHash,
		ConfigEpoch:        appliedState.ConfigEpoch,
		GatewayMode:        savedMode,
		SavedGatewayMode:   savedMode,
		AppliedGatewayMode: appliedMode,
		RunningGatewayMode: runningMode,
	}, nil
}

func (api configAPI) loadAppliedState(gatewayMode string) (state.AppliedState, error) {
	if _, err := os.Stat(api.layout.AppliedStatePath()); errors.Is(err, os.ErrNotExist) {
		return state.NormalizeAppliedState(state.AppliedState{GatewayMode: gatewayMode}), nil
	} else if err != nil {
		return state.AppliedState{}, err
	}
	appliedState, err := api.store.LoadAppliedState()
	if err != nil {
		return state.AppliedState{}, err
	}
	if appliedState.SchemaVersion == 0 {
		return state.NormalizeAppliedState(state.AppliedState{GatewayMode: gatewayMode}), nil
	}
	return appliedState, nil
}

func (api configAPI) loadSpecRuntime(name string) (state.SpecState, state.RuntimeState, error) {
	if api.locks != nil {
		session := api.locks.Session(name)
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return state.SpecState{}, state.RuntimeState{}, err
		}
		releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
		if err != nil {
			releaseStore()
			return state.SpecState{}, state.RuntimeState{}, err
		}
		defer releaseStore()
		defer releaseRuntime()
	}
	spec, err := api.store.LoadSpec()
	if err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	// Dry-run/apply/state views must use durable SelectionState when RuntimeDir
	// was recreated or lags after reboot; otherwise candidates and conflict
	// checks journal stale or empty ProxySelection.
	runtimeState, err = state.RuntimeWithDurableSelection(api.store, runtimeState)
	if err != nil {
		return state.SpecState{}, state.RuntimeState{}, err
	}
	return spec, runtimeState, nil
}

func (api configAPI) saveRuntimeState(name string, runtimeState state.RuntimeState) error {
	release, err := api.lockStoreRuntime(name)
	if err != nil {
		return err
	}
	defer release()
	return api.store.SaveRuntime(runtimeState)
}

func (api configAPI) dryRunPath() string {
	return api.layout.RuntimePath("config-dryrun")
}

func (api configAPI) saveDryRun(record configDryRunRecord) error {
	release, err := api.lockRuntime("config-dryrun-save")
	if err != nil {
		return err
	}
	defer release()
	previous, previousErr := api.loadDryRun()
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return previousErr
	}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > persistedDryRunMaxBytes {
		return fmt.Errorf("persisted config dry-run exceeds %d bytes", persistedDryRunMaxBytes)
	}
	tmp := api.dryRunPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, api.dryRunPath()); err != nil {
		return err
	}
	if previousErr == nil && previous.candidatePath() != record.candidatePath() {
		if err := api.cleanupDryRun(previous); err != nil {
			_ = os.Remove(api.dryRunPath())
			return err
		}
	}
	return nil
}

func (api configAPI) loadDryRun() (configDryRunRecord, error) {
	var record configDryRunRecord
	err := readJSONFileBounded(api.dryRunPath(), &record, persistedDryRunMaxBytes)
	return record, err
}

func readJSONFileBounded(path string, target any, maxBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > maxBytes {
		return fmt.Errorf("json file %s exceeds %d bytes", filepath.Base(path), maxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if int64(len(body)) > maxBytes {
		return fmt.Errorf("json file %s exceeds %d bytes", filepath.Base(path), maxBytes)
	}
	return json.Unmarshal(body, target)
}

func (api configAPI) loadDryRunLocked(name string) (configDryRunRecord, error) {
	release, err := api.lockRuntime(name)
	if err != nil {
		return configDryRunRecord{}, err
	}
	defer release()
	return api.loadDryRun()
}

func (api configAPI) removeDryRunLocked(name string) error {
	release, err := api.lockRuntime(name)
	if err != nil {
		return err
	}
	defer release()
	record, err := api.loadDryRun()
	if err != nil {
		return err
	}
	if err := api.cleanupDryRun(record); err != nil {
		return err
	}
	return os.Remove(api.dryRunPath())
}

func (api configAPI) removeDryRunIfIDLocked(name, expectedID string) error {
	release, err := api.lockRuntime(name)
	if err != nil {
		return err
	}
	defer release()
	record, err := api.loadDryRun()
	if err != nil {
		return err
	}
	if record.ID != expectedID {
		return nil
	}
	if err := api.cleanupDryRun(record); err != nil {
		return err
	}
	return os.Remove(api.dryRunPath())
}

func (api configAPI) cleanupDryRun(record configDryRunRecord) error {
	if record.WorkerPrepared != nil && api.configTxn != nil {
		api.configTxn.CleanupPrepared(configapply.Prepared{Workers: record.WorkerPrepared})
	}
	return nil
}

func (api configAPI) lockStoreRuntime(name string) (func(), error) {
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

func (api configAPI) lockRuntime(name string) (func(), error) {
	if api.locks == nil {
		return func() {}, nil
	}
	session := api.locks.Session(name)
	return session.Acquire(lockset.LevelRuntime)
}

func (api configAPI) lockNet(name string) (func(), error) {
	if api.locks == nil {
		return func() {}, nil
	}
	session := api.locks.Session(name)
	releaseRescue, err := session.Acquire(lockset.LevelRescueGate)
	if err != nil {
		return nil, err
	}
	releaseNet, err := session.Acquire(lockset.LevelNet)
	if err != nil {
		releaseRescue()
		return nil, err
	}
	return func() {
		releaseNet()
		releaseRescue()
	}, nil
}

func countFiles(dir, pattern string) int {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return 0
	}
	return len(matches)
}
