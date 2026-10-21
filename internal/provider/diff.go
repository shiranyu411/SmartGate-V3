package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"smartgate/internal/state"
)

type Plan struct {
	Added        []string `json:"added,omitempty"`
	Updated      []string `json:"updated,omitempty"`
	Deleted      []string `json:"deleted,omitempty"`
	OrderChanged bool     `json:"order_changed,omitempty"`
	TopologyHash string   `json:"topology_hash"`
	HasChanges   bool     `json:"has_changes"`
}

func Diff(oldSpec, newSpec state.SpecState) (Plan, error) {
	oldMap := mapProviders(oldSpec.Providers)
	newMap := mapProviders(newSpec.Providers)
	plan := Plan{TopologyHash: HashSpec(newSpec)}

	for id, np := range newMap {
		op, ok := oldMap[id]
		if !ok {
			plan.Added = append(plan.Added, id)
			continue
		}
		if !providersEqual(op, np) {
			plan.Updated = append(plan.Updated, id)
		}
	}
	for id := range oldMap {
		if _, ok := newMap[id]; !ok {
			plan.Deleted = append(plan.Deleted, id)
		}
	}
	plan.OrderChanged = !sameProviderOrder(oldSpec.Providers, newSpec.Providers)
	plan.HasChanges = len(plan.Added) > 0 || len(plan.Updated) > 0 || len(plan.Deleted) > 0 || plan.OrderChanged
	return plan, nil
}

func HashSpec(spec state.SpecState) string {
	b, _ := json.Marshal(spec)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mapProviders(list []state.ProviderSpec) map[string]state.ProviderSpec {
	out := make(map[string]state.ProviderSpec, len(list))
	for _, p := range list {
		out[p.ID] = p
	}
	return out
}

func providersEqual(a, b state.ProviderSpec) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func sameProviderOrder(a, b []state.ProviderSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}
