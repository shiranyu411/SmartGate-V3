package smartnode

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type pendingNode struct {
	providerID string
	rawTag     string
	normalized normalizedNode
}

func Reconcile(existing map[string]SmartNode, rawNodes []RawNode, now time.Time) (map[string]SmartNode, ReconcileReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	pending := make([]pendingNode, 0, len(rawNodes))
	for _, raw := range rawNodes {
		if raw.SeenAt.IsZero() {
			raw.SeenAt = now
		}
		normalized, err := normalize(raw)
		if err != nil {
			return nil, ReconcileReport{}, err
		}
		pending = append(pending, pendingNode{providerID: raw.ProviderID, rawTag: raw.RawTag, normalized: normalized})
	}
	return reconcilePending(existing, pending)
}

// ReconcileIdentityInputs reconciles the catalog from the platform IR. It is
// the provider path once a snapshot contains the identities field.
func ReconcileIdentityInputs(existing map[string]SmartNode, inputs []IdentityInput, now time.Time) (map[string]SmartNode, ReconcileReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	pending := make([]pendingNode, 0, len(inputs))
	for _, input := range inputs {
		normalized, err := normalizeIdentityInput(input, now)
		if err != nil {
			return nil, ReconcileReport{}, err
		}
		pending = append(pending, pendingNode{providerID: input.ProviderID, rawTag: input.RawTag, normalized: normalized})
	}
	return reconcilePending(existing, pending)
}

func normalizeIdentityInput(input IdentityInput, now time.Time) (normalizedNode, error) {
	providerID := strings.TrimSpace(input.ProviderID)
	rawTag := strings.TrimSpace(input.RawTag)
	if providerID == "" {
		return normalizedNode{}, fmt.Errorf("smartnode provider id is required")
	}
	if rawTag == "" {
		return normalizedNode{}, fmt.Errorf("smartnode alias tag is required")
	}
	identity := input.Identity
	identity.Extra = cloneStringMap(identity.Extra)
	identity.Security.ALPN = append([]string(nil), identity.Security.ALPN...)
	if identity.Protocol == "" {
		return normalizedNode{}, fmt.Errorf("smartnode protocol is required")
	}
	if identity.Server == "" {
		return normalizedNode{}, fmt.Errorf("smartnode server is required")
	}
	if identity.Port < 0 || identity.Port > 65535 {
		return normalizedNode{}, fmt.Errorf("smartnode port must be between 0 and 65535")
	}
	seenAt := input.SeenAt.UTC()
	if seenAt.IsZero() {
		seenAt = now
	}
	digest := IdentityDigest(identity)
	node := SmartNode{
		ID:         "sn_" + digest[:16],
		Revision:   1,
		CoreChoice: DefaultCoreChoice(),
		Identity:   identity,
		Aliases: []NodeAlias{{
			ProviderID: providerID,
			RawTag:     rawTag,
			FirstSeen:  seenAt,
			LastSeen:   seenAt,
		}},
	}
	capability, err := EvaluateCapability(identity, UnassessedReasons(identity))
	if err != nil {
		return normalizedNode{}, err
	}
	node.Capability = capability
	return normalizedNode{Node: node, Digest: digest, Unassessed: UnassessedReasons(identity)}, nil
}

func reconcilePending(existing map[string]SmartNode, pending []pendingNode) (map[string]SmartNode, ReconcileReport, error) {
	existing, _ = WithDefaultCoreChoices(existing)
	catalog := cloneCatalog(existing)
	identityIndex := map[string]string{}
	aliasIndex := map[string]string{}
	ids := make([]string, 0, len(catalog))
	for id := range catalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := catalog[id]
		if node.Revision < 1 {
			node.Revision = 1
		}
		catalog[id] = node
		digest := IdentityDigest(node.Identity)
		if _, exists := identityIndex[digest]; !exists {
			identityIndex[digest] = id
		}
		for _, alias := range node.Aliases {
			aliasIndex[aliasKey(alias.ProviderID, alias.RawTag)] = id
		}
	}

	sort.SliceStable(pending, func(i, j int) bool {
		left, right := pending[i], pending[j]
		if left.providerID != right.providerID {
			return left.providerID < right.providerID
		}
		if left.rawTag != right.rawTag {
			return left.rawTag < right.rawTag
		}
		return left.normalized.Digest < right.normalized.Digest
	})

	report := ReconcileReport{}
	for _, item := range pending {
		candidate := item.normalized.Node
		id := identityIndex[item.normalized.Digest]
		if id == "" {
			id = aliasIndex[aliasKey(item.providerID, item.rawTag)]
		}
		if id == "" {
			id = uniqueNodeID(candidate.ID, item.normalized.Digest, catalog)
			candidate.ID = id
			catalog[id] = candidate
			identityIndex[item.normalized.Digest] = id
			for _, alias := range candidate.Aliases {
				aliasIndex[aliasKey(alias.ProviderID, alias.RawTag)] = id
			}
			report.Added = append(report.Added, id)
			continue
		}

		current := catalog[id]
		if current.ID == "" {
			current.ID = id
		}
		beforeDigest := IdentityDigest(current.Identity)
		aliasesChanged := false
		for _, alias := range candidate.Aliases {
			var changed bool
			current.Aliases, changed = mergeAlias(current.Aliases, alias)
			aliasesChanged = aliasesChanged || changed
			aliasIndex[aliasKey(alias.ProviderID, alias.RawTag)] = id
		}
		if beforeDigest != item.normalized.Digest {
			delete(identityIndex, beforeDigest)
			current.Identity = candidate.Identity
			current.Capability = candidate.Capability
			if current.Revision < 1 {
				current.Revision = 1
			}
			current.Revision++
			identityIndex[item.normalized.Digest] = id
			report.Revised = append(report.Revised, id)
		} else {
			current.Capability = candidate.Capability
			identityIndex[item.normalized.Digest] = id
			if aliasesChanged {
				report.MetadataOnly = append(report.MetadataOnly, id)
			}
		}
		current.Aliases = sortedAliases(current.Aliases)
		catalog[id] = current
	}
	report.Added = uniqueSorted(report.Added)
	report.Revised = uniqueSorted(report.Revised)
	report.MetadataOnly = uniqueSorted(report.MetadataOnly)
	return catalog, report, nil
}

