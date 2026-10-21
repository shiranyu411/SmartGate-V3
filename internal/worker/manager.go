package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"smartgate/internal/opid"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

const defaultEventLimit = 200

type Manager struct {
	mu             sync.RWMutex
	adapters       map[smartnode.CoreID]Adapter
	status         map[string]Status
	events         map[string][]Event
	active         map[string]state.WorkerTarget
	configured     Prepared
	eventLimit     int
	failureHandler FailureHandler
	probeInterval  time.Duration
}

func NewManager(adapters ...Adapter) (*Manager, error) {
	manager := &Manager{
		adapters: make(map[smartnode.CoreID]Adapter, len(adapters)), status: map[string]Status{},
		events: map[string][]Event{}, active: map[string]state.WorkerTarget{}, eventLimit: defaultEventLimit,
		probeInterval: 2 * time.Second,
	}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, fmt.Errorf("worker adapter is required")
		}
		core := adapter.Core()
		if _, exists := manager.adapters[core]; exists {
			return nil, fmt.Errorf("duplicate worker adapter for %s", core)
		}
		manager.adapters[core] = adapter
		workerID, err := manager.workerID(core)
		if err != nil {
			return nil, err
		}
		manager.status[workerID] = Status{ID: workerID, Core: core, Version: adapter.Descriptor().Version, State: StateStopped, Executors: []state.NodeExecutor{}, Pools: []string{}}
	}
	return manager, nil
}

func (manager *Manager) WithFailureHandler(handler FailureHandler) *Manager {
	manager.mu.Lock()
	manager.failureHandler = handler
	manager.mu.Unlock()
	return manager
}

func (manager *Manager) Prepare(ctx context.Context, opID string, graph state.ExecutionGraph, identities map[string]smartnode.IdentitySource) (Prepared, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return Prepared{}, err
	}
	prepared := Prepared{
		OpID: safeOpID, Candidates: map[string]Candidate{}, Executors: append([]state.NodeExecutor(nil), graph.NodeExecutors...),
		Bindings: append([]state.PoolBinding(nil), graph.PoolBindings...),
	}
	targets := append([]state.WorkerTarget(nil), graph.Workers...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	prepared.Targets = append([]state.WorkerTarget(nil), targets...)
	for _, target := range activeTargets(graph.Workers, graph.PoolBindings) {
		prepared.ActiveTargets = append(prepared.ActiveTargets, target.ID)
	}
	for _, target := range targets {
		adapter := manager.adapters[target.Core]
		if adapter == nil {
			manager.cleanupPrepared(prepared)
			return Prepared{}, fmt.Errorf("worker adapter for %s is required", target.Core)
		}
		request, err := compileRequest(target, graph, identities)
		if err != nil {
			manager.cleanupPrepared(prepared)
			return Prepared{}, err
		}
		candidate, err := adapter.Prepare(ctx, safeOpID, request)
		if candidate.WorkerID == "" {
			candidate.WorkerID = target.ID
		}
		if candidate.Core == "" {
			candidate.Core = target.Core
		}
		prepared.Candidates[target.ID] = candidate
		if err != nil {
			manager.cleanupPrepared(prepared)
			if candidate.CheckLog != "" {
				return Prepared{}, fmt.Errorf("prepare worker %s: %w: %s", target.ID, err, strings.TrimSpace(boundedText(candidate.CheckLog, 4096)))
			}
			return Prepared{}, fmt.Errorf("prepare worker %s: %w", target.ID, err)
		}
		if err := adapter.Verify(candidate); err != nil {
			manager.cleanupPrepared(prepared)
			return Prepared{}, fmt.Errorf("verify worker %s candidate: %w", target.ID, err)
		}
	}
	return prepared, nil
}

