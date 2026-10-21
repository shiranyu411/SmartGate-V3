package configapply

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"smartgate/internal/business"
	"smartgate/internal/fabric"
	"smartgate/internal/netgate"
	"smartgate/internal/opid"
	"smartgate/internal/planner"
	"smartgate/internal/provider"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type TargetState struct {
	Spec             state.SpecState                `json:"spec"`
	LastGood         []state.LastGoodSnapshot       `json:"last_good,omitempty"`
	FallbackLastGood []state.LastGoodSnapshot       `json:"fallback_last_good,omitempty"`
	ProxySelection   map[string]string              `json:"proxy_selection,omitempty"`
	Selections       map[string]state.PoolSelection `json:"selections,omitempty"`
	PreviousGraph    state.ExecutionGraph           `json:"previous_graph"`
	RuleSetsPinned   bool                           `json:"rule_sets_pinned,omitempty"`
	RuleSetRefs      []state.RuleSetSnapshotRef     `json:"rule_set_refs,omitempty"`
}

type Prepared struct {
	OpID           string                 `json:"op_id"`
	Target         TargetState            `json:"target"`
	SpecHash       string                 `json:"spec_hash"`
	TopologyHash   string                 `json:"topology_hash"`
	GatewayMode    string                 `json:"gateway_mode"`
	ConfigEpoch    int64                  `json:"config_epoch"`
	OpenWrtPlan    netgate.DryRunPlan     `json:"openwrt_plan"`
	Nodes          []provider.NodeView    `json:"-"`
	ExecutionGraph state.ExecutionGraph   `json:"execution_graph"`
	GraphHash      string                 `json:"graph_hash,omitempty"`
	GraphIssues    []planner.GraphIssue   `json:"graph_issues,omitempty"`
	Workers        *worker.Prepared       `json:"workers,omitempty"`
	Fabric         *fabric.PreparedFabric `json:"fabric,omitempty"`
}

type Result struct {
	OpenWrt          netgate.ApplyResult      `json:"openwrt"`
	Workers          worker.ApplyResult       `json:"workers"`
	Fabric           fabric.FabricApplyResult `json:"fabric"`
	FabricApplied    bool                     `json:"fabric_applied"`
	WorkersRestored  bool                     `json:"workers_restored,omitempty"`
	FabricRestored   bool                     `json:"fabric_restored,omitempty"`
	BindingsRestored bool                     `json:"bindings_restored,omitempty"`
	FinalSelections  map[string]string        `json:"final_selections,omitempty"`
	Warnings         []string                 `json:"warnings,omitempty"`
}

type Checkpoint struct {
	Workers         worker.Checkpoint     `json:"workers"`
	WorkersCaptured bool                  `json:"workers_captured"`
	Fabric          fabric.PreparedFabric `json:"fabric"`
	FabricCaptured  bool                  `json:"fabric_captured"`
}

type WorkerController interface {
	Prepare(context.Context, string, state.ExecutionGraph, map[string]smartnode.IdentitySource) (worker.Prepared, error)
	Checkpoint(string) (worker.Checkpoint, error)
	Apply(context.Context, worker.Prepared) (worker.ApplyResult, error)
	Rollback(context.Context, worker.Checkpoint) (worker.ApplyResult, error)
	StopAll(context.Context) error
	Cleanup(worker.Prepared)
}

type FabricController interface {
	Prepare(context.Context, string, state.SpecState, state.FabricGraph) (fabric.PreparedFabric, error)
	Apply(context.Context, fabric.PreparedFabric) (fabric.FabricApplyResult, error)
	Stop(context.Context) error
	Current() (fabric.PreparedFabric, bool)
}

type Engine struct {
	Owners               *netgate.OwnerRegistry
	WorkerCleanupTargets []netgate.WorkerCleanupTarget
	NetGateRunner        netgate.Runner
	Workers              WorkerController
	Fabric               FabricController
}

