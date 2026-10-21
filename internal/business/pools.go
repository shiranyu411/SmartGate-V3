package business

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"smartgate/internal/opid"
	"smartgate/internal/state"
)

const (
	ActionUpsert = "upsert"
	ActionDelete = "delete"
)

var builtinOrigins = map[string]struct{}{
	"general":   {},
	"streaming": {},
	"ai":        {},
	"banking":   {},
	"crypto":    {},
}

type Change struct {
	Action string                 `json:"action"`
	ID     string                 `json:"id,omitempty"`
	Pool   state.BusinessPoolSpec `json:"pool,omitempty"`
}

type Plan struct {
	Added        []string `json:"added,omitempty"`
	Updated      []string `json:"updated,omitempty"`
	Deleted      []string `json:"deleted,omitempty"`
	OrderChanged []string `json:"order_changed,omitempty"`
	PoolsHash    string   `json:"pools_hash"`
	HasChanges   bool     `json:"has_changes"`
}

func ApplyChanges(spec state.SpecState, changes []Change) (state.SpecState, error) {
	next := spec
	next.BusinessPools = append([]state.BusinessPoolSpec(nil), state.EffectiveBusinessPools(spec.BusinessPools)...)
	indexByID := make(map[string]int, len(next.BusinessPools))
	for i, pool := range next.BusinessPools {
		indexByID[pool.ID] = i
	}
	for _, change := range changes {
		switch change.Action {
		case ActionUpsert:
			pool := change.Pool
			if pool.ID == "" {
				pool.ID = change.ID
			}
			normalized, err := NormalizePool(pool)
			if err != nil {
				return spec, err
			}
			if idx, ok := indexByID[normalized.ID]; ok {
				next.BusinessPools[idx] = normalized
			} else {
				indexByID[normalized.ID] = len(next.BusinessPools)
				next.BusinessPools = append(next.BusinessPools, normalized)
			}
		case ActionDelete:
			id := change.ID
			if id == "" {
				id = change.Pool.ID
			}
			if err := ValidatePoolID(id); err != nil {
				return spec, err
			}
			idx, ok := indexByID[id]
			if !ok {
				return spec, fmt.Errorf("business pool %s does not exist", id)
			}
			next.BusinessPools = append(next.BusinessPools[:idx], next.BusinessPools[idx+1:]...)
			indexByID = make(map[string]int, len(next.BusinessPools))
			for i, pool := range next.BusinessPools {
				indexByID[pool.ID] = i
			}
		default:
			return spec, fmt.Errorf("unknown business pool action %q", change.Action)
		}
	}
	return ValidatePools(next)
}

func NormalizePool(pool state.BusinessPoolSpec) (state.BusinessPoolSpec, error) {
	pool.ID = strings.TrimSpace(pool.ID)
	pool.Name = strings.TrimSpace(pool.Name)
	pool.Role = strings.ToLower(strings.TrimSpace(pool.Role))
	pool.BuiltinOrigin = strings.ToLower(strings.TrimSpace(pool.BuiltinOrigin))
	pool.NodeIDs = normalizeNodeIDs(pool.NodeIDs)
	if pool.ID == "" {
		return pool, fmt.Errorf("business pool id is required")
	}
	if err := ValidatePoolID(pool.ID); err != nil {
		return pool, err
	}
	if pool.Name == "" {
		return pool, fmt.Errorf("business pool %s name is required", pool.ID)
	}
	if pool.BuiltinOrigin == "" {
		if _, ok := builtinOrigins[pool.Role]; ok {
			pool.BuiltinOrigin = pool.Role
		}
	}
	if pool.BuiltinOrigin != "" {
		if _, ok := builtinOrigins[pool.BuiltinOrigin]; !ok {
			return pool, fmt.Errorf("business pool %s has invalid builtin origin %q", pool.ID, pool.BuiltinOrigin)
		}
	}
	if pool.Policy == (state.PoolPolicy{}) {
		pool.Policy = state.TemplatePoolPolicy(pool.BuiltinOrigin)
	}
	if err := ValidatePolicy(pool.Policy); err != nil {
		return pool, fmt.Errorf("business pool %s policy: %w", pool.ID, err)
	}
	return pool, nil
}

func ValidatePolicy(policy state.PoolPolicy) error {
	switch policy.Cutover {
	case state.CutoverStrict, state.CutoverGraceful:
	default:
		return fmt.Errorf("cutover must be strict or graceful")
	}
	if policy.GracefulDrainSeconds < 1 || policy.GracefulDrainSeconds > 3600 {
		return fmt.Errorf("graceful_drain_seconds must be between 1 and 3600")
	}
	switch policy.NoCandidate {
	case state.NoCandidateBlock, state.NoCandidateDirect, state.NoCandidateHold:
	default:
		return fmt.Errorf("no_candidate must be block, direct, or hold")
	}
	return nil
}

func ValidatePoolID(id string) error {
	if _, err := opid.Clean(id); err != nil {
		return fmt.Errorf("business pool id %q is unsafe: %w", id, err)
	}
	return nil
}

func ValidatePools(spec state.SpecState) (state.SpecState, error) {
	seen := make(map[string]struct{}, len(spec.BusinessPools))
	normalized := make([]state.BusinessPoolSpec, 0, len(spec.BusinessPools))
	for _, pool := range spec.BusinessPools {
		np, err := NormalizePool(pool)
		if err != nil {
			return spec, err
		}
		if _, ok := seen[np.ID]; ok {
			return spec, fmt.Errorf("duplicate business pool id %q", np.ID)
		}
		seen[np.ID] = struct{}{}
		normalized = append(normalized, np)
	}
	spec.BusinessPools = normalized
	return spec, nil
}

func Diff(oldSpec, newSpec state.SpecState) (Plan, error) {
	oldMap := mapPools(oldSpec.BusinessPools)
	newMap := mapPools(newSpec.BusinessPools)
	plan := Plan{PoolsHash: HashPools(newSpec.BusinessPools)}
	for id, np := range newMap {
		op, ok := oldMap[id]
		if !ok {
			plan.Added = append(plan.Added, id)
			continue
		}
		if !sameNodeOrder(op.NodeIDs, np.NodeIDs) {
			plan.OrderChanged = append(plan.OrderChanged, id)
		}
		if !poolsEqual(op, np) {
			plan.Updated = append(plan.Updated, id)
		}
	}
	for id := range oldMap {
		if _, ok := newMap[id]; !ok {
			plan.Deleted = append(plan.Deleted, id)
		}
	}
	plan.HasChanges = len(plan.Added) > 0 || len(plan.Updated) > 0 || len(plan.Deleted) > 0 || len(plan.OrderChanged) > 0
	return plan, nil
}

func HashPools(pools []state.BusinessPoolSpec) string {
	b, _ := json.Marshal(pools)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func normalizeNodeIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func mapPools(list []state.BusinessPoolSpec) map[string]state.BusinessPoolSpec {
	out := make(map[string]state.BusinessPoolSpec, len(list))
	for _, pool := range list {
		out[pool.ID] = pool
	}
	return out
}

func poolsEqual(a, b state.BusinessPoolSpec) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func sameNodeOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
