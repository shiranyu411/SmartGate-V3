package fabric

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

type PreparedFabric struct {
	OpID        string             `json:"op_id"`
	Graph       state.FabricGraph  `json:"graph"`
	DNS         state.DNSSpec      `json:"dns"`
	GatewayMode string             `json:"gateway_mode"`
	Plan        netgate.DryRunPlan `json:"plan"`
	Warnings    []string           `json:"warnings,omitempty"`
}

type FabricApplyResult struct {
	Network netgate.ApplyResult `json:"network"`
	DNS     DNSMetrics          `json:"dns"`
	Applied bool                `json:"applied"`
}

type Manager struct {
	mu       sync.Mutex
	owners   *netgate.OwnerRegistry
	runner   netgate.Runner
	tracker  ConnectionTracker
	ruleSets *RuleSetStore
	dns      *DNSServer
	current  *PreparedFabric
}

func NewManager(owners *netgate.OwnerRegistry, runner netgate.Runner) *Manager {
	if owners == nil {
		owners = netgate.NewOwnerRegistry("smartgate")
	}
	return &Manager{owners: owners, runner: runner, tracker: ShellConnectionTracker{Runner: runner}}
}

func (manager *Manager) WithRuleSetStore(store *RuleSetStore) *Manager {
	manager.mu.Lock()
	manager.ruleSets = store
	manager.mu.Unlock()
	return manager
}

func (manager *Manager) RuleSetProxyAddress() (string, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current == nil {
		return "", fmt.Errorf("Proxy rule-set download requires an applied General Worker path")
	}
	for _, dispatch := range manager.current.Graph.Dispatch {
		if dispatch.Pool != manager.current.Graph.FallbackPool {
			continue
		}
		if !strings.HasPrefix(dispatch.Target, "w_") || dispatch.DNSPort < 1 || dispatch.DNSPort > 65535 {
			return "", fmt.Errorf("applied General binding has no Worker SOCKS path")
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(dispatch.DNSPort)), nil
	}
	return "", fmt.Errorf("applied General binding is missing")
}

func (manager *Manager) WithConnectionTracker(tracker ConnectionTracker) *Manager {
	manager.mu.Lock()
	if tracker == nil {
		tracker = ShellConnectionTracker{Runner: manager.runner}
	}
	manager.tracker = tracker
	manager.mu.Unlock()
	return manager
}

func (manager *Manager) Prepare(ctx context.Context, opID string, spec state.SpecState, graph state.FabricGraph) (PreparedFabric, error) {
	manager.mu.Lock()
	ruleSets := manager.ruleSets
	manager.mu.Unlock()
	var warnings []string
	if ruleSets != nil {
		// Rule-set resolution materializes rule fields before validation.
		// Keep that work private even if the caller reused an active graph.
		graph.Rules = append([]state.FabricRuleGraph(nil), graph.Rules...)
		resolved, resolvedWarnings, err := ruleSets.Resolve(ctx, spec, graph, manager.owners)
		if err != nil {
			return PreparedFabric{}, fmt.Errorf("prepare Gateway Fabric rule sets: %w", err)
		}
		graph = resolved
		warnings = append(warnings, resolvedWarnings...)
	}
	if _, err := ClassifierFromGraph(graph); err != nil {
		return PreparedFabric{}, fmt.Errorf("prepare fabric classifier: %w", err)
	}
	if _, err := BuildDNSUpstreamRouter(spec, graph); err != nil {
		return PreparedFabric{}, fmt.Errorf("prepare fabric DNS routes: %w", err)
	}
	plan, err := BuildDispatchDryRunPlan(opID, manager.owners, graph, DispatchPlanOptions{})
	if err != nil {
		return PreparedFabric{}, err
	}
	return PreparedFabric{
		OpID: opID, Graph: graph, DNS: state.EffectiveDNSSpec(spec.DNS),
		GatewayMode: state.EffectiveGatewayMode(spec.Settings.GatewayMode), Plan: plan, Warnings: warnings,
	}, nil
}