// PrepareRollbackFromCheckpoint binds the already captured live worker and
// Fabric state to a new operation id. It deliberately avoids compiling and
// checking the durable source configuration again; callers still fall back to
// Prepare when the checkpoint is incomplete.
func (e Engine) PrepareRollbackFromCheckpoint(_ context.Context, opID string, target TargetState, _ int64, checkpoint Checkpoint) (Prepared, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return Prepared{}, err
	}
	if !checkpoint.WorkersCaptured || !checkpoint.FabricCaptured {
		return Prepared{}, fmt.Errorf("rollback checkpoint is incomplete")
	}
	mode := state.EffectiveGatewayMode(target.Spec.Settings.GatewayMode)
	if !gatewayModeRunsDataPlane(mode) {
		return Prepared{}, fmt.Errorf("rollback checkpoint fast path requires an active data plane")
	}
	workers := checkpoint.Workers.Prepared
	workers.OpID = safeOpID
	fabricTarget := checkpoint.Fabric
	fabricTarget.OpID = safeOpID
	fabricTarget.Plan.OpID = safeOpID
	fabricTarget.Plan.DryRunOnly = true
	graph := target.PreviousGraph
	if len(graph.Fabric.Dispatch) == 0 && len(fabricTarget.Graph.Dispatch) > 0 {
		graph = state.ExecutionGraph{Fabric: fabricTarget.Graph}
	}
	return Prepared{
		OpID: safeOpID, Target: target, SpecHash: provider.HashSpec(target.Spec),
		GatewayMode: mode,
		OpenWrtPlan: fabricTarget.Plan, ExecutionGraph: graph,
		GraphHash: planner.HashExecutionGraph(graph), Workers: &workers, Fabric: &fabricTarget,
	}, nil
}

func (e Engine) Checkpoint(opID string) (Checkpoint, error) {
	if e.Workers == nil || e.Fabric == nil {
		return Checkpoint{}, fmt.Errorf("worker and fabric managers are required")
	}
	workers, err := e.Workers.Checkpoint(opID)
	if err != nil {
		return Checkpoint{}, err
	}
	checkpoint := Checkpoint{Workers: workers, WorkersCaptured: true}
	if current, ok := e.Fabric.Current(); ok {
		checkpoint.Fabric = current
		checkpoint.FabricCaptured = true
	}
	return checkpoint, nil
}

