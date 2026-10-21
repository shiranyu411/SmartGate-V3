package scheduler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type blockingDelayProbe struct {
	mu           sync.Mutex
	active, peak int
	started      chan struct{}
	release      chan struct{}
}

func (p *blockingDelayProbe) Delay(ctx context.Context, _, _ string, _ time.Duration) (DelayResult, error) {
	p.mu.Lock()
	p.active++
	if p.active > p.peak {
		p.peak = p.active
	}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.active--; p.mu.Unlock() }()
	p.started <- struct{}{}
	select {
	case <-ctx.Done():
		return DelayResult{}, ctx.Err()
	case <-p.release:
		return DelayResult{DelayMS: 25}, nil
	}
}

func TestDelayParallelBoundAndCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[cancelRun], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			p := &blockingDelayProbe{started: make(chan struct{}, 32), release: make(chan struct{})}
			s := &fakeStore{}
			r := &Runner{runtimeStore: s, delayTester: p}
			nodes := make([]provider.NodeView, 0, delayTestConcurrency+2)
			for i := 0; i < delayTestConcurrency+1; i++ {
				nodes = append(nodes, provider.NodeView{ID: fmt.Sprintf("node-%d", i)})
			}
			nodes = append(nodes, provider.NodeView{ID: "excluded", Excluded: true})
			done := make(chan struct{})
			var result DelayTestResult
			var err error
			go func() { result, err = r.measureNodes(ctx, nodes, nil); close(done) }()
			for i := 0; i < delayTestConcurrency; i++ {
				select {
				case <-p.started:
				case <-ctx.Done():
					t.Fatal("probes did not reach the concurrency limit")
				}
			}
			select {
			case <-p.started:
				t.Fatal("exceeded the concurrency limit")
			case <-time.After(200 * time.Millisecond):
			}
			if cancelRun {
				cancel()
			} else {
				close(p.release)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("workers did not finish")
			}
			if p.peak != delayTestConcurrency {
				t.Fatalf("peak=%d", p.peak)
			}
			if cancelRun {
				if err != context.Canceled || result.Tested != 0 || len(s.runtime.NodeHealth) != 0 {
					t.Fatalf("cancellation recorded failures: %+v %v", result, err)
				}
			} else {
				if err != nil || result.Tested != delayTestConcurrency+1 || result.OK != delayTestConcurrency+1 || len(s.runtime.NodeHealth) != delayTestConcurrency+1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			}
		})
	}
}

func TestDelayRoundDeadlineMarksUnknownWithAppliedContext(t *testing.T) {
	probe := &blockingDelayProbe{started: make(chan struct{}, 32), release: make(chan struct{})}
	store := &fakeStore{}
	runner := &Runner{runtimeStore: store, delayTester: probe}
	nodes := []provider.NodeView{{ID: "first"}, {ID: "second"}, {ID: "third"}, {ID: "fourth"}}
	contexts := map[string]state.NodeHealthContext{}
	for _, node := range nodes {
		contexts[node.ID] = state.NodeHealthContext{Revision: 7, Core: "core-a", Method: "unknown"}
	}
	roundCtx, cancel := context.WithTimeout(context.Background(), 175*time.Millisecond)
	defer cancel()
	result, err := runner.measureNodesInRound(context.Background(), roundCtx, nodes, contexts, probe)
	if err != nil || result.Unknown != 4 || result.Failed != 0 || result.Tested != 0 {
		t.Fatalf("deadline fabricated failures: %+v %v", result, err)
	}
	for _, node := range nodes {
		health := store.runtime.NodeHealth[node.ID]
		if health.Status != "unknown" || health.Context.Revision != 7 {
			t.Fatalf("unknown lost applied identity: %+v", health)
		}
	}
}