func (manager *Manager) Checkpoint(opID string) (Checkpoint, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return Checkpoint{}, err
	}
	manager.mu.RLock()
	configured := clonePrepared(manager.configured)
	active := make(map[string]state.WorkerTarget, len(manager.active))
	for workerID, target := range manager.active {
		active[workerID] = target
	}
	manager.mu.RUnlock()
	checkpoint := Checkpoint{
		Backups: map[string]Backup{}, Targets: map[string]state.WorkerTarget{}, Statuses: map[string]Status{}, Prepared: configured,
	}
	targets := configured.Targets
	if len(targets) == 0 {
		for core := range manager.adapters {
			workerID, err := manager.workerID(core)
			if err != nil {
				return Checkpoint{}, err
			}
			targets = append(targets, state.WorkerTarget{ID: workerID, Core: core})
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	}
	for _, target := range targets {
		workerID := target.ID
		adapter := manager.adapters[target.Core]
		if adapter == nil {
			return Checkpoint{}, fmt.Errorf("worker adapter for %s is required", target.Core)
		}
		backup, err := adapter.Backup(safeOpID, workerID)
		if err != nil {
			return Checkpoint{}, fmt.Errorf("backup worker %s: %w", workerID, err)
		}
		backup.WorkerID = workerID
		backup.Core = target.Core
		_, backup.WasActive = active[workerID]
		checkpoint.Backups[workerID] = backup
		checkpoint.Targets[workerID] = target
		manager.mu.RLock()
		checkpoint.Statuses[workerID] = cloneStatus(manager.status[workerID])
		manager.mu.RUnlock()
	}
	return checkpoint, nil
}

func (manager *Manager) Apply(ctx context.Context, prepared Prepared) (ApplyResult, error) {
	if _, err := opid.Clean(prepared.OpID); err != nil {
		return ApplyResult{}, err
	}
	result := ApplyResult{}
	desiredActive := make(map[string]struct{}, len(prepared.ActiveTargets))
	for _, workerID := range prepared.ActiveTargets {
		desiredActive[workerID] = struct{}{}
	}
	targetByID := make(map[string]state.WorkerTarget, len(prepared.Targets))
	for _, target := range prepared.Targets {
		targetByID[target.ID] = target
		candidate, ok := prepared.Candidates[target.ID]
		if !ok {
			return result, fmt.Errorf("worker %s candidate is missing", target.ID)
		}
		adapter := manager.adapters[target.Core]
		if adapter == nil {
			return result, fmt.Errorf("worker adapter for %s is required", target.Core)
		}
		if err := adapter.Verify(candidate); err != nil {
			return result, fmt.Errorf("verify worker %s candidate: %w", target.ID, err)
		}
		_, shouldRun := desiredActive[target.ID]
		var activation Activation
		var err error
		if shouldRun {
			manager.transition(target, prepared, StateStarting, "start", "candidate validated")
			activation, err = adapter.Activate(ctx, candidate)
		} else {
			activation, err = adapter.Install(ctx, candidate)
		}
		result.Workers = append(result.Workers, activation)
		if err != nil {
			manager.transition(target, prepared, StateFailed, "install_failed", err.Error())
			return result, fmt.Errorf("install worker %s: %w", target.ID, err)
		}
		if shouldRun {
			if err := manager.selectRouteDefaults(ctx, target, prepared); err != nil {
				manager.transition(target, prepared, StateFailed, "route_prepare_failed", err.Error())
				return result, fmt.Errorf("prepare worker %s routes: %w", target.ID, err)
			}
			manager.transition(target, prepared, StateRunning, "ready", "worker ready")
		} else {
			manager.mu.RLock()
			_, running := manager.active[target.ID]
			manager.mu.RUnlock()
			if !running {
				manager.configureStopped(target, prepared)
			}
		}
	}

	manager.mu.RLock()
	var stale []state.WorkerTarget
	for workerID, target := range manager.active {
		if _, keep := desiredActive[workerID]; !keep {
			stale = append(stale, target)
		}
	}
	manager.mu.RUnlock()
	for _, target := range stale {
		manager.record(target.ID, target.Core, StateDraining, "drain", "worker no longer has bound pools")
		if err := manager.adapters[target.Core].Stop(ctx, target.ID); err != nil {
			manager.record(target.ID, target.Core, StateFailed, "stop_failed", err.Error())
			return result, fmt.Errorf("stop unbound worker %s: %w", target.ID, err)
		}
		if configured, ok := targetByID[target.ID]; ok {
			manager.configureStopped(configured, prepared)
		} else {
			manager.markStopped(target)
		}
	}
	manager.cleanupPrepared(prepared)
	manager.mu.Lock()
	manager.configured = clonePrepared(prepared)
	manager.configured.Candidates = nil
	manager.mu.Unlock()
	return result, nil
}

