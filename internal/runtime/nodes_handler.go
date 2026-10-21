package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/failover"
	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/redact"
	"smartgate/internal/scheduler"
	"smartgate/internal/selection"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

const defaultNodeDelayURL = worker.DefaultRouteProbeURL
const failoverConfirmURL = worker.DefaultRouteConfirmURL
const maxProxyGroupCandidates = 10
const failoverHealthTTL = 3 * time.Minute
const failoverFailureThreshold = 2

var failoverProbeBudget = 20 * time.Second

type nodesAPI struct {
	store        *state.Store
	layout       state.Layout
	locks        *lockset.LockSet
	proxyControl selection.ProxyController
	workers      *worker.Manager
	selectorTxn  *commitlane.SelectorTxn
	smartNodeTxn *commitlane.SmartNodeTxn
	delayRunner  *scheduler.Runner
}

type smartNodeView struct {
	ID                string                     `json:"id"`
	Revision          int                        `json:"revision"`
	DisplayName       string                     `json:"display_name"`
	Identity          smartNodeIdentityView      `json:"identity"`
	Aliases           []smartnode.NodeAlias      `json:"aliases"`
	Capability        smartnode.CapabilityMatrix `json:"capability"`
	CoreChoice        *smartnode.CoreID          `json:"core_choice"`
	ReferencedByPools []string                   `json:"referenced_by_pools"`
	SourceMissing     bool                       `json:"source_missing,omitempty"`
	Health            *state.NodeHealth          `json:"health,omitempty"`
}

type smartNodeIdentityView struct {
	Protocol        string                `json:"protocol"`
	Server          string                `json:"server"`
	Port            int                   `json:"port"`
	Transport       smartnode.Transport   `json:"transport"`
	Security        smartNodeSecurityView `json:"security"`
	SecretRefMasked string                `json:"secret_ref_masked"`
}

type smartNodeSecurityView struct {
	Type              string   `json:"type"`
	SNI               string   `json:"sni,omitempty"`
	ALPN              []string `json:"alpn,omitempty"`
	Fingerprint       string   `json:"fingerprint,omitempty"`
	Flow              string   `json:"flow,omitempty"`
	PublicKeyMasked   string   `json:"public_key_masked,omitempty"`
	ShortID           string   `json:"short_id,omitempty"`
	SkipCertVerify    bool     `json:"skip_cert_verify,omitempty"`
	CertificateMasked string   `json:"certificate_masked,omitempty"`
}

type proxyGroupView struct {
	Pool            string                    `json:"pool"`
	Binding         state.PoolBinding         `json:"binding"`
	CandidateViews  []proxyGroupCandidateView `json:"candidates"`
	Lock            *state.FailoverLock       `json:"lock"`
	CooldownUntil   *time.Time                `json:"cooldown_until"`
	FailoverEnabled bool                      `json:"failover_enabled"`

	ID         string           `json:"-"`
	Name       string           `json:"-"`
	Role       string           `json:"-"`
	Selector   string           `json:"-"`
	Candidates []string         `json:"-"`
	Selected   string           `json:"-"`
	Policy     state.PoolPolicy `json:"-"`
}

type proxyGroupCandidateView struct {
	SmartNode string           `json:"smart_node"`
	Core      smartnode.CoreID `json:"core,omitempty"`
	Health    string           `json:"health"`
	DelayMS   int              `json:"delay_ms,omitempty"`
}

type proxySelectPayload struct {
	TargetSmartNode     string                   `json:"target_smart_node"`
	Source              commitlane.CutoverSource `json:"source"`
	ExpectedConfigEpoch int64                    `json:"expected_config_epoch"`
	OpID                string                   `json:"op_id"`
}