func (e Engine) Prepare(ctx context.Context, opID string, target TargetState, configEpoch int64) (Prepared, error) {
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return Prepared{}, err
	}
	normalized, err := provider.ValidateSpecProviders(target.Spec)
	if err != nil {
		return Prepared{}, err
	}
	normalized, err = business.ValidatePools(normalized)
	if err != nil {
		return Prepared{}, err
	}
	target.Spec = normalized
	target.LastGood = mergeRequiredLastGood(target.Spec, target.LastGood, target.FallbackLastGood, target.PreviousGraph, target.ProxySelection, target.Selections)
	topologyHash, err := provider.HashEffectiveTopologyFromSnapshots(target.Spec, target.LastGood)
	if err != nil {
		return Prepared{}, err
	}
	loader, err := provider.NewSnapshotLastGoodLoader(target.LastGood)
	if err != nil {
		return Prepared{}, err
	}
	nodes, err := provider.BuildNodeViews(target.Spec, loader)
	if err != nil {
		return Prepared{}, err
	}
	owners := e.Owners
	if owners == nil {
		owners = netgate.NewOwnerRegistry("smartgate")
	}
	mode := state.EffectiveGatewayMode(target.Spec.Settings.GatewayMode)
	prepared := Prepared{
		OpID:         safeOpID,
		Target:       target,
		SpecHash:     provider.HashSpec(target.Spec),
		TopologyHash: topologyHash,
		GatewayMode:  mode,
		ConfigEpoch:  configEpoch,
		Nodes:        nodes,
	}
	if e.Workers == nil || e.Fabric == nil {
		return prepared, fmt.Errorf("worker and fabric managers are required")
	}
	if !gatewayModeRunsDataPlane(mode) {
		prepared.ExecutionGraph = state.ExecutionGraph{}
		prepared.GraphHash = planner.HashExecutionGraph(prepared.ExecutionGraph)
		prepared.OpenWrtPlan, err = netgate.OpenWrtRescueCleanupDryRunPlan(safeOpID, owners, e.WorkerCleanupTargets)
		return prepared, err
	}

	graphPlan := planner.BuildExecutionGraph(target.Spec, selectionsForTarget(target), target.PreviousGraph, time.Now().UTC())
	prepared.ExecutionGraph = graphPlan.Graph
	if target.RuleSetsPinned {
		prepared.ExecutionGraph.Fabric.RuleSetsPinned = true
		prepared.ExecutionGraph.Fabric.RuleSets = cloneRuleSetRefs(target.RuleSetRefs)
	}
	prepared.GraphHash = graphPlan.GraphHash
	prepared.GraphIssues = append([]planner.GraphIssue(nil), graphPlan.Issues...)
	if !graphPlan.ReadyToApply {
		return prepared, GraphValidationError{Issues: prepared.GraphIssues}
	}
	workerPrepared, err := e.Workers.Prepare(ctx, safeOpID, prepared.ExecutionGraph, protectedIdentities(target.Spec, target.FallbackLastGood, nodes))
	prepared.Workers = &workerPrepared
	if err != nil {
		return prepared, err
	}
	applyWorkerCandidateMetadata(&prepared.ExecutionGraph, &workerPrepared)
	prepared.Workers = &workerPrepared
	prepared.GraphHash = planner.HashExecutionGraph(prepared.ExecutionGraph)
	fabricPrepared, err := e.Fabric.Prepare(ctx, safeOpID, target.Spec, prepared.ExecutionGraph.Fabric)
	prepared.Fabric = &fabricPrepared
	if err != nil {
		e.Workers.Cleanup(workerPrepared)
		return prepared, err
	}
	prepared.OpenWrtPlan = fabricPrepared.Plan
	prepared.ExecutionGraph.Fabric = fabricPrepared.Graph
	prepared.GraphHash = planner.HashExecutionGraph(prepared.ExecutionGraph)
	prepared.Fabric.Graph = prepared.ExecutionGraph.Fabric
	return prepared, nil
}

func (e Engine) Apply(ctx context.Context, prepared Prepared, selected map[string]string) (Result, error) {
	if prepared.OpenWrtPlan.OpID != prepared.OpID || !prepared.OpenWrtPlan.DryRunOnly {
		return Result{}, fmt.Errorf("prepared OpenWrt plan is not bound to dry-run %s", prepared.OpID)
	}
	return e.applyGraph(ctx, prepared, selected)
}

// Rollback restores workers and Gateway Fabric from the captured graph
// checkpoint, then reconciles network state to the durable source target.
func (e Engine) Rollback(ctx context.Context, prepared Prepared, selected map[string]string, checkpoint Checkpoint) (Result, error) {
	if prepared.OpenWrtPlan.OpID != prepared.OpID || !prepared.OpenWrtPlan.DryRunOnly {
		return Result{}, fmt.Errorf("prepared OpenWrt rollback plan is not bound to %s", prepared.OpID)
	}
	return e.rollbackGraph(ctx, prepared, selected, checkpoint)
}

type GraphValidationError struct {
	Issues []planner.GraphIssue
}

func (e GraphValidationError) Error() string {
	for _, issue := range e.Issues {
		if !issue.Blocking {
			continue
		}
		return fmt.Sprintf("execution graph blocked by %s: %s (pool=%s node=%s)", issue.Code, issue.Message, issue.Pool, issue.SmartNode)
	}
	return "execution graph is not ready to apply"
}

