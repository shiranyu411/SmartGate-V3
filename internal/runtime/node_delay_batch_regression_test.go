package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"smartgate/internal/coredesc"
	"smartgate/internal/provider"
	"smartgate/internal/scheduler"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

var errProbeCompileReached = errors.New("probe compiler reached")

type recordingBatchProbeAdapter struct {
	runtimeTestWorkerAdapter
	requests []worker.CompileRequest
}

func (a *recordingBatchProbeAdapter) Descriptor() coredesc.Descriptor {
	d := a.runtimeTestWorkerAdapter.Descriptor()
	d.Process.Binary = "not-started"
	d.Process.StartArgs = func(p string) []string { return []string{p} }
	d.Process.CheckArgs = func(p string) []string { return []string{p} }
	return d
}
func (a *recordingBatchProbeAdapter) Compile(r worker.CompileRequest) (worker.CandidateConfig, error) {
	a.requests = append(a.requests, r)
	return worker.CandidateConfig{}, errProbeCompileReached
}
func TestBatchDelayKeepsSupportedUnboundNodesWithUnavailablePeers(t *testing.T) {
	for _, peerCore := range []smartnode.CoreID{"", runtimeCoreB} {
		t.Run(string(peerCore), func(t *testing.T) {
			adapter := &recordingBatchProbeAdapter{runtimeTestWorkerAdapter: runtimeTestWorkerAdapter{core: runtimeCoreA}}
			manager, err := worker.NewManager(adapter)
			if err != nil {
				t.Fatal(err)
			}
			tester := schedulerNodeDelayTester{api: nodesAPI{workers: manager}}
			topology := state.AppliedTopology{Spec: state.SpecState{SmartNodes: map[string]smartnode.SmartNode{
				"unavailable": {ID: "unavailable", Revision: 1, CoreChoice: peerCore},
				"good":        {ID: "good", Revision: 1, CoreChoice: runtimeCoreA},
			}}}
			_, _, err = tester.Begin(context.Background(), topology, []provider.NodeView{{ID: "unavailable"}, {ID: "good"}})
			if err == nil || !strings.Contains(err.Error(), errProbeCompileReached.Error()) {
				t.Fatalf("supported node was never independently probed: %v", err)
			}
			if len(adapter.requests) != 1 || len(adapter.requests[0].Executors) != 1 || adapter.requests[0].Executors[0].SmartNode != "good" {
				t.Fatalf("unavailable peer polluted supported batch: %+v", adapter.requests)
			}
		})
	}
}
func TestBatchDelayUnavailableNodeDoesNotReportUpstreamFailure(t *testing.T) {
	manager, err := worker.NewManager(runtimeTestWorkerAdapter{core: runtimeCoreA})
	if err != nil {
		t.Fatal(err)
	}
	tester := schedulerNodeDelayTester{api: nodesAPI{workers: manager}}
	topology := state.AppliedTopology{Spec: state.SpecState{SmartNodes: map[string]smartnode.SmartNode{
		"unbound": {ID: "unbound", Revision: 2, CoreChoice: runtimeCoreA},
	}}}
	batch, closeBatch, err := tester.Begin(context.Background(), topology, []provider.NodeView{{ID: "unbound"}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBatch()
	result, err := batch.Delay(context.Background(), "unbound", "http://example.com", time.Second)
	if !errors.Is(err, scheduler.ErrDelayUnavailable) || result.Context.Revision != 2 {
		t.Fatalf("unavailable setup reported as node failure: %+v %v", result, err)
	}
}
