package provider

import (
	"fmt"
	"sort"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func BuildSmartNodeCatalogFromSnapshots(spec state.SpecState, snapshots []state.LastGoodSnapshot, now time.Time) (map[string]smartnode.SmartNode, smartnode.ReconcileReport, error) {
	identityInputs, err := smartNodeInputs(spec, snapshots, now)
	if err != nil {
		return nil, smartnode.ReconcileReport{}, err
	}
	catalog, report, err := smartnode.ReconcileIdentityInputs(spec.SmartNodes, identityInputs, now)
	if err != nil {
		return nil, smartnode.ReconcileReport{}, err
	}
	seen := make(map[string]struct{}, len(identityInputs))
	for _, input := range identityInputs {
		if id, ok := smartnode.ResolveIdentityID(catalog, input); ok {
			seen[id] = struct{}{}
		}
	}
	referenced := referencedSmartNodes(spec.BusinessPools)
	for id := range catalog {
		if _, ok := seen[id]; ok {
			continue
		}
		if _, ok := referenced[id]; ok {
			continue
		}
		delete(catalog, id)
		report.Removed = append(report.Removed, id)
	}
	sort.Strings(report.Removed)
	return catalog, report, nil
}

func ReconcileSmartNodes(spec state.SpecState, snapshots []state.LastGoodSnapshot, now time.Time) (state.SpecState, smartnode.ReconcileReport, error) {
	catalog, report, err := BuildSmartNodeCatalogFromSnapshots(spec, snapshots, now)
	if err != nil {
		return spec, smartnode.ReconcileReport{}, err
	}
	spec.SmartNodes = catalog
	return spec, report, nil
}

func SmartNodeIDsInProviderOrder(spec state.SpecState, snapshots []state.LastGoodSnapshot, catalog map[string]smartnode.SmartNode) ([]string, error) {
	inputs, err := smartNodeInputs(spec, snapshots, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(catalog))
	seen := make(map[string]bool, len(catalog))
	for _, input := range inputs {
		id, ok := smartnode.ResolveIdentityID(catalog, input)
		if ok && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	var missing []string
	for id := range catalog {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	sort.Slice(missing, func(i, j int) bool {
		left, right := smartnode.DisplayName(catalog[missing[i]]), smartnode.DisplayName(catalog[missing[j]])
		if left != right {
			return left < right
		}
		return missing[i] < missing[j]
	})
	return append(ids, missing...), nil
}

func smartNodeInputs(spec state.SpecState, snapshots []state.LastGoodSnapshot, now time.Time) ([]smartnode.IdentityInput, error) {
	byProvider := make(map[string]state.LastGoodSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if _, exists := byProvider[snapshot.ProviderID]; exists {
			return nil, fmt.Errorf("duplicate last-good snapshot for provider %s", snapshot.ProviderID)
		}
		byProvider[snapshot.ProviderID] = snapshot
	}
	normalized, err := ValidateSpecProviders(spec)
	if err != nil {
		return nil, err
	}
	var inputs []smartnode.IdentityInput
	for _, providerSpec := range normalized.Providers {
		if !providerSpec.Enabled {
			continue
		}
		snapshot := byProvider[providerSpec.ID]
		seenAt := snapshot.SavedAt
		if seenAt.IsZero() {
			seenAt = now
		}
		identities := snapshot.Identities
		if identities == nil {
			var err error
			identities, err = identitySourcesFromOutbounds(providerSpec.ID, snapshot.Outbounds)
			if err != nil {
				return nil, err
			}
		}
		if len(snapshot.Outbounds) > 0 {
			if err := validateIdentitySources(providerSpec.ID, snapshot.Outbounds, identities); err != nil {
				return nil, fmt.Errorf("last-good provider %s: %w", providerSpec.ID, err)
			}
		}
		nodes, err := normalizeProviderIdentitySources(providerSpec, snapshot.Outbounds, identities)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			if node.Excluded {
				continue
			}
			inputs = append(inputs, smartnode.IdentityInput{
				ProviderID: providerSpec.ID,
				RawTag:     node.Tag,
				Identity:   node.Identity,
				SeenAt:     seenAt,
			})
		}
	}
	return inputs, nil
}

func referencedSmartNodes(pools []state.BusinessPoolSpec) map[string]struct{} {
	out := map[string]struct{}{}
	for _, pool := range state.EffectiveBusinessPools(pools) {
		for _, id := range pool.NodeIDs {
			out[id] = struct{}{}
		}
	}
	return out
}

func reportHasCatalogChanges(report smartnode.ReconcileReport) bool {
	return len(report.Added) > 0 || len(report.Revised) > 0 || len(report.MetadataOnly) > 0 || len(report.Removed) > 0
}