func (e Engine) Cleanup(prepared Prepared) {
	if prepared.Workers != nil && e.Workers != nil {
		e.Workers.Cleanup(*prepared.Workers)
	}
}

func (e Engine) applyGraph(ctx context.Context, prepared Prepared, selected map[string]string) (Result, error) {
	if err := e.validateGraphPrepared(prepared); err != nil {
		return Result{}, err
	}
	result := Result{}
	if !gatewayModeRunsDataPlane(prepared.GatewayMode) {
		if err := e.Workers.StopAll(ctx); err != nil {
			return result, fmt.Errorf("stop workers for gateway %s: %w", prepared.GatewayMode, err)
		}
		if err := e.Fabric.Stop(ctx); err != nil {
			return result, fmt.Errorf("stop embedded DNS for gateway %s: %w", prepared.GatewayMode, err)
		}
		plan := prepared.OpenWrtPlan
		plan.DryRunOnly = false
		openWrt, err := netgate.ExecutePlan(ctx, plan, e.NetGateRunner)
		result.OpenWrt = openWrt
		return result, err
	}

	workers, err := e.Workers.Apply(ctx, *prepared.Workers)
	result.Workers = workers
	if err != nil {
		return result, err
	}
	fabricResult, err := e.Fabric.Apply(ctx, *prepared.Fabric)
	result.Fabric = fabricResult
	result.FabricApplied = fabricResult.Applied
	result.OpenWrt = fabricResult.Network
	if err != nil {
		return result, err
	}
	result.FinalSelections = selectionsFromGraph(prepared.ExecutionGraph)
	for pool, requested := range selected {
		if requested != "" && result.FinalSelections[pool] != requested {
			result.Warnings = append(result.Warnings, fmt.Sprintf("Pool %s 的选点已从 %s 调整为 %s", pool, requested, result.FinalSelections[pool]))
		}
	}
	for _, issue := range prepared.GraphIssues {
		if !issue.Blocking {
			result.Warnings = append(result.Warnings, issue.Message)
		}
	}
	if prepared.Fabric != nil {
		result.Warnings = append(result.Warnings, prepared.Fabric.Warnings...)
	}
	return result, nil
}

func (e Engine) rollbackGraph(ctx context.Context, prepared Prepared, selected map[string]string, checkpoint Checkpoint) (Result, error) {
	if err := e.validateGraphPrepared(prepared); err != nil {
		return Result{}, err
	}
	result := Result{}
	if gatewayModeRunsDataPlane(prepared.GatewayMode) {
		fabricTarget := *prepared.Fabric
		if checkpoint.FabricCaptured {
			fabricTarget = checkpoint.Fabric
		}
		fabricResult, err := e.Fabric.Apply(ctx, fabricTarget)
		result.Fabric = fabricResult
		result.FabricApplied = fabricResult.Applied
		result.OpenWrt = fabricResult.Network
		if err != nil {
			return result, fmt.Errorf("restore Gateway Fabric: %w", err)
		}
	} else {
		if err := e.Fabric.Stop(ctx); err != nil {
			return result, fmt.Errorf("stop embedded DNS during rollback: %w", err)
		}
		plan := prepared.OpenWrtPlan
		plan.DryRunOnly = false
		openWrt, err := netgate.ExecutePlan(ctx, plan, e.NetGateRunner)
		result.OpenWrt = openWrt
		if err != nil {
			return result, fmt.Errorf("restore owner-tagged network baseline: %w", err)
		}
	}
	result.FabricRestored = true
	result.BindingsRestored = true
	if checkpoint.WorkersCaptured {
		workers, err := e.Workers.Rollback(ctx, checkpoint.Workers)
		result.Workers = workers
		if err != nil {
			return result, err
		}
	} else if err := e.Workers.StopAll(ctx); err != nil {
		return result, fmt.Errorf("stop workers without rollback checkpoint: %w", err)
	}
	result.WorkersRestored = true
	if gatewayModeRunsDataPlane(prepared.GatewayMode) {
		result.FinalSelections = selectionsFromGraph(prepared.ExecutionGraph)
	} else {
		result.FinalSelections = copySelections(selected)
	}
	return result, nil
}

