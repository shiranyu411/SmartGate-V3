package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/failover"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type workerFailureController struct {
	store       *state.Store
	layout      state.Layout
	selectorTxn *commitlane.SelectorTxn
}

func newWorkerFailureController(cfg Config) *workerFailureController {
	return &workerFailureController{store: cfg.Store, layout: cfg.Layout, selectorTxn: cfg.SelectorTxn}
}

func (controller *workerFailureController) Handle(ctx context.Context, failure worker.Failure) error {
	if controller == nil || controller.store == nil || controller.selectorTxn == nil {
		return fmt.Errorf("worker failure controller is not configured")
	}
	if failure.WorkerID == "" || len(failure.Pools) == 0 {
		return nil
	}
	topology, err := controller.store.LoadAppliedTopology()
	if err != nil {
		return err
	}
	runtimeState, err := controller.store.LoadRuntime()
	if err != nil {
		return err
	}
	failoverStore := state.NewFailoverStateStore(controller.layout)
	failoverState, err := failoverStore.Load()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	groups, err := buildProxyGroupViews(topology, runtimeState, failoverState, now)
	if err != nil {
		return err
	}
	graph, err := rebuildLiveExecutionGraph(topology, now)
	if err != nil {
		return err
	}
	groupByPool := make(map[string]proxyGroupView, len(groups))
	for _, group := range groups {
		groupByPool[group.Pool] = group
	}
	var failures []error
	for index, poolID := range uniqueFailurePools(failure.Pools) {
		group, ok := groupByPool[poolID]
		if !ok || group.Binding.Target.Worker != failure.WorkerID {
			continue
		}
		if !group.Policy.AutoFailover {
			controller.appendWorkerFailureEvent(group, "worker_failed_auto_failover_disabled", "skipped", failure)
			continue
		}
		target := healthyWorkerFailureTarget(group, graph, runtimeState.NodeHealth, failure.WorkerID, now)
		reason := "worker_failed_switch_candidate"
		kind := failover.KindSwitch
		if target == "" {
			switch group.Policy.NoCandidate {
			case state.NoCandidateBlock:
				target = "Block"
				kind = failover.KindFailClosed
				reason = "worker_failed_no_candidate_block"
			case state.NoCandidateDirect:
				target = "Direct"
				reason = "worker_failed_no_candidate_direct"
			case state.NoCandidateHold:
				controller.appendWorkerFailureEvent(group, "worker_failed_no_candidate_hold", "held", failure)
				continue
			default:
				controller.appendWorkerFailureEvent(group, "worker_failed_invalid_no_candidate_policy", "failed", failure)
				failures = append(failures, fmt.Errorf("pool %s has invalid no_candidate policy %q", poolID, group.Policy.NoCandidate))
				continue
			}
		}
		decision := failover.Decision{
			PoolID: poolID, Role: group.Role, From: group.Selected, To: target,
			Kind: kind, Reason: reason, Time: now,
		}
		targetPool := buildFailoverPoolState(decision, failoverState)
		result, cutoverErr := controller.selectorTxn.Cutover(ctx, commitlane.CutoverRequest{
			OpID: fmt.Sprintf("worker-failure-%d-%d", now.UnixNano(), index), PoolID: poolID,
			TargetSmartNode: target, Source: cutoverSourceForBinding(group.Binding),
			TargetPool: targetPool, ExpectedConfigEpoch: topology.ConfigEpoch,
		})
		if cutoverErr != nil {
			stage, _, _ := commitlaneCutoverErrorDetails(cutoverErr)
			if stage == "" {
				stage = "cutover_transaction"
			}
			controller.appendWorkerFailureCutoverFailure(group, decision, stage, failure)
			failures = append(failures, fmt.Errorf("pool %s worker failure cutover: %w", poolID, cutoverErr))
			continue
		}
		failoverState = state.NormalizeFailoverState(failoverState)
		failoverState.Pools[poolID] = targetPool
		events := failoverEventsFromCutovers([]failover.Decision{decision}, []commitlane.CutoverResult{result}, "worker_supervisor", "applied", "")
		if _, appendErr := state.NewFailoverEventStore(controller.layout).Append(events); appendErr != nil {
			failures = append(failures, fmt.Errorf("pool %s append worker failure event: %w", poolID, appendErr))
		}
	}
	return errors.Join(failures...)
}

func healthyWorkerFailureTarget(group proxyGroupView, graph state.ExecutionGraph, health map[string]state.NodeHealth, failedWorker string, now time.Time) string {
	executors := make(map[string]state.NodeExecutor, len(graph.NodeExecutors))
	for _, executor := range graph.NodeExecutors {
		executors[executor.SmartNode] = executor
	}
	fresh := freshNodeHealth(health, now, failoverHealthTTL)
	for _, candidate := range group.Candidates {
		if !isProbeableNode(candidate) {
			continue
		}
		executor, ok := executors[candidate]
		if !ok || executor.Worker == failedWorker {
			continue
		}
		if fresh[candidate].Status == "ok" {
			return candidate
		}
	}
	return ""
}

func uniqueFailurePools(pools []string) []string {
	seen := make(map[string]struct{}, len(pools))
	out := make([]string, 0, len(pools))
	for _, pool := range pools {
		if pool == "" {
			continue
		}
		if _, exists := seen[pool]; exists {
			continue
		}
		seen[pool] = struct{}{}
		out = append(out, pool)
	}
	return out
}

func (controller *workerFailureController) appendWorkerFailureEvent(group proxyGroupView, reason, result string, failure worker.Failure) {
	event := state.FailoverEvent{
		Time: time.Now().UTC(), PoolID: group.Pool, Role: group.Role,
		From: group.Selected, To: group.Selected, GenerationFrom: group.Binding.Generation,
		GenerationTo: group.Binding.Generation, Executor: group.Binding.Target.Executor,
		CutoverResult: result, Kind: string(failover.KindNoop), Reason: reason,
		Source: "worker_supervisor", Status: result,
	}
	if failure.Err != nil {
		event.Error = "worker_unavailable"
	}
	_, _ = state.NewFailoverEventStore(controller.layout).Append([]state.FailoverEvent{event})
}

func (controller *workerFailureController) appendWorkerFailureCutoverFailure(group proxyGroupView, decision failover.Decision, stage string, failure worker.Failure) {
	event := state.FailoverEvent{
		Time: decision.Time, PoolID: group.Pool, Role: group.Role,
		From: decision.From, To: decision.To, GenerationFrom: group.Binding.Generation,
		Executor: group.Binding.Target.Executor, CutoverResult: "failed", Kind: string(decision.Kind),
		Reason: decision.Reason, Source: "worker_supervisor", Status: "failed", Error: stage,
	}
	if failure.Err == nil {
		event.Error = stage
	}
	_, _ = state.NewFailoverEventStore(controller.layout).Append([]state.FailoverEvent{event})
}
