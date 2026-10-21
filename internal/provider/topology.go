package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

const (
	MaxDryRunLastGoodBytes = 1 << 20
	MaxDryRunLastGoodNodes = 4096
)

type TopologyResourceLimitError struct{ Reason string }

func (e *TopologyResourceLimitError) Error() string {
	return "last-good topology resource limit exceeded: " + e.Reason
}

type limitHashWriter struct {
	n, max  int
	hash    hash.Hash
	pending []byte
}

func (w *limitHashWriter) Write(p []byte) (int, error) {
	if w.n+len(p) > w.max {
		return 0, &TopologyResourceLimitError{Reason: fmt.Sprintf("encoded topology exceeds %d bytes", w.max)}
	}
	w.n += len(p)
	if len(w.pending) == 1 {
		_, _ = w.hash.Write(w.pending)
	}
	if len(p) > 1 {
		_, _ = w.hash.Write(p[:len(p)-1])
	}
	if len(p) > 0 {
		w.pending = append(w.pending[:0], p[len(p)-1])
	}
	return len(p), nil
}

func (w *limitHashWriter) Sum() []byte {
	if len(w.pending) == 1 && w.pending[0] != '\n' {
		_, _ = w.hash.Write(w.pending)
	}
	return w.hash.Sum(nil)
}

type executionProviderSpec struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Enabled         bool     `json:"enabled"`
	FetchMode       string   `json:"fetch_mode,omitempty"`
	Roles           []string `json:"roles,omitempty"`
	Bootstrap       string   `json:"bootstrap,omitempty"`
	Priority        int      `json:"priority,omitempty"`
	IncludeKeywords []string `json:"include_keywords,omitempty"`
	ExcludeKeywords []string `json:"exclude_keywords,omitempty"`
}

type executionPoolSpec struct {
	ID      string           `json:"id"`
	Role    string           `json:"role,omitempty"`
	NodeIDs []string         `json:"node_ids"`
	Policy  state.PoolPolicy `json:"policy"`
}

type executionSmartNode struct {
	ID         string                 `json:"id"`
	Revision   int                    `json:"revision"`
	Identity   smartnode.NodeIdentity `json:"identity"`
	CoreChoice smartnode.CoreID       `json:"core_choice,omitempty"`
}

type effectiveTopology struct {
	Settings      state.SettingsSpec       `json:"settings"`
	DNS           state.DNSSpec            `json:"dns"`
	Providers     []executionProviderSpec  `json:"providers,omitempty"`
	SmartNodes    []executionSmartNode     `json:"smart_nodes,omitempty"`
	BusinessPools []executionPoolSpec      `json:"business_pools"`
	Rules         state.RuleCatalog        `json:"rules"`
	RemoteRules   state.RemoteRuleSettings `json:"remote_rules"`
	Fabric        state.FabricSpec         `json:"fabric"`
}

func HashEffectiveTopology(ctx context.Context, spec state.SpecState, lastGood *LastGoodStore) (string, error) {
	snapshots, err := LoadLastGoodSnapshotsBounded(ctx, lastGood)
	if err != nil {
		return "", err
	}
	return HashEffectiveTopologyFromSnapshots(spec, snapshots)
}

func LoadLastGoodSnapshotsBounded(ctx context.Context, lastGood *LastGoodStore) ([]state.LastGoodSnapshot, error) {
	entries, err := os.ReadDir(lastGood.layout.LastGoodDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []state.LastGoodSnapshot{}, nil
		}
		return nil, err
	}
	total := int64(0)
	files := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if info.Size() > MaxDryRunLastGoodBytes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("%s is %d bytes", entry.Name(), info.Size())}
		}
		total += info.Size()
		if total > MaxDryRunLastGoodBytes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("aggregate files are %d bytes", total)}
		}
		files = append(files, entry)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	snapshots := make([]state.LastGoodSnapshot, 0, len(files))
	nodes := 0
	actualTotal := 0
	for _, entry := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		providerID := strings.TrimSuffix(entry.Name(), ".json")
		if err := ValidateProviderID(providerID); err != nil {
			return nil, err
		}
		path := filepath.Join(lastGood.layout.LastGoodDir(), entry.Name())
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(file, MaxDryRunLastGoodBytes+1))
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(body) > MaxDryRunLastGoodBytes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("%s exceeds %d bytes", entry.Name(), MaxDryRunLastGoodBytes)}
		}
		actualTotal += len(body)
		if actualTotal > MaxDryRunLastGoodBytes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("aggregate decoded files exceed %d bytes", MaxDryRunLastGoodBytes)}
		}
		env, err := decodeLastGoodBody(providerID, body)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", entry.Name(), err)
		}
		if env.ProviderID != "" && env.ProviderID != providerID {
			return nil, fmt.Errorf("last-good provider id mismatch for %s", entry.Name())
		}
		nodes += len(env.Identities)
		if nodes > MaxDryRunLastGoodNodes {
			return nil, &TopologyResourceLimitError{Reason: fmt.Sprintf("node count exceeds %d", MaxDryRunLastGoodNodes)}
		}
		snapshots = append(snapshots, state.LastGoodSnapshot{
			ProviderID: providerID, SavedAt: env.SavedAt, Subscription: env.Subscription,
			Identities: env.Identities,
		})
	}
	return snapshots, nil
}