func (manager *Manager) Rollback(ctx context.Context, checkpoint Checkpoint) (ApplyResult, error) {
	result := ApplyResult{}
	manager.mu.RLock()
	active := make(map[string]state.WorkerTarget, len(manager.active))
	for workerID, target := range manager.active {
		active[workerID] = target
	}
	manager.mu.RUnlock()
	for workerID, target := range active {
		if backup, ok := checkpoint.Backups[workerID]; ok && backup.WasActive {
			continue
		}
		if err := manager.adapters[target.Core].Stop(ctx, workerID); err != nil {
			return result, fmt.Errorf("stop rollback worker %s: %w", workerID, err)
		}
		manager.markStopped(target)
	}
	ids := make([]string, 0, len(checkpoint.Backups))
	for workerID := range checkpoint.Backups {
		ids = append(ids, workerID)
	}
	sort.Strings(ids)
	for _, workerID := range ids {
		backup := checkpoint.Backups[workerID]
		adapter := manager.adapters[backup.Core]
		if adapter == nil {
			return result, fmt.Errorf("worker adapter for %s is required", backup.Core)
		}
		activation, err := adapter.Restore(ctx, backup)
		result.Workers = append(result.Workers, activation)
		if err != nil {
			manager.record(workerID, backup.Core, StateFailed, "restore_failed", err.Error())
			return result, fmt.Errorf("restore worker %s: %w", workerID, err)
		}
		if backup.WasActive {
			manager.markRestored(workerID, backup.Core, checkpoint)
		} else if target, ok := checkpoint.Targets[workerID]; ok {
			manager.configureStopped(target, checkpoint.Prepared)
		}
	}
	manager.mu.Lock()
	manager.configured = clonePrepared(checkpoint.Prepared)
	manager.mu.Unlock()
	return result, nil
}