func (e Engine) validateGraphPrepared(prepared Prepared) error {
	if e.Workers == nil || e.Fabric == nil {
		return fmt.Errorf("worker and fabric managers are required")
	}
	if prepared.GraphHash == "" {
		return fmt.Errorf("prepared execution graph is required")
	}
	if planner.HashExecutionGraph(prepared.ExecutionGraph) != prepared.GraphHash {
		return fmt.Errorf("prepared execution graph hash mismatch")
	}
	for _, issue := range prepared.GraphIssues {
		if issue.Blocking {
			return GraphValidationError{Issues: prepared.GraphIssues}
		}
	}
	if !gatewayModeRunsDataPlane(prepared.GatewayMode) {
		return nil
	}
	if prepared.Workers == nil || prepared.Workers.OpID != prepared.OpID {
		return fmt.Errorf("prepared workers are not bound to dry-run %s", prepared.OpID)
	}
	if prepared.Fabric == nil || prepared.Fabric.OpID != prepared.OpID {
		return fmt.Errorf("prepared Gateway Fabric is not bound to dry-run %s", prepared.OpID)
	}
	if prepared.Fabric.Plan.OpID != prepared.OpID || !prepared.Fabric.Plan.DryRunOnly {
		return fmt.Errorf("prepared Gateway Fabric plan is not bound to dry-run %s", prepared.OpID)
	}
	return nil
}

func selectionsForTarget(target TargetState) map[string]state.PoolSelection {
	selections := make(map[string]state.PoolSelection, len(target.Selections)+len(target.ProxySelection))
	for pool, selection := range target.Selections {
		selections[pool] = selection
	}
	previous := make(map[string]state.PoolBinding, len(target.PreviousGraph.PoolBindings))
	for _, binding := range target.PreviousGraph.PoolBindings {
		previous[binding.Pool] = binding
	}
	for pool, value := range target.ProxySelection {
		selection := selections[pool]
		if selection.Generation == 0 {
			selection.Generation = previous[pool].Generation
		}
		switch value {
		case "Direct", state.NoCandidateDirect:
			selection.Target = state.NoCandidateDirect
			selection.SmartNode = ""
		case "Block", state.NoCandidateBlock:
			selection.Target = state.NoCandidateBlock
			selection.SmartNode = ""
		default:
			selection.Target = "node"
			selection.SmartNode = value
		}
		selections[pool] = selection
	}
	return selections
}

func protectedIdentities(spec state.SpecState, fallback []state.LastGoodSnapshot, nodes []provider.NodeView) map[string]smartnode.IdentitySource {
	// Provider refreshes deliberately keep SmartNodes referenced by the saved
	// pools so the running topology is not mutated before Apply. Those nodes
	// may be absent from the refreshed snapshot, but a dry-run still needs the
	// identity that is already present in the target Spec. Fresh snapshot data
	// wins when the same ID is available, so revised nodes use current input.
	identities := make(map[string]smartnode.IdentitySource, len(spec.SmartNodes)+len(nodes))
	for id, node := range spec.SmartNodes {
		if id == "" || node.Identity.Protocol == "" {
			continue
		}
		identities[id] = smartnode.IdentitySource{
			RawTag:   smartnode.DisplayName(node),
			Identity: node.Identity,
		}
	}
	for _, snapshot := range fallback {
		for _, source := range snapshot.Identities {
			id, ok := smartnode.ResolveIdentityID(spec.SmartNodes, smartnode.IdentityInput{
				ProviderID: snapshot.ProviderID,
				RawTag:     source.RawTag,
				Identity:   source.Identity,
			})
			if !ok {
				continue
			}
			identities[id] = source
		}
	}
	for _, node := range nodes {
		if node.Excluded || node.ID == "" {
			continue
		}
		identities[node.ID] = smartnode.IdentitySource{
			RawTag: node.Tag, Identity: node.Identity, References: node.References,
		}
	}
	return identities
}