func (manager *Manager) Apply(ctx context.Context, prepared PreparedFabric) (FabricApplyResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	classifier, err := ClassifierFromGraph(prepared.Graph)
	if err != nil {
		return FabricApplyResult{}, err
	}
	resolverSpec := state.SpecState{DNS: prepared.DNS, Settings: state.SettingsSpec{GatewayMode: prepared.GatewayMode}}
	resolver, err := BuildDNSUpstreamRouter(resolverSpec, prepared.Graph)
	if err != nil {
		return FabricApplyResult{}, err
	}
	nftWriter := NftSetWriterForGraph(manager.runner, prepared.Graph)
	exclusive := &ExclusiveSetWriter{Next: nftWriter}
	dnsConfig := DNSConfig{
		Listen: prepared.Graph.DNS.Listen, RejectHTTPSRecords: prepared.Graph.DNS.RejectHTTPSRecords,
		Classifier: classifier, Resolver: resolver, SetWriter: exclusive,
	}
	var started *DNSServer
	if manager.dns == nil {
		started, err = NewDNSServer(dnsConfig)
		if err != nil {
			return FabricApplyResult{}, err
		}
		if err := started.Start(context.WithoutCancel(ctx)); err != nil {
			return FabricApplyResult{}, err
		}
	} else if err := manager.dns.Reconfigure(dnsConfig); err != nil {
		return FabricApplyResult{}, err
	}
	plan := prepared.Plan
	if plan.OpID != prepared.OpID || !plan.DryRunOnly {
		if started != nil {
			_ = started.Shutdown(context.Background())
		}
		return FabricApplyResult{}, fmt.Errorf("fabric plan is not bound to dry-run %s", prepared.OpID)
	}
	plan.DryRunOnly = false
	network, err := netgate.ExecutePlan(ctx, plan, manager.runner)
	if err != nil {
		if started != nil {
			_ = started.Shutdown(context.Background())
		}
		return FabricApplyResult{Network: network}, err
	}
	if started != nil {
		manager.dns = started
	}
	copyPrepared := prepared
	manager.current = &copyPrepared
	return FabricApplyResult{Network: network, DNS: manager.dns.Metrics(), Applied: true}, nil
}

// Verify checks that the host still carries every owned resource of the
// applied Gateway Fabric. fw4 reloads leave the owner table alone, but a
// firewall stop/flush or a manual delete removes it while the embedded DNS
// keeps answering; the supervisor calls this every poll to notice that drift.
func (manager *Manager) Verify(ctx context.Context) error {
	manager.mu.Lock()
	current := manager.current
	runner := manager.runner
	manager.mu.Unlock()
	if current == nil {
		return fmt.Errorf("Gateway Fabric has no active plane")
	}
	return VerifyDispatch(ctx, runner, current.Plan, current.Graph)
}

// Reassert re-executes the applied plan's host steps (owner table, policy
// routes, reconcile hook, dnsmasq override) without restarting the embedded
// DNS server or touching Worker state. Pool set contents rebuild as clients
// resolve names again.
func (manager *Manager) Reassert(ctx context.Context) (netgate.ApplyResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current == nil || manager.dns == nil {
		return netgate.ApplyResult{}, fmt.Errorf("Gateway Fabric has no active plane")
	}
	plan := manager.current.Plan
	if plan.OpID != manager.current.OpID || !plan.DryRunOnly {
		return netgate.ApplyResult{}, fmt.Errorf("fabric plan is not bound to dry-run %s", manager.current.OpID)
	}
	plan.DryRunOnly = false
	return netgate.ExecutePlan(ctx, plan, manager.runner)
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.dns == nil {
		manager.current = nil
		return nil
	}
	err := manager.dns.Shutdown(ctx)
	manager.dns = nil
	manager.current = nil
	return err
}

func (manager *Manager) Metrics() DNSMetrics {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.dns == nil {
		return DNSMetrics{ByTarget: map[string]uint64{}}
	}
	return manager.dns.Metrics()
}

func (manager *Manager) Current() (PreparedFabric, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current == nil {
		return PreparedFabric{}, false
	}
	return *manager.current, true
}

func (manager *Manager) PrepareCutover(ctx context.Context, opID, poolID string, spec state.SpecState, graph state.FabricGraph) (PreparedFabric, error) {
	manager.mu.Lock()
	current := manager.current
	manager.mu.Unlock()
	if current == nil {
		return PreparedFabric{}, fmt.Errorf("Gateway Fabric has no applied graph")
	}
	graph.RuleSetsPinned = len(current.Graph.RuleSets) > 0
	graph.RuleSets = append([]state.RuleSetSnapshotRef(nil), current.Graph.RuleSets...)
	prepared, err := manager.Prepare(ctx, opID, spec, graph)
	if err != nil {
		return PreparedFabric{}, err
	}
	if err := validatePoolTransition(current.Graph, prepared.Graph, poolID, true); err != nil {
		return PreparedFabric{}, err
	}
	return prepared, nil
}