func (manager *Manager) StopAll(ctx context.Context) error {
	manager.mu.RLock()
	targets := make([]state.WorkerTarget, 0, len(manager.adapters))
	for core := range manager.adapters {
		workerID, err := manager.workerID(core)
		if err != nil {
			manager.mu.RUnlock()
			return err
		}
		target, ok := manager.active[workerID]
		if !ok {
			target = state.WorkerTarget{ID: workerID, Core: core}
		}
		targets = append(targets, target)
	}
	manager.mu.RUnlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	var errs []string
	for _, target := range targets {
		adapter := manager.adapters[target.Core]
		if adapter == nil {
			errs = append(errs, fmt.Sprintf("%s: adapter unavailable", target.ID))
			continue
		}
		manager.record(target.ID, target.Core, StateDraining, "rescue_stop", "stopping for rescue")
		if err := adapter.Stop(ctx, target.ID); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", target.ID, err))
			manager.record(target.ID, target.Core, StateFailed, "rescue_stop_failed", err.Error())
			continue
		}
		manager.markStopped(target)
	}
	if len(errs) > 0 {
		return fmt.Errorf("stop workers: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (manager *Manager) StartSupervisor(ctx context.Context) func() {
	supervisorCtx, cancel := context.WithCancel(ctx)
	go manager.supervise(supervisorCtx)
	return cancel
}

func (manager *Manager) Statuses() []Status {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	ids := make([]string, 0, len(manager.status))
	for workerID := range manager.status {
		ids = append(ids, workerID)
	}
	sort.Strings(ids)
	out := make([]Status, 0, len(ids))
	for _, workerID := range ids {
		out = append(out, cloneStatus(manager.status[workerID]))
	}
	return out
}

func (manager *Manager) Events(workerID string) []Event {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return append([]Event{}, manager.events[workerID]...)
}

func (manager *Manager) Status(workerID string) (Status, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	status, ok := manager.status[workerID]
	return cloneStatus(status), ok
}

func (manager *Manager) Cleanup(prepared Prepared) {
	manager.cleanupPrepared(prepared)
}

func (manager *Manager) ResolvePath(workerID, poolID string, lane int, executorID string) (BindingPath, error) {
	manager.mu.RLock()
	prepared := clonePrepared(manager.configured)
	manager.mu.RUnlock()
	for _, target := range prepared.Targets {
		if target.ID != workerID {
			continue
		}
		for _, route := range target.Routes {
			if route.Pool != poolID || route.Lane != lane {
				continue
			}
			return bindingPath(prepared, target, route, executorID)
		}
		return BindingPath{}, fmt.Errorf("worker %s has no route for pool %s lane %d", workerID, poolID, lane)
	}
	return BindingPath{}, fmt.Errorf("worker %s is not in the applied layout", workerID)
}

func (manager *Manager) PathsForPoolLane(poolID string, lane int) ([]BindingPath, error) {
	manager.mu.RLock()
	prepared := clonePrepared(manager.configured)
	manager.mu.RUnlock()
	paths := make([]BindingPath, 0)
	for _, target := range prepared.Targets {
		for _, route := range target.Routes {
			if route.Pool != poolID || route.Lane != lane || len(route.Candidates) == 0 {
				continue
			}
			executorID := route.Selected
			if !containsStringValue(route.Candidates, executorID) {
				executorID = route.Candidates[0]
			}
			path, err := bindingPath(prepared, target, route, executorID)
			if err != nil {
				return nil, err
			}
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no worker path exists for pool %s lane %d", poolID, lane)
	}
	return paths, nil
}

func (manager *Manager) EnsureRunning(ctx context.Context, workerID string) (bool, error) {
	manager.mu.RLock()
	if _, ok := manager.active[workerID]; ok {
		manager.mu.RUnlock()
		return false, nil
	}
	prepared := clonePrepared(manager.configured)
	manager.mu.RUnlock()
	var target state.WorkerTarget
	for _, candidate := range prepared.Targets {
		if candidate.ID == workerID {
			target = candidate
			break
		}
	}
	if target.ID == "" {
		return false, fmt.Errorf("worker %s is not installed by the applied layout", workerID)
	}
	adapter := manager.adapters[target.Core]
	if adapter == nil {
		return false, fmt.Errorf("worker adapter for %s is required", target.Core)
	}
	manager.transition(target, prepared, StateStarting, "cutover_start", "starting installed worker for cutover")
	if _, err := adapter.Start(ctx, target); err != nil {
		manager.transition(target, prepared, StateFailed, "cutover_start_failed", err.Error())
		return false, err
	}
	if err := manager.selectRouteDefaults(ctx, target, prepared); err != nil {
		_ = adapter.Stop(context.WithoutCancel(ctx), target.ID)
		manager.transition(target, prepared, StateFailed, "route_prepare_failed", err.Error())
		return false, err
	}
	manager.transition(target, prepared, StateRunning, "cutover_ready", "worker ready for cutover")
	return true, nil
}

// RestartFailed restores an installed process after an unexpected exit. The
// caller must hold the commit lane and reconcile durable route selections
// before reporting recovery: installed route defaults may predate a cutover.
func (manager *Manager) RestartFailed(ctx context.Context, workerID string) (bool, error) {
	manager.mu.RLock()
	target, active := manager.active[workerID]
	failed := manager.status[workerID].State == StateFailed
	prepared := clonePrepared(manager.configured)
	manager.mu.RUnlock()
	if !active || !failed {
		return false, nil
	}
	adapter := manager.adapters[target.Core]
	if adapter == nil {
		return false, fmt.Errorf("worker adapter for %s is required", target.Core)
	}
	manager.transition(target, prepared, StateStarting, "restart", "recovering exited worker")
	if err := adapter.Stop(ctx, workerID); err != nil {
		manager.record(workerID, target.Core, StateFailed, "restart_failed", err.Error())
		return false, err
	}
	if _, err := adapter.Start(ctx, target); err != nil {
		manager.record(workerID, target.Core, StateFailed, "restart_failed", err.Error())
		return false, err
	}
	if err := manager.selectRouteDefaults(ctx, target, prepared); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = adapter.Stop(cleanupCtx, workerID)
		manager.record(workerID, target.Core, StateFailed, "restart_failed", err.Error())
		return false, err
	}
	manager.transition(target, prepared, StateRunning, "restarted", "worker process restored")
	return true, nil
}

func (manager *Manager) SelectPath(ctx context.Context, path BindingPath) error {
	adapter, err := manager.routeAdapter(path.Worker)
	if err != nil {
		return err
	}
	return adapter.SelectRoute(ctx, path)
}

func (manager *Manager) VerifyPath(ctx context.Context, path BindingPath) error {
	adapter, err := manager.routeAdapter(path.Worker)
	if err != nil {
		return err
	}
	return adapter.VerifyRoute(ctx, path)
}

// VerifyPathSelection checks the live selector without probing the upstream
// node; adapters that cannot separate the two fall back to VerifyPath.
func (manager *Manager) VerifyPathSelection(ctx context.Context, path BindingPath) error {
	adapter, err := manager.routeAdapter(path.Worker)
	if err != nil {
		return err
	}
	if verifier, ok := adapter.(RouteSelectionVerifier); ok {
		return verifier.VerifyRouteSelection(ctx, path)
	}
	return adapter.VerifyRoute(ctx, path)
}

func (manager *Manager) DelayPath(ctx context.Context, path BindingPath, testURL string, timeout time.Duration) (DelayResult, error) {
	adapter, err := manager.routeAdapter(path.Worker)
	if err != nil {
		return DelayResult{}, err
	}
	return adapter.DelayRoute(ctx, path, testURL, timeout)
}

func (manager *Manager) ListPathConnections(ctx context.Context, path BindingPath) ([]Connection, error) {
	adapter, err := manager.routeAdapter(path.Worker)
	if err != nil {
		return nil, err
	}
	return adapter.ListRouteConnections(ctx, path)
}

func (manager *Manager) ClosePathConnection(ctx context.Context, path BindingPath, connectionID string) error {
	adapter, err := manager.routeAdapter(path.Worker)
	if err != nil {
		return err
	}
	return adapter.CloseRouteConnection(ctx, path, connectionID)
}

func (manager *Manager) CountPathConnections(ctx context.Context, path BindingPath) (int, error) {
	adapter, err := manager.routeAdapter(path.Worker)
	if err != nil {
		return 0, err
	}
	return adapter.CountRouteConnections(ctx, path)
}

func (manager *Manager) ClosePathConnections(ctx context.Context, path BindingPath) (int, error) {
	adapter, err := manager.routeAdapter(path.Worker)
	if err != nil {
		return 0, err
	}
	return adapter.CloseRouteConnections(ctx, path)
}

func (manager *Manager) UpdateBindings(bindings []state.PoolBinding) {
	manager.mu.Lock()
	manager.configured.Bindings = append([]state.PoolBinding(nil), bindings...)
	manager.configured.ActiveTargets = manager.configured.ActiveTargets[:0]
	for _, target := range activeTargets(manager.configured.Targets, bindings) {
		manager.configured.ActiveTargets = append(manager.configured.ActiveTargets, target.ID)
	}
	prepared := clonePrepared(manager.configured)
	for workerID, target := range manager.active {
		executors, pools := targetMetadata(workerID, prepared)
		status := manager.status[workerID]
		status.Executors = executors
		status.Pools = pools
		manager.status[workerID] = status
		manager.active[workerID] = target
	}
	manager.mu.Unlock()
}

func (manager *Manager) StopUnbound(ctx context.Context) error {
	manager.mu.RLock()
	prepared := clonePrepared(manager.configured)
	desired := make(map[string]struct{}, len(prepared.ActiveTargets))
	for _, workerID := range prepared.ActiveTargets {
		desired[workerID] = struct{}{}
	}
	var stale []state.WorkerTarget
	for workerID, target := range manager.active {
		if _, ok := desired[workerID]; !ok {
			stale = append(stale, target)
		}
	}
	manager.mu.RUnlock()
	for _, target := range stale {
		adapter := manager.adapters[target.Core]
		if adapter == nil {
			return fmt.Errorf("worker adapter for %s is required", target.Core)
		}
		manager.record(target.ID, target.Core, StateDraining, "drain_complete", "worker no longer has bound pools")
		if err := adapter.Stop(ctx, target.ID); err != nil {
			manager.record(target.ID, target.Core, StateFailed, "stop_failed", err.Error())
			return err
		}
		manager.configureStopped(target, prepared)
	}
	return nil
}

func (manager *Manager) IsRunning(workerID string) bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	_, ok := manager.active[workerID]
	return ok
}

