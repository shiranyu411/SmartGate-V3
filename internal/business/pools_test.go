package business

import (
	"testing"

	"smartgate/internal/state"
)

func TestApplyChangesUpsertDeletePool(t *testing.T) {
	spec := state.DefaultSpecState()
	next, err := ApplyChanges(spec, []Change{{
		Action: ActionUpsert,
		Pool: state.BusinessPoolSpec{
			ID:      "ai",
			Name:    "AI",
			Role:    "AI",
			NodeIDs: []string{" n1 ", "n1", "n2"},
		},
	}})
	if err != nil {
		t.Fatalf("apply upsert: %v", err)
	}
	if len(next.BusinessPools) != 5 || next.BusinessPools[1].Role != "ai" {
		t.Fatalf("unexpected pool after upsert: %+v", next.BusinessPools)
	}
	if got := next.BusinessPools[1].NodeIDs; len(got) != 2 || got[0] != "n1" || got[1] != "n2" {
		t.Fatalf("node ids should be trimmed and deduplicated, got %+v", got)
	}

	next, err = ApplyChanges(next, []Change{{Action: ActionDelete, ID: "ai"}})
	if err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	if len(next.BusinessPools) != 4 {
		t.Fatalf("expected pool delete, got %+v", next.BusinessPools)
	}
}

func TestDiffDetectsBusinessPoolOrderChange(t *testing.T) {
	oldSpec := state.DefaultSpecState()
	oldSpec.BusinessPools = []state.BusinessPoolSpec{{
		ID:      "ai",
		Name:    "AI",
		Role:    "ai",
		NodeIDs: []string{"n1", "n2", "n3"},
	}}
	newSpec := state.DefaultSpecState()
	newSpec.BusinessPools = []state.BusinessPoolSpec{{
		ID:      "ai",
		Name:    "AI",
		Role:    "ai",
		NodeIDs: []string{"n2", "n1", "n3"},
	}}

	plan, err := Diff(oldSpec, newSpec)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !plan.HasChanges || len(plan.OrderChanged) != 1 || plan.OrderChanged[0] != "ai" {
		t.Fatalf("expected order change, got %+v", plan)
	}
}

func TestValidatePoolsRejectsUnsafeOrDuplicateIDs(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.BusinessPools = []state.BusinessPoolSpec{{ID: "../escape", Name: "Bad", Role: "ai"}}
	if _, err := ValidatePools(spec); err == nil {
		t.Fatal("expected unsafe id to fail")
	}

	spec.BusinessPools = []state.BusinessPoolSpec{
		{ID: "ai", Name: "AI 1", Role: "ai"},
		{ID: "ai", Name: "AI 2", Role: "ai"},
	}
	if _, err := ValidatePools(spec); err == nil {
		t.Fatal("expected duplicate id to fail")
	}
}
