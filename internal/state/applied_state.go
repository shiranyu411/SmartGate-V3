package state

import (
	"errors"
	"fmt"
	"os"
	"time"

	"smartgate/internal/smartnode"
)

const AppliedStateSchemaVersion = 2

// AppliedState is the durable control-plane description of the configuration
// that is expected to be active in the data plane and OpenWrt. Ephemeral health,
// scheduler and log data remains in RuntimeState under RuntimeDir.
type AppliedState struct {
	SchemaVersion  int                `json:"schema_version"`
	ConfigEpoch    int64              `json:"config_epoch"`
	TopologyHash   string             `json:"topology_hash,omitempty"`
	GatewayMode    string             `json:"gateway_mode,omitempty"`
	ConfigSHA256   string             `json:"config_sha256,omitempty"`
	LastAppliedAt  *time.Time         `json:"last_applied_at,omitempty"`
	LastRollbackAt *time.Time         `json:"last_rollback_at,omitempty"`
	LastRestoreAt  *time.Time         `json:"last_restore_at,omitempty"`
	ProxySelection map[string]string  `json:"proxy_selection,omitempty"`
	ExecutionGraph ExecutionGraph     `json:"execution_graph"`
	SpecCaptured   bool               `json:"spec_captured,omitempty"`
	Spec           SpecState          `json:"spec,omitempty"`
	LastGood       []LastGoodSnapshot `json:"last_good,omitempty"`
}

// AppliedTopology is an immutable snapshot of the topology that is allowed to
// drive the live data plane. Configuration and provider save paths may update
// the saved SpecState and last-good files, but those changes are deliberately
// absent here until a ConfigTxn commits them. Live selector changes are read
// from durable SelectionState because SelectorTxn must not rewrite AppliedState
// and RuntimeDir may be recreated on reboot.
type AppliedTopology struct {
	ConfigEpoch    int64
	TopologyHash   string
	GatewayMode    string
	Spec           SpecState
	LastGood       []LastGoodSnapshot
	ProxySelection map[string]string
	Selections     map[string]PoolSelection
	ExecutionGraph ExecutionGraph
}

// LoadAppliedTopology returns the only topology view that live data-plane
// consumers should use. A missing legacy capture fails closed: using the saved
// spec as a fallback would expose unapplied provider refreshes to the runtime.
func (s *Store) LoadAppliedTopology() (AppliedTopology, error) {
	applied, err := s.LoadAppliedState()
	if err != nil {
		return AppliedTopology{}, err
	}
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return AppliedTopology{}, fmt.Errorf("applied topology is not captured")
	}
	selection, err := s.LoadSelectionForApplied(applied)
	if err != nil {
		return AppliedTopology{}, err
	}
	return AppliedTopology{
		ConfigEpoch:    applied.ConfigEpoch,
		TopologyHash:   applied.TopologyHash,
		GatewayMode:    applied.GatewayMode,
		Spec:           applied.Spec,
		LastGood:       cloneLastGoodSnapshots(applied.LastGood),
		ProxySelection: cloneStringMap(selection.ProxySelection),
		Selections:     clonePoolSelections(selection.Selections),
		ExecutionGraph: applied.ExecutionGraph,
	}, nil
}

func (s *Store) LoadAppliedState() (AppliedState, error) {
	var applied AppliedState
	err := loadJSON(s.layout.AppliedStatePath(), &applied)
	if errors.Is(err, os.ErrNotExist) {
		return AppliedState{}, nil
	}
	if err != nil {
		return AppliedState{}, err
	}
	return NormalizeAppliedState(applied), nil
}

func (s *Store) SaveAppliedState(applied AppliedState) error {
	applied = NormalizeAppliedState(applied)
	return saveJSONDurable(s.layout.AppliedStatePath(), applied, 0o600)
}

func NormalizeAppliedState(applied AppliedState) AppliedState {
	// Pre-V3 captures (schema 1) carry the same fields minus the execution
	// graph. Promote them on load so every write from this daemon advertises
	// the V3 schema; deploy scripts key their data-plane validation on it.
	if applied.SchemaVersion < AppliedStateSchemaVersion {
		applied.SchemaVersion = AppliedStateSchemaVersion
	}
	if applied.GatewayMode != "" {
		applied.GatewayMode = EffectiveGatewayMode(applied.GatewayMode)
	}
	applied.Spec = NormalizeSpecState(applied.Spec)
	return applied
}

func AppliedStateFromRuntime(runtime RuntimeState, gatewayMode string) AppliedState {
	return NormalizeAppliedState(AppliedState{
		ConfigEpoch:    runtime.ConfigEpoch,
		TopologyHash:   runtime.TopologyHash,
		GatewayMode:    EffectiveGatewayMode(gatewayMode),
		LastAppliedAt:  runtime.LastAppliedAt,
		LastRollbackAt: runtime.LastRollbackAt,
		LastRestoreAt:  runtime.LastRestoreAt,
		ProxySelection: cloneStringMap(runtime.ProxySelection),
	})
}

func AppliedStateWithTarget(applied AppliedState, spec SpecState, lastGood []LastGoodSnapshot) AppliedState {
	applied.SpecCaptured = true
	applied.Spec = spec
	applied.LastGood = cloneLastGoodSnapshots(lastGood)
	return NormalizeAppliedState(applied)
}

func cloneLastGoodSnapshots(in []LastGoodSnapshot) []LastGoodSnapshot {
	if len(in) == 0 {
		return nil
	}
	out := make([]LastGoodSnapshot, len(in))
	copy(out, in)
	for index := range out {
		out[index].Identities = append([]smartnode.IdentitySource(nil), in[index].Identities...)
		out[index].Outbounds = append([]map[string]any(nil), in[index].Outbounds...)
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