type nodeDelayPayload struct {
	URL       string `json:"url,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type coreChoicePayload struct {
	Core            smartnode.CoreID `json:"core"`
	ConfirmDegraded bool             `json:"confirm_degraded,omitempty"`
	OpID            string           `json:"op_id"`
}

type failoverEnabledPayload struct {
	Enabled *bool `json:"enabled"`
}

type failoverLockPayload struct {
	PoolID string `json:"pool_id"`
	Reason string `json:"reason,omitempty"`
}

type failoverUnlockPayload struct {
	PoolID string `json:"pool_id"`
}

type failoverRevertPayload struct {
	PoolID string `json:"pool_id"`
}

type failoverRunResult struct {
	Decisions []failover.Decision   `json:"decisions"`
	Applied   []failover.Decision   `json:"applied"`
	Events    []state.FailoverEvent `json:"events,omitempty"`
	Failures  []failoverFailure     `json:"failures,omitempty"`
}

type failoverFailure struct {
	PoolID string `json:"pool_id"`
	Stage  string `json:"stage"`
	Error  string `json:"error"`
}

func sanitizeFailoverRunResult(result failoverRunResult) failoverRunResult {
	result.Failures = append([]failoverFailure(nil), result.Failures...)
	for index := range result.Failures {
		result.Failures[index].Error = sanitizeAPIMessage(result.Failures[index].Error)
	}
	result.Events = sanitizeFailoverEvents(result.Events)
	return result
}

func sanitizeFailoverEvents(events []state.FailoverEvent) []state.FailoverEvent {
	safe := append([]state.FailoverEvent(nil), events...)
	for index := range safe {
		safe[index].Error = sanitizeAPIMessage(safe[index].Error)
	}
	return safe
}

func sanitizeNodeHealth(health state.NodeHealth) state.NodeHealth {
	health.Error = sanitizeAPIMessage(health.Error)
	return health
}

type failoverApply struct {
	group    proxyGroupView
	decision failover.Decision
}

type schedulerFailoverRunner struct {
	api nodesAPI
}

type schedulerNodeDelayTester struct {
	api nodesAPI
}

var failoverSelectMu sync.Mutex

func registerNodeHandlers(mux *http.ServeMux, store *state.Store, layout state.Layout, locks *lockset.LockSet, proxyControl selection.ProxyController, workers *worker.Manager, selectorTxn *commitlane.SelectorTxn, smartNodeTxn *commitlane.SmartNodeTxn, delayRunner *scheduler.Runner) {
	api := nodesAPI{store: store, layout: layout, locks: locks, proxyControl: proxyControl, workers: workers, smartNodeTxn: smartNodeTxn, delayRunner: delayRunner}
	if selectorTxn != nil {
		api.selectorTxn = selectorTxn
	} else if store != nil && locks != nil && proxyControl != nil {
		api.selectorTxn = commitlane.NewSelectorTxn(commitlane.New(locks, layout, netgate.NewOwnerRegistry("smartgate")), store, proxyControl)
	}
	mux.HandleFunc("/api/nodes", api.nodes)
	mux.HandleFunc("/api/nodes/delay-all", api.delayAll)
	mux.HandleFunc("/api/nodes/", api.nodeAction)
	mux.HandleFunc("/api/proxy-groups", api.proxyGroups)
	mux.HandleFunc("/api/proxy-groups/", api.proxyGroupAction)
}

func (api nodesAPI) delayAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if api.delayRunner == nil {
		writeAPIError(w, http.StatusNotImplemented, "delay tester is not configured")
		return
	}
	result, err := api.delayRunner.RunNow(r.Context(), scheduler.TaskDelayTest)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, scheduler.ErrTaskRunning):
			status = http.StatusConflict
		case errors.Is(err, scheduler.ErrGatewayOff):
			status = http.StatusLocked
		case errors.Is(err, scheduler.ErrTaskNotRunnable):
			status = http.StatusNotImplemented
		}
		writeAPIError(w, status, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, result)
}

func NewSchedulerFailoverRunner(cfg Config) schedulerFailoverRunner {
	selectorTxn := cfg.SelectorTxn
	if selectorTxn == nil && cfg.Store != nil && cfg.Locks != nil && cfg.ProxyControl != nil {
		selectorTxn = commitlane.NewSelectorTxn(commitlane.New(cfg.Locks, cfg.Layout, netgate.NewOwnerRegistry("smartgate")), cfg.Store, cfg.ProxyControl)
	}
	return schedulerFailoverRunner{api: nodesAPI{
		store:        cfg.Store,
		layout:       cfg.Layout,
		locks:        cfg.Locks,
		proxyControl: cfg.ProxyControl,
		workers:      cfg.Workers,
		selectorTxn:  selectorTxn,
	}}
}

func NewSchedulerNodeDelayTester(cfg Config) scheduler.DelayTester {
	return schedulerNodeDelayTester{api: nodesAPI{
		store:       cfg.Store,
		layout:      cfg.Layout,
		locks:       cfg.Locks,
		workers:     cfg.Workers,
		selectorTxn: cfg.SelectorTxn,
	}}
}

func (t schedulerNodeDelayTester) Delay(ctx context.Context, nodeID, testURL string, timeout time.Duration) (scheduler.DelayResult, error) {
	topology, _, err := t.api.loadAppliedTopologyRuntime("scheduler-node-delay-read")
	if err != nil {
		return scheduler.DelayResult{}, err
	}
	if _, err := t.api.appliedNodeBindingPath(topology, nodeID); err != nil {
		return scheduler.DelayResult{URL: testURL, Context: nodeHealthContext(topology, nodeID)}, fmt.Errorf("%w: %v", scheduler.ErrDelayUnavailable, err)
	}
	result, err := t.api.delayAppliedNode(ctx, topology, nodeID, testURL, timeout)
	return scheduler.DelayResult{DelayMS: result.DelayMS, URL: result.URL, Context: result.Context}, err
}

type schedulerBatchDelayTester struct {
	batch       *worker.NodeProbeBatch
	contexts    map[string]state.NodeHealthContext
	fallback    schedulerNodeDelayTester
	unavailable map[string]string
}

func (t schedulerNodeDelayTester) Begin(ctx context.Context, topology state.AppliedTopology, nodes []provider.NodeView) (scheduler.DelayTester, func(), error) {
	if t.api.workers == nil {
		return nil, nil, fmt.Errorf("worker manager is not configured")
	}
	targets := make([]worker.NodeProbeTarget, 0, len(nodes))
	contexts := map[string]state.NodeHealthContext{}
	unavailable := map[string]string{}
	for _, view := range nodes {
		if view.Excluded {
			continue
		}
		node, ok := topology.Spec.SmartNodes[view.ID]
		if !ok {
			unavailable[view.ID] = "node is absent from applied catalog"
			continue
		}
		contexts[node.ID] = state.NodeHealthContext{Revision: node.Revision, Core: node.CoreChoice, Method: "unknown"}
		if node.CoreChoice == "" {
			unavailable[node.ID] = "node has no selected execution core"
			continue
		}
		if capability, known := node.Capability.ForCore(node.CoreChoice); known && capability.Status == smartnode.CapabilityUnsupportedByCore {
			unavailable[node.ID] = "selected core cannot execute this node"
			continue
		}
		if !t.api.workers.SupportsNodeProbeBatch(node.CoreChoice) {
			// Only this adapter uses installed routes. Other cores retain their
			// isolated batch, including nodes outside all business Pools.
			if _, err := t.api.appliedNodeBindingPath(topology, node.ID); err != nil {
				unavailable[node.ID] = "selected core has no isolated probe or installed route"
			}
			continue
		}
		targets = append(targets, worker.NodeProbeTarget{Node: node, Source: smartnode.IdentitySource{RawTag: view.Tag, Identity: view.Identity, References: view.References}})
		contexts[node.ID] = state.NodeHealthContext{Revision: node.Revision, Core: node.CoreChoice, Method: "isolated_batch_http_204"}
	}
	var batch *worker.NodeProbeBatch
	closeBatch := func() {}
	if len(targets) > 0 {
		var err error
		batch, err = t.api.workers.StartNodeProbeBatch(ctx, targets)
		if err != nil {
			return nil, nil, err
		}
		closeBatch = batch.Close
	}
	return schedulerBatchDelayTester{batch: batch, contexts: contexts, fallback: t, unavailable: unavailable}, closeBatch, nil
}

func (t schedulerBatchDelayTester) Delay(ctx context.Context, nodeID, testURL string, timeout time.Duration) (scheduler.DelayResult, error) {
	if reason, unavailable := t.unavailable[nodeID]; unavailable {
		return scheduler.DelayResult{URL: testURL, Context: t.contexts[nodeID]}, fmt.Errorf("%w: %s", scheduler.ErrDelayUnavailable, reason)
	}
	if t.contexts[nodeID].Method != "isolated_batch_http_204" {
		return t.fallback.Delay(ctx, nodeID, testURL, timeout)
	}
	result, err := t.batch.DelayLatency(ctx, nodeID, testURL, timeout)
	if errors.Is(err, worker.ErrNodeProbeUnavailable) {
		err = fmt.Errorf("%w: %v", scheduler.ErrDelayUnavailable, err)
	}
	return scheduler.DelayResult{DelayMS: result.DelayMS, URL: result.URL, Context: t.contexts[nodeID]}, err
}

func (r schedulerFailoverRunner) Run(ctx context.Context) (scheduler.FailoverResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, failoverProbeBudget)
	eligiblePools, probeErr := r.refreshWatchdogHealth(probeCtx)
	cancel()
	if eligiblePools == nil && probeErr != nil {
		return scheduler.FailoverResult{}, probeErr
	}
	result, err := r.api.executeFailover(ctx, "watchdog", eligiblePools)
	schedulerResult := scheduler.FailoverResult{
		Decisions: len(result.Decisions),
		Applied:   len(result.Applied),
		Events:    len(result.Events),
		Failed:    len(result.Failures),
	}
	if probeErr != nil {
		schedulerResult.Failed++
		schedulerResult.Error = "health probe degraded: " + probeErr.Error()
	}
	if err != nil && len(result.Failures) > 0 {
		if schedulerResult.Error != "" {
			schedulerResult.Error += "; "
		}
		schedulerResult.Error += err.Error()
		return schedulerResult, nil
	}
	if err != nil {
		return schedulerResult, err
	}
	if probeErr != nil {
		return schedulerResult, nil
	}
	return schedulerResult, err
}

func (r schedulerFailoverRunner) refreshWatchdogHealth(ctx context.Context) (map[string]bool, error) {
	failoverState, err := state.NewFailoverStateStore(r.api.layout).Load()
	if err != nil {
		return nil, err
	}
	if !failoverState.Enabled {
		return map[string]bool{}, nil
	}
	if r.api.workers == nil {
		return nil, fmt.Errorf("worker manager is not configured")
	}
	topology, runtimeState, err := r.api.loadAppliedTopologyRuntime("failover-watchdog-applied-read")
	if err != nil {
		return nil, err
	}
	groups, err := buildProxyGroupViews(topology, runtimeState, failoverState, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	probe := map[string]struct{}{}
	for _, group := range groups {
		if _, locked := failoverState.Locks[group.ID]; locked {
			continue
		}
		poolState := failoverState.Pools[group.ID]
		if poolState.CooldownUntil != nil && poolState.CooldownUntil.After(time.Now().UTC()) {
			continue
		}
		if isProbeableNode(group.Selected) {
			probe[group.Selected] = struct{}{}
			continue
		}
		for _, candidate := range group.Candidates {
			if isProbeableNode(candidate) {
				probe[candidate] = struct{}{}
			}
		}
	}
	if len(probe) == 0 {
		return watchdogEligiblePools(groups, nil), nil
	}
	health := map[string]state.NodeHealth{}
	failedCurrent := map[string]struct{}{}
	currentHealth, probeErr := r.probeBatch(ctx, topology, probe, runtimeState.NodeHealth)
	for nodeID, item := range currentHealth {
		health[nodeID] = item
		if item.Status != "ok" {
			failedCurrent[nodeID] = struct{}{}
		}
	}
	if probeErr != nil {
		if len(health) > 0 {
			if err := r.api.mergeNodeHealth("failover-watchdog-health-partial", health); err != nil {
				return nil, fmt.Errorf("persist partial watchdog health: %w (probe error: %v)", err, probeErr)
			}
		}
		return watchdogEligiblePools(groups, health), probeErr
	}
	candidates := map[string]struct{}{}
	for _, group := range groups {
		if _, failed := failedCurrent[group.Selected]; !failed && isProbeableNode(group.Selected) {
			continue
		}
		for _, candidate := range group.Candidates {
			if !isProbeableNode(candidate) {
				continue
			}
			if _, ok := health[candidate]; ok {
				continue
			}
			candidates[candidate] = struct{}{}
		}
	}
	candidateHealth, probeErr := r.probeBatch(ctx, topology, candidates, runtimeState.NodeHealth)
	for nodeID, item := range candidateHealth {
		health[nodeID] = item
	}
	if probeErr != nil {
		if len(health) > 0 {
			if err := r.api.mergeNodeHealth("failover-watchdog-health-partial", health); err != nil {
				return nil, fmt.Errorf("persist partial watchdog health: %w (probe error: %v)", err, probeErr)
			}
		}
		return watchdogEligiblePools(groups, health), probeErr
	}
	if err := r.api.mergeNodeHealth("failover-watchdog-health", health); err != nil {
		return nil, err
	}
	return watchdogEligiblePools(groups, health), nil
}

func watchdogEligiblePools(groups []proxyGroupView, probed map[string]state.NodeHealth) map[string]bool {
	eligible := make(map[string]bool, len(groups))
	for _, group := range groups {
		if probeableCandidateCount(group.Candidates) == 0 {
			eligible[group.ID] = true
			continue
		}
		current, currentProbed := probed[group.Selected]
		if isProbeableNode(group.Selected) && (!currentProbed || current.Status == "") {
			continue
		}
		if isProbeableNode(group.Selected) && current.Status == "ok" {
			eligible[group.ID] = true
			continue
		}
		complete := true
		for _, candidate := range group.Candidates {
			if !isProbeableNode(candidate) {
				continue
			}
			if item, ok := probed[candidate]; !ok || item.Status == "" {
				complete = false
				break
			}
		}
		eligible[group.ID] = complete
	}
	return eligible
}

type failoverProbeOutcome struct {
	nodeID string
	health state.NodeHealth
	err    error
}

func (r schedulerFailoverRunner) probeBatch(ctx context.Context, topology state.AppliedTopology, nodes map[string]struct{}, previous map[string]state.NodeHealth) (map[string]state.NodeHealth, error) {
	results := make(map[string]state.NodeHealth, len(nodes))
	if len(nodes) == 0 {
		return results, nil
	}
	jobs := make(chan string)
	outcomes := make(chan failoverProbeOutcome, len(nodes))
	workers := 4
	if len(nodes) < workers {
		workers = len(nodes)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for nodeID := range jobs {
				item, err := r.probeNode(ctx, topology, nodeID, previous[nodeID])
				outcomes <- failoverProbeOutcome{nodeID: nodeID, health: item, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for nodeID := range nodes {
			select {
			case <-ctx.Done():
				return
			case jobs <- nodeID:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(outcomes)
	}()
	var firstErr error
	for outcome := range outcomes {
		if outcome.err != nil {
			if firstErr == nil {
				firstErr = outcome.err
			}
			continue
		}
		results[outcome.nodeID] = outcome.health
	}
	if firstErr != nil {
		return results, firstErr
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

func (r schedulerFailoverRunner) probeNode(ctx context.Context, topology state.AppliedTopology, nodeID string, previous state.NodeHealth) (state.NodeHealth, error) {
	if err := ctx.Err(); err != nil {
		return state.NodeHealth{}, err
	}
	now := time.Now().UTC()
	item := state.NodeHealth{LastTest: &now, UpdatedAt: &now, TestURL: defaultNodeDelayURL}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	result, err := r.api.delayAppliedNode(probeCtx, topology, nodeID, defaultNodeDelayURL, 5*time.Second)
	cancel()
	if ctx.Err() != nil {
		return state.NodeHealth{}, ctx.Err()
	}
	if err != nil {
		confirmCtx, cancelConfirm := context.WithTimeout(ctx, 5*time.Second)
		confirmed, confirmErr := r.api.delayAppliedNode(confirmCtx, topology, nodeID, failoverConfirmURL, 5*time.Second)
		cancelConfirm()
		if ctx.Err() != nil {
			return state.NodeHealth{}, ctx.Err()
		}
		if confirmErr == nil {
			result, err = confirmed, nil
		} else {
			err = fmt.Errorf("primary probe: %v; alternate probe: %w", err, confirmErr)
		}
	}
	item.Context = result.Context
	item.WatchdogContext = result.Context
	item.WatchdogUpdatedAt = &now
	if err != nil {
		item.Status = "failed"
		item.WatchdogStatus = "failed"
		item.Error = err.Error()
		previousStatus, previousAt, previousContext := previous.WatchdogStatus, previous.WatchdogUpdatedAt, previous.WatchdogContext
		if previousStatus == "" {
			previousStatus, previousAt, previousContext = previous.Status, previous.UpdatedAt, previous.Context
		}
		if previousStatus == "failed" && previousAt != nil && now.Sub(*previousAt) <= failoverHealthTTL && nodeHealthMatches(state.NodeHealth{Context: previousContext}, item.Context.Revision, item.Context.Core) {
			item.ConsecutiveFailures = previous.ConsecutiveFailures + 1
		} else {
			item.ConsecutiveFailures = 1
		}
		return item, nil
	}
	item.Status = "ok"
	item.WatchdogStatus = "ok"
	item.ConsecutiveFailures = 0
	item.DelayMS = result.DelayMS
	if result.URL != "" {
		item.TestURL = result.URL
	}
	return item, nil
}

func isProbeableNode(nodeID string) bool {
	return nodeID != "" && nodeID != "Block" && nodeID != "Direct"
}

func (api nodesAPI) nodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	spec, runtimeState, err := api.loadSpecRuntime("nodes-read")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(r.Context(), provider.NewLastGoodStore(api.layout))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	catalog, _, err := provider.BuildSmartNodeCatalogFromSnapshots(spec, snapshots, time.Now().UTC())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	activeIDs := activeSmartNodeIDs(spec, snapshots)
	poolRefs := smartNodePoolReferences(spec.BusinessPools)
	ids, err := provider.SmartNodeIDsInProviderOrder(spec, snapshots, catalog)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	views := make([]smartNodeView, 0, len(ids))
	for _, id := range ids {
		view := smartNodePublicView(catalog[id], poolRefs[id], runtimeState.NodeHealth[id], activeIDs[id])
		if matchesSmartNodeFilters(view, r) {
			views = append(views, view)
		}
	}
	writeAPIData(w, http.StatusOK, map[string]any{"nodes": views})
}

func (api nodesAPI) proxyGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	topology, runtimeState, err := api.loadAppliedTopologyRuntime("proxy-groups-read")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	failoverState, err := state.NewFailoverStateStore(api.layout).Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups, err := buildProxyGroupViews(topology, runtimeState, failoverState, time.Now().UTC())
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, "runtime_graph_invalid", "live ExecutionGraph cannot be rebuilt", nil)
		return
	}
	writeAPIData(w, http.StatusOK, map[string]any{"groups": groups})
}

func (api nodesAPI) proxyGroupAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/proxy-groups/")
	if path == "failover/run" {
		api.runFailover(w, r)
		return
	}
	if path == "failover/events" {
		api.failoverEvents(w, r)
		return
	}
	if path == "failover/state" {
		api.failoverState(w, r)
		return
	}
	if path == "failover/enable" {
		api.setFailoverEnabled(w, r)
		return
	}
	if path == "failover/lock" {
		api.lockFailoverPool(w, r)
		return
	}
	if path == "failover/unlock" {
		api.unlockFailoverPool(w, r)
		return
	}
	if path == "failover/revert" {
		api.revertFailoverPool(w, r)
		return
	}
	id, action, ok := strings.Cut(path, "/")
	if !ok || action != "select" || id == "" {
		writeAPIError(w, http.StatusNotFound, "proxy group action not found")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload proxySelectPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(payload.TargetSmartNode) == "" || strings.TrimSpace(payload.Source.SmartNode) == "" || payload.Source.Generation == 0 || payload.ExpectedConfigEpoch <= 0 || strings.TrimSpace(payload.OpID) == "" {
		writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", "target_smart_node, source.smart_node, source.generation, expected_config_epoch, and op_id are required", nil)
		return
	}
	failoverSelectMu.Lock()
	defer failoverSelectMu.Unlock()
	releaseMutation, err := lockProxyMutation(api.locks, api.layout, "proxy-select-mutation")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mutationReleased := false
	defer func() {
		if !mutationReleased {
			releaseMutation()
		}
	}()
	topology, err := api.store.LoadAppliedTopology()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	failoverState, err := state.NewFailoverStateStore(api.layout).Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups, err := buildProxyGroupViews(topology, runtimeState, failoverState, time.Now().UTC())
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, "runtime_graph_invalid", "live ExecutionGraph cannot be rebuilt", nil)
		return
	}
	group, ok := findProxyGroup(groups, id)
	if !ok {
		writeAPIErrorCode(w, http.StatusNotFound, "proxy_group_not_found", "proxy group not found", map[string]any{"pool": id})
		return
	}
	if !containsString(group.Candidates, payload.TargetSmartNode) {
		releaseMutation()
		mutationReleased = true
		writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", "target_smart_node is not a candidate of the Pool", map[string]any{"pool": id, "target": payload.TargetSmartNode})
		return
	}
	targetFailover, err := api.manualProxySelectionState(id, group, group.Selected, payload.TargetSmartNode)
	releaseMutation()
	mutationReleased = true
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if api.selectorTxn == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "selector transaction is not configured")
		return
	}
	result, err := api.selectorTxn.Cutover(r.Context(), commitlane.CutoverRequest{
		OpID: payload.OpID, PoolID: id, TargetSmartNode: payload.TargetSmartNode,
		Source: payload.Source, TargetPool: targetFailover.Pools[id], ExpectedConfigEpoch: payload.ExpectedConfigEpoch,
	})
	if err != nil {
		writeCutoverError(w, err)
		return
	}
	writeAPIData(w, http.StatusOK, result)
}

func (api nodesAPI) manualProxySelectionState(poolID string, group proxyGroupView, previous, selected string) (state.FailoverState, error) {
	store := state.NewFailoverStateStore(api.layout)
	failoverState, err := store.Load()
	if err != nil {
		return state.FailoverState{}, err
	}
	now := time.Now().UTC()
	poolState := failoverState.Pools[poolID]
	if previous == "" {
		previous = poolState.Current
	}
	if previous == selected {
		previous = poolState.Previous
	}
	failoverState.Pools[poolID] = state.FailoverPoolState{
		PoolID:        poolID,
		Role:          group.Role,
		Current:       selected,
		Previous:      previous,
		LastKind:      "manual",
		LastReason:    "manual_select",
		LastChangedAt: &now,
		Terminal:      selected == "Block",
	}
	return failoverState, nil
}

func (api nodesAPI) runFailover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	result, err := api.executeFailover(r.Context(), "manual")
	if err != nil {
		if len(result.Failures) > 0 {
			writeAPIData(w, http.StatusMultiStatus, map[string]any{"status": "degraded", "result": sanitizeFailoverRunResult(result), "error": sanitizeAPIMessage(err.Error())})
			return
		}
		if code, ok := runtimeHTTPErrorStatus(err); ok {
			writeAPIError(w, code, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, sanitizeFailoverRunResult(result))
}

func (api nodesAPI) failoverEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	events, err := state.NewFailoverEventStore(api.layout).List()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]any{"events": sanitizeFailoverEvents(events)})
}

func (api nodesAPI) failoverState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	failoverState, err := state.NewFailoverStateStore(api.layout).Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, failoverState)
}

func (api nodesAPI) setFailoverEnabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload failoverEnabledPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if payload.Enabled == nil {
		writeAPIError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	failoverSelectMu.Lock()
	defer failoverSelectMu.Unlock()
	releaseMutation, err := lockProxyMutation(api.locks, api.layout, "failover-enabled-mutation")
	if err != nil {
		writeAPIError(w, http.StatusLocked, err.Error())
		return
	}
	defer releaseMutation()
	store := state.NewFailoverStateStore(api.layout)
	failoverState, err := store.Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	failoverState.Enabled = *payload.Enabled
	if err := store.Save(failoverState); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, failoverState)
}

func (api nodesAPI) lockFailoverPool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload failoverLockPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(payload.PoolID) == "" {
		writeAPIError(w, http.StatusBadRequest, "pool_id is required")
		return
	}
	failoverSelectMu.Lock()
	defer failoverSelectMu.Unlock()
	releaseMutation, err := lockProxyMutation(api.locks, api.layout, "failover-lock-mutation")
	if err != nil {
		writeAPIError(w, http.StatusLocked, err.Error())
		return
	}
	defer releaseMutation()
	store := state.NewFailoverStateStore(api.layout)
	failoverState, err := store.Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	failoverState.Locks[payload.PoolID] = state.FailoverLock{Reason: strings.TrimSpace(payload.Reason), LockedAt: time.Now().UTC()}
	if err := store.Save(failoverState); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, failoverState)
}

func (api nodesAPI) unlockFailoverPool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload failoverUnlockPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(payload.PoolID) == "" {
		writeAPIError(w, http.StatusBadRequest, "pool_id is required")
		return
	}
	failoverSelectMu.Lock()
	defer failoverSelectMu.Unlock()
	releaseMutation, err := lockProxyMutation(api.locks, api.layout, "failover-unlock-mutation")
	if err != nil {
		writeAPIError(w, http.StatusLocked, err.Error())
		return
	}
	defer releaseMutation()
	store := state.NewFailoverStateStore(api.layout)
	failoverState, err := store.Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	delete(failoverState.Locks, payload.PoolID)
	if err := store.Save(failoverState); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, failoverState)
}

func (api nodesAPI) revertFailoverPool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload failoverRevertPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(payload.PoolID) == "" {
		writeAPIError(w, http.StatusBadRequest, "pool_id is required")
		return
	}
	failoverSelectMu.Lock()
	defer failoverSelectMu.Unlock()
	releaseMutation, err := lockProxyMutation(api.locks, api.layout, "proxy-failover-revert-mutation")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mutationReleased := false
	defer func() {
		if !mutationReleased {
			releaseMutation()
		}
	}()
	topology, err := api.store.LoadAppliedTopology()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	store := state.NewFailoverStateStore(api.layout)
	failoverState, err := store.Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups, err := buildProxyGroupViews(topology, runtimeState, failoverState, time.Now().UTC())
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, "runtime_graph_invalid", "live ExecutionGraph cannot be rebuilt", nil)
		return
	}
	group, ok := findProxyGroup(groups, payload.PoolID)
	if !ok {
		writeAPIErrorCode(w, http.StatusNotFound, "proxy_group_not_found", "proxy group not found", map[string]any{"pool": payload.PoolID})
		return
	}
	poolState := failoverState.Pools[payload.PoolID]
	if poolState.Previous == "" {
		writeAPIError(w, http.StatusConflict, "failover pool has no previous target")
		return
	}
	if !containsString(group.Candidates, poolState.Previous) {
		writeAPIError(w, http.StatusConflict, "previous target is not a candidate: "+poolState.Previous)
		return
	}
	now := time.Now().UTC()
	decision := failover.Decision{
		PoolID: payload.PoolID,
		Role:   group.Role,
		From:   group.Selected,
		To:     poolState.Previous,
		Kind:   failover.KindRecovery,
		Reason: "manual_revert",
		Time:   now,
	}
	targetPool := buildFailoverPoolState(decision, failoverState)
	releaseMutation()
	mutationReleased = true
	if api.selectorTxn == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "selector transaction is not configured")
		return
	}
	cutoverResult, err := api.selectorTxn.Cutover(r.Context(), commitlane.CutoverRequest{
		OpID: fmt.Sprintf("failover-revert-%s-%d", payload.PoolID, time.Now().UnixNano()), PoolID: payload.PoolID,
		TargetSmartNode: decision.To, Source: cutoverSourceForBinding(group.Binding), TargetPool: targetPool,
		ExpectedConfigEpoch: topology.ConfigEpoch,
	})
	if err != nil {
		writeCutoverError(w, err)
		return
	}
	events := failoverEventsFromCutovers([]failover.Decision{decision}, []commitlane.CutoverResult{cutoverResult}, "manual", "applied", "")
	if _, err := state.NewFailoverEventStore(api.layout).Append(events); err != nil {
		events = nil
	}
	writeAPIData(w, http.StatusOK, failoverRunResult{
		Decisions: []failover.Decision{decision},
		Applied:   []failover.Decision{decision},
		Events:    events,
	})
}

func (api nodesAPI) executeFailover(ctx context.Context, source string, eligibleSets ...map[string]bool) (failoverRunResult, error) {
	failoverSelectMu.Lock()
	defer failoverSelectMu.Unlock()
	releaseMutation, err := lockProxyMutation(api.locks, api.layout, "proxy-failover-mutation")
	if err != nil {
		return failoverRunResult{}, err
	}
	mutationReleased := false
	defer func() {
		if !mutationReleased {
			releaseMutation()
		}
	}()

	failoverStore := state.NewFailoverStateStore(api.layout)
	failoverState, err := failoverStore.Load()
	if err != nil {
		return failoverRunResult{}, err
	}
	result := failoverRunResult{}
	if !failoverState.Enabled {
		return result, nil
	}

	topology, err := api.store.LoadAppliedTopology()
	if err != nil {
		return failoverRunResult{}, err
	}
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		return failoverRunResult{}, err
	}
	now := time.Now().UTC()
	groups, err := buildProxyGroupViews(topology, runtimeState, failoverState, now)
	if err != nil {
		return failoverRunResult{}, runtimeHTTPError{code: http.StatusInternalServerError, err: err}
	}
	var planned []failoverApply
	var eligible map[string]bool
	if len(eligibleSets) > 0 {
		eligible = eligibleSets[0]
	}
	for _, group := range groups {
		if !group.Policy.AutoFailover {
			result.Decisions = append(result.Decisions, failover.Decision{
				PoolID: group.ID, Role: group.Role, From: group.Selected, To: group.Selected,
				Kind: failover.KindNoop, Reason: "auto_failover_disabled", Time: now,
			})
			continue
		}
		if source != "manual" && eligible != nil && !eligible[group.ID] {
			result.Decisions = append(result.Decisions, failover.Decision{
				PoolID: group.ID, Role: group.Role, From: group.Selected, To: group.Selected,
				Kind: failover.KindNoop, Reason: "probe_incomplete_skip", Time: now,
			})
			continue
		}
		if _, locked := failoverState.Locks[group.ID]; locked {
			result.Decisions = append(result.Decisions, failover.Decision{
				PoolID: group.ID,
				Role:   group.Role,
				From:   group.Selected,
				To:     group.Selected,
				Kind:   failover.KindNoop,
				Reason: "failover_locked",
				Time:   now,
			})
			continue
		}
		if poolState := failoverState.Pools[group.ID]; poolState.CooldownUntil != nil && poolState.CooldownUntil.After(now) {
			result.Decisions = append(result.Decisions, failover.Decision{
				PoolID: group.ID,
				Role:   group.Role,
				From:   group.Selected,
				To:     group.Selected,
				Kind:   failover.KindNoop,
				Reason: "cooldown_active",
				Time:   now,
			})
			continue
		}
		healthSource := runtimeState.NodeHealth
		if source != "manual" {
			healthSource = make(map[string]state.NodeHealth, len(runtimeState.NodeHealth))
			for id, item := range runtimeState.NodeHealth {
				if item.WatchdogStatus == "" {
					continue
				}
				item.Status, item.UpdatedAt, item.Context = item.WatchdogStatus, item.WatchdogUpdatedAt, item.WatchdogContext
				healthSource[id] = item
			}
		}
		health := freshNodeHealth(appliedNodeHealth(topology, healthSource), now, failoverHealthTTL)
		if probeableCandidateCount(group.Candidates) > 0 && observedHealthCandidates(group.Candidates, health) == 0 {
			result.Decisions = append(result.Decisions, failover.Decision{
				PoolID: group.ID,
				Role:   group.Role,
				From:   group.Selected,
				To:     group.Selected,
				Kind:   failover.KindNoop,
				Reason: "no_health_sample_skip",
				Time:   now,
			})
			continue
		}
		if source != "manual" {
			if current := health[group.Selected]; current.Status == "failed" && current.ConsecutiveFailures < failoverFailureThreshold {
				result.Decisions = append(result.Decisions, failover.Decision{
					PoolID: group.ID, Role: group.Role, From: group.Selected, To: group.Selected,
					Kind: failover.KindNoop, Reason: "current_failure_unconfirmed", Time: now,
				})
				continue
			}
			if isProbeableNode(group.Selected) && len(healthyCandidates(group.Candidates, health)) == 0 {
				result.Decisions = append(result.Decisions, failover.Decision{
					PoolID: group.ID, Role: group.Role, From: group.Selected, To: group.Selected,
					Kind: failover.KindNoop, Reason: "no_verified_replacement", Time: now,
				})
				continue
			}
			if noHealthyCandidateConfirmed(group.Candidates, health) {
				result.Decisions = append(result.Decisions, failover.Decision{
					PoolID: group.ID, Role: group.Role, From: group.Selected, To: group.Selected,
					Kind: failover.KindNoop, Reason: "candidate_failures_unconfirmed", Time: now,
				})
				continue
			}
		}
		decision := failover.Decide(failover.PoolHealth{
			PoolID: group.ID, Role: group.Role, NoCandidate: group.Policy.NoCandidate,
			Current: group.Selected, Candidates: group.Candidates, Healthy: healthyCandidates(group.Candidates, health),
		}, now)
		result.Decisions = append(result.Decisions, decision)
		if shouldApplyFailoverDecision(decision) {
			if !containsString(group.Candidates, decision.To) {
				return failoverRunResult{}, runtimeHTTPError{code: http.StatusConflict, err: fmt.Errorf("failover target is not a candidate: %s", decision.To)}
			}
			planned = append(planned, failoverApply{group: group, decision: decision})
			continue
		}
		// Prefer durable applied topology selections over RuntimeDir, which may
		// lag or be empty after reboot while SelectionState still holds live choices.
		if decision.Kind == failover.KindFailClosed && topology.ProxySelection[group.ID] != decision.To {
			if current := topology.ProxySelection[group.ID]; current != "" {
				decision.From = current
			}
			planned = append(planned, failoverApply{group: group, decision: decision})
		}
	}
	if len(planned) == 0 {
		return result, nil
	}
	releaseMutation()
	mutationReleased = true

	for _, item := range planned {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if api.selectorTxn == nil {
			return result, fmt.Errorf("selector transaction is not configured")
		}
		// Build target pool snapshot from the pre-txn view; only commit it into
		// failoverState after selector CAS/Apply succeeds so a rejected txn cannot
		// leave Pools[id].Current pointing at a node that never became live.
		targetPool := buildFailoverPoolState(item.decision, failoverState)
		cutoverResult, err := api.selectorTxn.Cutover(ctx, commitlane.CutoverRequest{
			OpID: fmt.Sprintf("failover-%s-%d", item.group.ID, time.Now().UnixNano()), PoolID: item.group.ID,
			TargetSmartNode: item.decision.To, Source: cutoverSourceForBinding(item.group.Binding),
			TargetPool: targetPool, ExpectedConfigEpoch: topology.ConfigEpoch,
		})
		if err != nil {
			stage, _, _ := commitlaneCutoverErrorDetails(err)
			if stage == "" {
				stage = "cutover_transaction"
			}
			result.Failures = append(result.Failures, failoverFailure{PoolID: item.group.ID, Stage: stage, Error: sanitizeAPIMessage(err.Error())})
			if restored, loadErr := state.NewFailoverStateStore(api.layout).Load(); loadErr == nil {
				failoverState = restored
			}
			continue
		}
		failoverState = state.NormalizeFailoverState(failoverState)
		failoverState.Pools[item.group.ID] = targetPool
		result.Applied = append(result.Applied, item.decision)
		events := failoverEventsFromCutovers([]failover.Decision{item.decision}, []commitlane.CutoverResult{cutoverResult}, source, "applied", "")
		if _, err := state.NewFailoverEventStore(api.layout).Append(events); err != nil {
			events = nil
		}
		result.Events = append(result.Events, events...)
	}
	if len(result.Failures) > 0 {
		return result, fmt.Errorf("failover completed with %d failed pool mutation(s)", len(result.Failures))
	}
	return result, nil
}

func buildFailoverPoolState(decision failover.Decision, failoverState state.FailoverState) state.FailoverPoolState {
	failoverState = state.NormalizeFailoverState(failoverState)
	when := decision.Time
	if when.IsZero() {
		when = time.Now().UTC()
	}
	previous := decision.From
	if previous == "" {
		previous = failoverState.Pools[decision.PoolID].Current
	}
	cooldown := time.Duration(failoverState.CooldownSeconds) * time.Second
	cooldownUntil := when.Add(cooldown)
	return state.FailoverPoolState{
		PoolID:        decision.PoolID,
		Role:          decision.Role,
		Current:       decision.To,
		Previous:      previous,
		LastKind:      string(decision.Kind),
		LastReason:    decision.Reason,
		LastChangedAt: &when,
		CooldownUntil: &cooldownUntil,
		Terminal:      decision.Kind == failover.KindFailClosed && decision.To == "Block",
	}
}

func (api nodesAPI) lockStoreRuntime(name string) (func(), error) {
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

func failoverEventsFromCutovers(decisions []failover.Decision, results []commitlane.CutoverResult, source, status, errMsg string) []state.FailoverEvent {
	events := make([]state.FailoverEvent, 0, len(decisions))
	for index, decision := range decisions {
		event := state.FailoverEvent{
			Time:   decision.Time,
			PoolID: decision.PoolID,
			Role:   decision.Role,
			From:   decision.From,
			To:     decision.To,
			Kind:   string(decision.Kind),
			Reason: decision.Reason,
			Source: source,
			Status: status,
			Error:  errMsg,
		}
		if index < len(results) {
			result := results[index]
			event.GenerationTo = result.Binding.Generation
			if result.Binding.Generation > 0 {
				event.GenerationFrom = result.Binding.Generation - 1
			}
			event.Executor = result.Binding.Target.Executor
			event.CutoverResult = result.Result
		}
		events = append(events, event)
	}
	return events
}

func cutoverSourceForBinding(binding state.PoolBinding) commitlane.CutoverSource {
	return commitlane.CutoverSource{SmartNode: bindingSelection(binding), Generation: binding.Generation}
}

func commitlaneCutoverErrorDetails(err error) (stage string, rolledBack, blocked bool) {
	var cutoverErr *commitlane.CutoverError
	if errors.As(err, &cutoverErr) {
		return cutoverErr.Stage, cutoverErr.RolledBack, cutoverErr.Blocked
	}
	return "", false, false
}

func writeCutoverError(w http.ResponseWriter, err error) {
	var selectionConflict commitlane.SelectionConflictError
	if errors.As(err, &selectionConflict) {
		writeAPIErrorCode(w, http.StatusConflict, "selection_conflict", selectionConflict.Error(), map[string]any{
			"pool": selectionConflict.Pool, "expected": selectionConflict.Expected,
			"actual": selectionConflict.Actual, "reason": selectionConflict.Reason,
		})
		return
	}
	var requiresApply commitlane.RequiresConfigApplyError
	if errors.As(err, &requiresApply) {
		writeAPIErrorCode(w, http.StatusConflict, "requires_config_apply", "target requires a configuration Apply", map[string]any{
			"pool": requiresApply.Pool, "target": requiresApply.Target, "reason": requiresApply.Reason,
		})
		return
	}
	var cutoverConflict commitlane.CutoverConflictError
	if errors.As(err, &cutoverConflict) {
		writeAPIErrorCode(w, http.StatusConflict, "cutover_conflict", cutoverConflict.Error(), map[string]any{
			"pool": cutoverConflict.Pool, "reason": cutoverConflict.Reason, "old_generation": cutoverConflict.OldGeneration,
		})
		return
	}
	if errors.Is(err, commitlane.ErrRescueActive) || errors.Is(err, commitlane.ErrRecoveryRequired) {
		writeAPIErrorCode(w, http.StatusLocked, "commit_locked", err.Error(), nil)
		return
	}
	stage, rolledBack, blocked := commitlaneCutoverErrorDetails(err)
	if stage != "" {
		status := http.StatusBadGateway
		if strings.Contains(stage, "persist") || strings.Contains(stage, "journal") || strings.Contains(stage, "remove_") {
			status = http.StatusInternalServerError
		}
		writeAPIErrorCode(w, status, "cutover_failed", "cutover failed at "+stage, map[string]any{
			"stage": stage, "rolled_back": rolledBack, "blocked": blocked,
		})
		return
	}
	writeAPIErrorCode(w, http.StatusInternalServerError, "cutover_failed", "cutover failed", nil)
}

func freshNodeHealth(health map[string]state.NodeHealth, now time.Time, ttl time.Duration) map[string]state.NodeHealth {
	if ttl <= 0 {
		return health
	}
	out := map[string]state.NodeHealth{}
	for nodeID, item := range health {
		updatedAt := item.UpdatedAt
		if updatedAt == nil {
			updatedAt = item.LastTest
		}
		if updatedAt == nil || now.Sub(*updatedAt) > ttl {
			continue
		}
		out[nodeID] = item
	}
	return out
}

func observedHealthCandidates(candidates []string, health map[string]state.NodeHealth) int {
	count := 0
	for _, candidate := range candidates {
		if candidate == "Block" || candidate == "Direct" {
			continue
		}
		if _, ok := health[candidate]; ok {
			count++
		}
	}
	return count
}

func probeableCandidateCount(candidates []string) int {
	count := 0
	for _, candidate := range candidates {
		if isProbeableNode(candidate) {
			count++
		}
	}
	return count
}

func healthyCandidates(candidates []string, health map[string]state.NodeHealth) []string {
	var healthy []string
	for _, candidate := range candidates {
		if candidate == "Block" || candidate == "Direct" {
			continue
		}
		if health[candidate].Status == "ok" {
			healthy = append(healthy, candidate)
		}
	}
	return healthy
}

func noHealthyCandidateConfirmed(candidates []string, health map[string]state.NodeHealth) bool {
	hasHealthy := false
	hasUnconfirmed := false
	for _, candidate := range candidates {
		if !isProbeableNode(candidate) {
			continue
		}
		item, ok := health[candidate]
		if ok && item.Status == "ok" {
			hasHealthy = true
			continue
		}
		if !ok || item.ConsecutiveFailures < failoverFailureThreshold {
			hasUnconfirmed = true
		}
	}
	return !hasHealthy && hasUnconfirmed
}

func shouldApplyFailoverDecision(decision failover.Decision) bool {
	if decision.To == "" || decision.To == decision.From {
		return false
	}
	switch decision.Kind {
	case failover.KindSwitch, failover.KindFailClosed, failover.KindRecovery:
		return true
	default:
		return false
	}
}

func (api nodesAPI) nodeAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	if path == "" {
		writeAPIError(w, http.StatusNotFound, "node action not found")
		return
	}
	id, action, hasAction := strings.Cut(path, "/")
	if !hasAction {
		api.smartNodeDetail(w, r, id)
		return
	}
	switch action {
	case "core-choice":
		api.setCoreChoice(w, r, id)
	case "delay":
		api.nodeDelay(w, r, id)
	case "deep-test":
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeAPIErrorCode(w, http.StatusNotImplemented, "deep_test_not_implemented", "deep node testing is deferred", nil)
	default:
		writeAPIError(w, http.StatusNotFound, "node action not found")
	}
}

func (api nodesAPI) smartNodeDetail(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	spec, runtimeState, err := api.loadSpecRuntime("smartnode-detail-read")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(r.Context(), provider.NewLastGoodStore(api.layout))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	catalog, _, err := provider.BuildSmartNodeCatalogFromSnapshots(spec, snapshots, time.Now().UTC())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	node, ok := catalog[id]
	if !ok {
		writeAPIErrorCode(w, http.StatusNotFound, "node_not_found", "SmartNode not found", nil)
		return
	}
	activeIDs := activeSmartNodeIDs(spec, snapshots)
	poolRefs := smartNodePoolReferences(spec.BusinessPools)
	writeAPIData(w, http.StatusOK, smartNodePublicView(node, poolRefs[id], runtimeState.NodeHealth[id], activeIDs[id]))
}

func (api nodesAPI) setCoreChoice(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPut {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if api.smartNodeTxn == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "SmartNode transaction is not configured")
		return
	}
	var payload coreChoicePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", "invalid json", nil)
		return
	}
	if payload.OpID == "" {
		writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", "op_id is required", nil)
		return
	}
	result, err := api.smartNodeTxn.SetCoreChoice(r.Context(), commitlane.SmartNodeCoreChoiceRequest{
		OpID: payload.OpID, SmartNodeID: id, Core: payload.Core, ConfirmDegraded: payload.ConfirmDegraded,
	})
	if err != nil {
		var txnErr *commitlane.SmartNodeTxnError
		if errors.As(err, &txnErr) {
			status := http.StatusBadRequest
			switch txnErr.Code {
			case "node_not_found":
				status = http.StatusNotFound
			case "capability_unsupported":
				status = http.StatusUnprocessableEntity
			case "confirm_degraded_required":
				status = http.StatusConflict
			}
			writeAPIErrorCode(w, status, txnErr.Code, txnErr.Message, map[string]any{"reasons": txnErr.Reasons})
			return
		}
		if errors.Is(err, commitlane.ErrRescueActive) || errors.Is(err, commitlane.ErrRecoveryRequired) {
			writeAPIErrorCode(w, http.StatusLocked, "commit_locked", err.Error(), nil)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view := smartNodePublicView(result.SmartNode, result.AffectedPools, state.NodeHealth{}, true)
	writeAPIData(w, http.StatusOK, map[string]any{
		"smart_node": view, "affected_pools": view.ReferencedByPools, "dirty": result.Dirty, "metadata_only": result.MetadataOnly,
	})
}

func (api nodesAPI) nodeDelay(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload nodeDelayPayload
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
	}
	if payload.URL == "" {
		payload.URL = defaultNodeDelayURL
	}
	testURL, err := url.Parse(payload.URL)
	if err != nil || (testURL.Scheme != "http" && testURL.Scheme != "https") || testURL.Host == "" || testURL.User != nil {
		writeAPIError(w, http.StatusBadRequest, "probe URL must be HTTP(S) without credentials")
		return
	}
	if payload.TimeoutMS < 0 || payload.TimeoutMS > 30000 {
		writeAPIError(w, http.StatusBadRequest, "timeout_ms must be between 0 and 30000")
		return
	}
	timeout := time.Duration(payload.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	spec, _, err := api.loadSpecRuntime("node-delay-desired-read")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(r.Context(), provider.NewLastGoodStore(api.layout))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	catalog, _, err := provider.BuildSmartNodeCatalogFromSnapshots(spec, snapshots, time.Now().UTC())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	node, ok := catalog[id]
	if !ok {
		writeAPIErrorCode(w, http.StatusNotFound, "node_not_found", "SmartNode not found", nil)
		return
	}
	if node.CoreChoice == "" {
		// Older captures may have an applied core before the choice was saved.
		// Reuse it only for the identical identity, without changing Spec.
		if topology, _, loadErr := api.loadAppliedTopologyRuntime("node-delay-legacy-core"); loadErr == nil {
			if applied, exists := topology.Spec.SmartNodes[id]; exists &&
				applied.Revision == node.Revision && smartnode.IdentityDigest(applied.Identity) == smartnode.IdentityDigest(node.Identity) {
				node.CoreChoice = applied.CoreChoice
			}
		}
	}
	if node.CoreChoice == "" {
		writeAPIErrorCode(w, http.StatusUnprocessableEntity, "node_core_unset", "SmartNode has no selected execution core", nil)
		return
	}
	capability, known := node.Capability.ForCore(node.CoreChoice)
	if !known || capability.Status == smartnode.CapabilityUnsupportedByCore {
		writeAPIErrorCode(w, http.StatusUnprocessableEntity, "capability_unsupported", "selected execution core cannot run this node", nil)
		return
	}
	views, err := provider.BuildNodeViewsFromSnapshots(spec, snapshots)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	var source *smartnode.IdentitySource
	for _, view := range views {
		if view.ID == id && !view.Excluded {
			source = &smartnode.IdentitySource{RawTag: view.Tag, Identity: view.Identity, References: view.References}
			break
		}
	}
	if source == nil {
		writeAPIErrorCode(w, http.StatusNotFound, "node_source_missing", "SmartNode has no active provider source", nil)
		return
	}
	result, err := api.delayDesiredNode(r.Context(), node, *source, payload.URL, timeout)
	if r.Context().Err() != nil {
		return
	}
	if errors.Is(err, worker.ErrNodeProbeUnavailable) {
		writeAPIErrorCode(w, http.StatusServiceUnavailable, "node_probe_unavailable", redact.MessageWithPaths(err.Error()), nil)
		return
	}
	now := time.Now().UTC()
	health := state.NodeHealth{LastTest: &now, UpdatedAt: &now, TestURL: payload.URL}
	health.Context = result.Context
	if err != nil {
		health.Status = "failed"
		health.Error = err.Error()
	} else {
		health.Status = "ok"
		health.DelayMS = result.DelayMS
		health.TestURL = result.URL
	}
	if err := api.mergeNodeHealth("node-delay-write", map[string]state.NodeHealth{id: health}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]any{"node_id": id, "health": sanitizeNodeHealth(health)})
}

type appliedNodeDelayResult struct {
	DelayMS int
	URL     string
	Context state.NodeHealthContext
}

func (api nodesAPI) delayDesiredNode(ctx context.Context, node smartnode.SmartNode, source smartnode.IdentitySource, testURL string, timeout time.Duration) (appliedNodeDelayResult, error) {
	healthContext := state.NodeHealthContext{Revision: node.Revision, Core: node.CoreChoice, Method: "isolated_worker_http_204"}
	if api.workers == nil {
		return appliedNodeDelayResult{URL: testURL, Context: healthContext}, fmt.Errorf("%w: worker manager is not configured", worker.ErrNodeProbeUnavailable)
	}
	topology, _, loadErr := api.loadAppliedTopologyRuntime("node-delay-applied-read")
	if loadErr == nil {
		path, pathErr := api.appliedNodeBindingPath(topology, node.ID)
		if pathErr == nil && path.Executor.Revision == node.Revision && path.Executor.Core == node.CoreChoice {
			if startErr := api.ensureProbeWorker(ctx, node.ID, path.Worker.ID); startErr == nil {
				result, err := api.workers.DelayPath(ctx, path, testURL, timeout)
				if err == nil {
					healthContext.Worker = path.Worker.ID
					healthContext.Method = "worker_route_http_204"
					return appliedNodeDelayResult{DelayMS: result.DelayMS, URL: result.URL, Context: healthContext}, nil
				}
			}
			if ctx.Err() != nil {
				return appliedNodeDelayResult{URL: testURL, Context: healthContext}, ctx.Err()
			}
		}
	}
	// A missing executor or broken control endpoint is not proof of a dead
	// upstream. Probe the same identity independently before recording failure.
	result, err := api.workers.DelayNode(ctx, node, source, testURL, timeout)
	return appliedNodeDelayResult{DelayMS: result.DelayMS, URL: result.URL, Context: healthContext}, err
}

func (api nodesAPI) delayAppliedNode(ctx context.Context, topology state.AppliedTopology, nodeID, testURL string, timeout time.Duration) (appliedNodeDelayResult, error) {
	path, err := api.appliedNodeBindingPath(topology, nodeID)
	context := nodeHealthContext(topology, nodeID)
	if err != nil {
		context.Method = "worker_route_http_204"
		return appliedNodeDelayResult{URL: testURL, Context: context}, err
	}
	context.Revision = path.Executor.Revision
	context.Core = path.Executor.Core
	context.Worker = path.Worker.ID
	context.Method = "worker_route_http_204"
	// A Pool on its no_candidate fallback binds nothing, so the Worker that
	// hosts its candidates is installed but stopped; probing through it would
	// fail forever and the pool could never fail back. Start it on demand.
	if err := api.ensureProbeWorker(ctx, nodeID, path.Worker.ID); err != nil {
		return appliedNodeDelayResult{URL: testURL, Context: context}, err
	}
	result, err := api.workers.DelayPath(ctx, path, testURL, timeout)
	return appliedNodeDelayResult{DelayMS: result.DelayMS, URL: result.URL, Context: context}, err
}

func (api nodesAPI) ensureProbeWorker(ctx context.Context, nodeID, workerID string) error {
	if api.workers.IsRunning(workerID) {
		return nil
	}
	if api.selectorTxn == nil {
		return fmt.Errorf("worker %s is not running and no selector transaction can start it", workerID)
	}
	opID := fmt.Sprintf("probe-start-%s-%d", workerID, time.Now().UnixNano())
	started, err := api.selectorTxn.EnsureWorkerRunning(ctx, opID, workerID)
	if err != nil {
		return fmt.Errorf("start worker %s to probe node %s: %w", workerID, nodeID, err)
	}
	if started {
		log.Printf("started worker %s to probe node %s (no pool binding references it)", workerID, nodeID)
	}
	return nil
}

func (api nodesAPI) appliedNodeBindingPath(topology state.AppliedTopology, nodeID string) (worker.BindingPath, error) {
	if api.workers == nil {
		return worker.BindingPath{}, fmt.Errorf("worker manager is not configured")
	}
	for _, executor := range topology.ExecutionGraph.NodeExecutors {
		if executor.SmartNode != nodeID {
			continue
		}
		for _, target := range topology.ExecutionGraph.Workers {
			if target.ID != executor.Worker {
				continue
			}
			for _, route := range target.Routes {
				if !containsString(route.Candidates, executor.ID) {
					continue
				}
				return api.workers.ResolvePath(target.ID, route.Pool, route.Lane, executor.ID)
			}
		}
		return worker.BindingPath{}, fmt.Errorf("node %s executor %s has no worker route", nodeID, executor.ID)
	}
	return worker.BindingPath{}, fmt.Errorf("node %s has no applied executor", nodeID)
}

func smartNodePublicView(node smartnode.SmartNode, pools []string, health state.NodeHealth, sourcePresent bool) smartNodeView {
	capabilities := make(smartnode.CapabilityMatrix, len(node.Capability))
	for core, capability := range node.Capability {
		capability.Reasons = append([]string{}, capability.Reasons...)
		capabilities[core] = capability
	}
	var coreChoice *smartnode.CoreID
	if smartnode.ValidCore(node.CoreChoice) {
		choice := node.CoreChoice
		coreChoice = &choice
	}
	var healthView *state.NodeHealth
	if nodeHealthMatches(health, node.Revision, node.CoreChoice) && (health.Status != "" || health.UpdatedAt != nil || health.LastTest != nil) {
		health = sanitizeNodeHealth(health)
		if health.Context.Revision == 0 {
			health.Context.Revision = node.Revision
		}
		if health.Context.Core == "" {
			health.Context.Core = node.CoreChoice
		}
		if health.Context.Method == "" {
			health.Context.Method = "unknown"
		}
		healthView = &health
	}
	return smartNodeView{
		ID: node.ID, Revision: node.Revision, DisplayName: smartnode.DisplayName(node),
		Identity: smartNodeIdentityView{
			Protocol: node.Identity.Protocol, Server: node.Identity.Server, Port: node.Identity.Port,
			Transport: node.Identity.Transport,
			Security: smartNodeSecurityView{
				Type: node.Identity.Security.Type, SNI: node.Identity.Security.SNI,
				ALPN: append([]string(nil), node.Identity.Security.ALPN...), Fingerprint: node.Identity.Security.Fingerprint,
				Flow: node.Identity.Security.Flow, PublicKeyMasked: smartnode.MaskRef(node.Identity.Security.PublicKeyRef),
				ShortID: node.Identity.Security.ShortID, SkipCertVerify: node.Identity.Security.SkipCertVerify,
				CertificateMasked: smartnode.MaskRef(node.Identity.Security.CertificateRef),
			},
			SecretRefMasked: smartnode.MaskRef(node.Identity.SecretRef),
		},
		Aliases: append([]smartnode.NodeAlias{}, node.Aliases...), Capability: capabilities,
		CoreChoice: coreChoice, ReferencedByPools: append([]string{}, pools...), SourceMissing: !sourcePresent, Health: healthView,
	}
}

func nodeHealthMatches(health state.NodeHealth, revision int, core smartnode.CoreID) bool {
	return (health.Context.Revision == 0 || health.Context.Revision == revision) &&
		(health.Context.Core == "" || health.Context.Core == core)
}

func appliedNodeHealth(topology state.AppliedTopology, health map[string]state.NodeHealth) map[string]state.NodeHealth {
	matching := make(map[string]state.NodeHealth, len(health))
	for id, item := range health {
		node, ok := topology.Spec.SmartNodes[id]
		if ok && nodeHealthMatches(item, node.Revision, node.CoreChoice) {
			matching[id] = item
		}
	}
	return matching
}

func smartNodePoolReferences(pools []state.BusinessPoolSpec) map[string][]string {
	refs := map[string][]string{}
	for _, pool := range state.EffectiveBusinessPools(pools) {
		for _, id := range pool.NodeIDs {
			refs[id] = append(refs[id], pool.ID)
		}
	}
	for id := range refs {
		sort.Strings(refs[id])
	}
	return refs
}

func activeSmartNodeIDs(spec state.SpecState, snapshots []state.LastGoodSnapshot) map[string]bool {
	nodes, err := provider.BuildNodeViewsFromSnapshots(spec, snapshots)
	if err != nil {
		return map[string]bool{}
	}
	active := map[string]bool{}
	for _, node := range nodes {
		if !node.Excluded {
			active[node.ID] = true
		}
	}
	return active
}

func matchesSmartNodeFilters(view smartNodeView, r *http.Request) bool {
	query := r.URL.Query()
	if value := strings.TrimSpace(query.Get("provider")); value != "" {
		matched := false
		for _, alias := range view.Aliases {
			if alias.ProviderID == value {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if value := strings.ToLower(strings.TrimSpace(query.Get("protocol"))); value != "" && view.Identity.Protocol != value {
		return false
	}
	if value := strings.TrimSpace(query.Get("core")); value != "" {
		if value == "unset" && view.CoreChoice != nil {
			return false
		}
		if value != "unset" && (view.CoreChoice == nil || string(*view.CoreChoice) != value) {
			return false
		}
	}
	if value := strings.TrimSpace(query.Get("capability")); value != "" {
		matched := false
		for _, capability := range view.Capability {
			if capability.Status == value {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if value := strings.TrimSpace(query.Get("health")); value != "" {
		if view.Health == nil || view.Health.Status != value {
			return false
		}
	}
	return true
}

func nodeHealthContext(topology state.AppliedTopology, nodeID string) state.NodeHealthContext {
	context := state.NodeHealthContext{Method: "unknown"}
	if node, ok := topology.Spec.SmartNodes[nodeID]; ok {
		context.Revision = node.Revision
		context.Core = node.CoreChoice
	}
	for _, executor := range topology.ExecutionGraph.NodeExecutors {
		if executor.SmartNode == nodeID && (context.Core == "" || executor.Core == context.Core) {
			context.Revision = executor.Revision
			context.Core = executor.Core
			context.Worker = executor.Worker
			break
		}
	}
	return context
}

func findUsableAppliedNode(topology state.AppliedTopology, id string) (provider.NodeView, bool, error) {
	nodes, err := provider.BuildNodeViewsFromSnapshots(topology.Spec, topology.LastGood)
	if err != nil {
		return provider.NodeView{}, false, err
	}
	for _, node := range nodes {
		if node.ID == id && !node.Excluded {
			return node, true, nil
		}
	}
	return provider.NodeView{}, false, nil
}

func (api nodesAPI) loadSpecRuntime(name string) (state.SpecState, state.RuntimeState, error) {
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
	runtimeState.ProxySelection = ensureStringMap(runtimeState.ProxySelection)
	runtimeState.NodeHealth = ensureHealthMap(runtimeState.NodeHealth)
	return spec, runtimeState, nil
}

func (api nodesAPI) loadAppliedTopologyRuntime(name string) (state.AppliedTopology, state.RuntimeState, error) {
	if api.locks != nil {
		session := api.locks.Session(name)
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return state.AppliedTopology{}, state.RuntimeState{}, err
		}
		releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
		if err != nil {
			releaseStore()
			return state.AppliedTopology{}, state.RuntimeState{}, err
		}
		defer releaseStore()
		defer releaseRuntime()
	}
	topology, err := api.store.LoadAppliedTopology()
	if err != nil {
		return state.AppliedTopology{}, state.RuntimeState{}, err
	}
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		return state.AppliedTopology{}, state.RuntimeState{}, err
	}
	runtimeState.ProxySelection = ensureStringMap(runtimeState.ProxySelection)
	runtimeState.NodeHealth = ensureHealthMap(runtimeState.NodeHealth)
	return topology, runtimeState, nil
}

func (api nodesAPI) mergeNodeHealth(name string, health map[string]state.NodeHealth) error {
	if len(health) == 0 {
		return nil
	}
	release, err := api.lockStoreRuntime(name)
	if err != nil {
		return err
	}
	defer release()
	return api.store.MergeNodeHealth(health)
}

func buildProxyGroupViews(topology state.AppliedTopology, runtimeState state.RuntimeState, failoverState state.FailoverState, now time.Time) ([]proxyGroupView, error) {
	graph, err := rebuildLiveExecutionGraph(topology, now)
	if err != nil {
		return nil, err
	}
	failoverState = state.NormalizeFailoverState(failoverState)
	executors := make(map[string]state.NodeExecutor, len(graph.NodeExecutors))
	for _, executor := range graph.NodeExecutors {
		executors[executor.SmartNode] = executor
	}
	bindings := make(map[string]state.PoolBinding, len(graph.PoolBindings))
	for _, binding := range graph.PoolBindings {
		bindings[binding.Pool] = binding
	}
	groups := make([]proxyGroupView, 0, len(state.EffectiveBusinessPools(topology.Spec.BusinessPools)))
	for _, pool := range state.EffectiveBusinessPools(topology.Spec.BusinessPools) {
		candidateIDs := make([]string, 0, len(pool.NodeIDs)+1)
		candidateViews := make([]proxyGroupCandidateView, 0, len(pool.NodeIDs)+1)
		for _, nodeID := range pool.NodeIDs {
			executor, ok := executors[nodeID]
			if !ok {
				continue
			}
			health := runtimeState.NodeHealth[nodeID]
			if !nodeHealthMatches(health, executor.Revision, executor.Core) {
				health = state.NodeHealth{}
			}
			status := health.Status
			if status == "" {
				status = "unknown"
			}
			candidateIDs = append(candidateIDs, nodeID)
			candidateViews = append(candidateViews, proxyGroupCandidateView{
				SmartNode: nodeID, Core: executor.Core, Health: status, DelayMS: health.DelayMS,
			})
			if len(candidateIDs) >= maxProxyGroupCandidates {
				break
			}
		}
		fallback := ""
		switch pool.Policy.NoCandidate {
		case state.NoCandidateBlock:
			fallback = "Block"
		case state.NoCandidateDirect:
			fallback = "Direct"
		}
		if fallback != "" && !containsString(candidateIDs, fallback) {
			candidateIDs = append(candidateIDs, fallback)
			candidateViews = append(candidateViews, proxyGroupCandidateView{SmartNode: fallback, Health: "unknown"})
		}
		binding, ok := bindings[pool.ID]
		if !ok {
			return nil, fmt.Errorf("pool %s has no live Binding", pool.ID)
		}
		var poolLock *state.FailoverLock
		if item, locked := failoverState.Locks[pool.ID]; locked {
			copyItem := item
			poolLock = &copyItem
		}
		groups = append(groups, proxyGroupView{
			Pool: pool.ID, Binding: binding, CandidateViews: candidateViews,
			Lock: poolLock, CooldownUntil: failoverState.Pools[pool.ID].CooldownUntil,
			FailoverEnabled: failoverState.Enabled && pool.Policy.AutoFailover,
			ID:              pool.ID, Name: pool.Name, Role: pool.Role, Selector: selection.PoolTag(pool.ID),
			Candidates: candidateIDs, Selected: bindingSelection(binding), Policy: pool.Policy,
		})
	}
	return groups, nil
}

func rebuildLiveExecutionGraph(topology state.AppliedTopology, now time.Time) (state.ExecutionGraph, error) {
	plan := planner.BuildExecutionGraph(topology.Spec, topology.Selections, topology.ExecutionGraph, now)
	if plan.ReadyToApply {
		return plan.Graph, nil
	}
	issues := make([]string, 0, len(plan.Issues))
	for _, issue := range plan.Issues {
		if issue.Blocking {
			issues = append(issues, issue.Code+": "+issue.Message)
		}
	}
	return state.ExecutionGraph{}, fmt.Errorf("rebuild live ExecutionGraph: %s", strings.Join(issues, "; "))
}

func bindingSelection(binding state.PoolBinding) string {
	switch binding.Target.Kind {
	case state.NoCandidateDirect:
		return "Direct"
	case state.NoCandidateBlock:
		return "Block"
	default:
		return binding.Target.SmartNode
	}
}

func findProxyGroup(groups []proxyGroupView, id string) (proxyGroupView, bool) {
	for _, group := range groups {
		if group.ID == id {
			return group, true
		}
	}
	return proxyGroupView{}, false
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func ensureStringMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

func ensureHealthMap(in map[string]state.NodeHealth) map[string]state.NodeHealth {
	if in == nil {
		return map[string]state.NodeHealth{}
	}
	return in
}

func lockProxyMutation(locks *lockset.LockSet, layout state.Layout, name string) (func(), error) {
	if locks == nil {
		return func() {}, nil
	}
	session := locks.Session(name)
	releaseRescue, err := session.Acquire(lockset.LevelRescueGate)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(layout.RescueStatusPath()); err == nil {
		releaseRescue()
		return nil, commitlane.ErrRescueActive
	}
	releaseStore, err := session.Acquire(lockset.LevelStore)
	if err != nil {
		releaseRescue()
		return nil, err
	}
	journals, err := state.NewTransactionStore(layout).List()
	if err != nil {
		releaseStore()
		releaseRescue()
		return nil, err
	}
	if len(journals) > 0 {
		releaseStore()
		releaseRescue()
		return nil, fmt.Errorf("%w: transaction %s (%s) is %s", commitlane.ErrRecoveryRequired, journals[0].OpID, journals[0].Kind, journals[0].Phase)
	}
	releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
	if err != nil {
		releaseStore()
		releaseRescue()
		return nil, err
	}
	releaseNet, err := session.Acquire(lockset.LevelNet)
	if err != nil {
		releaseRuntime()
		releaseStore()
		releaseRescue()
		return nil, err
	}
	return func() {
		releaseNet()
		releaseRuntime()
		releaseStore()
		releaseRescue()
	}, nil
}
