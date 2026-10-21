package provider

import (
	"fmt"

	"smartgate/internal/state"
)

const (
	ActionUpsert = "upsert"
	ActionDelete = "delete"
)

type Change struct {
	Action   string             `json:"action"`
	ID       string             `json:"id,omitempty"`
	Provider state.ProviderSpec `json:"provider,omitempty"`
}

func ApplyChanges(spec state.SpecState, changes []Change) (state.SpecState, error) {
	next := spec
	next.Providers = append([]state.ProviderSpec(nil), spec.Providers...)
	indexByID := make(map[string]int, len(next.Providers))
	for i, p := range next.Providers {
		indexByID[p.ID] = i
	}

	for _, change := range changes {
		switch change.Action {
		case ActionUpsert:
			p := change.Provider
			if p.ID == "" {
				p.ID = change.ID
			}
			np, err := NormalizeProviderSpec(p)
			if err != nil {
				return spec, err
			}
			if idx, ok := indexByID[np.ID]; ok {
				next.Providers[idx] = np
			} else {
				indexByID[np.ID] = len(next.Providers)
				next.Providers = append(next.Providers, np)
			}
		case ActionDelete:
			id := change.ID
			if id == "" {
				id = change.Provider.ID
			}
			idx, ok := indexByID[id]
			if !ok {
				return spec, fmt.Errorf("provider %s does not exist", id)
			}
			next.Providers = append(next.Providers[:idx], next.Providers[idx+1:]...)
			indexByID = make(map[string]int, len(next.Providers))
			for i, p := range next.Providers {
				indexByID[p.ID] = i
			}
		default:
			return spec, fmt.Errorf("unknown provider action %q", change.Action)
		}
	}
	return ValidateSpecProviders(next)
}
