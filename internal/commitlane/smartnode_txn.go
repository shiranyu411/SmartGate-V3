package commitlane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/opid"
	"smartgate/internal/provider"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

type SmartNodeTxn struct {
	commit    *Manager
	store     *state.Store
	snapshots *state.SnapshotStore
}

type SmartNodeCoreChoiceRequest struct {
	OpID            string
	SmartNodeID     string
	Core            smartnode.CoreID
	ConfirmDegraded bool
}

type SmartNodeCoreChoiceResult struct {
	SmartNode     smartnode.SmartNode `json:"smart_node"`
	AffectedPools []string            `json:"affected_pools"`
	Dirty         bool                `json:"dirty"`
	MetadataOnly  []string            `json:"metadata_only,omitempty"`
}

type SmartNodeTxnError struct {
	Code    string
	Message string
	Reasons []string
}

func (e *SmartNodeTxnError) Error() string {
	return e.Message
}

func NewSmartNodeTxn(commit *Manager, store *state.Store, snapshots *state.SnapshotStore) *SmartNodeTxn {
	return &SmartNodeTxn{commit: commit, store: store, snapshots: snapshots}
}

func (t *SmartNodeTxn) SetCoreChoice(ctx context.Context, req SmartNodeCoreChoiceRequest) (*SmartNodeCoreChoiceResult, error) {
	if t == nil || t.commit == nil || t.store == nil || t.snapshots == nil {
		return nil, fmt.Errorf("SmartNode transaction is not configured")
	}
	safeOpID, err := opid.Clean(req.OpID)
	if err != nil {
		return nil, err
	}
	if req.SmartNodeID == "" {
		return nil, &SmartNodeTxnError{Code: "node_not_found", Message: "SmartNode id is required"}
	}
	if !smartnode.ValidCore(req.Core) {
		cores := smartnode.RegisteredCoreIDs()
		names := make([]string, 0, len(cores))
		for _, core := range cores {
			names = append(names, string(core))
		}
		return nil, &SmartNodeTxnError{Code: "validation_failed", Message: "core must be " + strings.Join(names, " or ")}
	}
	req.OpID = safeOpID
	var result *SmartNodeCoreChoiceResult
	_, err = t.commit.WithCommit(ctx, CommitRequest{
		OpID: req.OpID, Kind: "smartnode_core_choice", StartedAt: time.Now().UTC(),
	}, func(session *lockset.Session) error {
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		defer releaseStore()
		releaseSnapshot, err := session.Acquire(lockset.LevelSnapshot)
		if err != nil {
			return err
		}
		defer releaseSnapshot()
		releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
		if err != nil {
			return err
		}
		defer releaseRuntime()

		spec, err := t.store.LoadSpec()
		if err != nil {
			return err
		}
		runtimeState, err := t.store.LoadRuntime()
		if err != nil {
			return err
		}
		lastGood, err := provider.LoadLastGoodSnapshotsBounded(ctx, provider.NewLastGoodStore(t.commit.Layout()))
		if err != nil {
			return err
		}
		nextSpec, report, err := provider.ReconcileSmartNodes(spec, lastGood, time.Now().UTC())
		if err != nil {
			return err
		}
		node, ok := nextSpec.SmartNodes[req.SmartNodeID]
		if !ok {
			return &SmartNodeTxnError{Code: "node_not_found", Message: "SmartNode not found"}
		}
		capability, ok := node.Capability.ForCore(req.Core)
		if !ok || capability.Status == smartnode.CapabilityUnsupportedByCore {
			return &SmartNodeTxnError{
				Code: "capability_unsupported", Message: "selected core cannot fully execute this SmartNode", Reasons: capability.Reasons,
			}
		}
		if capability.Status == smartnode.CapabilityDegraded && !req.ConfirmDegraded {
			return &SmartNodeTxnError{
				Code: "confirm_degraded_required", Message: "degraded core selection requires explicit confirmation", Reasons: capability.Reasons,
			}
		}
		node.CoreChoice = req.Core
		nextSpec.SmartNodes[req.SmartNodeID] = node
		affectedPools := poolsReferencingNode(nextSpec.BusinessPools, req.SmartNodeID)
		sourceHash, err := provider.HashEffectiveTopologyFromSnapshots(spec, lastGood)
		if err != nil {
			return err
		}
		targetHash, err := provider.HashEffectiveTopologyFromSnapshots(nextSpec, lastGood)
		if err != nil {
			return err
		}
		catalogChanged := len(report.Added) > 0 || len(report.Revised) > 0 || len(report.MetadataOnly) > 0 || len(report.Removed) > 0
		if sourceHash == targetHash && !catalogChanged {
			result = &SmartNodeCoreChoiceResult{SmartNode: node, AffectedPools: affectedPools, Dirty: false}
			return nil
		}

		journalStore := state.NewTransactionStore(t.commit.Layout())
		journal := state.TransactionJournal{
			OpID: req.OpID, Kind: "smartnode_core_choice", Phase: state.TransactionPrepared,
			SourceHash: sourceHash, TargetHash: targetHash,
			Source: state.TransactionState{Spec: spec, LastGoodCaptured: true, LastGood: lastGood, Runtime: runtimeState},
			Target: state.TransactionState{Spec: nextSpec, LastGoodCaptured: true, LastGood: lastGood},
		}
		if err := journalStore.Save(journal); err != nil {
			return err
		}
		if err := t.snapshots.SavePreimage(state.PreimageEnvelope{
			OpID: req.OpID, Kind: "smartnode_core_choice", TopologyHash: runtimeState.TopologyHash,
			SavedAt: time.Now().UTC(), Spec: spec, Runtime: runtimeState, LastGoodCaptured: true, LastGood: lastGood,
		}); err != nil {
			return err
		}
		runtimeState.CommitInFlight = &state.CommitRef{OpID: req.OpID, StartedAt: time.Now().UTC(), TopologyHash: targetHash}
		if err := t.store.SaveRuntime(runtimeState); err != nil {
			return err
		}
		if _, err := t.store.SaveSpec(nextSpec); err != nil {
			return err
		}
		runtimeState.CommitInFlight = nil
		if err := t.store.SaveRuntime(runtimeState); err != nil {
			return err
		}
		journal.Phase = state.TransactionCommitted
		journal.Target.Runtime = runtimeState
		if err := journalStore.Save(journal); err != nil {
			return err
		}
		if err := t.snapshots.RemovePreimage(req.OpID); err != nil {
			return err
		}
		if err := journalStore.Remove(req.OpID); err != nil {
			return err
		}
		result = &SmartNodeCoreChoiceResult{
			SmartNode: node, AffectedPools: affectedPools, Dirty: sourceHash != targetHash, MetadataOnly: report.MetadataOnly,
		}
		return nil
	})
	return result, err
}

func poolsReferencingNode(pools []state.BusinessPoolSpec, nodeID string) []string {
	var result []string
	for _, pool := range state.EffectiveBusinessPools(pools) {
		for _, id := range pool.NodeIDs {
			if id == nodeID {
				result = append(result, pool.ID)
				break
			}
		}
	}
	return result
}
