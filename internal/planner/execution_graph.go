package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"smartgate/internal/coredesc"
	"smartgate/internal/fabric"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

var coreRegistry *coredesc.Registry

func ConfigureCoreRegistry(registry *coredesc.Registry) {
	coreRegistry = registry
}

type GraphIssue struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Pool      string `json:"pool,omitempty"`
	SmartNode string `json:"smart_node,omitempty"`
	Blocking  bool   `json:"blocking"`
}

type ExecutionGraphPlan struct {
	Graph        state.ExecutionGraph `json:"execution_graph"`
	Issues       []GraphIssue         `json:"issues"`
	GraphHash    string               `json:"graph_hash"`
	ReadyToApply bool                 `json:"ready_to_apply"`
}

type graphBuilder struct {
	registry      *coredesc.Registry
	spec          state.SpecState
	selections    map[string]state.PoolSelection
	previous      state.ExecutionGraph
	now           time.Time
	executors     map[string]state.NodeExecutor
	executorByKey map[string]string
	workerCores   map[smartnode.CoreID]coredesc.Descriptor
	issues        []GraphIssue
}

func BuildExecutionGraph(spec state.SpecState, selections map[string]state.PoolSelection, previous state.ExecutionGraph, now time.Time) ExecutionGraphPlan {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	builder := graphBuilder{
		registry: coreRegistry,
		spec:     state.NormalizeSpecState(spec), selections: selections, previous: previous, now: now.UTC(),
		executors: map[string]state.NodeExecutor{}, executorByKey: map[string]string{}, workerCores: map[smartnode.CoreID]coredesc.Descriptor{},
	}
	builder.compileExecutors()
	graph := builder.compileBindings()
	graph.NodeExecutors = sortedExecutors(builder.executors)
	graph.Workers = builder.compileWorkers(graph.PoolBindings)
	graph.Fabric = builder.compileFabric(graph.PoolBindings, graph.Workers)
	graphHash := HashExecutionGraph(graph)
	return ExecutionGraphPlan{
		Graph: graph, Issues: builder.issues, GraphHash: graphHash, ReadyToApply: !hasBlockingIssues(builder.issues),
	}
}

func (builder *graphBuilder) compileExecutors() {
	for _, pool := range state.EffectiveBusinessPools(builder.spec.BusinessPools) {
		for _, nodeID := range pool.NodeIDs {
			node, ok := builder.spec.SmartNodes[nodeID]
			if !ok {
				builder.addIssue("node_not_found", "Pool 引用了不存在的 SmartNode", pool.ID, nodeID, true)
				continue
			}
			if node.CoreChoice == "" {
				builder.addIssue("node_core_unset", "SmartNode 尚未选择执行核心", pool.ID, nodeID, true)
				continue
			}
			descriptor, registered := builder.registry.Lookup(node.CoreChoice)
			if !registered {
				builder.addIssue("node_core_unknown", "SmartNode 选择的执行核心未注册", pool.ID, nodeID, true)
				continue
			}
			capability, _ := node.Capability.ForCore(node.CoreChoice)
			if capability.Status == smartnode.CapabilityUnsupportedByCore {
				builder.addIssue("node_core_unsupported", "SmartNode 选择的执行核心不受支持", pool.ID, nodeID, true)
				continue
			}
			if capability.Status == smartnode.CapabilityDegraded {
				builder.addIssue("node_core_degraded", "SmartNode 在所选执行核心上将以降级能力运行", pool.ID, nodeID, false)
			}
			key := executorKey(node)
			if _, ok := builder.executorByKey[key]; ok {
				continue
			}
			executorID := stableID("ne", key)
			workerID := descriptor.WorkerID(1)
			builder.executors[executorID] = state.NodeExecutor{
				ID: executorID, SmartNode: node.ID, Revision: node.Revision, Core: node.CoreChoice, Worker: workerID,
			}
			builder.executorByKey[key] = executorID
			builder.workerCores[node.CoreChoice] = descriptor
		}
	}
}