func (manager *Manager) PrepareCompensation(ctx context.Context, opID, poolID string, spec state.SpecState, graph state.FabricGraph) (PreparedFabric, error) {
	manager.mu.Lock()
	current := manager.current
	manager.mu.Unlock()
	if current == nil {
		return PreparedFabric{}, fmt.Errorf("Gateway Fabric has no applied graph")
	}
	graph.RuleSetsPinned = len(current.Graph.RuleSets) > 0
	graph.RuleSets = append([]state.RuleSetSnapshotRef(nil), current.Graph.RuleSets...)
	prepared, err := manager.Prepare(ctx, opID, spec, graph)
	if err != nil {
		return PreparedFabric{}, err
	}
	if err := validatePoolTransition(current.Graph, prepared.Graph, poolID, false); err != nil {
		return PreparedFabric{}, err
	}
	return prepared, nil
}

func (manager *Manager) CountConnections(ctx context.Context, dispatch state.PoolDispatchGraph) (int, error) {
	manager.mu.Lock()
	tracker := manager.tracker
	manager.mu.Unlock()
	if tracker == nil {
		return 0, fmt.Errorf("Fabric connection tracker is not configured")
	}
	return tracker.Count(ctx, dispatch)
}

func (manager *Manager) CloseConnections(ctx context.Context, dispatch state.PoolDispatchGraph) (int, error) {
	manager.mu.Lock()
	tracker := manager.tracker
	manager.mu.Unlock()
	if tracker == nil {
		return 0, fmt.Errorf("Fabric connection tracker is not configured")
	}
	return tracker.Close(ctx, dispatch)
}

func (manager *Manager) ListConnections(ctx context.Context, dispatch state.PoolDispatchGraph) ([]TrackedConnection, error) {
	manager.mu.Lock()
	tracker := manager.tracker
	manager.mu.Unlock()
	ledger, ok := tracker.(ConnectionLedger)
	if !ok {
		return nil, fmt.Errorf("Fabric connection ledger is not configured")
	}
	return ledger.List(ctx, dispatch)
}

func (manager *Manager) CloseConnection(ctx context.Context, dispatch state.PoolDispatchGraph, connection TrackedConnection) (int, error) {
	manager.mu.Lock()
	tracker := manager.tracker
	manager.mu.Unlock()
	ledger, ok := tracker.(ConnectionLedger)
	if !ok {
		return 0, fmt.Errorf("Fabric connection ledger is not configured")
	}
	return ledger.CloseOne(ctx, dispatch, connection)
}

func validatePoolTransition(current, target state.FabricGraph, poolID string, requireAdvance bool) error {
	if poolID == "" {
		return fmt.Errorf("cutover pool is required")
	}
	currentDispatch, currentOK := dispatchForPool(current, poolID)
	targetDispatch, targetOK := dispatchForPool(target, poolID)
	if !currentOK || !targetOK {
		return fmt.Errorf("cutover pool %s is missing from Fabric dispatch", poolID)
	}
	currentCopy := current
	// Pinning controls preparation, not the applied routing semantics.
	currentCopy.RuleSetsPinned = target.RuleSetsPinned
	// Validation must not rewrite the live graph through its shared slice.
	currentCopy.Dispatch = append([]state.PoolDispatchGraph(nil), current.Dispatch...)
	replaceDispatch(&currentCopy, poolID, targetDispatch)
	if !reflect.DeepEqual(currentCopy, target) {
		return fmt.Errorf("cutover for pool %s changes non-target Fabric state", poolID)
	}
	if requireAdvance && (currentDispatch.Binding == targetDispatch.Binding || currentDispatch.Generation >= targetDispatch.Generation) {
		return fmt.Errorf("cutover for pool %s does not advance Binding generation", poolID)
	}
	return nil
}

func dispatchForPool(graph state.FabricGraph, poolID string) (state.PoolDispatchGraph, bool) {
	for _, dispatch := range graph.Dispatch {
		if dispatch.Pool == poolID {
			return dispatch, true
		}
	}
	return state.PoolDispatchGraph{}, false
}

func replaceDispatch(graph *state.FabricGraph, poolID string, replacement state.PoolDispatchGraph) {
	for index := range graph.Dispatch {
		if graph.Dispatch[index].Pool == poolID {
			graph.Dispatch[index] = replacement
			return
		}
	}
}
