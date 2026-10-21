package runtime

import (
	"encoding/json"
	"errors"
	"net/http"

	"smartgate/internal/business"
	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

type businessPoolAPI struct {
	store *state.Store
	txn   *commitlane.BusinessPoolTxn
	locks *lockset.LockSet
}

type businessPoolTxnPayload struct {
	OpID    string            `json:"op_id"`
	Changes []business.Change `json:"changes"`
}

type businessPoolView struct {
	ID                 string           `json:"id"`
	Name               string           `json:"name"`
	Role               string           `json:"role,omitempty"`
	NodeIDs            []string         `json:"node_ids"`
	Policy             state.PoolPolicy `json:"policy"`
	BuiltinOrigin      string           `json:"builtin_origin,omitempty"`
	MembersCoreSummary map[string]int   `json:"members_core_summary"`
}

func registerBusinessPoolHandlers(mux *http.ServeMux, store *state.Store, txn *commitlane.BusinessPoolTxn, locks *lockset.LockSet) {
	api := businessPoolAPI{store: store, txn: txn, locks: locks}
	mux.HandleFunc("/api/business-pools", api.businessPools)
	mux.HandleFunc("/api/business-pools/dryrun", api.dryRun)
	mux.HandleFunc("/api/business-pools/apply", api.apply)
}

func (api businessPoolAPI) businessPools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if api.locks != nil {
		session := api.locks.Session("business-pools-read")
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer releaseStore()
	}
	spec, err := api.store.LoadSpec()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]any{"business_pools": businessPoolViews(state.EffectiveBusinessPools(spec.BusinessPools), spec.SmartNodes)})
}

func (api businessPoolAPI) dryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	req, ok := api.decodeTxnPayload(w, r)
	if !ok {
		return
	}
	result, err := api.txn.DryRun(r.Context(), req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, result)
}

func businessPoolViews(pools []state.BusinessPoolSpec, nodes map[string]smartnode.SmartNode) []businessPoolView {
	views := make([]businessPoolView, 0, len(pools))
	for _, p := range pools {
		views = append(views, businessPoolView{
			ID: p.ID, Name: p.Name, Role: p.Role, NodeIDs: stringSlice(p.NodeIDs), Policy: p.Policy,
			BuiltinOrigin: p.BuiltinOrigin, MembersCoreSummary: memberCoreSummary(p.NodeIDs, nodes),
		})
	}
	return views
}

func memberCoreSummary(nodeIDs []string, nodes map[string]smartnode.SmartNode) map[string]int {
	summary := map[string]int{"unset": 0}
	for _, core := range smartnode.RegisteredCoreIDs() {
		summary[string(core)] = 0
	}
	for _, id := range nodeIDs {
		node, ok := nodes[id]
		if !ok || !smartnode.ValidCore(node.CoreChoice) {
			summary["unset"]++
			continue
		}
		summary[string(node.CoreChoice)]++
	}
	return summary
}

func (api businessPoolAPI) apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	req, ok := api.decodeTxnPayload(w, r)
	if !ok {
		return
	}
	result, err := api.txn.Apply(r.Context(), req)
	if err != nil {
		if errors.Is(err, commitlane.ErrRescueActive) {
			writeAPIError(w, http.StatusLocked, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAPIData(w, http.StatusAccepted, result)
}

func (api businessPoolAPI) decodeTxnPayload(w http.ResponseWriter, r *http.Request) (commitlane.BusinessPoolTxnRequest, bool) {
	var payload businessPoolTxnPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return commitlane.BusinessPoolTxnRequest{}, false
	}
	if payload.OpID == "" {
		writeAPIError(w, http.StatusBadRequest, "op_id is required")
		return commitlane.BusinessPoolTxnRequest{}, false
	}
	return commitlane.BusinessPoolTxnRequest{
		OpID:    payload.OpID,
		Changes: payload.Changes,
	}, true
}