// mergeRequiredLastGood keeps protected identities needed by the previously
// applied execution graph when a provider refresh removes those nodes from its
// newest snapshot. The merged snapshot is persisted with AppliedState so a
// subsequent daemon restart can rebuild the same graph without re-reading the
// old provider payload from a transient fallback only.
func mergeRequiredLastGood(spec state.SpecState, current, fallback []state.LastGoodSnapshot, previous state.ExecutionGraph, proxySelection map[string]string, selections map[string]state.PoolSelection) []state.LastGoodSnapshot {
	needed := map[string]struct{}{}
	for _, executor := range previous.NodeExecutors {
		if executor.SmartNode != "" {
			needed[executor.SmartNode] = struct{}{}
		}
	}
	for _, binding := range previous.PoolBindings {
		if binding.Target.SmartNode != "" {
			needed[binding.Target.SmartNode] = struct{}{}
		}
	}
	for _, id := range proxySelection {
		if id != "" && id != state.NoCandidateDirect && id != state.NoCandidateBlock {
			needed[id] = struct{}{}
		}
	}
	for _, selection := range selections {
		if selection.SmartNode != "" && selection.Target == "node" {
			needed[selection.SmartNode] = struct{}{}
		}
	}
	if len(needed) == 0 || len(fallback) == 0 {
		return cloneLastGood(current)
	}

	merged := cloneLastGood(current)
	locations := make(map[string][2]int)
	for snapshotIndex := range merged {
		for identityIndex := range merged[snapshotIndex].Identities {
			id, ok := resolveRequiredSnapshotIdentityID(spec, merged[snapshotIndex].ProviderID, merged[snapshotIndex].Identities[identityIndex], needed)
			if ok {
				locations[id] = [2]int{snapshotIndex, identityIndex}
			}
		}
	}

	for _, snapshot := range fallback {
		for _, source := range snapshot.Identities {
			id, ok := resolveRequiredSnapshotIdentityID(spec, snapshot.ProviderID, source, needed)
			if !ok {
				continue
			}
			if _, required := needed[id]; !required {
				continue
			}
			if location, exists := locations[id]; exists {
				dst := &merged[location[0]].Identities[location[1]]
				mergeIdentityReferences(dst, source)
				continue
			}
			index := snapshotIndexByProvider(merged, snapshot.ProviderID)
			if index < 0 {
				merged = append(merged, state.LastGoodSnapshot{ProviderID: snapshot.ProviderID})
				index = len(merged) - 1
			}
			merged[index].Identities = append(merged[index].Identities, cloneIdentitySource(source))
			locations[id] = [2]int{index, len(merged[index].Identities) - 1}
		}
	}
	return merged
}

func cloneLastGood(in []state.LastGoodSnapshot) []state.LastGoodSnapshot {
	if len(in) == 0 {
		return nil
	}
	out := make([]state.LastGoodSnapshot, len(in))
	for index, snapshot := range in {
		out[index] = snapshot
		if snapshot.Identities != nil {
			out[index].Identities = make([]smartnode.IdentitySource, len(snapshot.Identities))
			for identityIndex, source := range snapshot.Identities {
				out[index].Identities[identityIndex] = cloneIdentitySource(source)
			}
		}
		if snapshot.Outbounds != nil {
			out[index].Outbounds = append([]map[string]any(nil), snapshot.Outbounds...)
		}
	}
	return out
}

