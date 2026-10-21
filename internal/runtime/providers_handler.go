package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/lockset"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type providerAPI struct {
	store     *state.Store
	txn       *commitlane.ProviderTxn
	refresher *provider.RefreshService
	layout    state.Layout
	locks     *lockset.LockSet
	commit    *commitlane.Manager
}

type providerTxnPayload struct {
	OpID    string            `json:"op_id"`
	Changes []provider.Change `json:"changes"`
}

type manualProviderNodesPayload struct {
	Outbounds []map[string]any `json:"outbounds"`
}

type providerView struct {
	ID              string                     `json:"id"`
	Name            string                     `json:"name"`
	Type            string                     `json:"type"`
	Enabled         bool                       `json:"enabled"`
	FetchMode       string                     `json:"fetch_mode"`
	Roles           []string                   `json:"roles"`
	Bootstrap       string                     `json:"bootstrap"`
	URLConfigured   bool                       `json:"url_configured"`
	URLMasked       string                     `json:"url_masked,omitempty"`
	FileConfigured  bool                       `json:"file_path_configured"`
	UserAgent       string                     `json:"user_agent,omitempty"`
	Priority        int                        `json:"priority"`
	IncludeKeywords []string                   `json:"include_keywords"`
	ExcludeKeywords []string                   `json:"exclude_keywords"`
	NodeCount       int                        `json:"node_count,omitempty"`
	LastRefreshAt   *time.Time                 `json:"last_refresh_at,omitempty"`
	Subscription    *provider.SubscriptionInfo `json:"subscription,omitempty"`
}

type providerLastGoodView struct {
	ProviderID   string                     `json:"provider_id"`
	SavedAt      time.Time                  `json:"saved_at,omitempty"`
	Count        int                        `json:"count"`
	Subscription *provider.SubscriptionInfo `json:"subscription,omitempty"`
	Configured   bool                       `json:"configured"`
}

func registerProviderHandlers(mux *http.ServeMux, cfg Config) {
	api := providerAPI{
		store:     cfg.Store,
		txn:       cfg.ProviderTxn,
		refresher: cfg.ProviderRef,
		layout:    cfg.Layout,
		locks:     cfg.Locks,
		commit:    cfg.CommitLane,
	}
	mux.HandleFunc("/api/providers", api.providers)
	mux.HandleFunc("/api/providers/dryrun", api.dryRun)
	mux.HandleFunc("/api/providers/apply", api.apply)
	mux.HandleFunc("/api/providers/logs", api.logs)
	mux.HandleFunc("/api/providers/refresh-all", api.refreshAll)
	mux.HandleFunc("/api/providers/", api.providerAction)
}

func (api providerAPI) providers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	spec, err := api.store.LoadSpec()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	snapshots, err := provider.LoadLastGoodSnapshotsBounded(r.Context(), provider.NewLastGoodStore(api.layout))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	lastGood := make(map[string]provider.LastGoodEnvelope, len(snapshots))
	for _, snapshot := range snapshots {
		lastGood[snapshot.ProviderID] = provider.LastGoodEnvelope{
			ProviderID: snapshot.ProviderID, Count: len(snapshot.Identities), Subscription: snapshot.Subscription,
			Identities: snapshot.Identities, Outbounds: snapshot.Outbounds,
		}
	}
	writeAPIData(w, http.StatusOK, map[string]any{"providers": providerViews(spec.Providers, lastGood)})
}

