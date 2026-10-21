package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/fabric"
	"smartgate/internal/lockset"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type connectionsAPI struct {
	store    *state.Store
	workers  *worker.Manager
	fabric   *fabric.Manager
	selector *commitlane.SelectorTxn
	locks    *lockset.LockSet
	layout   state.Layout
}

type connectionClosePayload struct {
	Scope   string `json:"scope"`
	ID      string `json:"id,omitempty"`
	Pool    string `json:"pool,omitempty"`
	Confirm bool   `json:"confirm"`
}

type connectionView struct {
	ID              string         `json:"id"`
	Metadata        map[string]any `json:"metadata"`
	Upload          float64        `json:"upload"`
	Download        float64        `json:"download"`
	Chains          []string       `json:"chains"`
	Start           string         `json:"start,omitempty"`
	Pool            string         `json:"pool"`
	Generation      uint64         `json:"generation"`
	StaleGeneration bool           `json:"stale_generation"`
	SmartNode       string         `json:"smart_node"`
	Worker          string         `json:"worker"`
	Core            string         `json:"core"`
}

type observedConnection struct {
	view               connectionView
	workerConnectionID string
	path               *worker.BindingPath
	tracked            *fabric.TrackedConnection
	dispatch           state.PoolDispatchGraph
}

type connectionScope struct {
	pool       string
	generation uint64
	dispatch   state.PoolDispatchGraph
	path       *worker.BindingPath
	binding    state.PoolBinding
	stale      bool
}

type connectionTotals struct {
	download float64
	upload   float64
	memory   any
}

func registerConnectionHandlers(mux *http.ServeMux) {
	registerConnectionHandlersWithConfig(mux, connectionsAPI{})
}

func registerConnectionHandlersWithLocks(mux *http.ServeMux, locks *lockset.LockSet, layout state.Layout) {
	registerConnectionHandlersWithConfig(mux, connectionsAPI{locks: locks, layout: layout})
}

func registerConnectionHandlersWithConfig(mux *http.ServeMux, api connectionsAPI) {
	mux.HandleFunc("/api/connections", api.connections)
	mux.HandleFunc("/api/connections/close", api.closeConnections)
}

