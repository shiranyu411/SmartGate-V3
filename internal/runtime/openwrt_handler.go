package runtime

import (
	"encoding/json"
	"net/http"

	"smartgate/internal/netgate"
)

type openWrtAPI struct {
	owners               *netgate.OwnerRegistry
	workerCleanupTargets []netgate.WorkerCleanupTarget
}

type openWrtDryRunPayload struct {
	OpID string `json:"op_id"`
}

func registerOpenWrtHandlers(mux *http.ServeMux, owners *netgate.OwnerRegistry, workerCleanupTargets []netgate.WorkerCleanupTarget) {
	api := openWrtAPI{owners: owners, workerCleanupTargets: workerCleanupTargets}
	mux.HandleFunc("/api/runtime/openwrt/dryrun", api.dryRun)
	mux.HandleFunc("/api/runtime/openwrt/rescue-cleanup/dryrun", api.rescueCleanupDryRun)
}

func (api openWrtAPI) dryRun(w http.ResponseWriter, r *http.Request) {
	payload, ok := api.decodeDryRunPayload(w, r)
	if !ok {
		return
	}
	plan, err := netgate.OpenWrtDryRunPlan(payload.OpID, api.owners)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, plan)
}

func (api openWrtAPI) rescueCleanupDryRun(w http.ResponseWriter, r *http.Request) {
	payload, ok := api.decodeDryRunPayload(w, r)
	if !ok {
		return
	}
	plan, err := netgate.OpenWrtRescueCleanupDryRunPlan(payload.OpID, api.owners, api.workerCleanupTargets)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	cleanup, err := netgate.CleanupPlanFromDryRun(plan)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]any{
		"plan":    plan,
		"cleanup": cleanup,
	})
}

func (api openWrtAPI) decodeDryRunPayload(w http.ResponseWriter, r *http.Request) (openWrtDryRunPayload, bool) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return openWrtDryRunPayload{}, false
	}
	var payload openWrtDryRunPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return openWrtDryRunPayload{}, false
	}
	return payload, true
}