func (builder *graphBuilder) compileBindings() state.ExecutionGraph {
	graph := state.ExecutionGraph{}
	previous := map[string]state.PoolBinding{}
	for _, binding := range builder.previous.PoolBindings {
		previous[binding.Pool] = binding
	}
	for _, pool := range state.EffectiveBusinessPools(builder.spec.BusinessPools) {
		selection := builder.selections[pool.ID]
		old, hadOld := previous[pool.ID]
		generation := selection.Generation
		if generation == 0 {
			generation = old.Generation
		}
		if generation == 0 {
			generation = 1
		}
		target, ok := builder.resolveSelection(pool, selection)
		if !ok {
			if pool.Policy.NoCandidate == state.NoCandidateHold {
				if old, exists := previous[pool.ID]; exists {
					graph.PoolBindings = append(graph.PoolBindings, old)
					continue
				}
				builder.addIssue("hold_without_binding", "Pool 没有可保留的历史绑定", pool.ID, "", true)
				target = state.BindingTarget{Kind: state.NoCandidateBlock}
			} else {
				target = state.BindingTarget{Kind: pool.Policy.NoCandidate}
			}
		}
		since := builder.now
		if hadOld && bindingTargetsEqual(old.Target, target) {
			if old.Generation > 0 && (selection.Generation == 0 || selection.Generation <= old.Generation) {
				generation = old.Generation
			}
			since = old.Since
		} else if hadOld && generation <= old.Generation {
			generation = old.Generation + 1
		}
		bindingID := stableID("pb", fmt.Sprintf("%s\x00%d\x00%s\x00%s", pool.ID, generation, target.Kind, target.Executor))
		graph.PoolBindings = append(graph.PoolBindings, state.PoolBinding{
			ID: bindingID, Pool: pool.ID, Target: target, Generation: generation, Since: since,
		})
	}
	sort.Slice(graph.PoolBindings, func(i, j int) bool { return graph.PoolBindings[i].Pool < graph.PoolBindings[j].Pool })
	return graph
}

func (builder *graphBuilder) resolveSelection(pool state.BusinessPoolSpec, selection state.PoolSelection) (state.BindingTarget, bool) {
	if selection.Target == state.NoCandidateDirect || selection.Target == state.NoCandidateBlock {
		return state.BindingTarget{Kind: selection.Target}, true
	}
	selectedNode := selection.SmartNode
	if selectedNode != "" && !containsString(pool.NodeIDs, selectedNode) && builder.previousPoolHadNode(pool.ID, selectedNode) {
		// Pool edits leave durable Selection untouched until Apply. Reconcile
		// removal of the applied member, but still reject unrelated bad IDs.
		builder.addIssue("pool_selection_removed", "Pool 原选节点已移除，将按剩余成员和空池策略重选", pool.ID, selectedNode, false)
		selectedNode = ""
	}
	if selectedNode == "" {
		for _, nodeID := range pool.NodeIDs {
			if _, ok := builder.executorForNode(nodeID); ok {
				selectedNode = nodeID
				break
			}
		}
	}
	if selectedNode == "" {
		return state.BindingTarget{}, false
	}
	if !containsString(pool.NodeIDs, selectedNode) {
		builder.addIssue("selection_not_in_pool", "所选 SmartNode 不属于该 Pool", pool.ID, selectedNode, true)
		return state.BindingTarget{}, false
	}
	executor, ok := builder.executorForNode(selectedNode)
	if !ok {
		return state.BindingTarget{}, false
	}
	return state.BindingTarget{
		Kind: "node", SmartNode: executor.SmartNode, Executor: executor.ID, Core: executor.Core, Worker: executor.Worker,
	}, true
}