func (api connectionsAPI) connections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	items, totals, _, err := api.observe(r.Context())
	if err != nil {
		if errors.Is(err, errConnectionObservationUnavailable) {
			writeAPIError(w, http.StatusNotImplemented, err.Error())
		} else {
			writeAPIError(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	poolFilter := strings.TrimSpace(r.URL.Query().Get("pool"))
	workerFilter := strings.TrimSpace(r.URL.Query().Get("worker"))
	views := make([]connectionView, 0, len(items))
	for _, item := range items {
		if poolFilter != "" && item.view.Pool != poolFilter {
			continue
		}
		if workerFilter != "" && item.view.Worker != workerFilter {
			continue
		}
		if item.view.Chains == nil {
			item.view.Chains = []string{}
		}
		if api.fabric != nil {
			metadata := item.view.Metadata
			if hosts := api.fabric.ConnectionDNSHosts(stringValue(metadata["sourceIP"]), stringValue(metadata["destinationIP"])); len(hosts) > 0 {
				metadata["dnsHosts"] = hosts
			}
		}
		views = append(views, item.view)
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	data := map[string]any{
		"connections": views, "download_total": totals.download,
		"upload_total": totals.upload,
	}
	if totals.memory != nil {
		data["memory"] = totals.memory
	}
	writeAPIData(w, http.StatusOK, data)
}

func (api connectionsAPI) closeConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload connectionClosePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	payload.Scope = strings.ToLower(strings.TrimSpace(payload.Scope))
	if payload.Scope == "" {
		payload.Scope = "all"
	}
	switch payload.Scope {
	case "single":
		if strings.TrimSpace(payload.ID) == "" {
			writeAPIError(w, http.StatusBadRequest, "id is required for single scope")
			return
		}
	case "binding":
		if strings.TrimSpace(payload.Pool) == "" {
			writeAPIError(w, http.StatusBadRequest, "pool is required for binding scope")
			return
		}
		if !payload.Confirm {
			writeAPIError(w, http.StatusBadRequest, "confirm must be true for binding scope")
			return
		}
	case "all":
		if !payload.Confirm {
			writeAPIError(w, http.StatusBadRequest, "confirm must be true for all scope")
			return
		}
	default:
		writeAPIError(w, http.StatusBadRequest, "scope must be single, binding, or all")
		return
	}
	release, err := lockProxyMutation(api.locks, api.layout, "connections-close-net")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer release()

	var result map[string]any
	switch payload.Scope {
	case "single":
		result, err = api.closeSingle(r.Context(), payload.ID)
	case "binding":
		result, err = api.closeBinding(r.Context(), payload.Pool)
	case "all":
		result, err = api.closeAll(r.Context())
	}
	if err != nil {
		if errors.Is(err, errConnectionNotFound) || errors.Is(err, errConnectionPoolNotFound) {
			writeAPIError(w, http.StatusNotFound, err.Error())
		} else if errors.Is(err, errConnectionObservationUnavailable) {
			writeAPIError(w, http.StatusNotImplemented, err.Error())
		} else {
			writeAPIError(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	writeAPIData(w, http.StatusAccepted, result)
}

var (
	errConnectionObservationUnavailable = errors.New("connection observation is not configured")
	errConnectionNotFound               = errors.New("connection not found")
	errConnectionPoolNotFound           = errors.New("connection pool not found")
)

func (api connectionsAPI) observe(ctx context.Context) ([]observedConnection, connectionTotals, bool, error) {
	topology, hasApplied, err := api.appliedTopology()
	if err != nil {
		return nil, connectionTotals{}, false, err
	}
	if !hasApplied {
		return nil, connectionTotals{}, false, errConnectionObservationUnavailable
	}

	scopes, err := api.scopes(topology, true)
	if err != nil {
		return nil, connectionTotals{}, true, err
	}
	if api.workers == nil && api.fabric == nil {
		return nil, connectionTotals{}, true, errConnectionObservationUnavailable
	}
	items := make([]observedConnection, 0)
	if api.workers != nil {
		workerItems, err := api.workerConnectionItems(ctx, scopes)
		if err != nil {
			return nil, connectionTotals{}, true, err
		}
		items = append(items, workerItems...)
	}
	if api.fabric != nil {
		tracked, err := api.trackedConnectionItems(ctx, scopes)
		if err != nil {
			return nil, connectionTotals{}, true, err
		}
		items = mergeObservedConnections(items, tracked)
	}
	return items, totalsFromObservedConnections(items), true, nil
}

func (api connectionsAPI) appliedTopology() (state.AppliedTopology, bool, error) {
	if api.store == nil {
		return state.AppliedTopology{}, false, nil
	}
	applied, err := api.store.LoadAppliedState()
	if err != nil {
		return state.AppliedTopology{}, false, err
	}
	if applied.SchemaVersion == 0 || !applied.SpecCaptured {
		return state.AppliedTopology{}, false, nil
	}
	topology, err := api.store.LoadAppliedTopology()
	return topology, err == nil, err
}

func (api connectionsAPI) scopes(topology state.AppliedTopology, hasApplied bool) ([]connectionScope, error) {
	if !hasApplied {
		return nil, nil
	}
	graph, err := rebuildLiveExecutionGraph(topology, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	bindings := make(map[string]state.PoolBinding, len(graph.PoolBindings))
	for _, binding := range graph.PoolBindings {
		bindings[binding.Pool] = binding
	}
	scopes := make([]connectionScope, 0, len(graph.Fabric.Dispatch)+2)
	for _, dispatch := range graph.Fabric.Dispatch {
		binding := bindings[dispatch.Pool]
		scope := connectionScope{
			pool: dispatch.Pool, generation: dispatch.Generation, dispatch: dispatch,
			binding: binding, stale: binding.Generation != 0 && binding.Generation != dispatch.Generation,
		}
		if binding.Target.Worker != "" && binding.Target.Executor != "" {
			if api.workers == nil {
				return nil, fmt.Errorf("worker manager is required to observe connections for pool %s", dispatch.Pool)
			}
			path, err := api.workers.ResolvePath(binding.Target.Worker, dispatch.Pool, dispatch.Lane, binding.Target.Executor)
			if err != nil {
				return nil, err
			}
			scope.path = &path
		}
		scopes = append(scopes, scope)
	}
	if api.selector != nil {
		_, drains := api.selector.Progress()
		for _, drain := range drains {
			scopes = append(scopes, connectionScope{
				pool: drain.Pool, generation: drain.OldGeneration, dispatch: drain.Dispatch,
				path: drain.Path, binding: drain.Binding, stale: true,
			})
		}
	}
	return deduplicateConnectionScopes(scopes), nil
}

func (api connectionsAPI) workerConnectionItems(ctx context.Context, scopes []connectionScope) ([]observedConnection, error) {
	out := make([]observedConnection, 0)
	for _, scope := range scopes {
		if scope.path == nil {
			continue
		}
		connections, err := api.workers.ListPathConnections(ctx, *scope.path)
		if errors.Is(err, worker.ErrConnectionObservationUnsupported) && api.fabric != nil {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("observe worker connections for pool %s generation %d: %w", scope.pool, scope.generation, err)
		}
		for _, connection := range connections {
			if strings.TrimSpace(connection.ID) == "" {
				return nil, fmt.Errorf("worker connection id is required for pool %s generation %d", scope.pool, scope.generation)
			}
			view := connectionView{
				ID:       connection.ID,
				Metadata: cloneConnectionMetadata(connection.Metadata),
				Upload:   connection.Upload,
				Download: connection.Download,
				Chains:   append([]string(nil), connection.Chains...),
				Start:    connection.Start,
			}
			applyScope(&view, scope)
			path := *scope.path
			out = append(out, observedConnection{
				view: view, workerConnectionID: connection.ID, path: &path, dispatch: scope.dispatch,
			})
		}
	}
	return out, nil
}

func (api connectionsAPI) trackedConnectionItems(ctx context.Context, scopes []connectionScope) ([]observedConnection, error) {
	out := make([]observedConnection, 0)
	for _, scope := range scopes {
		connections, err := api.fabric.ListConnections(ctx, scope.dispatch)
		if err != nil {
			return nil, err
		}
		for _, tracked := range connections {
			trackedCopy := tracked
			view := connectionView{
				ID: trackedConnectionViewID(scope, tracked),
				Metadata: map[string]any{
					"network": tracked.Protocol, "sourceIP": tracked.SourceIP,
					"destinationIP": tracked.DestinationIP, "sourcePort": tracked.SourcePort,
					"destinationPort": tracked.DestinationPort,
				},
				Upload: float64(tracked.Upload), Download: float64(tracked.Download),
			}
			applyScope(&view, scope)
			out = append(out, observedConnection{view: view, tracked: &trackedCopy, dispatch: scope.dispatch})
		}
	}
	return out, nil
}

func mergeObservedConnections(workerItems, trackedItems []observedConnection) []observedConnection {
	byTuple := make(map[string]int, len(trackedItems))
	out := append([]observedConnection(nil), trackedItems...)
	for index, item := range out {
		if key := connectionTupleKey(item.view); key != "" {
			byTuple[key] = index
		}
	}
	for _, workerItem := range workerItems {
		key := connectionTupleKey(workerItem.view)
		index, ok := byTuple[key]
		if !ok || key == "" {
			out = append(out, workerItem)
			continue
		}
		merged := out[index]
		merged.workerConnectionID = workerItem.workerConnectionID
		merged.path = workerItem.path
		merged.view.ID = workerItem.view.ID
		merged.view.Chains = workerItem.view.Chains
		merged.view.Start = workerItem.view.Start
		merged.view.Metadata = mergeConnectionMetadata(merged.view.Metadata, workerItem.view.Metadata)
		if workerItem.view.Upload > merged.view.Upload {
			merged.view.Upload = workerItem.view.Upload
		}
		if workerItem.view.Download > merged.view.Download {
			merged.view.Download = workerItem.view.Download
		}
		out[index] = merged
	}
	return out
}

func (api connectionsAPI) closeSingle(ctx context.Context, id string) (map[string]any, error) {
	items, _, _, err := api.observe(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.view.ID != id {
			continue
		}
		closed := 0
		if item.workerConnectionID != "" {
			if item.path == nil || api.workers == nil {
				return nil, errConnectionObservationUnavailable
			}
			if err := api.workers.ClosePathConnection(ctx, *item.path, item.workerConnectionID); err != nil {
				return nil, err
			}
			closed = 1
		}
		if item.tracked != nil && api.fabric != nil {
			count, err := api.fabric.CloseConnection(ctx, item.dispatch, *item.tracked)
			if err != nil {
				return nil, err
			}
			if count > closed {
				closed = count
			}
		}
		return map[string]any{"scope": "single", "id": id, "closed": closed, "closed_exact": true}, nil
	}
	return nil, fmt.Errorf("%w: %s", errConnectionNotFound, id)
}

func (api connectionsAPI) closeBinding(ctx context.Context, pool string) (map[string]any, error) {
	topology, hasApplied, err := api.appliedTopology()
	if err != nil {
		return nil, err
	}
	if !hasApplied {
		return nil, fmt.Errorf("%w: %s", errConnectionPoolNotFound, pool)
	}
	scopes, err := api.scopes(topology, true)
	if err != nil {
		return nil, err
	}
	closed := 0
	exact := api.fabric != nil
	found := false
	for _, scope := range scopes {
		if scope.pool != pool {
			continue
		}
		found = true
		count, err := api.closeScope(ctx, scope)
		if err != nil {
			return nil, err
		}
		closed += count
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", errConnectionPoolNotFound, pool)
	}
	return map[string]any{"scope": "binding", "pool": pool, "closed": closed, "closed_exact": exact}, nil
}

func (api connectionsAPI) closeAll(ctx context.Context) (map[string]any, error) {
	topology, hasApplied, err := api.appliedTopology()
	if err != nil {
		return nil, err
	}
	if !hasApplied {
		return nil, errConnectionObservationUnavailable
	}
	closed := 0
	exact := api.fabric != nil
	scopes, err := api.scopes(topology, true)
	if err != nil {
		return nil, err
	}
	for _, scope := range scopes {
		count, err := api.closeScope(ctx, scope)
		if err != nil {
			return nil, err
		}
		closed += count
	}
	return map[string]any{"scope": "all", "closed": closed, "closed_exact": exact}, nil
}

func (api connectionsAPI) closeScope(ctx context.Context, scope connectionScope) (int, error) {
	workerClosed := 0
	if scope.path != nil && api.workers != nil {
		count, err := api.workers.ClosePathConnections(ctx, *scope.path)
		if err != nil {
			return 0, err
		}
		workerClosed = count
	}
	kernelClosed := 0
	if api.fabric != nil {
		count, err := api.fabric.CloseConnections(ctx, scope.dispatch)
		if err != nil {
			return workerClosed, err
		}
		kernelClosed = count
	}
	if workerClosed > kernelClosed {
		return workerClosed, nil
	}
	return kernelClosed, nil
}

func applyScope(view *connectionView, scope connectionScope) {
	view.Pool = scope.pool
	view.Generation = scope.generation
	view.StaleGeneration = scope.stale
	view.SmartNode = scope.binding.Target.SmartNode
	view.Worker = scope.binding.Target.Worker
	view.Core = string(scope.binding.Target.Core)
	if scope.path != nil {
		view.SmartNode = scope.path.Executor.SmartNode
		view.Worker = scope.path.Worker.ID
		view.Core = string(scope.path.Worker.Core)
	}
}

func trackedConnectionViewID(scope connectionScope, tracked fabric.TrackedConnection) string {
	return fmt.Sprintf("conntrack:%s:%d:%s", scope.pool, scope.generation, tracked.ID)
}

func connectionTupleKey(view connectionView) string {
	metadata := view.Metadata
	protocol := strings.ToLower(stringValue(metadata["network"]))
	if protocol == "" {
		protocol = strings.ToLower(stringValue(metadata["type"]))
	}
	values := []string{
		view.Pool, fmt.Sprint(view.Generation), protocol, stringValue(metadata["sourceIP"]), stringValue(metadata["destinationIP"]),
		stringValue(metadata["sourcePort"]), stringValue(metadata["destinationPort"]),
	}
	if values[3] == "" || values[4] == "" {
		return ""
	}
	return strings.Join(values, "\x00")
}

func mergeConnectionMetadata(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		if stringValue(value) != "" {
			out[key] = value
		}
	}
	return out
}

func deduplicateConnectionScopes(scopes []connectionScope) []connectionScope {
	seen := map[string]struct{}{}
	out := make([]connectionScope, 0, len(scopes))
	for _, scope := range scopes {
		key := fmt.Sprintf("%s:%d", scope.pool, scope.generation)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, scope)
	}
	return out
}

func totalsFromObservedConnections(items []observedConnection) connectionTotals {
	totals := connectionTotals{}
	for _, item := range items {
		totals.download += item.view.Download
		totals.upload += item.view.Upload
	}
	return totals
}

func cloneConnectionMetadata(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func stringValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
