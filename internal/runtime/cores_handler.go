package runtime

import (
	"fmt"
	"net/http"
	"strings"

	"smartgate/internal/coredesc"
)

type coresAPI struct {
	coreRegistry *coredesc.Registry
}

type coreView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func registerCoreHandlers(mux *http.ServeMux, coreRegistry *coredesc.Registry) {
	api := coresAPI{coreRegistry: coreRegistry}
	mux.HandleFunc("/api/cores", api.cores)
}

func (api coresAPI) cores(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if api.coreRegistry == nil {
		writeAPIError(w, http.StatusNotImplemented, "core registry is not configured")
		return
	}
	cores, err := coreViews(api.coreRegistry)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, cores)
}

func coreViews(coreRegistry *coredesc.Registry) ([]coreView, error) {
	descriptors := coreRegistry.List()
	views := make([]coreView, 0, len(descriptors))
	for _, descriptor := range descriptors {
		id := strings.TrimSpace(string(descriptor.ID))
		if id == "" {
			return nil, fmt.Errorf("registered core descriptor has an empty id")
		}
		displayName := strings.TrimSpace(descriptor.DisplayName)
		if displayName == "" {
			return nil, fmt.Errorf("registered core descriptor %s has an empty display name", id)
		}
		views = append(views, coreView{ID: id, DisplayName: displayName})
	}
	return views, nil
}
