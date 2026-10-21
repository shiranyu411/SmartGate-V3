package fakecore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type Adapter struct {
	Runtime *worker.FileRuntime
}

func (adapter *Adapter) Core() smartnode.CoreID { return CoreID }

func (adapter *Adapter) Prepare(ctx context.Context, opID string, request worker.CompileRequest) (worker.Candidate, error) {
	if adapter.Runtime == nil {
		return worker.Candidate{}, fmt.Errorf("FakeCore worker runtime is required")
	}
	config, err := BuildConfig(request)
	if err != nil {
		return worker.Candidate{}, err
	}
	adapter.Runtime.Core = CoreID
	return adapter.Runtime.Prepare(ctx, opID, request.Worker.ID, worker.CandidateConfig{
		Value: config, ReadyPorts: readyPorts(request.Worker),
	})
}

func (adapter *Adapter) Verify(candidate worker.Candidate) error {
	if adapter.Runtime == nil {
		return fmt.Errorf("FakeCore worker runtime is required")
	}
	return adapter.Runtime.Verify(candidate)
}

func (adapter *Adapter) Install(ctx context.Context, candidate worker.Candidate) (worker.Activation, error) {
	return adapter.Runtime.Install(ctx, candidate)
}

func (adapter *Adapter) Activate(ctx context.Context, candidate worker.Candidate) (worker.Activation, error) {
	return adapter.Runtime.Activate(ctx, candidate)
}

func (adapter *Adapter) Start(ctx context.Context, target state.WorkerTarget) (worker.Activation, error) {
	return adapter.Runtime.StartInstalled(ctx, target.ID, readyPorts(target))
}

func (adapter *Adapter) Backup(opID, workerID string) (worker.Backup, error) {
	return adapter.Runtime.Backup(opID, workerID)
}

func (adapter *Adapter) Restore(ctx context.Context, backup worker.Backup) (worker.Activation, error) {
	return adapter.Runtime.Restore(ctx, backup)
}

func (adapter *Adapter) Stop(ctx context.Context, workerID string) error {
	return adapter.Runtime.Stop(ctx, workerID)
}

func (adapter *Adapter) Probe(ctx context.Context, workerID string) error {
	return adapter.Runtime.Probe(ctx, workerID)
}

func (adapter *Adapter) RemoveCandidate(candidate worker.Candidate) error {
	return adapter.Runtime.RemoveCandidate(candidate)
}

func (adapter *Adapter) SelectRoute(_ context.Context, path worker.BindingPath) error {
	return validateBindingPath(path)
}

func (adapter *Adapter) VerifyRoute(_ context.Context, path worker.BindingPath) error {
	return validateBindingPath(path)
}

func (adapter *Adapter) VerifyRouteSelection(_ context.Context, path worker.BindingPath) error {
	return validateBindingPath(path)
}

func (adapter *Adapter) DelayRoute(_ context.Context, _ worker.BindingPath, _ string, _ time.Duration) (worker.DelayResult, error) {
	return worker.DelayResult{}, worker.ErrRouteDelayUnsupported
}

func (adapter *Adapter) CountRouteConnections(context.Context, worker.BindingPath) (int, error) {
	return 0, worker.ErrConnectionCountUnsupported
}

func (adapter *Adapter) ListRouteConnections(context.Context, worker.BindingPath) ([]worker.Connection, error) {
	return nil, worker.ErrConnectionObservationUnsupported
}

func (adapter *Adapter) CloseRouteConnection(context.Context, worker.BindingPath, string) error {
	return worker.ErrConnectionObservationUnsupported
}

func (adapter *Adapter) CloseRouteConnections(context.Context, worker.BindingPath) (int, error) {
	return 0, worker.ErrConnectionObservationUnsupported
}

func validateBindingPath(path worker.BindingPath) error {
	if path.Worker.Core != CoreID {
		return fmt.Errorf("worker %s is not a FakeCore target", path.Worker.ID)
	}
	if path.Route.ID == "" || path.Route.Selected == "" {
		return fmt.Errorf("FakeCore route is incomplete")
	}
	if path.Executor.ID != path.Route.Selected {
		return fmt.Errorf("FakeCore route %s selected %s, expected %s", path.Route.ID, path.Executor.ID, path.Route.Selected)
	}
	if !containsString(path.Route.Candidates, path.Executor.ID) {
		return fmt.Errorf("FakeCore route %s selected executor is not a candidate", path.Route.ID)
	}
	if path.TProxy.ID == "" || path.DNS.ID == "" {
		return fmt.Errorf("FakeCore route %s has incomplete inbounds", path.Route.ID)
	}
	return nil
}

func readyPorts(target state.WorkerTarget) []string {
	ports := []string{fmt.Sprintf("%s:%d", target.Control.Listen, target.Control.Port)}
	for _, inbound := range target.Inbounds {
		if inbound.Kind == "socks" {
			ports = append(ports, fmt.Sprintf("%s:%d", inbound.Listen, inbound.Port))
		}
	}
	return ports
}

func sortedExecutors(executors []state.NodeExecutor) []state.NodeExecutor {
	out := append([]state.NodeExecutor(nil), executors...)
	sort.Slice(out, func(left, right int) bool { return out[left].ID < out[right].ID })
	return out
}

func sortedRoutes(routes []state.WorkerRoute) []state.WorkerRoute {
	out := append([]state.WorkerRoute(nil), routes...)
	sort.Slice(out, func(left, right int) bool { return out[left].ID < out[right].ID })
	return out
}

func sortedInbounds(inbounds []state.WorkerInbound) []state.WorkerInbound {
	out := append([]state.WorkerInbound(nil), inbounds...)
	sort.Slice(out, func(left, right int) bool { return out[left].ID < out[right].ID })
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