func ResolveID(catalog map[string]SmartNode, raw RawNode) (string, bool) {
	normalized, err := normalize(raw)
	if err != nil {
		return "", false
	}
	return resolveIdentityID(catalog, normalized.Digest, raw.ProviderID, raw.RawTag)
}

func ResolveIdentityID(catalog map[string]SmartNode, input IdentityInput) (string, bool) {
	if strings.TrimSpace(input.ProviderID) == "" || strings.TrimSpace(input.RawTag) == "" || input.Identity.Protocol == "" {
		return "", false
	}
	return resolveIdentityID(catalog, IdentityDigest(input.Identity), input.ProviderID, input.RawTag)
}

func resolveIdentityID(catalog map[string]SmartNode, digest, providerID, rawTag string) (string, bool) {
	ids := make([]string, 0, len(catalog))
	for id := range catalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if IdentityDigest(catalog[id].Identity) == digest {
			return id, true
		}
	}
	for _, id := range ids {
		for _, alias := range catalog[id].Aliases {
			if alias.ProviderID == providerID && alias.RawTag == rawTag {
				return id, true
			}
		}
	}
	return "", false
}

func DisplayName(node SmartNode) string {
	if len(node.Aliases) == 0 {
		return node.ID
	}
	aliases := append([]NodeAlias(nil), node.Aliases...)
	sort.SliceStable(aliases, func(i, j int) bool {
		if !aliases[i].LastSeen.Equal(aliases[j].LastSeen) {
			return aliases[i].LastSeen.After(aliases[j].LastSeen)
		}
		if aliases[i].ProviderID != aliases[j].ProviderID {
			return aliases[i].ProviderID < aliases[j].ProviderID
		}
		return aliases[i].RawTag < aliases[j].RawTag
	})
	return aliases[0].RawTag
}

func cloneCatalog(existing map[string]SmartNode) map[string]SmartNode {
	out := make(map[string]SmartNode, len(existing))
	for id, node := range existing {
		node.Aliases = append([]NodeAlias(nil), node.Aliases...)
		node.Identity.Extra = cloneStringMap(node.Identity.Extra)
		node.Identity.Security.ALPN = append([]string(nil), node.Identity.Security.ALPN...)
		capability := make(CapabilityMatrix, len(node.Capability))
		for core, result := range node.Capability {
			result.Reasons = append([]string(nil), result.Reasons...)
			result.ReasonTexts = append([]string(nil), result.ReasonTexts...)
			capability[core] = result
		}
		node.Capability = capability
		out[id] = node
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func mergeAlias(aliases []NodeAlias, incoming NodeAlias) ([]NodeAlias, bool) {
	for index := range aliases {
		if aliases[index].ProviderID != incoming.ProviderID || aliases[index].RawTag != incoming.RawTag {
			continue
		}
		changed := false
		if aliases[index].FirstSeen.IsZero() || incoming.FirstSeen.Before(aliases[index].FirstSeen) {
			aliases[index].FirstSeen = incoming.FirstSeen
			changed = true
		}
		if incoming.LastSeen.After(aliases[index].LastSeen) {
			aliases[index].LastSeen = incoming.LastSeen
			changed = true
		}
		return aliases, changed
	}
	return append(aliases, incoming), true
}

func sortedAliases(aliases []NodeAlias) []NodeAlias {
	out := append([]NodeAlias(nil), aliases...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].RawTag < out[j].RawTag
	})
	return out
}

func uniqueNodeID(shortID, digest string, catalog map[string]SmartNode) string {
	if existing, ok := catalog[shortID]; !ok || IdentityDigest(existing.Identity) == digest {
		return shortID
	}
	for _, length := range []int{24, 32, 48, 64} {
		id := "sn_" + digest[:length]
		if existing, ok := catalog[id]; !ok || IdentityDigest(existing.Identity) == digest {
			return id
		}
	}
	panic(fmt.Sprintf("sha256 collision for SmartNode %s", shortID))
}

func aliasKey(providerID, rawTag string) string {
	return providerID + "\x00" + rawTag
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	return uniqueStrings(values)
}