func (api providerAPI) dryRun(w http.ResponseWriter, r *http.Request) {
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

func (api providerAPI) apply(w http.ResponseWriter, r *http.Request) {
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

func (api providerAPI) refreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if api.refresher == nil {
		writeAPIError(w, http.StatusNotImplemented, "provider refresh is not configured")
		return
	}
	spec, err := api.store.LoadSpec()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result, err := api.refresher.RefreshAll(r.Context(), spec)
	if err != nil {
		if errors.Is(err, commitlane.ErrRescueActive) {
			writeAPIError(w, http.StatusLocked, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.recordRefreshLogs("all", "", result)
	writeAPIData(w, http.StatusAccepted, safeRefreshResult(result))
}

func (api providerAPI) providerAction(w http.ResponseWriter, r *http.Request) {
	idAndAction := strings.TrimPrefix(r.URL.Path, "/api/providers/")
	providerID, action, ok := strings.Cut(idAndAction, "/")
	if !ok || providerID == "" {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	switch action {
	case "refresh":
		api.refreshOne(w, r, providerID)
	case "nodes":
		api.manualNodes(w, r, providerID)
	default:
		writeAPIError(w, http.StatusNotFound, "not found")
	}
}

func (api providerAPI) refreshOne(w http.ResponseWriter, r *http.Request, providerID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if api.refresher == nil {
		writeAPIError(w, http.StatusNotImplemented, "provider refresh is not configured")
		return
	}
	spec, err := api.store.LoadSpec()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result, err := api.refresher.RefreshOne(r.Context(), spec, providerID)
	if err != nil {
		if errors.Is(err, commitlane.ErrRescueActive) {
			writeAPIError(w, http.StatusLocked, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.recordRefreshLogs("one", providerID, result)
	writeAPIData(w, http.StatusAccepted, safeRefreshResult(result))
}

func (api providerAPI) manualNodes(w http.ResponseWriter, r *http.Request, providerID string) {
	switch r.Method {
	case http.MethodGet:
		snapshots, err := provider.LoadLastGoodSnapshotsBounded(r.Context(), provider.NewLastGoodStore(api.layout))
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err.Error())
			return
		}
		var env provider.LastGoodEnvelope
		for _, snapshot := range snapshots {
			if snapshot.ProviderID == providerID {
				env = provider.LastGoodEnvelope{
					ProviderID: snapshot.ProviderID, Count: len(snapshot.Identities), Subscription: snapshot.Subscription,
					Identities: snapshot.Identities, Outbounds: snapshot.Outbounds,
				}
				break
			}
		}
		if env.ProviderID == "" {
			writeAPIError(w, http.StatusNotFound, "last-good not found")
			return
		}
		writeAPIData(w, http.StatusOK, providerLastGoodResponse(env))
	case http.MethodPut:
		spec, err := api.store.LoadSpec()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p, ok := findProviderSpec(spec.Providers, providerID)
		if !ok {
			writeAPIError(w, http.StatusNotFound, "provider not found")
			return
		}
		if p.Type != provider.TypeManual && p.Type != provider.TypeTemporary {
			writeAPIError(w, http.StatusBadRequest, "provider nodes can only be written for manual or temporary providers")
			return
		}
		var payload manualProviderNodesPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		body, err := json.Marshal(payload.Outbounds)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		parsed, err := provider.ParseSubscriptionPayload(providerID, body)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		repo := api.lastGoodRepository()
		envs, err := repo.SaveMany(r.Context(), []provider.LastGoodUpdate{{
			ProviderID: providerID, Identities: parsed.Identities,
		}})
		if err != nil {
			if errors.Is(err, commitlane.ErrRescueActive) {
				writeAPIError(w, http.StatusLocked, err.Error())
				return
			}
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIData(w, http.StatusOK, providerLastGoodResponse(envs[providerID]))
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api providerAPI) logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	runtimeState, err := api.store.LoadRuntime()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logs := append([]state.ProviderRefreshLog(nil), runtimeState.ProviderLogs...)
	for index := range logs {
		if logs[index].Error != "" {
			logs[index].Error = provider.RefreshErrorText(sanitizeAPIMessage(logs[index].Error))
		}
	}
	writeAPIData(w, http.StatusOK, map[string]any{"logs": logs})
}

func (api providerAPI) lastGoodRepository() provider.LastGoodRepository {
	store := provider.NewLastGoodStore(api.layout)
	if api.commit == nil {
		return store
	}
	return commitlane.NewLastGoodTxn(api.commit, store).WithStateStore(api.store)
}

func (api providerAPI) recordRefreshLogs(scope, providerID string, result provider.RefreshResult) {
	if api.store == nil {
		return
	}
	now := time.Now().UTC()
	logs := make([]state.ProviderRefreshLog, 0, len(result.Results))
	for _, item := range result.Results {
		id := item.ProviderID
		if providerID != "" {
			id = providerID
		}
		logs = append(logs, state.ProviderRefreshLog{
			Time:       now,
			Scope:      scope,
			ProviderID: id,
			Status:     item.Status,
			Changed:    item.Changed,
			Count:      item.Count,
			Error:      sanitizeAPIMessage(item.Error),
		})
	}
	_ = api.appendProviderLogs(logs)
}

func (api providerAPI) appendProviderLogs(logs []state.ProviderRefreshLog) error {
	if api.locks != nil {
		session := api.locks.Session("provider-runtime-log")
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
		if err != nil {
			releaseStore()
			return err
		}
		defer releaseStore()
		defer releaseRuntime()
	}
	return api.store.AppendProviderLogs(logs, 100)
}

func findProviderSpec(providers []state.ProviderSpec, id string) (state.ProviderSpec, bool) {
	for _, p := range providers {
		if p.ID == id {
			return p, true
		}
	}
	return state.ProviderSpec{}, false
}

func providerViews(providers []state.ProviderSpec, lastGood map[string]provider.LastGoodEnvelope) []providerView {
	views := make([]providerView, 0, len(providers))
	for _, p := range providers {
		view := providerView{
			ID:              p.ID,
			Name:            p.Name,
			Type:            p.Type,
			Enabled:         p.Enabled,
			FetchMode:       p.FetchMode,
			Roles:           stringSlice(p.Roles),
			Bootstrap:       p.Bootstrap,
			URLConfigured:   strings.TrimSpace(p.URL) != "",
			URLMasked:       maskProviderURL(p.URL),
			FileConfigured:  strings.TrimSpace(p.FilePath) != "",
			UserAgent:       p.UserAgent,
			Priority:        p.Priority,
			IncludeKeywords: stringSlice(p.IncludeKeywords),
			ExcludeKeywords: stringSlice(p.ExcludeKeywords),
		}
		if view.FetchMode == "" {
			view.FetchMode = provider.FetchDirect
		}
		if env, ok := lastGood[p.ID]; ok {
			view.NodeCount = env.Count
			view.Subscription = safeSubscriptionInfo(env.Subscription)
			if !env.SavedAt.IsZero() {
				savedAt := env.SavedAt
				view.LastRefreshAt = &savedAt
			}
		}
		views = append(views, view)
	}
	return views
}

func providerLastGoodResponse(env provider.LastGoodEnvelope) providerLastGoodView {
	return providerLastGoodView{
		ProviderID:   env.ProviderID,
		SavedAt:      env.SavedAt,
		Count:        env.Count,
		Subscription: safeSubscriptionInfo(env.Subscription),
		Configured:   env.ProviderID != "" && env.Count > 0,
	}
}

func safeSubscriptionInfo(info *provider.SubscriptionInfo) *provider.SubscriptionInfo {
	if info == nil {
		return nil
	}
	safe := *info
	safe.Raw = ""
	return &safe
}

func safeRefreshResult(result provider.RefreshResult) provider.RefreshResult {
	safe := result
	safe.Results = append([]provider.RefreshStepResult(nil), result.Results...)
	for index := range safe.Results {
		safe.Results[index].Subscription = safeSubscriptionInfo(safe.Results[index].Subscription)
		if safe.Results[index].SubscriptionError != "" {
			safe.Results[index].SubscriptionError = provider.RefreshErrorText(sanitizeAPIMessage(safe.Results[index].SubscriptionError))
		}
		if safe.Results[index].Error != "" {
			safe.Results[index].Error = provider.RefreshErrorText(sanitizeAPIMessage(safe.Results[index].Error))
		}
	}
	return safe
}

func maskProviderURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return parsed.Scheme + "://[configured]"
}

func stringSlice(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func (api providerAPI) decodeTxnPayload(w http.ResponseWriter, r *http.Request) (commitlane.ProviderTxnRequest, bool) {
	var payload providerTxnPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return commitlane.ProviderTxnRequest{}, false
	}
	if payload.OpID == "" {
		writeAPIError(w, http.StatusBadRequest, "op_id is required")
		return commitlane.ProviderTxnRequest{}, false
	}
	return commitlane.ProviderTxnRequest{
		OpID:    payload.OpID,
		Changes: payload.Changes,
	}, true
}
