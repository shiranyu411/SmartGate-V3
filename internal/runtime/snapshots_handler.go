package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/configapply"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type snapshotAPI struct {
	layout state.Layout
	txn    *commitlane.SnapshotTxn
}

type snapshotCreatePayload struct {
	ID          string `json:"id,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Description string `json:"description,omitempty"`
}

type snapshotRollbackDryRunPayload struct {
	DryRunID   string `json:"dryrun_id"`
	SnapshotID string `json:"snapshot_id"`
}

type snapshotRollbackPayload struct {
	DryRunID   string `json:"dryrun_id"`
	SnapshotID string `json:"snapshot_id"`
}

type snapshotRollbackDryRunRecord struct {
	ID              string                          `json:"id"`
	SnapshotID      string                          `json:"snapshot_id"`
	CreatedAt       time.Time                       `json:"created_at"`
	Plan            commitlane.SnapshotRollbackPlan `json:"plan"`
	Prepared        configapply.Prepared            `json:"prepared"`
	ChangesComplete bool                            `json:"changes_complete"`
}

type snapshotRollbackDryRunView struct {
	ID              string             `json:"id"`
	SnapshotID      string             `json:"snapshot_id"`
	CreatedAt       time.Time          `json:"created_at"`
	SpecHash        string             `json:"spec_hash"`
	TopologyHash    string             `json:"topology_hash"`
	ConfigEpoch     int64              `json:"config_epoch"`
	GatewayMode     string             `json:"gateway_mode"`
	RuntimeAction   string             `json:"runtime_action"`
	ProviderCounts  [2]int             `json:"provider_counts"`
	PoolCounts      [2]int             `json:"pool_counts"`
	LastGoodCounts  [2]int             `json:"last_good_counts"`
	Changes         []configChangeView `json:"changes"`
	ChangesComplete bool               `json:"changes_complete"`
	ChangesOmitted  int                `json:"changes_omitted,omitempty"`
}

type snapshotRollbackApplyView struct {
	Status       string   `json:"status"`
	ConfigEpoch  int64    `json:"config_epoch"`
	TopologyHash string   `json:"topology_hash"`
	Warnings     []string `json:"warnings"`
}

func registerSnapshotHandlers(mux *http.ServeMux, layout state.Layout, txn *commitlane.SnapshotTxn) {
	api := snapshotAPI{layout: layout, txn: txn}
	mux.HandleFunc("/api/snapshots", api.snapshots)
	mux.HandleFunc("/api/snapshots/rollback/dryrun", api.rollbackDryRun)
	mux.HandleFunc("/api/snapshots/rollback/apply", api.rollbackApply)
}

func (api snapshotAPI) snapshots(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := state.NewSnapshotStore(api.layout).ListSnapshots(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if items == nil {
			items = []state.SnapshotSummary{}
		}
		writeAPIData(w, http.StatusOK, map[string]any{"snapshots": items})
	case http.MethodPost:
		var payload snapshotCreatePayload
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
				return
			}
		}
		snapshot, err := api.txn.Create(r.Context(), commitlane.SnapshotCreateRequest{
			ID:          payload.ID,
			Kind:        payload.Kind,
			Description: payload.Description,
		})
		if err != nil {
			if errors.Is(err, commitlane.ErrRescueActive) || errors.Is(err, commitlane.ErrRecoveryRequired) {
				writeAPIError(w, http.StatusLocked, err.Error())
				return
			}
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIData(w, http.StatusCreated, state.SummarizeSnapshot(*snapshot))
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api snapshotAPI) rollbackDryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload snapshotRollbackDryRunPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if payload.DryRunID == "" || payload.SnapshotID == "" {
		writeAPIError(w, http.StatusBadRequest, "dryrun_id and snapshot_id are required")
		return
	}
	artifact, err := api.txn.PrepareRollback(r.Context(), commitlane.SnapshotRollbackDryRunRequest{
		DryRunID:   payload.DryRunID,
		SnapshotID: payload.SnapshotID,
	})
	if err != nil {
		if artifact != nil {
			cleanupSnapshotPrepared(artifact.Prepared)
		}
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	record := snapshotRollbackDryRunRecord{
		ID:         payload.DryRunID,
		SnapshotID: payload.SnapshotID,
		CreatedAt:  time.Now().UTC(),
		Plan:       artifact.Plan,
		Prepared:   artifact.Prepared,
	}
	preview := snapshotRollbackDryRunResponse(record)
	enforceSnapshotDryRunResponseLimit(&preview)
	record.ChangesComplete = preview.ChangesComplete
	if err := api.saveRollbackDryRun(record); err != nil {
		cleanupSnapshotPrepared(record.Prepared)
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeDryRunAPIData(w, http.StatusOK, preview)
}

func enforceSnapshotDryRunResponseLimit(view *snapshotRollbackDryRunView) {
	encoded, _ := json.Marshal(apiEnvelope{OK: true, Data: *view})
	if len(encoded)+1 <= dryRunResponseHardLimit {
		return
	}
	view.ChangesComplete = false
	view.ChangesOmitted++
	view.Changes = []configChangeView{{Category: "摘要限制", Title: "回滚预演响应超过设备安全上限", Impact: "本次预演禁止回滚；请缩小变更范围"}}
}

func (api snapshotAPI) rollbackApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload snapshotRollbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	record, err := api.consumeRollbackDryRun(payload.DryRunID, payload.SnapshotID)
	if err != nil {
		writeAPIError(w, http.StatusPreconditionRequired, err.Error())
		return
	}
	defer cleanupSnapshotPrepared(record.Prepared)
	if !record.ChangesComplete {
		writeAPIError(w, http.StatusPreconditionRequired, "snapshot rollback readable change summary is incomplete; reduce the rollback scope")
		return
	}
	result, err := api.txn.Rollback(r.Context(), commitlane.SnapshotRollbackRequest{
		OpID:                       payload.DryRunID,
		SnapshotID:                 payload.SnapshotID,
		ExpectedConfigEpoch:        record.Plan.CurrentConfigEpoch,
		ExpectedSourceTopologyHash: record.Plan.CurrentTopologyHash,
		ExpectedTargetTopologyHash: record.Plan.TargetTopologyHash,
		ExpectedSnapshotTargetHash: record.Plan.SnapshotTargetHash,
		ExpectedSourceSelection:    cloneSelectionMap(record.Plan.CurrentRuntime.ProxySelection),
		ExpectedSourceSelections:   clonePoolSelectionMap(record.Plan.CurrentSelection.Selections),
		RequireDryRunMatch:         true,
		Prepared:                   record.Prepared,
	})
	if err != nil {
		if errors.Is(err, commitlane.ErrRescueActive) || errors.Is(err, commitlane.ErrRecoveryRequired) {
			writeAPIError(w, http.StatusLocked, err.Error())
			return
		}
		if strings.Contains(err.Error(), "changed after rollback dry-run") {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, snapshotRollbackApplyResponse(result))
}

func snapshotRollbackDryRunResponse(record snapshotRollbackDryRunRecord) snapshotRollbackDryRunView {
	target := record.Plan.TargetSpec
	currentApplied := appliedWithRuntimeSelection(record.Plan.CurrentApplied, record.Plan.CurrentRuntime.ProxySelection)
	collector := configChangeCollector{}
	collector.add(operationalChange(runtimeActionForGatewayMode(target.Settings.GatewayMode), record.Prepared.OpenWrtPlan))
	summarizeConfigChangesInto(&collector, currentApplied, target, record.Plan.TargetTopologyHash, record.Plan.TargetLastGood, record.Plan.TargetRuntime.ProxySelection)
	if currentApplied.SpecCaptured {
		savedTopology, _ := provider.HashEffectiveTopologyFromSnapshots(record.Plan.CurrentSpec, record.Plan.CurrentLastGood)
		if provider.HashSpec(record.Plan.CurrentSpec) != provider.HashSpec(currentApplied.Spec) || savedTopology != currentApplied.TopologyHash {
			start := len(collector.changes)
			summarizeConfigChangesInto(&collector, currentApplied, record.Plan.CurrentSpec, savedTopology, record.Plan.CurrentLastGood, record.Plan.CurrentRuntime.ProxySelection)
			for index := start; index < len(collector.changes); index++ {
				collector.changes[index].Category = "未应用编辑 / " + collector.changes[index].Category
				collector.changes[index].Impact = "当前尚未应用的保存态将被快照覆盖；" + collector.changes[index].Impact
			}
		}
	}
	collector.emit(func(yield func(configChangeView) bool) {
		persistedSelectionChangesEmit(record.Plan.CurrentRuntime.ProxySelection, record.Plan.TargetRuntime.ProxySelection, yield)
	})
	collector.emit(func(yield func(configChangeView) bool) {
		for _, change := range gatewayModeDerivedChanges(currentApplied, target) {
			if !yield(change) {
				return
			}
		}
	})
	changeRecord := configDryRunRecord{Changes: collector.result()}
	finalizeConfigChanges(&changeRecord)
	return snapshotRollbackDryRunView{
		ID:              record.ID,
		SnapshotID:      record.SnapshotID,
		CreatedAt:       record.CreatedAt,
		SpecHash:        provider.HashSpec(target),
		TopologyHash:    record.Plan.TargetTopologyHash,
		ConfigEpoch:     record.Plan.CurrentConfigEpoch,
		GatewayMode:     state.EffectiveGatewayMode(target.Settings.GatewayMode),
		RuntimeAction:   runtimeActionForGatewayMode(target.Settings.GatewayMode),
		ProviderCounts:  [2]int{record.Plan.CurrentProviderCount, record.Plan.TargetProviderCount},
		PoolCounts:      [2]int{record.Plan.CurrentPoolCount, record.Plan.TargetPoolCount},
		LastGoodCounts:  [2]int{record.Plan.CurrentLastGoodCount, record.Plan.TargetLastGoodCount},
		Changes:         changeRecord.Changes,
		ChangesComplete: changeRecord.ChangesComplete,
		ChangesOmitted:  changeRecord.ChangesOmitted,
	}
}

func persistedSelectionChanges(before, after map[string]string) []configChangeView {
	out := []configChangeView{}
	persistedSelectionChangesEmit(before, after, func(change configChangeView) bool { out = append(out, change); return true })
	return out
}
func persistedSelectionChangesEmit(before, after map[string]string, yield func(configChangeView) bool) {
	keys := make([]string, 0, len(before)+len(after))
	seen := map[string]bool{}
	for key := range before {
		seen[key] = true
		keys = append(keys, key)
	}
	for key := range after {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if before[key] == after[key] {
			continue
		}
		oldValue, newValue := before[key], after[key]
		if oldValue == "" {
			oldValue = "未保存选择"
		}
		if newValue == "" {
			newValue = "清除保存选择"
		}
		if !yield(configChangeView{Category: "持久选择器", Title: "保存选点 " + key, Before: oldValue, After: newValue, Impact: "即使代理当前关闭，此选择也会被保存，并在以后启用代理时参与节点恢复"}) {
			return
		}
	}
}

func snapshotRollbackApplyResponse(result *commitlane.SnapshotRollbackResult) snapshotRollbackApplyView {
	view := snapshotRollbackApplyView{
		Status:   "ok",
		Warnings: []string{},
	}
	if result != nil {
		view.ConfigEpoch = result.Runtime.ConfigEpoch
		view.TopologyHash = result.Runtime.TopologyHash
	}
	return view
}

func (api snapshotAPI) rollbackDryRunPath() string {
	return api.layout.RuntimePath("snapshot-rollback-dryrun")
}

func (api snapshotAPI) saveRollbackDryRun(record snapshotRollbackDryRunRecord) error {
	runtimeServiceMu.Lock()
	defer runtimeServiceMu.Unlock()
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if len(body) > persistedDryRunMaxBytes {
		return fmt.Errorf("persisted snapshot rollback dry-run exceeds %d bytes", persistedDryRunMaxBytes)
	}
	tmp := api.rollbackDryRunPath() + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, api.rollbackDryRunPath()); err != nil {
		return err
	}
	return nil
}

func (api snapshotAPI) loadRollbackDryRun() (snapshotRollbackDryRunRecord, error) {
	runtimeServiceMu.Lock()
	defer runtimeServiceMu.Unlock()
	return api.loadRollbackDryRunUnlocked()
}

func (api snapshotAPI) loadRollbackDryRunUnlocked() (snapshotRollbackDryRunRecord, error) {
	var record snapshotRollbackDryRunRecord
	err := readJSONFileBounded(api.rollbackDryRunPath(), &record, persistedDryRunMaxBytes)
	return record, err
}

func (api snapshotAPI) consumeRollbackDryRun(id, snapshotID string) (snapshotRollbackDryRunRecord, error) {
	runtimeServiceMu.Lock()
	defer runtimeServiceMu.Unlock()
	record, err := api.loadRollbackDryRunUnlocked()
	if err != nil {
		return record, err
	}
	if record.ID != id || record.SnapshotID != snapshotID {
		return record, fmt.Errorf("dryrun_id and snapshot_id must match latest rollback dry-run")
	}
	if err := os.Remove(api.rollbackDryRunPath()); err != nil {
		return record, err
	}
	return record, nil
}

func cleanupSnapshotPrepared(prepared configapply.Prepared) {
	_ = prepared
}

func (api snapshotAPI) removeRollbackDryRunIfMatch(id, snapshotID string) error {
	runtimeServiceMu.Lock()
	defer runtimeServiceMu.Unlock()
	record, err := api.loadRollbackDryRunUnlocked()
	if err != nil {
		return err
	}
	if record.ID != id || record.SnapshotID != snapshotID {
		return nil
	}
	return os.Remove(api.rollbackDryRunPath())
}