func (manager *Manager) IsReady(workerID string) bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	status, ok := manager.status[workerID]
	return ok && status.State == StateRunning
}

func (manager *Manager) cleanupPrepared(prepared Prepared) {
	for _, candidate := range prepared.Candidates {
		if adapter := manager.adapters[candidate.Core]; adapter != nil {
			_ = adapter.RemoveCandidate(candidate)
		}
	}
}

func (manager *Manager) selectRouteDefaults(ctx context.Context, target state.WorkerTarget, prepared Prepared) error {
	adapter, err := manager.routeAdapter(target)
	if err != nil {
		return err
	}
	for _, route := range target.Routes {
		path, err := bindingPath(prepared, target, route, route.Selected)
		if err != nil {
			return err
		}
		if err := adapter.SelectRoute(ctx, path); err != nil {
			return fmt.Errorf("select route %s: %w", route.ID, err)
		}
	}
	return nil
}

func (manager *Manager) routeAdapter(target state.WorkerTarget) (RouteAdapter, error) {
	adapter := manager.adapters[target.Core]
	if adapter == nil {
		return nil, fmt.Errorf("worker adapter for %s is required", target.Core)
	}
	controller, ok := adapter.(RouteAdapter)
	if !ok {
		return nil, fmt.Errorf("worker adapter for %s has no binding-aware route control", target.Core)
	}
	return controller, nil
}

