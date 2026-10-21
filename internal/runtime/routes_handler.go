package runtime

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/routes"
	"smartgate/internal/state"
)

type routeAPI struct {
	store *state.Store
	locks *lockset.LockSet
}

func registerRouteHandlers(mux *http.ServeMux, store *state.Store, locks *lockset.LockSet) {
	api := routeAPI{store: store, locks: locks}
	mux.HandleFunc("/api/routes/explain", api.explain)
}

func (api routeAPI) explain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload routes.ExplainRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(payload.Target) == "" {
		writeAPIError(w, http.StatusBadRequest, "target is required")
		return
	}
	if api.store == nil {
		writeAPIData(w, http.StatusOK, routes.Explain(payload.Target))
		return
	}
	spec, topology, hasApplied, err := api.loadState()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hasApplied {
		graph, err := rebuildLiveExecutionGraph(topology, time.Now().UTC())
		if err != nil {
			writeAPIErrorCode(w, http.StatusInternalServerError, "runtime_graph_invalid", err.Error(), nil)
			return
		}
		explanation, err := routes.ExplainWithGraph(payload.Target, graph, topology.Spec.BusinessPools)
		if err != nil {
			writeAPIErrorCode(w, http.StatusInternalServerError, "runtime_graph_invalid", err.Error(), nil)
			return
		}
		writeAPIData(w, http.StatusOK, routes.AttachAppliedPath(explanation, graph))
		return
	}
	writeAPIData(w, http.StatusOK, routes.ExplainWithCatalog(payload.Target, state.DesiredRuleCatalog(spec), spec.BusinessPools))
}

func (api routeAPI) loadState() (state.SpecState, state.AppliedTopology, bool, error) {
	if api.locks != nil {
		session := api.locks.Session("route-explain")
		release, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return state.SpecState{}, state.AppliedTopology{}, false, err
		}
		defer release()
	}
	spec, err := api.store.LoadSpec()
	if err != nil {
		return state.SpecState{}, state.AppliedTopology{}, false, err
	}
	applied, err := api.store.LoadAppliedState()
	if err != nil {
		return state.SpecState{}, state.AppliedTopology{}, false, err
	}
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return spec, state.AppliedTopology{}, false, nil
	}
	topology, err := api.store.LoadAppliedTopology()
	return spec, topology, err == nil, err
}
