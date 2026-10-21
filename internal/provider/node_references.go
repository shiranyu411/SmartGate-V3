package provider

import "smartgate/internal/state"

type NodeReferenceCleanup struct {
	PoolNodeIDsRemoved     int  `json:"pool_node_ids_removed"`
	ProxySelectionsRemoved int  `json:"proxy_selections_removed"`
	NodeHealthRemoved      int  `json:"node_health_removed"`
	FailoverRefsRemoved    int  `json:"failover_refs_removed"`
	BusinessPoolsChanged   bool `json:"business_pools_changed"`
	RuntimeChanged         bool `json:"runtime_changed"`
	FailoverChanged        bool `json:"failover_changed"`
}

func CleanupStaleNodeReferences(spec state.SpecState, runtimeState state.RuntimeState, nodes []NodeView) (state.SpecState, state.RuntimeState, NodeReferenceCleanup) {
	valid := usableNodeSet(nodes)
	pools := poolRoleByID(spec)
	globalMode := state.EffectiveGatewayMode(spec.Settings.GatewayMode) == state.GatewayModeGlobal
	report := NodeReferenceCleanup{}
	if len(spec.BusinessPools) > 0 {
		for i := range spec.BusinessPools {
			filtered := filterExistingNodeIDs(spec.BusinessPools[i].NodeIDs, valid)
			removed := len(spec.BusinessPools[i].NodeIDs) - len(filtered)
			if removed > 0 {
				report.PoolNodeIDsRemoved += removed
				report.BusinessPoolsChanged = true
				spec.BusinessPools[i].NodeIDs = filtered
			}
		}
	}
	if len(runtimeState.ProxySelection) > 0 {
		next := make(map[string]string, len(runtimeState.ProxySelection))
		for groupID, nodeID := range runtimeState.ProxySelection {
			role, ok := pools[groupID]
			if groupID == "proxy" && !ok {
				role, ok = "general", true
			}
			if ok && validOrBuiltInTarget(nodeID, valid, role, globalMode) {
				next[groupID] = nodeID
				continue
			}
			report.ProxySelectionsRemoved++
			report.RuntimeChanged = true
		}
		if len(next) == 0 {
			runtimeState.ProxySelection = nil
		} else {
			runtimeState.ProxySelection = next
		}
	}
	if len(runtimeState.NodeHealth) > 0 {
		next := make(map[string]state.NodeHealth, len(runtimeState.NodeHealth))
		for nodeID, health := range runtimeState.NodeHealth {
			if _, ok := valid[nodeID]; ok {
				next[nodeID] = health
				continue
			}
			report.NodeHealthRemoved++
			report.RuntimeChanged = true
		}
		if len(next) == 0 {
			runtimeState.NodeHealth = nil
		} else {
			runtimeState.NodeHealth = next
		}
	}
	return spec, runtimeState, report
}

func CleanupStalePoolReferences(spec state.SpecState, runtimeState state.RuntimeState, failover state.FailoverState) (state.RuntimeState, state.FailoverState, NodeReferenceCleanup) {
	pools := poolRoleByID(spec)
	globalMode := state.EffectiveGatewayMode(spec.Settings.GatewayMode) == state.GatewayModeGlobal
	report := NodeReferenceCleanup{}
	if len(runtimeState.ProxySelection) > 0 {
		next := make(map[string]string, len(runtimeState.ProxySelection))
		for poolID, target := range runtimeState.ProxySelection {
			role, ok := pools[poolID]
			if poolID == "proxy" && !ok {
				role, ok = "general", true
			}
			if ok && targetAllowedWithoutNodeValidation(target, role, globalMode) {
				next[poolID] = target
				continue
			}
			report.ProxySelectionsRemoved++
			report.RuntimeChanged = true
		}
		if len(next) == 0 {
			runtimeState.ProxySelection = nil
		} else {
			runtimeState.ProxySelection = next
		}
	}
	failover, failoverCleanup := cleanupFailoverPools(failover, pools, globalMode, nil)
	report.FailoverRefsRemoved += failoverCleanup.FailoverRefsRemoved
	report.FailoverChanged = report.FailoverChanged || failoverCleanup.FailoverChanged
	return runtimeState, failover, report
}

