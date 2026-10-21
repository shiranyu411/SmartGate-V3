package failover

import (
	"testing"
	"time"
)

func TestDecideUsesConfiguredBlockFallbackRegardlessOfRole(t *testing.T) {
	decision := Decide(PoolHealth{
		PoolID:      "custom",
		Role:        "general",
		NoCandidate: "block",
		Current:     "node-1",
		Candidates:  []string{"node-1", "node-2"},
	}, time.Unix(1, 0).UTC())
	if decision.Kind != KindFailClosed || decision.To != "Block" || decision.Reason != "no_healthy_candidate_block" {
		t.Fatalf("expected policy-driven fail-closed decision, got %+v", decision)
	}
}

func TestDecideHoldsConfiguredPoolWithoutHealthyCandidate(t *testing.T) {
	decision := Decide(PoolHealth{
		PoolID:      "custom",
		Role:        "ai",
		NoCandidate: "hold",
		Current:     "node-1",
		Candidates:  []string{"node-1", "node-2"},
	}, time.Unix(1, 0).UTC())
	if decision.Kind != KindNoop || decision.To != "node-1" || decision.Reason != "no_healthy_candidate_hold" {
		t.Fatalf("expected policy-driven hold decision, got %+v", decision)
	}
}

func TestDecideSwitchesGeneralPoolToNextHealthyCandidate(t *testing.T) {
	decision := Decide(PoolHealth{
		PoolID:      "general",
		Role:        "general",
		NoCandidate: "direct",
		Current:     "node-1",
		Candidates:  []string{"node-1", "node-2", "node-3"},
		Healthy:     []string{"node-2", "node-3"},
	}, time.Unix(1, 0).UTC())
	if decision.Kind != KindSwitch || decision.From != "node-1" || decision.To != "node-2" {
		t.Fatalf("expected switch to node-2, got %+v", decision)
	}
}

func TestDecideNoopWhenCurrentHealthy(t *testing.T) {
	decision := Decide(PoolHealth{
		PoolID:      "streaming",
		Role:        "streaming",
		NoCandidate: "direct",
		Current:     "node-1",
		Candidates:  []string{"node-1", "node-2"},
		Healthy:     []string{"node-1", "node-2"},
	}, time.Unix(1, 0).UTC())
	if decision.Kind != KindNoop || decision.To != "node-1" {
		t.Fatalf("expected noop, got %+v", decision)
	}
}

func TestDecideMarksRecoveryFromFallback(t *testing.T) {
	for _, current := range []string{"Block", "Direct"} {
		t.Run(current, func(t *testing.T) {
			decision := Decide(PoolHealth{
				PoolID:      "ai",
				Role:        "ai",
				NoCandidate: "block",
				Current:     current,
				Candidates:  []string{"node-1", "Block"},
				Healthy:     []string{"node-1"},
			}, time.Unix(1, 0).UTC())
			if decision.Kind != KindRecovery || decision.From != current || decision.To != "node-1" {
				t.Fatalf("expected recovery from %s, got %+v", current, decision)
			}
		})
	}
}