// HashEffectiveTopologyFromSnapshots hashes the configuration that would be
// active for spec and the supplied last-good outbounds. Volatile envelope
// metadata such as save times and subscription counters is intentionally not
// included.
func HashEffectiveTopologyFromSnapshots(spec state.SpecState, snapshots []state.LastGoodSnapshot) (string, error) {
	normalized, err := ValidateSpecProviders(state.NormalizeSpecState(spec))
	if err != nil {
		return "", err
	}
	seenProviders := make(map[string]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.ProviderID == "" {
			return "", fmt.Errorf("last-good snapshot provider id is required")
		}
		if _, exists := seenProviders[snapshot.ProviderID]; exists {
			return "", fmt.Errorf("duplicate last-good snapshot for provider %s", snapshot.ProviderID)
		}
		seenProviders[snapshot.ProviderID] = struct{}{}
	}
	normalized, _, err = ReconcileSmartNodes(normalized, snapshots, time.Now().UTC())
	if err != nil {
		return "", err
	}
	topology := executionTopology(normalized)
	stream := &limitHashWriter{max: MaxDryRunLastGoodBytes, hash: sha256.New()}
	if err := json.NewEncoder(stream).Encode(topology); err != nil {
		return "", err
	}
	return hex.EncodeToString(stream.Sum()), nil
}

func HashExecutionSpec(spec state.SpecState) string {
	body, _ := json.Marshal(executionTopology(state.NormalizeSpecState(spec)))
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func executionTopology(spec state.SpecState) effectiveTopology {
	providers := make([]executionProviderSpec, 0, len(spec.Providers))
	for _, item := range spec.Providers {
		providers = append(providers, executionProviderSpec{
			ID:              item.ID,
			Type:            item.Type,
			Enabled:         item.Enabled,
			FetchMode:       item.FetchMode,
			Roles:           append([]string(nil), item.Roles...),
			Bootstrap:       item.Bootstrap,
			Priority:        item.Priority,
			IncludeKeywords: append([]string(nil), item.IncludeKeywords...),
			ExcludeKeywords: append([]string(nil), item.ExcludeKeywords...),
		})
	}
	pools := make([]executionPoolSpec, 0, len(spec.BusinessPools))
	for _, pool := range state.EffectiveBusinessPools(spec.BusinessPools) {
		pools = append(pools, executionPoolSpec{
			ID: pool.ID, Role: pool.Role, NodeIDs: append([]string(nil), pool.NodeIDs...), Policy: pool.Policy,
		})
	}
	ids := make([]string, 0, len(spec.SmartNodes))
	for id := range spec.SmartNodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	nodes := make([]executionSmartNode, 0, len(ids))
	for _, id := range ids {
		node := spec.SmartNodes[id]
		nodes = append(nodes, executionSmartNode{ID: id, Revision: node.Revision, Identity: node.Identity, CoreChoice: node.CoreChoice})
	}
	return effectiveTopology{
		Settings:      spec.Settings,
		DNS:           spec.DNS,
		Providers:     providers,
		SmartNodes:    nodes,
		BusinessPools: pools,
		Rules:         spec.Rules,
		RemoteRules:   spec.RemoteRules,
		Fabric:        spec.Fabric,
	}
}