func (builder *graphBuilder) previousPoolHadNode(poolID, nodeID string) bool {
	executors := map[string]bool{}
	for _, executor := range builder.previous.NodeExecutors {
		if executor.SmartNode == nodeID {
			executors[executor.ID] = true
		}
	}
	for _, target := range builder.previous.Workers {
		for _, route := range target.Routes {
			if route.Pool != poolID {
				continue
			}
			for _, candidate := range route.Candidates {
				if executors[candidate] {
					return true
				}
			}
		}
	}
	return false
}

func (builder *graphBuilder) executorForNode(nodeID string) (state.NodeExecutor, bool) {
	node, ok := builder.spec.SmartNodes[nodeID]
	if !ok {
		return state.NodeExecutor{}, false
	}
	if _, registered := builder.registry.Lookup(node.CoreChoice); !registered {
		return state.NodeExecutor{}, false
	}
	id, ok := builder.executorByKey[executorKey(node)]
	if !ok {
		return state.NodeExecutor{}, false
	}
	return builder.executors[id], true
}

func (builder *graphBuilder) compileWorkers(bindings []state.PoolBinding) []state.WorkerTarget {
	cores := make([]smartnode.CoreID, 0, len(builder.workerCores))
	for core := range builder.workerCores {
		cores = append(cores, core)
	}
	sort.Slice(cores, func(i, j int) bool { return cores[i] < cores[j] })
	workers := make([]state.WorkerTarget, 0, len(cores))
	for _, core := range cores {
		descriptor := builder.workerCores[core]
		workers = append(workers, state.WorkerTarget{
			ID: descriptor.WorkerID(1), Core: core, Version: descriptor.Version,
			Control: state.WorkerControl{Listen: descriptor.Control.Listen, Port: descriptor.Control.Port, Kind: descriptor.Control.Kind},
			Routes:  []state.WorkerRoute{}, Inbounds: []state.WorkerInbound{},
		})
	}
	current := make(map[string]state.PoolBinding, len(bindings))
	for _, binding := range bindings {
		current[binding.Pool] = binding
	}
	for index := range workers {
		worker := &workers[index]
		descriptor := builder.workerCores[worker.Core]
		tproxyPort := descriptor.Ports.TProxyBase
		dnsPort := descriptor.Ports.DNSBase
		for _, pool := range state.EffectiveBusinessPools(builder.spec.BusinessPools) {
			candidates := builder.routeCandidates(pool, worker.Core)
			if len(candidates) == 0 {
				continue
			}
			selected := candidates[0]
			binding := current[pool.ID]
			if binding.Target.Worker == worker.ID && containsString(candidates, binding.Target.Executor) {
				selected = binding.Target.Executor
			}
			for lane := 0; lane < 2; lane++ {
				routeID := stableID("wr", fmt.Sprintf("%s\x00%s\x00%d", worker.ID, pool.ID, lane))
				route := state.WorkerRoute{
					ID: routeID, Pool: pool.ID, Lane: lane, Selector: routeID,
					Candidates: append([]string(nil), candidates...), Selected: selected,
				}
				worker.Routes = append(worker.Routes, route)
				bindingID := ""
				generation := uint64(0)
				if binding.Target.Worker == worker.ID && BindingLane(binding.Generation) == lane {
					bindingID = binding.ID
					generation = binding.Generation
				}
				worker.Inbounds = append(worker.Inbounds,
					state.WorkerInbound{
						ID: stableID("wi", routeID+"\x00tproxy"), Kind: "tproxy", Pool: pool.ID, Route: routeID, Lane: lane,
						Binding: bindingID, Generation: generation, Listen: "::", Port: tproxyPort, Network: "tcp,udp",
					},
					state.WorkerInbound{
						ID: stableID("wi", routeID+"\x00dns"), Kind: "socks", Pool: pool.ID, Route: routeID, Lane: lane,
						Binding: bindingID, Generation: generation, Listen: "127.0.0.1", Port: dnsPort, Network: "tcp",
					},
				)
				tproxyPort++
				dnsPort++
			}
		}
		workers[index].Resources.Ports = len(workers[index].Inbounds) + 1
		workers[index].Resources.EstimatedFD = 32 + len(workers[index].Inbounds)*4
		workers[index].Resources.EstimatedRSSKB = descriptor.Resources.EstimatedRSSKB
	}
	return workers
}

