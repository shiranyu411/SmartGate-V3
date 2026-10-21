package provider

import (
	"testing"

	"smartgate/internal/state"
)

func TestApplyChangesUpsertDelete(t *testing.T) {
	spec := state.DefaultSpecState()
	next, err := ApplyChanges(spec, []Change{
		{
			Action: ActionUpsert,
			Provider: state.ProviderSpec{
				ID:        "p1",
				Name:      "Provider 1",
				Type:      TypeManual,
				Enabled:   true,
				Priority:  10,
				Roles:     []string{"general", "ai"},
				FetchMode: FetchDirect,
			},
		},
	})
	if err != nil {
		t.Fatalf("apply upsert: %v", err)
	}
	if len(next.Providers) != 1 || next.Providers[0].ID != "p1" {
		t.Fatalf("unexpected providers after upsert: %+v", next.Providers)
	}

	next, err = ApplyChanges(next, []Change{{Action: ActionDelete, ID: "p1"}})
	if err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	if len(next.Providers) != 0 {
		t.Fatalf("expected provider delete, got %+v", next.Providers)
	}
}

func TestValidateSpecProvidersRejectsBootstrapCycle(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "a", Name: "A", Type: TypeSubscription, URL: "https://a.example", Enabled: true, FetchMode: FetchViaBootstrap, Bootstrap: "b"},
		{ID: "b", Name: "B", Type: TypeSubscription, URL: "https://b.example", Enabled: true, FetchMode: FetchViaBootstrap, Bootstrap: "a"},
	}
	if _, err := ValidateSpecProviders(spec); err == nil {
		t.Fatal("expected bootstrap cycle to fail validation")
	}
}

func TestValidateSpecProvidersRejectsUnsafeProviderID(t *testing.T) {
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "../escape", Name: "Bad", Type: TypeManual, Enabled: true},
	}
	if _, err := ValidateSpecProviders(spec); err == nil {
		t.Fatal("expected unsafe provider id to fail validation")
	}
}

func TestDiffDetectsOrderChange(t *testing.T) {
	oldSpec := state.DefaultSpecState()
	oldSpec.Providers = []state.ProviderSpec{
		{ID: "a", Name: "A", Type: TypeManual, Enabled: true},
		{ID: "b", Name: "B", Type: TypeManual, Enabled: true},
	}
	newSpec := state.DefaultSpecState()
	newSpec.Providers = []state.ProviderSpec{
		{ID: "b", Name: "B", Type: TypeManual, Enabled: true},
		{ID: "a", Name: "A", Type: TypeManual, Enabled: true},
	}
	plan, err := Diff(oldSpec, newSpec)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !plan.OrderChanged || !plan.HasChanges {
		t.Fatalf("expected order change plan, got %+v", plan)
	}
}
