package worker

import (
	"context"
	"errors"
	"smartgate/internal/smartnode"
	"time"
)

var ErrNodeProbeUnavailable = errors.New("node probe could not start")

// Manual and full-node probes share one slot to bound temporary core memory.
var nodeProbeSlot = make(chan struct{}, 1)

type ConfigCompiler interface {
	Compile(CompileRequest) (CandidateConfig, error)
}

func (manager *Manager) DelayNode(ctx context.Context, node smartnode.SmartNode, source smartnode.IdentitySource, testURL string, timeout time.Duration) (DelayResult, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if testURL == "" {
		testURL = DefaultRouteProbeURL
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout+40*time.Second)
	defer cancel()
	batch, err := manager.StartNodeProbeBatch(probeCtx, []NodeProbeTarget{{Node: node, Source: source}})
	if err != nil {
		return DelayResult{URL: testURL}, err
	}
	defer batch.Close()
	return batch.Delay(probeCtx, node.ID, testURL, timeout)
}