func (builder *graphBuilder) routeCandidates(pool state.BusinessPoolSpec, core smartnode.CoreID) []string {
	candidates := make([]string, 0, len(pool.NodeIDs))
	for _, nodeID := range pool.NodeIDs {
		executor, ok := builder.executorForNode(nodeID)
		if !ok || executor.Core != core {
			continue
		}
		candidates = append(candidates, executor.ID)
	}
	sort.Strings(candidates)
	return candidates
}

func BindingLane(generation uint64) int {
	if generation == 0 {
		return 0
	}
	return int((generation - 1) % 2)
}

func (builder *graphBuilder) compileFabric(bindings []state.PoolBinding, workers []state.WorkerTarget) state.FabricGraph {
	tproxyPorts := map[string]state.WorkerInbound{}
	dnsPorts := map[string]state.WorkerInbound{}
	routes := map[string]state.WorkerRoute{}
	for _, worker := range workers {
		for _, route := range worker.Routes {
			routes[workerRouteKey(worker.ID, route.Pool, route.Lane)] = route
		}
		for _, inbound := range worker.Inbounds {
			key := workerRouteKey(worker.ID, inbound.Pool, inbound.Lane)
			switch inbound.Kind {
			case "tproxy":
				tproxyPorts[key] = inbound
			case "socks":
				dnsPorts[key] = inbound
			}
		}
	}
	rules, fallbackPool, err := fabric.RulesFromSpec(builder.spec)
	if err != nil {
		builder.addIssue("fabric_rules_invalid", err.Error(), "", "", true)
	}
	graph := state.FabricGraph{
		Takeover: state.FabricTakeoverGraph{
			Enabled: builder.spec.Fabric.Takeover.Enabled, TCP: builder.spec.Fabric.Takeover.TCP,
			UDP: builder.spec.Fabric.Takeover.UDP, IPv4: builder.spec.Fabric.Takeover.IPv4, IPv6: builder.spec.Fabric.Takeover.IPv6,
		},
		DNS: state.FabricDNSGraph{
			Listen: builder.spec.Fabric.DNS.Listen, RedirectLAN53: builder.spec.Fabric.DNS.RedirectLAN53,
			RejectHTTPSRecords: builder.spec.Fabric.DNS.RejectHTTPSRecords, BlockDoT853: builder.spec.Fabric.DNS.BlockDoT853,
			PoolSets: map[string]state.FabricPoolSets{},
		},
		FallbackPool: fallbackPool,
		RuleSets:     append([]state.RuleSetSnapshotRef(nil), builder.previous.Fabric.RuleSets...),
	}
	for _, rule := range rules {
		graph.Rules = append(graph.Rules, state.FabricRuleGraph{
			ID: rule.ID, Source: rule.Source, Kind: rule.Kind, Value: rule.Value, Target: rule.PoolID, Priority: rule.Priority,
		})
	}
	setTargets := []string{fabric.TargetDirect, fabric.TargetBlock}
	for _, binding := range bindings {
		setTargets = append(setTargets, binding.Pool)
	}
	for _, target := range uniqueStrings(setTargets) {
		base := stableResourceName("sgp", target)
		graph.DNS.PoolSets[target] = state.FabricPoolSets{IPv4: base + "_4", IPv6: base + "_6"}
	}
	slots := stablePoolSlots(bindings)
	for _, binding := range bindings {
		target := binding.Target.Kind
		lane := BindingLane(binding.Generation)
		routeID := ""
		inboundID := ""
		dnsInboundID := ""
		tproxyPort := 0
		dnsPort := 0
		if binding.Target.Kind == "node" {
			target = binding.Target.Worker
			key := workerRouteKey(binding.Target.Worker, binding.Pool, lane)
			route, routeOK := routes[key]
			tproxy, tproxyOK := tproxyPorts[key]
			dns, dnsOK := dnsPorts[key]
			if !routeOK || !tproxyOK || !dnsOK || !containsString(route.Candidates, binding.Target.Executor) {
				builder.addIssue("worker_route_missing", "Pool 绑定没有对应的已准备 Worker 路由", binding.Pool, binding.Target.SmartNode, true)
			} else {
				routeID = route.ID
				inboundID = tproxy.ID
				dnsInboundID = dns.ID
				tproxyPort = tproxy.Port
				dnsPort = dns.Port
			}
		}
		slot := slots[binding.Pool]
		mark := uint32(0x50000000) | uint32(slot)<<16 | uint32(binding.Generation&0xffff)
		graph.Dispatch = append(graph.Dispatch, state.PoolDispatchGraph{
			Pool: binding.Pool, Binding: binding.ID, Generation: binding.Generation, Lane: lane,
			Route: routeID, Inbound: inboundID, DNSInbound: dnsInboundID,
			Mark: mark, MarkMask: 0xffffffff, Table: 1000 + slot, Port: tproxyPort,
			DNSPort: dnsPort, Target: target,
		})
	}
	return graph
}

