package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

type LastGoodLoader interface {
	Load(providerID string) (LastGoodEnvelope, error)
}

type NodeView struct {
	ID             string                     `json:"id"`
	ProviderID     string                     `json:"provider_id"`
	ProviderName   string                     `json:"provider_name"`
	Roles          []string                   `json:"roles,omitempty"`
	Type           string                     `json:"type"`
	Tag            string                     `json:"tag"`
	Server         string                     `json:"server,omitempty"`
	Port           int                        `json:"port,omitempty"`
	Identity       smartnode.NodeIdentity     `json:"identity"`
	Excluded       bool                       `json:"excluded"`
	ExcludedReason string                     `json:"excluded_reason,omitempty"`
	References     map[string]json.RawMessage `json:"-"`
}

func BuildNodeViews(spec state.SpecState, lastGood LastGoodLoader) ([]NodeView, error) {
	normalized, err := ValidateSpecProviders(spec)
	if err != nil {
		return nil, err
	}
	var snapshots []state.LastGoodSnapshot
	for _, p := range normalized.Providers {
		if !p.Enabled {
			continue
		}
		env, err := lastGood.Load(p.ID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		snapshots = append(snapshots, state.LastGoodSnapshot{
			ProviderID: p.ID, SavedAt: env.SavedAt, Subscription: env.Subscription,
			Identities: env.Identities, Outbounds: env.Outbounds,
		})
	}
	return BuildNodeViewsFromSnapshots(spec, snapshots)
}

// BuildNodeViewsFromSnapshots builds the effective node list from one durable
// snapshot set so dry-run hashing, summaries, and candidate generation observe
// the same provider contents.
func BuildNodeViewsFromSnapshots(spec state.SpecState, snapshots []state.LastGoodSnapshot) ([]NodeView, error) {
	normalized, err := ValidateSpecProviders(spec)
	if err != nil {
		return nil, err
	}
	enabledProviders := make(map[string]struct{}, len(normalized.Providers))
	for _, providerSpec := range normalized.Providers {
		if providerSpec.Enabled {
			enabledProviders[providerSpec.ID] = struct{}{}
		}
	}
	type snapshotNodes struct {
		outbounds  []map[string]any
		identities []smartnode.IdentitySource
	}
	byProvider := make(map[string]snapshotNodes, len(snapshots))
	seenSnapshots := make(map[string]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		if _, exists := seenSnapshots[snapshot.ProviderID]; exists {
			return nil, fmt.Errorf("duplicate last-good snapshot for provider %s", snapshot.ProviderID)
		}
		seenSnapshots[snapshot.ProviderID] = struct{}{}
		if _, enabled := enabledProviders[snapshot.ProviderID]; !enabled {
			continue
		}
		identities := snapshot.Identities
		if identities == nil {
			var err error
			identities, err = identitySourcesFromOutbounds(snapshot.ProviderID, snapshot.Outbounds)
			if err != nil {
				return nil, err
			}
		}
		if len(snapshot.Outbounds) > 0 {
			if err := validateIdentitySources(snapshot.ProviderID, snapshot.Outbounds, identities); err != nil {
				return nil, fmt.Errorf("last-good provider %s: %w", snapshot.ProviderID, err)
			}
		}
		byProvider[snapshot.ProviderID] = snapshotNodes{outbounds: snapshot.Outbounds, identities: identities}
	}
	var nodes []NodeView
	for _, p := range normalized.Providers {
		if !p.Enabled {
			continue
		}
		providerNodes := byProvider[p.ID]
		normalizedNodes, err := normalizeProviderIdentitySources(p, providerNodes.outbounds, providerNodes.identities)
		if err != nil {
			return nil, err
		}
		for _, node := range normalizedNodes {
			nodes = append(nodes, node)
		}
	}
	catalog, _, err := BuildSmartNodeCatalogFromSnapshots(spec, snapshots, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	views := make([]NodeView, 0, len(nodes))
	seen := map[string]int{}
	for _, node := range nodes {
		if !node.Excluded {
			if id, resolved := smartnode.ResolveIdentityID(catalog, smartnode.IdentityInput{
				ProviderID: node.ProviderID,
				RawTag:     node.Tag,
				Identity:   node.Identity,
			}); resolved {
				node.ID = id
			}
		}
		if !node.Excluded {
			if existingIndex, ok := seen[node.ID]; ok {
				views[existingIndex].Roles = mergeStrings(views[existingIndex].Roles, node.Roles)
				continue
			}
			seen[node.ID] = len(views)
		}
		views = append(views, node)
	}
	return views, nil
}

// NormalizeProviderOutbounds is the pre-IR convenience entry retained for
// callers that already own a raw outbound slice. It is deliberately fail-loud;
// the provider/LastGood paths use the error-returning IR entry points below.
func NormalizeProviderOutbounds(p state.ProviderSpec, outbounds []map[string]any) []NodeView {
	identities := make([]smartnode.IdentitySource, len(outbounds))
	for index, outbound := range outbounds {
		if outboundString(outbound, "tag") == "" || outboundString(outbound, "server") == "" {
			continue
		}
		tag := strings.TrimSpace(fmt.Sprint(outbound["tag"]))
		source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{ProviderID: p.ID, RawTag: tag, Outbound: outbound})
		if err != nil {
			panic(fmt.Errorf("normalize provider %s node %q: %w", p.ID, tag, err))
		}
		identities[index] = source
	}
	nodes, err := normalizeProviderIdentitySources(p, outbounds, identities)
	if err != nil {
		panic(err)
	}
	return nodes
}

func normalizeProviderIdentitySources(p state.ProviderSpec, outbounds []map[string]any, identities []smartnode.IdentitySource) ([]NodeView, error) {
	count := len(identities)
	if len(outbounds) > count {
		count = len(outbounds)
	}
	nodes := make([]NodeView, 0, count)
	seenReal := map[string]int{}
	seenExcluded := map[string]int{}
	for index := 0; index < count; index++ {
		var source smartnode.IdentitySource
		if index < len(identities) {
			source = identities[index]
		}
		var clean map[string]any
		if index < len(outbounds) {
			clean = cloneOutbound(outbounds[index])
		}
		tag := outboundString(clean, "tag")
		if tag == "" {
			tag = source.RawTag
		}
		fp := smartnode.IdentityDigest(source.Identity)
		if clean != nil {
			fp = outboundFingerprint(clean)
		}
		typ := strings.ToLower(outboundString(clean, "type"))
		if typ == "" {
			typ = source.Identity.Protocol
		}
		server := outboundString(clean, "server")
		if server == "" {
			server = source.Identity.Server
		}
		port := outboundInt(clean, "server_port")
		if port == 0 {
			port = source.Identity.Port
		}
		node := NodeView{
			ProviderID:   p.ID,
			ProviderName: p.Name,
			Roles:        append([]string(nil), p.Roles...),
			Type:         typ,
			Tag:          tag,
			Server:       server,
			Port:         port,
			Identity:     source.Identity,
			References:   source.References,
		}
		if tag == "" {
			nodes = append(nodes, excludedNode(node, p.ID, fp, "missing_tag", seenExcluded))
			continue
		}
		if server == "" {
			nodes = append(nodes, excludedNode(node, p.ID, fp, "missing_server", seenExcluded))
			continue
		}
		if isPseudoNode(node) {
			nodes = append(nodes, excludedNode(node, p.ID, fp, "pseudo_node", seenExcluded))
			continue
		}
		seenReal[fp]++
		if seenReal[fp] > 1 {
			nodes = append(nodes, excludedNode(node, p.ID, fp, "duplicate", seenExcluded))
			continue
		}
		if !matchesIncludeKeywords(p, node) {
			nodes = append(nodes, excludedNode(node, p.ID, fp, "not_included", seenExcluded))
			continue
		}
		if matchesExcludeKeywords(p, node) {
			nodes = append(nodes, excludedNode(node, p.ID, fp, "excluded_by_filter", seenExcluded))
			continue
		}
		if source.RawTag == "" {
			nodes = append(nodes, excludedNode(node, p.ID, fp, "invalid_semantics", seenExcluded))
			continue
		}
		capability, err := smartnode.EvaluateCapability(source.Identity, smartnode.UnassessedReasons(source.Identity))
		if err != nil {
			return nil, err
		}
		if !hasAdmissibleCore(capability) {
			nodes = append(nodes, excludedNode(node, p.ID, fp, "unsupported_by_registered_cores", seenExcluded))
			continue
		}
		digest := smartnode.IdentityDigest(source.Identity)
		node.ID = "sn_" + digest[:16]
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// LegacyNodeID reproduces the pre-SmartNode node identifier
// (`<provider>:<outbound fingerprint>`) that older releases stored in pool
// bindings and selector state. It exists only so those references can be
// translated once on upgrade; nothing else may mint ids in this form.
func LegacyNodeID(providerID string, outbound map[string]any) string {
	return providerID + ":" + outboundFingerprint(cloneOutbound(outbound))
}

func mergeStrings(left, right []string) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	out := make([]string, 0, len(left)+len(right))
	for _, values := range [][]string{left, right} {
		for _, value := range values {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

func matchesIncludeKeywords(p state.ProviderSpec, node NodeView) bool {
	if len(p.IncludeKeywords) == 0 {
		return true
	}
	haystack := nodeSearchText(node)
	for _, keyword := range p.IncludeKeywords {
		if strings.Contains(haystack, keyword) {
			return true
		}
	}
	return false
}

func matchesExcludeKeywords(p state.ProviderSpec, node NodeView) bool {
	if len(p.ExcludeKeywords) == 0 {
		return false
	}
	haystack := nodeSearchText(node)
	for _, keyword := range p.ExcludeKeywords {
		if strings.Contains(haystack, keyword) {
			return true
		}
	}
	return false
}

func nodeSearchText(node NodeView) string {
	return strings.ToLower(strings.Join([]string{node.Tag, node.Server, node.Type}, " "))
}

func isPseudoNode(node NodeView) bool {
	if strings.EqualFold(node.Server, "notice.invalid") {
		return true
	}
	text := nodeSearchText(node)
	for _, marker := range []string{
		"剩余流量",
		"重置",
		"套餐到期",
		"官网",
		"发布页",
		"客户端",
		"traffic",
		"expire",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func excludedNode(node NodeView, providerID, fp, reason string, seen map[string]int) NodeView {
	key := reason + ":" + fp
	seen[key]++
	node.ID = fmt.Sprintf("%s:%s:%s:%d", providerID, reason, fp, seen[key])
	node.Excluded = true
	node.ExcludedReason = reason
	return node
}

func outboundFingerprint(outbound map[string]any) string {
	b, _ := json.Marshal(outbound)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func cloneOutbound(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func outboundString(outbound map[string]any, key string) string {
	v, ok := outbound[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func outboundInt(outbound map[string]any, key string) int {
	v, ok := outbound[key]
	if !ok || v == nil {
		return 0
	}
	switch typed := v.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	case string:
		var parsed int
		_, _ = fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed)
		return parsed
	default:
		return 0
	}
}
