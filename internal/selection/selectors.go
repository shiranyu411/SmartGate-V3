package selection

import (
	"strings"

	"smartgate/internal/provider"
	"smartgate/internal/state"
)

const DefaultMaxPoolCandidates = 10

const poolTagPrefix = "Pool_"

func PoolTag(poolID string) string {
	return poolTagPrefix + poolID
}

func PoolIDFromTag(tag string) (string, bool) {
	if !strings.HasPrefix(tag, poolTagPrefix) {
		return "", false
	}
	return strings.TrimPrefix(tag, poolTagPrefix), true
}

func PoolIDFromTagFold(tag string) (string, bool) {
	if len(tag) < len(poolTagPrefix) || !strings.EqualFold(tag[:len(poolTagPrefix)], poolTagPrefix) {
		return "", false
	}
	return tag[len(poolTagPrefix):], true
}

type SelectorState struct {
	PoolID     string   `json:"pool_id"`
	Role       string   `json:"role"`
	Selector   string   `json:"selector"`
	Candidates []string `json:"candidates"`
	Selected   string   `json:"selected"`
}

// BuildSelectorStates returns selector defaults overlaid with valid persisted
// selections.
func BuildSelectorStates(nodes []provider.NodeView, pools []state.BusinessPoolSpec, gatewayMode string, maxCandidates int, selected map[string]string) []SelectorState {
	if maxCandidates <= 0 {
		maxCandidates = DefaultMaxPoolCandidates
	}
	pools = state.EffectiveBusinessPools(pools)
	usable := usableNodeMap(nodes)
	globalMode := state.EffectiveGatewayMode(gatewayMode) == state.GatewayModeGlobal
	out := make([]SelectorState, 0, len(pools))
	for _, pool := range pools {
		candidates := poolCandidates(pool, usable, nodes, maxCandidates)
		if len(candidates) == 0 {
			fallback := "Direct"
			if globalMode || isFailClosedRole(pool.Role) {
				fallback = "Block"
			}
			candidates = []string{fallback}
		} else if isFailClosedRole(pool.Role) {
			if !ContainsCandidate(candidates, "Block") {
				candidates = append(candidates, "Block")
			}
		} else {
			fallback := "Direct"
			if globalMode {
				fallback = "Block"
			}
			if !ContainsCandidate(candidates, fallback) {
				candidates = append(candidates, fallback)
			}
		}
		current := candidates[0]
		if requested := selected[pool.ID]; requested != "" && ContainsCandidate(candidates, requested) {
			current = requested
		}
		out = append(out, SelectorState{
			PoolID: pool.ID, Role: pool.Role, Selector: PoolTag(pool.ID),
			Candidates: append([]string(nil), candidates...), Selected: current,
		})
	}
	return out
}

func ContainsCandidate(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func usableNodeMap(nodes []provider.NodeView) map[string]provider.NodeView {
	out := map[string]provider.NodeView{}
	for _, node := range nodes {
		if node.Excluded {
			continue
		}
		out[node.ID] = node
	}
	return out
}

func poolCandidates(pool state.BusinessPoolSpec, nodes map[string]provider.NodeView, allNodes []provider.NodeView, maxCandidates int) []string {
	if len(pool.NodeIDs) > 0 {
		return explicitPoolCandidates(pool.NodeIDs, nodes, maxCandidates)
	}
	var candidates []string
	for _, node := range allNodes {
		if node.Excluded {
			continue
		}
		if pool.Role != "general" && !nodeHasRole(node, pool.Role) {
			continue
		}
		candidates = append(candidates, node.ID)
		if len(candidates) >= maxCandidates {
			break
		}
	}
	return candidates
}

func explicitPoolCandidates(nodeIDs []string, nodes map[string]provider.NodeView, maxCandidates int) []string {
	var candidates []string
	for _, nodeID := range nodeIDs {
		if _, ok := nodes[nodeID]; !ok {
			continue
		}
		candidates = append(candidates, nodeID)
		if len(candidates) >= maxCandidates {
			break
		}
	}
	return candidates
}

func nodeHasRole(node provider.NodeView, role string) bool {
	for _, item := range node.Roles {
		if item == role {
			return true
		}
	}
	return false
}

func isFailClosedRole(role string) bool {
	switch role {
	case "ai", "banking", "crypto":
		return true
	default:
		return false
	}
}
