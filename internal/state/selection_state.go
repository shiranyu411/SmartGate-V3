package state

import (
	"errors"
	"fmt"
	"os"
)

const SelectionStateSchemaVersion = 2

// SelectionState is the durable authority for live selector choices. It is
// bound to one committed applied topology and survives RuntimeDir recreation.
type SelectionState struct {
	SchemaVersion  int                      `json:"schema_version"`
	ConfigEpoch    int64                    `json:"config_epoch"`
	TopologyHash   string                   `json:"topology_hash,omitempty"`
	ProxySelection map[string]string        `json:"proxy_selection,omitempty"`
	Selections     map[string]PoolSelection `json:"selections,omitempty"`
}

type PoolSelection struct {
	SmartNode         string `json:"smart_node,omitempty"`
	Target            string `json:"target,omitempty"`
	Generation        uint64 `json:"generation"`
	BoundEpoch        int64  `json:"bound_epoch"`
	BoundTopologyHash string `json:"bound_topology_hash"`
}

func (s *Store) LoadSelectionState() (SelectionState, error) {
	var selection SelectionState
	err := loadJSON(s.layout.SelectionStatePath(), &selection)
	if errors.Is(err, os.ErrNotExist) {
		return SelectionState{}, nil
	}
	if err != nil {
		return SelectionState{}, err
	}
	return normalizeSelectionState(selection), nil
}

func (s *Store) SaveSelectionState(selection SelectionState) error {
	if selection.SchemaVersion == 0 {
		selection.SchemaVersion = SelectionStateSchemaVersion
	}
	selection = normalizeSelectionState(selection)
	return saveJSONDurable(s.layout.SelectionStatePath(), selection, 0o600)
}

func (s *Store) LoadSelectionForApplied(applied AppliedState) (SelectionState, error) {
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return SelectionState{}, nil
	}
	selection, err := s.LoadSelectionState()
	if err != nil {
		return SelectionState{}, err
	}
	if selection.SchemaVersion == 0 {
		return SelectionStateForApplied(applied, applied.ProxySelection), nil
	}
	if !SelectionStateMatchesApplied(selection, applied) {
		return SelectionState{}, fmt.Errorf("durable selection does not match applied topology")
	}
	return normalizeSelectionState(selection), nil
}

func SelectionStateForApplied(applied AppliedState, selections map[string]string) SelectionState {
	selection := SelectionState{
		SchemaVersion:  SelectionStateSchemaVersion,
		ConfigEpoch:    applied.ConfigEpoch,
		TopologyHash:   applied.TopologyHash,
		ProxySelection: cloneStringMap(selections),
	}
	selection.Selections = selectionsForApplied(applied, selections)
	return selection
}

func SelectionStateMatchesApplied(selection SelectionState, applied AppliedState) bool {
	return selection.SchemaVersion != 0 && selection.ConfigEpoch == applied.ConfigEpoch && selection.TopologyHash == applied.TopologyHash
}

// RuntimeWithDurableSelection overlays live selector choices and applied
// topology metadata onto RuntimeState when AppliedState is captured.
// RuntimeDir may lag or be recreated on reboot; callers that journal, dry-run,
// or surface status from Runtime must use this so durable SelectionState and
// AppliedState remain authoritative over an empty/stale RuntimeDir.
// GatewayMode is left untouched so rescue/runtime mode markers are preserved.
func RuntimeWithDurableSelection(store *Store, runtimeState RuntimeState) (RuntimeState, error) {
	if store == nil {
		return runtimeState, fmt.Errorf("store is required")
	}
	applied, err := store.LoadAppliedState()
	if err != nil {
		return RuntimeState{}, err
	}
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return runtimeState, nil
	}
	runtimeState.ConfigEpoch = applied.ConfigEpoch
	runtimeState.TopologyHash = applied.TopologyHash
	selection, err := store.LoadSelectionForApplied(applied)
	if err != nil {
		return RuntimeState{}, err
	}
	runtimeState.ProxySelection = cloneStringMap(selection.ProxySelection)
	return runtimeState, nil
}

func normalizeSelectionState(selection SelectionState) SelectionState {
	if selection.SchemaVersion < SelectionStateSchemaVersion {
		selection.SchemaVersion = SelectionStateSchemaVersion
	}
	selection.ProxySelection = cloneStringMap(selection.ProxySelection)
	selection.Selections = clonePoolSelections(selection.Selections)
	if len(selection.Selections) == 0 && len(selection.ProxySelection) > 0 {
		selection.Selections = make(map[string]PoolSelection, len(selection.ProxySelection))
		for pool, target := range selection.ProxySelection {
			item := PoolSelection{Generation: 1, BoundEpoch: selection.ConfigEpoch, BoundTopologyHash: selection.TopologyHash}
			if target == "Direct" || target == "direct" {
				item.Target = NoCandidateDirect
			} else if target == "Block" || target == "block" {
				item.Target = NoCandidateBlock
			} else {
				item.SmartNode = target
				item.Target = "node"
			}
			selection.Selections[pool] = item
		}
	}
	if len(selection.ProxySelection) == 0 && len(selection.Selections) > 0 {
		selection.ProxySelection = make(map[string]string, len(selection.Selections))
		for pool, item := range selection.Selections {
			target := item.SmartNode
			if item.Target == NoCandidateDirect {
				target = "Direct"
			} else if item.Target == NoCandidateBlock {
				target = "Block"
			}
			selection.ProxySelection[pool] = target
		}
	}
	return selection
}

func selectionsForApplied(applied AppliedState, selections map[string]string) map[string]PoolSelection {
	out := make(map[string]PoolSelection, len(selections))
	generations := make(map[string]uint64, len(applied.ExecutionGraph.PoolBindings))
	for _, binding := range applied.ExecutionGraph.PoolBindings {
		generations[binding.Pool] = binding.Generation
	}
	for pool, target := range selections {
		generation := generations[pool]
		if generation == 0 {
			generation = 1
		}
		item := PoolSelection{Generation: generation, BoundEpoch: applied.ConfigEpoch, BoundTopologyHash: applied.TopologyHash}
		if target == "Direct" || target == "direct" {
			item.Target = NoCandidateDirect
		} else if target == "Block" || target == "block" {
			item.Target = NoCandidateBlock
		} else {
			item.Target = "node"
			item.SmartNode = target
		}
		out[pool] = item
	}
	return out
}

func clonePoolSelections(in map[string]PoolSelection) map[string]PoolSelection {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]PoolSelection, len(in))
	for pool, item := range in {
		out[pool] = item
	}
	return out
}