func workerRouteKey(workerID, pool string, lane int) string {
	return fmt.Sprintf("%s\x00%s\x00%d", workerID, pool, lane)
}

func stablePoolSlots(bindings []state.PoolBinding) map[string]int {
	pools := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		pools = append(pools, binding.Pool)
	}
	sort.Strings(pools)
	used := map[int]struct{}{}
	out := make(map[string]int, len(pools))
	for _, pool := range pools {
		sum := sha256.Sum256([]byte(pool))
		slot := int(sum[0])<<4 | int(sum[1]>>4)
		if slot == 0 {
			slot = 1
		}
		for {
			if _, exists := used[slot]; !exists {
				break
			}
			slot++
			if slot > 4095 {
				slot = 1
			}
		}
		used[slot] = struct{}{}
		out[pool] = slot
	}
	return out
}

func stableResourceName(prefix, value string) string {
	clean := sanitizeID(value)
	if len(clean) > 12 {
		clean = clean[:12]
	}
	if clean == "" {
		clean = "pool"
	}
	sum := sha256.Sum256([]byte(value))
	return prefix + "_" + clean + "_" + hex.EncodeToString(sum[:])[:8]
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (builder *graphBuilder) addIssue(code, message, pool, node string, blocking bool) {
	builder.issues = append(builder.issues, GraphIssue{Code: code, Message: message, Pool: pool, SmartNode: node, Blocking: blocking})
}

func sortedExecutors(executors map[string]state.NodeExecutor) []state.NodeExecutor {
	ids := make([]string, 0, len(executors))
	for id := range executors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]state.NodeExecutor, 0, len(ids))
	for _, id := range ids {
		out = append(out, executors[id])
	}
	return out
}

func executorKey(node smartnode.SmartNode) string {
	return fmt.Sprintf("%s\x00%d\x00%s", node.ID, node.Revision, node.CoreChoice)
}

func stableID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(sum[:])[:16]
}

func HashExecutionGraph(graph state.ExecutionGraph) string {
	copyGraph := graph
	copyGraph.PoolBindings = append([]state.PoolBinding(nil), graph.PoolBindings...)
	for index := range copyGraph.PoolBindings {
		copyGraph.PoolBindings[index].Since = time.Time{}
	}
	body, _ := json.Marshal(copyGraph)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func bindingTargetsEqual(left, right state.BindingTarget) bool {
	return left.Kind == right.Kind && left.SmartNode == right.SmartNode && left.Executor == right.Executor && left.Core == right.Core && left.Worker == right.Worker
}

func hasBlockingIssues(issues []GraphIssue) bool {
	for _, issue := range issues {
		if issue.Blocking {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sanitizeID(value string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(value) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteByte('_')
	}
	return strings.Trim(builder.String(), "_")
}