func bindingPath(prepared Prepared, target state.WorkerTarget, route state.WorkerRoute, executorID string) (BindingPath, error) {
	if !containsStringValue(route.Candidates, executorID) {
		return BindingPath{}, fmt.Errorf("executor %s is not a candidate of route %s", executorID, route.ID)
	}
	path := BindingPath{Worker: target, Route: route}
	for _, executor := range prepared.Executors {
		if executor.ID == executorID && executor.Worker == target.ID {
			path.Executor = executor
			break
		}
	}
	if path.Executor.ID == "" {
		return BindingPath{}, fmt.Errorf("route %s executor %s is not installed", route.ID, executorID)
	}
	for _, inbound := range target.Inbounds {
		if inbound.Route != route.ID {
			continue
		}
		switch inbound.Kind {
		case "tproxy":
			path.TProxy = inbound
		case "socks":
			path.DNS = inbound
		}
	}
	if path.TProxy.ID == "" || path.DNS.ID == "" {
		return BindingPath{}, fmt.Errorf("route %s has incomplete inbounds", route.ID)
	}
	return path, nil
}

func containsStringValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (manager *Manager) supervise(ctx context.Context) {
	ticker := time.NewTicker(manager.probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.probe(ctx)
		}
	}
}

func (manager *Manager) probe(ctx context.Context) {
	manager.mu.RLock()
	targets := make([]state.WorkerTarget, 0, len(manager.active))
	for _, target := range manager.active {
		targets = append(targets, target)
	}
	handler := manager.failureHandler
	manager.mu.RUnlock()
	for _, target := range targets {
		adapter := manager.adapters[target.Core]
		if adapter == nil {
			continue
		}
		manager.mu.RLock()
		before := manager.status[target.ID]
		manager.mu.RUnlock()
		if before.State != StateRunning {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, manager.probeInterval)
		err := adapter.Probe(probeCtx, target.ID)
		cancel()
		if err == nil {
			continue
		}
		manager.mu.Lock()
		status := manager.status[target.ID]
		// A probe can finish after an apply, stop or restart. Discard that
		// stale result instead of failing the replacement process.
		if status.State != StateRunning || status.LastEvent != before.LastEvent {
			manager.mu.Unlock()
			continue
		}
		status.State = StateFailed
		manager.status[target.ID] = status
		manager.appendEventLocked(target.ID, target.Core, StateFailed, "exited", err.Error())
		manager.mu.Unlock()
		if handler != nil {
			handler(ctx, Failure{WorkerID: target.ID, Core: target.Core, Pools: append([]string(nil), status.Pools...), Err: err})
		}
	}
}

