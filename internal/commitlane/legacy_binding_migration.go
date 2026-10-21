package commitlane

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/provider"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

// Pre-SmartNode releases identified a node as `<provider>:<16 hex outbound
// fingerprint>`. SmartNode ids never contain a colon, so this shape alone marks
// a reference that must be translated before the planner sees it.
var legacyNodeIDPattern = regexp.MustCompile(`^[^:\s]+:[0-9a-f]{16}$`)

const (
	legacyMigrationScopeSpecPool         = "spec_pool"
	legacyMigrationScopeAppliedPool      = "applied_pool"
	legacyMigrationScopeAppliedSelection = "applied_selection"
	legacyMigrationScopeSelection        = "selection"
	legacyMigrationScopeRuntime          = "runtime_selection"
	legacyMigrationScopeNodeHealth       = "node_health"
	legacyMigrationScopeFailover         = "failover"
)

// WithLegacyCore names the execution core the previous release ran every node
// on. Migrated SmartNodes default to it when it admits them, so bindings that
// worked before the upgrade keep working without a manual core choice.
func (r *RecoveryOrchestrator) WithLegacyCore(core smartnode.CoreID) *RecoveryOrchestrator {
	if r != nil {
		r.legacyCore = core
	}
	return r
}

// MigrateLegacyBindings translates pre-SmartNode pool bindings and selector
// choices into SmartNode references before any recovery step builds an
// execution graph from them. It is idempotent: once no legacy reference
// remains it returns (nil, nil). References that cannot be matched to a
// last-good node are dropped, recorded in the durable report, and surfaced as
// warnings instead of blocking startup.
func (r *RecoveryOrchestrator) MigrateLegacyBindings(ctx context.Context) (*state.LegacyBindingMigrationReport, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	var report *state.LegacyBindingMigrationReport
	_, err := r.config.commit.WithCommit(ctx, CommitRequest{OpID: "legacy-binding-migration", Kind: "recovery", StartedAt: time.Now().UTC()}, func(session *lockset.Session) error {
		for _, level := range []lockset.Level{lockset.LevelStore, lockset.LevelSnapshot, lockset.LevelRuntime} {
			release, err := session.Acquire(level)
			if err != nil {
				return err
			}
			defer release()
		}
		var err error
		report, err = migrateLegacyBindingsLocked(ctx, r.config.commit.Layout(), r.config.store, r.legacyCore, time.Now().UTC())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("migrate legacy node bindings: %w", err)
	}
	return report, nil
}

func migrateLegacyBindingsLocked(ctx context.Context, layout state.Layout, store *state.Store, legacyCore smartnode.CoreID, now time.Time) (*state.LegacyBindingMigrationReport, error) {
	spec, err := store.LoadSpec()
	if err != nil {
		return nil, err
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		return nil, err
	}
	selection, err := store.LoadSelectionState()
	if err != nil {
		return nil, err
	}
	runtimeState, err := store.LoadRuntime()
	if err != nil {
		return nil, err
	}
	failoverStore := state.NewFailoverStateStore(layout)
	failover, err := failoverStore.Load()
	if err != nil {
		return nil, err
	}
	if !hasLegacyNodeReferences(spec, applied, selection, runtimeState, failover) {
		return nil, nil
	}

	lastGoodStore := provider.NewLastGoodStore(layout)
	envelopes, err := lastGoodStore.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	index := legacyIdentityIndex{}
	providerIDs := make([]string, 0, len(envelopes))
	for providerID := range envelopes {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	for _, providerID := range providerIDs {
		index.add(providerID, envelopes[providerID].Outbounds, envelopes[providerID].Identities)
	}
	for _, snapshot := range applied.LastGood {
		index.add(snapshot.ProviderID, snapshot.Outbounds, snapshot.Identities)
	}

	report := &state.LegacyBindingMigrationReport{SchemaVersion: state.LegacyBindingMigrationSchemaVersion, MigratedAt: now}

	snapshots, err := provider.LoadLastGoodSnapshotsBounded(ctx, lastGoodStore)
	if err != nil {
		return nil, err
	}
	spec, _, err = provider.ReconcileSmartNodes(spec, snapshots, now)
	if err != nil {
		return nil, fmt.Errorf("build SmartNode catalog from last-good: %w", err)
	}
	assignMigratedCores(spec.SmartNodes, legacyCore)
	resolveSpec := index.resolver(spec.SmartNodes)
	spec.BusinessPools = migrateLegacyPoolBindings(spec.BusinessPools, resolveSpec, report, legacyMigrationScopeSpecPool)

	resolveApplied := resolveSpec
	appliedCaptured := applied.SchemaVersion != 0 && applied.SpecCaptured
	if appliedCaptured {
		appliedSnapshots, err := provider.NormalizeLastGoodSnapshots(applied.LastGood)
		if err != nil {
			return nil, err
		}
		applied.Spec, _, err = provider.ReconcileSmartNodes(applied.Spec, appliedSnapshots, now)
		if err != nil {
			return nil, fmt.Errorf("build SmartNode catalog from applied last-good: %w", err)
		}
		assignMigratedCores(applied.Spec.SmartNodes, legacyCore)
		resolveApplied = index.resolver(applied.Spec.SmartNodes)
		applied.Spec.BusinessPools = migrateLegacyPoolBindings(applied.Spec.BusinessPools, resolveApplied, report, legacyMigrationScopeAppliedPool)
		applied.ProxySelection = migrateLegacySelectionMap(applied.ProxySelection, resolveApplied, report, legacyMigrationScopeAppliedSelection)
	}
	if selection.SchemaVersion != 0 {
		selection.ProxySelection = migrateLegacySelectionMap(selection.ProxySelection, resolveApplied, report, legacyMigrationScopeSelection)
		selection.Selections = migrateLegacyPoolSelections(selection.Selections, resolveApplied, report)
	}
	runtimeState.ProxySelection = migrateLegacySelectionMap(runtimeState.ProxySelection, resolveApplied, report, legacyMigrationScopeRuntime)
	runtimeState.NodeHealth = migrateLegacyNodeHealth(runtimeState.NodeHealth, resolveApplied, report)
	failover.Pools = migrateLegacyFailoverPools(failover.Pools, resolveApplied, report)
	sort.Slice(report.Resolved, func(i, j int) bool {
		if report.Resolved[i].LegacyID != report.Resolved[j].LegacyID {
			return report.Resolved[i].LegacyID < report.Resolved[j].LegacyID
		}
		return report.Resolved[i].SmartNode < report.Resolved[j].SmartNode
	})

	if _, err := store.SaveSpec(spec); err != nil {
		return nil, err
	}
	if appliedCaptured {
		if err := store.SaveAppliedState(applied); err != nil {
			return nil, err
		}
	}
	if selection.SchemaVersion != 0 {
		if err := store.SaveSelectionState(selection); err != nil {
			return nil, err
		}
	}
	if err := store.SaveRuntime(runtimeState); err != nil {
		return nil, err
	}
	if err := failoverStore.Save(failover); err != nil {
		return nil, err
	}
	if err := store.SaveLegacyMigrationReport(*report); err != nil {
		return nil, err
	}
	return report, nil
}

func isLegacyNodeID(id string, catalogs ...map[string]smartnode.SmartNode) bool {
	if id == "" || id == "Direct" || id == "Block" {
		return false
	}
	for _, catalog := range catalogs {
		if _, ok := catalog[id]; ok {
			return false
		}
	}
	return legacyNodeIDPattern.MatchString(id)
}

func hasLegacyNodeReferences(spec state.SpecState, applied state.AppliedState, selection state.SelectionState, runtimeState state.RuntimeState, failover state.FailoverState) bool {
	catalogs := []map[string]smartnode.SmartNode{spec.SmartNodes, applied.Spec.SmartNodes}
	legacy := func(id string) bool { return isLegacyNodeID(id, catalogs...) }
	for _, pools := range [][]state.BusinessPoolSpec{spec.BusinessPools, applied.Spec.BusinessPools} {
		for _, pool := range pools {
			for _, id := range pool.NodeIDs {
				if legacy(id) {
					return true
				}
			}
		}
	}
	for _, selections := range []map[string]string{applied.ProxySelection, selection.ProxySelection, runtimeState.ProxySelection} {
		for _, target := range selections {
			if legacy(target) {
				return true
			}
		}
	}
	for _, item := range selection.Selections {
		if legacy(item.SmartNode) {
			return true
		}
	}
	for id := range runtimeState.NodeHealth {
		if legacy(id) {
			return true
		}
	}
	for _, pool := range failover.Pools {
		if legacy(pool.Current) || legacy(pool.Previous) {
			return true
		}
	}
	return false
}

type legacyIdentityEntry struct {
	providerID string
	source     smartnode.IdentitySource
}

// legacyIdentityIndex maps legacy `<provider>:<fingerprint>` ids to the
// platform identity of the same last-good outbound. The fingerprint is only
// reproducible while the raw outbound is still on disk, which is why the
// migration runs before any recovery step rewrites last-good files.
type legacyIdentityIndex map[string]legacyIdentityEntry

func (index legacyIdentityIndex) add(providerID string, outbounds []map[string]any, identities []smartnode.IdentitySource) {
	if len(outbounds) == 0 || len(outbounds) != len(identities) {
		return
	}
	for position, outbound := range outbounds {
		legacyID := provider.LegacyNodeID(providerID, outbound)
		if _, exists := index[legacyID]; exists {
			continue
		}
		index[legacyID] = legacyIdentityEntry{providerID: providerID, source: identities[position]}
	}
}

// legacyNodeResolver maps a legacy id to a SmartNode the planner can bind: the
// node must exist in the catalog and have an execution core. Nodes without an
// admissible core are reported as unresolved so they never block startup.
type legacyNodeResolver func(legacyID string) (string, smartnode.CoreID, bool)

func (index legacyIdentityIndex) resolver(catalog map[string]smartnode.SmartNode) legacyNodeResolver {
	return func(legacyID string) (string, smartnode.CoreID, bool) {
		entry, ok := index[legacyID]
		if !ok {
			return "", "", false
		}
		id, ok := smartnode.ResolveIdentityID(catalog, smartnode.IdentityInput{
			ProviderID: entry.providerID, RawTag: entry.source.RawTag, Identity: entry.source.Identity,
		})
		if !ok {
			return "", "", false
		}
		core := catalog[id].CoreChoice
		if core == "" {
			return id, "", false
		}
		return id, core, true
	}
}

// assignMigratedCores gives every migrated SmartNode a core so the planner can
// bind it. The legacy core wins when it admits the node; otherwise the first
// registered core (by id) that supports it, then one that runs it degraded.
func assignMigratedCores(catalog map[string]smartnode.SmartNode, legacyCore smartnode.CoreID) {
	for id, node := range catalog {
		if node.CoreChoice != "" {
			continue
		}
		node.CoreChoice = migratedCoreChoice(node.Capability, legacyCore)
		catalog[id] = node
	}
}

func migratedCoreChoice(matrix smartnode.CapabilityMatrix, legacyCore smartnode.CoreID) smartnode.CoreID {
	admissible := func(status string) bool {
		return status == smartnode.CapabilitySupported || status == smartnode.CapabilityDegraded
	}
	if legacyCore != "" {
		if capability, ok := matrix.ForCore(legacyCore); ok && admissible(capability.Status) {
			return legacyCore
		}
	}
	cores := make([]string, 0, len(matrix))
	for core := range matrix {
		cores = append(cores, string(core))
	}
	sort.Strings(cores)
	for _, status := range []string{smartnode.CapabilitySupported, smartnode.CapabilityDegraded} {
		for _, core := range cores {
			if matrix[smartnode.CoreID(core)].Status == status {
				return smartnode.CoreID(core)
			}
		}
	}
	return ""
}

func migrateLegacyPoolBindings(pools []state.BusinessPoolSpec, resolve legacyNodeResolver, report *state.LegacyBindingMigrationReport, scope string) []state.BusinessPoolSpec {
	out := append([]state.BusinessPoolSpec(nil), pools...)
	for index := range out {
		pool := &out[index]
		if len(pool.NodeIDs) == 0 {
			continue
		}
		next := make([]string, 0, len(pool.NodeIDs))
		seen := make(map[string]struct{}, len(pool.NodeIDs))
		for _, id := range pool.NodeIDs {
			target := id
			if isLegacyNodeID(id) {
				resolved, core, ok := resolve(id)
				if !ok {
					report.Unresolved = append(report.Unresolved, state.LegacyBindingReference{Scope: scope, Pool: pool.ID, LegacyID: id})
					if resolved == "" {
						report.Warnings = appendUniqueString(report.Warnings, fmt.Sprintf("Pool %s 的旧节点 %s 无法匹配到任何 SmartNode，已解除绑定；请在节点页重新选择", pool.ID, id))
					} else {
						report.Warnings = appendUniqueString(report.Warnings, fmt.Sprintf("Pool %s 的旧节点 %s 对应 SmartNode %s，但没有任何已注册核心可以执行，已解除绑定", pool.ID, id, resolved))
					}
					continue
				}
				recordLegacyResolution(report, id, resolved, core)
				target = resolved
			}
			if _, duplicate := seen[target]; duplicate {
				continue
			}
			seen[target] = struct{}{}
			next = append(next, target)
		}
		if len(next) == 0 {
			next = nil
			report.Warnings = appendUniqueString(report.Warnings, fmt.Sprintf("Pool %s 迁移后没有可用节点，将按其无候选策略运行", pool.ID))
		}
		pool.NodeIDs = next
	}
	return out
}

func migrateLegacySelectionMap(selections map[string]string, resolve legacyNodeResolver, report *state.LegacyBindingMigrationReport, scope string) map[string]string {
	if len(selections) == 0 {
		return selections
	}
	out := make(map[string]string, len(selections))
	for pool, target := range selections {
		if !isLegacyNodeID(target) {
			out[pool] = target
			continue
		}
		resolved, core, ok := resolve(target)
		if !ok {
			report.Unresolved = append(report.Unresolved, state.LegacyBindingReference{Scope: scope, Pool: pool, LegacyID: target})
			continue
		}
		recordLegacyResolution(report, target, resolved, core)
		out[pool] = resolved
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func migrateLegacyPoolSelections(selections map[string]state.PoolSelection, resolve legacyNodeResolver, report *state.LegacyBindingMigrationReport) map[string]state.PoolSelection {
	if len(selections) == 0 {
		return selections
	}
	out := make(map[string]state.PoolSelection, len(selections))
	for pool, item := range selections {
		if !isLegacyNodeID(item.SmartNode) {
			out[pool] = item
			continue
		}
		resolved, core, ok := resolve(item.SmartNode)
		if !ok {
			report.Unresolved = append(report.Unresolved, state.LegacyBindingReference{Scope: legacyMigrationScopeSelection, Pool: pool, LegacyID: item.SmartNode})
			continue
		}
		recordLegacyResolution(report, item.SmartNode, resolved, core)
		item.SmartNode = resolved
		out[pool] = item
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func migrateLegacyNodeHealth(health map[string]state.NodeHealth, resolve legacyNodeResolver, report *state.LegacyBindingMigrationReport) map[string]state.NodeHealth {
	if len(health) == 0 {
		return health
	}
	out := make(map[string]state.NodeHealth, len(health))
	for id, item := range health {
		if !isLegacyNodeID(id) {
			out[id] = item
			continue
		}
		resolved, core, ok := resolve(id)
		if !ok {
			report.Warnings = appendUniqueString(report.Warnings, fmt.Sprintf("旧节点 %s 的健康记录无法匹配到 SmartNode，已丢弃历史记录", id))
			continue
		}
		recordLegacyResolution(report, id, resolved, core)
		if _, exists := out[resolved]; exists {
			continue
		}
		out[resolved] = item
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func migrateLegacyFailoverPools(pools map[string]state.FailoverPoolState, resolve legacyNodeResolver, report *state.LegacyBindingMigrationReport) map[string]state.FailoverPoolState {
	if len(pools) == 0 {
		return pools
	}
	out := make(map[string]state.FailoverPoolState, len(pools))
	for poolID, pool := range pools {
		for _, field := range []*string{&pool.Current, &pool.Previous} {
			if !isLegacyNodeID(*field) {
				continue
			}
			resolved, core, ok := resolve(*field)
			if !ok {
				report.Unresolved = append(report.Unresolved, state.LegacyBindingReference{Scope: legacyMigrationScopeFailover, Pool: poolID, LegacyID: *field})
				*field = ""
				continue
			}
			recordLegacyResolution(report, *field, resolved, core)
			*field = resolved
		}
		if pool.Current == "" {
			pool.CooldownUntil = nil
		}
		out[poolID] = pool
	}
	return out
}

func recordLegacyResolution(report *state.LegacyBindingMigrationReport, legacyID, smartNode string, core smartnode.CoreID) {
	for _, existing := range report.Resolved {
		if existing.LegacyID == legacyID && existing.SmartNode == smartNode {
			return
		}
	}
	report.Resolved = append(report.Resolved, state.LegacyBindingResolution{LegacyID: legacyID, SmartNode: smartNode, Core: core})
}

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