func cloneIdentitySource(source smartnode.IdentitySource) smartnode.IdentitySource {
	out := source
	if source.Identity.Extra != nil {
		out.Identity.Extra = make(map[string]string, len(source.Identity.Extra))
		for key, value := range source.Identity.Extra {
			out.Identity.Extra[key] = value
		}
	}
	if source.Identity.Security.ALPN != nil {
		out.Identity.Security.ALPN = append([]string(nil), source.Identity.Security.ALPN...)
	}
	if source.References != nil {
		out.References = make(map[string]json.RawMessage, len(source.References))
		for key, value := range source.References {
			out.References[key] = append(json.RawMessage(nil), value...)
		}
	}
	return out
}

func resolveSnapshotIdentityID(spec state.SpecState, providerID string, source smartnode.IdentitySource) (string, bool) {
	return smartnode.ResolveIdentityID(spec.SmartNodes, smartnode.IdentityInput{
		ProviderID: providerID,
		RawTag:     source.RawTag,
		Identity:   source.Identity,
	})
}

func resolveRequiredSnapshotIdentityID(spec state.SpecState, providerID string, source smartnode.IdentitySource, needed map[string]struct{}) (string, bool) {
	if id, ok := resolveSnapshotIdentityID(spec, providerID, source); ok {
		return id, true
	}
	// A provider refresh may have removed a still-selected SmartNode from the
	// current catalog. SmartNode IDs are derived from the identity digest, with
	// longer prefixes used only when a short-prefix collision exists. Match the
	// old execution graph's required ID directly so its protected source can be
	// carried into the refreshed snapshot.
	digest := smartnode.IdentityDigest(source.Identity)
	for _, length := range []int{16, 24, 32, 48, 64} {
		id := "sn_" + digest[:length]
		if _, ok := needed[id]; ok {
			return id, true
		}
	}
	return "", false
}

func snapshotIndexByProvider(snapshots []state.LastGoodSnapshot, providerID string) int {
	for index := range snapshots {
		if snapshots[index].ProviderID == providerID {
			return index
		}
	}
	return -1
}

func mergeIdentityReferences(dst *smartnode.IdentitySource, source smartnode.IdentitySource) {
	if dst == nil || len(source.References) == 0 {
		return
	}
	if dst.References == nil {
		dst.References = make(map[string]json.RawMessage, len(source.References))
	}
	for key, value := range source.References {
		if _, exists := dst.References[key]; !exists {
			dst.References[key] = append(json.RawMessage(nil), value...)
		}
	}
}

func applyWorkerCandidateMetadata(graph *state.ExecutionGraph, prepared *worker.Prepared) {
	if graph == nil || prepared == nil {
		return
	}
	for index := range graph.Workers {
		candidate, ok := prepared.Candidates[graph.Workers[index].ID]
		if !ok {
			continue
		}
		graph.Workers[index].ConfigSHA256 = candidate.ConfigSHA256
		graph.Workers[index].Resources.ConfigBytes = candidate.Size
	}
	for index := range prepared.Targets {
		for _, target := range graph.Workers {
			if prepared.Targets[index].ID == target.ID {
				prepared.Targets[index] = target
				break
			}
		}
	}
}

func selectionsFromGraph(graph state.ExecutionGraph) map[string]string {
	out := make(map[string]string, len(graph.PoolBindings))
	for _, binding := range graph.PoolBindings {
		switch binding.Target.Kind {
		case state.NoCandidateDirect:
			out[binding.Pool] = "Direct"
		case state.NoCandidateBlock:
			out[binding.Pool] = "Block"
		default:
			out[binding.Pool] = binding.Target.SmartNode
		}
	}
	return out
}

func copySelections(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for pool, selection := range in {
		out[pool] = selection
	}
	return out
}

func cloneRuleSetRefs(in []state.RuleSetSnapshotRef) []state.RuleSetSnapshotRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]state.RuleSetSnapshotRef, len(in))
	copy(out, in)
	return out
}

func gatewayModeRunsDataPlane(mode string) bool {
	switch state.EffectiveGatewayMode(mode) {
	case state.GatewayModeOn, state.GatewayModeGlobal:
		return true
	default:
		return false
	}
}