func (manager *Manager) transition(target state.WorkerTarget, prepared Prepared, nextState, kind, message string) {
	executors, pools := targetMetadata(target.ID, prepared)
	manager.mu.Lock()
	manager.active[target.ID] = target
	status := manager.status[target.ID]
	status.ID = target.ID
	status.Core = target.Core
	status.Version = target.Version
	status.State = nextState
	status.Executors = executors
	status.Pools = pools
	manager.status[target.ID] = status
	manager.appendEventLocked(target.ID, target.Core, nextState, kind, message)
	manager.mu.Unlock()
}

func (manager *Manager) configureStopped(target state.WorkerTarget, prepared Prepared) {
	executors, pools := targetMetadata(target.ID, prepared)
	manager.mu.Lock()
	delete(manager.active, target.ID)
	status := manager.status[target.ID]
	status.ID = target.ID
	status.Core = target.Core
	status.Version = target.Version
	status.State = StateStopped
	status.Executors = executors
	status.Pools = pools
	manager.status[target.ID] = status
	manager.appendEventLocked(target.ID, target.Core, StateStopped, "configured", "worker configuration installed")
	manager.mu.Unlock()
}

func targetMetadata(workerID string, prepared Prepared) ([]state.NodeExecutor, []string) {
	executors := make([]state.NodeExecutor, 0)
	for _, executor := range prepared.Executors {
		if executor.Worker == workerID {
			executors = append(executors, executor)
		}
	}
	pools := make([]string, 0)
	for _, binding := range prepared.Bindings {
		if binding.Target.Worker == workerID {
			pools = append(pools, binding.Pool)
		}
	}
	sort.Strings(pools)
	return executors, pools
}

func (manager *Manager) record(workerID string, core smartnode.CoreID, nextState, kind, message string) {
	manager.mu.Lock()
	status := manager.status[workerID]
	status.ID = workerID
	status.Core = core
	status.State = nextState
	manager.status[workerID] = status
	manager.appendEventLocked(workerID, core, nextState, kind, message)
	manager.mu.Unlock()
}

func (manager *Manager) markStopped(target state.WorkerTarget) {
	manager.mu.Lock()
	delete(manager.active, target.ID)
	status := manager.status[target.ID]
	status.ID = target.ID
	status.Core = target.Core
	status.State = StateStopped
	status.Executors = []state.NodeExecutor{}
	status.Pools = []string{}
	manager.status[target.ID] = status
	manager.appendEventLocked(target.ID, target.Core, StateStopped, "stopped", "worker stopped")
	manager.mu.Unlock()
}

func (manager *Manager) markRestored(workerID string, core smartnode.CoreID, checkpoint Checkpoint) {
	manager.mu.Lock()
	if target, ok := checkpoint.Targets[workerID]; ok {
		manager.active[workerID] = target
	}
	status := cloneStatus(checkpoint.Statuses[workerID])
	status.ID = workerID
	status.Core = core
	status.State = StateRunning
	manager.status[workerID] = status
	manager.appendEventLocked(workerID, core, StateRunning, "restored", "worker restored")
	manager.mu.Unlock()
}