func CleanupStaleFailoverReferences(failover state.FailoverState, spec state.SpecState, nodes []NodeView) (state.FailoverState, NodeReferenceCleanup) {
	valid := usableNodeSet(nodes)
	pools := poolRoleByID(spec)
	globalMode := state.EffectiveGatewayMode(spec.Settings.GatewayMode) == state.GatewayModeGlobal
	return cleanupFailoverPools(failover, pools, globalMode, valid)
}

func cleanupFailoverPools(failover state.FailoverState, pools map[string]string, globalMode bool, valid map[string]struct{}) (state.FailoverState, NodeReferenceCleanup) {
	report := NodeReferenceCleanup{}
	if len(failover.Pools) == 0 && len(failover.Locks) == 0 {
		return failover, report
	}
	next := make(map[string]state.FailoverPoolState, len(failover.Pools))
	for poolID, pool := range failover.Pools {
		role, ok := pools[poolID]
		if !ok {
			report.FailoverRefsRemoved++
			report.FailoverChanged = true
			continue
		}
		if pool.Current != "" && !validOrBuiltInTarget(pool.Current, valid, role, globalMode) {
			pool.Current = ""
			pool.Terminal = false
			pool.CooldownUntil = nil
			report.FailoverRefsRemoved++
			report.FailoverChanged = true
		}
		if pool.Previous != "" && !validOrBuiltInTarget(pool.Previous, valid, role, globalMode) {
			pool.Previous = ""
			report.FailoverRefsRemoved++
			report.FailoverChanged = true
		}
		if pool.Current == "" && pool.Previous == "" {
			continue
		}
		next[poolID] = pool
	}
	failover.Pools = next
	if len(failover.Locks) > 0 {
		nextLocks := make(map[string]state.FailoverLock, len(failover.Locks))
		for poolID, lock := range failover.Locks {
			if _, ok := pools[poolID]; ok {
				nextLocks[poolID] = lock
				continue
			}
			report.FailoverRefsRemoved++
			report.FailoverChanged = true
		}
		failover.Locks = nextLocks
	}
	return failover, report
}

func poolRoleByID(spec state.SpecState) map[string]string {
	effective := state.EffectiveBusinessPools(spec.BusinessPools)
	out := make(map[string]string, len(effective))
	for _, pool := range effective {
		out[pool.ID] = pool.Role
	}
	return out
}

func usableNodeSet(nodes []NodeView) map[string]struct{} {
	valid := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node.Excluded || node.ID == "" {
			continue
		}
		valid[node.ID] = struct{}{}
	}
	return valid
}

func filterExistingNodeIDs(in []string, valid map[string]struct{}) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, nodeID := range in {
		if _, ok := valid[nodeID]; ok {
			out = append(out, nodeID)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func validOrBuiltInTarget(nodeID string, valid map[string]struct{}, role string, globalMode bool) bool {
	if nodeID == "" {
		return true
	}
	if builtInTargetAllowed(nodeID, role, globalMode) {
		return true
	}
	if nodeID == "Direct" || nodeID == "Block" {
		return false
	}
	if valid == nil {
		return true
	}
	_, ok := valid[nodeID]
	return ok
}

func targetAllowedWithoutNodeValidation(target, role string, globalMode bool) bool {
	if target == "" {
		return true
	}
	if target == "Direct" || target == "Block" {
		return builtInTargetAllowed(target, role, globalMode)
	}
	return true
}

func builtInTargetAllowed(target, role string, globalMode bool) bool {
	switch target {
	case "":
		return true
	case "Block":
		return globalMode || failClosedRole(role)
	case "Direct":
		return !globalMode && !failClosedRole(role)
	default:
		return false
	}
}

func failClosedRole(role string) bool {
	switch role {
	case "ai", "banking", "crypto":
		return true
	default:
		return false
	}
}