func cloneStatus(status Status) Status {
	status.Executors = append([]state.NodeExecutor{}, status.Executors...)
	status.Pools = append([]string{}, status.Pools...)
	if status.LastEvent != nil {
		last := *status.LastEvent
		status.LastEvent = &last
	}
	return status
}

func (manager *Manager) appendEventLocked(workerID string, core smartnode.CoreID, nextState, kind, message string) {
	event := Event{At: time.Now().UTC(), Worker: workerID, Kind: kind, State: nextState, Message: message}
	events := append(manager.events[workerID], event)
	if len(events) > manager.eventLimit {
		events = append([]Event(nil), events[len(events)-manager.eventLimit:]...)
	}
	manager.events[workerID] = events
	status := manager.status[workerID]
	status.Core = core
	copyEvent := event
	status.LastEvent = &copyEvent
	manager.status[workerID] = status
}

func activeTargets(workers []state.WorkerTarget, bindings []state.PoolBinding) []state.WorkerTarget {
	bound := map[string]struct{}{}
	for _, binding := range bindings {
		if binding.Target.Kind == "node" && binding.Target.Worker != "" {
			bound[binding.Target.Worker] = struct{}{}
		}
	}
	targets := make([]state.WorkerTarget, 0, len(bound))
	for _, target := range workers {
		if _, ok := bound[target.ID]; ok {
			targets = append(targets, target)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return targets
}

func compileRequest(target state.WorkerTarget, graph state.ExecutionGraph, identities map[string]smartnode.IdentitySource) (CompileRequest, error) {
	request := CompileRequest{Worker: target, Identities: map[string]smartnode.IdentitySource{}, Generation: time.Now().UTC()}
	for _, executor := range graph.NodeExecutors {
		if executor.Worker != target.ID {
			continue
		}
		source, exists := identities[executor.SmartNode]
		if !exists {
			return CompileRequest{}, fmt.Errorf("worker %s executor %s has no protected identity input", target.ID, executor.ID)
		}
		request.Executors = append(request.Executors, executor)
		request.Identities[executor.SmartNode] = cloneIdentitySource(source)
	}
	for _, binding := range graph.PoolBindings {
		if binding.Target.Worker == target.ID {
			request.Bindings = append(request.Bindings, binding)
		}
	}
	if len(target.Routes) == 0 {
		return CompileRequest{}, fmt.Errorf("worker %s has no prepared routes", target.ID)
	}
	return request, nil
}

func clonePrepared(prepared Prepared) Prepared {
	next := prepared
	next.Targets = append([]state.WorkerTarget(nil), prepared.Targets...)
	next.ActiveTargets = append([]string(nil), prepared.ActiveTargets...)
	next.Executors = append([]state.NodeExecutor(nil), prepared.Executors...)
	next.Bindings = append([]state.PoolBinding(nil), prepared.Bindings...)
	if prepared.Candidates != nil {
		next.Candidates = make(map[string]Candidate, len(prepared.Candidates))
		for workerID, candidate := range prepared.Candidates {
			next.Candidates[workerID] = candidate
		}
	}
	return next
}

func cloneIdentitySource(source smartnode.IdentitySource) smartnode.IdentitySource {
	cloned := source
	cloned.Identity.Security.ALPN = append([]string(nil), source.Identity.Security.ALPN...)
	if source.Identity.Extra != nil {
		cloned.Identity.Extra = make(map[string]string, len(source.Identity.Extra))
		for key, value := range source.Identity.Extra {
			cloned.Identity.Extra[key] = value
		}
	}
	if source.References != nil {
		cloned.References = make(map[string]json.RawMessage, len(source.References))
		for key, value := range source.References {
			cloned.References[key] = append(json.RawMessage(nil), value...)
		}
	}
	return cloned
}

func (manager *Manager) workerID(core smartnode.CoreID) (string, error) {
	adapter, exists := manager.adapters[core]
	if !exists {
		return "", fmt.Errorf("worker adapter for %s is required", core)
	}
	return adapter.Descriptor().WorkerID(1), nil
}
