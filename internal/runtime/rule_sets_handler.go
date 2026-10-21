package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"smartgate/internal/coredesc"
	"smartgate/internal/lockset"
	"smartgate/internal/opid"
	"smartgate/internal/state"
)

type ruleSetsAPI struct {
	store        *state.Store
	locks        *lockset.LockSet
	coreRegistry *coredesc.Registry
}

type remoteRuleSettingsView struct {
	state.RemoteRuleSettings
	Warnings []string `json:"warnings,omitempty"`
}

func registerRuleSetHandlers(mux *http.ServeMux, store *state.Store, locks *lockset.LockSet, coreRegistry *coredesc.Registry) {
	api := ruleSetsAPI{store: store, locks: locks, coreRegistry: coreRegistry}
	mux.HandleFunc("/api/rule-sets", api.ruleSets)
}

func (api ruleSetsAPI) ruleSets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		spec, err := api.loadSpec("rule-sets-read")
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		settings := state.EffectiveRemoteRuleSettings(spec.RemoteRules)
		writeAPIData(w, http.StatusOK, remoteRuleSettingsView{
			RemoteRuleSettings: settings,
			Warnings:           remoteRuleSettingsWarnings(settings),
		})
	case http.MethodPut:
		var payload state.RemoteRuleSettings
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, state.MaxControlStateJSONBytes))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)
		_, explicitEnabled := raw["enabled"]
		settings, err := normalizeRemoteRuleSettings(payload, explicitEnabled, api.coreRegistry)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := api.saveRuleSets("rule-sets-write", settings); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAPIData(w, http.StatusOK, remoteRuleSettingsView{
			RemoteRuleSettings: settings,
			Warnings:           remoteRuleSettingsWarnings(settings),
		})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api ruleSetsAPI) loadSpec(name string) (state.SpecState, error) {
	if api.locks != nil {
		session := api.locks.Session(name)
		release, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return state.SpecState{}, err
		}
		defer release()
	}
	return api.store.LoadSpec()
}

func (api ruleSetsAPI) saveRuleSets(name string, settings state.RemoteRuleSettings) error {
	if api.locks != nil {
		session := api.locks.Session(name)
		release, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		defer release()
	}
	spec, err := api.store.LoadSpec()
	if err != nil {
		return err
	}
	spec.RemoteRules = settings
	_, err = api.store.SaveSpec(spec)
	return err
}

func normalizeRemoteRuleSettings(payload state.RemoteRuleSettings, explicitEnabled bool, coreRegistry *coredesc.Registry) (state.RemoteRuleSettings, error) {
	settings := state.EffectiveRemoteRuleSettings(payload)
	if explicitEnabled {
		settings.Enabled = payload.Enabled
	}
	settings.DefaultFormat = strings.ToLower(strings.TrimSpace(settings.DefaultFormat))
	settings.DownloadDetour = normalizeRuleDetour(settings.DownloadDetour)
	settings.UpdateInterval = strings.TrimSpace(settings.UpdateInterval)
	settings.AdBlock.Mode = strings.ToLower(strings.TrimSpace(settings.AdBlock.Mode))
	settings.AdBlock.Sets = normalizeStringIDs(settings.AdBlock.Sets)
	if settings.DefaultFormat != "binary" && settings.DefaultFormat != "source" {
		return settings, fmt.Errorf("default_format must be binary or source")
	}
	if settings.AdBlock.Mode == "" {
		settings.AdBlock.Mode = "remote"
	}
	if settings.AdBlock.Mode != "remote" && settings.AdBlock.Mode != "off" {
		return settings, fmt.Errorf("ad_block.mode must be remote or off")
	}
	if settings.AdBlock.Mode == "off" {
		settings.AdBlock.Enabled = false
	}
	if settings.MemoryGuard.MaxEnabledSets <= 0 || settings.MemoryGuard.MaxTotalBytes <= 0 || settings.MemoryGuard.MaxSetBytes <= 0 {
		return settings, fmt.Errorf("memory_guard max values must be positive")
	}
	if settings.MemoryGuard.WarnEnabledSets < 0 || settings.MemoryGuard.WarnTotalBytes < 0 {
		return settings, fmt.Errorf("memory_guard warn values must not be negative")
	}
	sets, err := normalizeRemoteRuleSets(settings.Sets, settings, coreRegistry)
	if err != nil {
		return settings, err
	}
	settings.Sets = sets
	if err := enforceRemoteRuleHardLimits(settings); err != nil {
		return settings, err
	}
	return settings, nil
}

func normalizeRemoteRuleSets(raw []state.RemoteRuleSet, settings state.RemoteRuleSettings, coreRegistry *coredesc.Registry) ([]state.RemoteRuleSet, error) {
	seen := map[string]struct{}{}
	out := make([]state.RemoteRuleSet, 0, len(raw))
	var localDirs []string
	localDirsLoaded := false
	for _, item := range raw {
		item.ID = strings.TrimSpace(item.ID)
		item.Name = strings.TrimSpace(item.Name)
		item.Kind = strings.ToLower(strings.TrimSpace(item.Kind))
		item.Target = strings.ToLower(strings.TrimSpace(item.Target))
		item.Outbound = normalizeRuleDetour(item.Outbound)
		item.Format = strings.ToLower(strings.TrimSpace(item.Format))
		item.Type = strings.ToLower(strings.TrimSpace(item.Type))
		item.URL = strings.TrimSpace(item.URL)
		item.Path = strings.TrimSpace(item.Path)
		item.DownloadDetour = normalizeRuleDetour(item.DownloadDetour)
		item.UpdateInterval = strings.TrimSpace(item.UpdateInterval)
		item.Tags = normalizeStringIDs(item.Tags)
		item.Description = strings.TrimSpace(item.Description)
		if item.ID == "" {
			return nil, fmt.Errorf("rule set id is required")
		}
		if _, err := opid.Clean(item.ID); err != nil {
			return nil, fmt.Errorf("rule set id %q is unsafe: %w", item.ID, err)
		}
		if _, ok := seen[item.ID]; ok {
			return nil, fmt.Errorf("duplicate rule set id %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		if item.Name == "" {
			item.Name = item.ID
		}
		if item.Kind == "" {
			item.Kind = "custom"
		}
		if !validRemoteRuleKind(item.Kind) {
			return nil, fmt.Errorf("rule set %s kind must be geo, adblock, custom, or provider", item.ID)
		}
		if item.Target == "" {
			item.Target = "proxy"
		}
		if !validRemoteRuleTarget(item.Target) {
			return nil, fmt.Errorf("rule set %s target must be direct, proxy, or block", item.ID)
		}
		if item.Format == "" {
			item.Format = settings.DefaultFormat
		}
		if item.Format != "binary" && item.Format != "source" {
			return nil, fmt.Errorf("rule set %s format must be binary or source", item.ID)
		}
		if item.Type == "" {
			item.Type = "remote"
		}
		if item.Type != "remote" && item.Type != "local" {
			return nil, fmt.Errorf("rule set %s type must be remote or local", item.ID)
		}
		if item.Type == "local" {
			if !localDirsLoaded {
				var err error
				localDirs, err = registeredRuleSetLocalDirs(coreRegistry)
				if err != nil {
					return nil, err
				}
				localDirsLoaded = true
			}
			if err := validateLocalRulePath(item.Path, localDirs); err != nil {
				return nil, fmt.Errorf("rule set %s path invalid: %w", item.ID, err)
			}
			item.URL = ""
			item.DownloadDetour = ""
			item.UpdateInterval = ""
		} else {
			if err := validateRemoteRuleURL(item.URL); err != nil {
				return nil, fmt.Errorf("rule set %s url invalid: %w", item.ID, err)
			}
			item.Path = ""
		}
		if item.EstimatedBytes < 0 {
			return nil, fmt.Errorf("rule set %s estimated_bytes must not be negative", item.ID)
		}
		out = append(out, item)
	}
	return out, nil
}

func enforceRemoteRuleHardLimits(settings state.RemoteRuleSettings) error {
	if !settings.Enabled {
		return nil
	}
	enabledCount := 0
	totalBytes := 0
	for _, item := range settings.Sets {
		enabled := item.Enabled
		if item.Kind == "adblock" {
			enabled = settings.AdBlock.Enabled && item.Enabled
		}
		if !enabled {
			continue
		}
		enabledCount++
		totalBytes += item.EstimatedBytes
		if item.EstimatedBytes > settings.MemoryGuard.MaxSetBytes {
			return fmt.Errorf("rule set %s estimated_bytes exceeds max_set_bytes", item.ID)
		}
	}
	if enabledCount > settings.MemoryGuard.MaxEnabledSets {
		return fmt.Errorf("enabled rule sets exceed max_enabled_sets")
	}
	if totalBytes > settings.MemoryGuard.MaxTotalBytes {
		return fmt.Errorf("enabled rule sets estimated_bytes exceed max_total_bytes")
	}
	return nil
}

func remoteRuleSettingsWarnings(settings state.RemoteRuleSettings) []string {
	if !settings.Enabled {
		return nil
	}
	enabledCount := 0
	totalBytes := 0
	for _, item := range settings.Sets {
		enabled := item.Enabled
		if item.Kind == "adblock" {
			enabled = settings.AdBlock.Enabled && item.Enabled
		}
		if !enabled {
			continue
		}
		enabledCount++
		totalBytes += item.EstimatedBytes
	}
	var warnings []string
	if settings.MemoryGuard.WarnEnabledSets > 0 && enabledCount > settings.MemoryGuard.WarnEnabledSets {
		warnings = append(warnings, "启用的规则集数量超过建议值")
	}
	if settings.MemoryGuard.WarnTotalBytes > 0 && totalBytes > settings.MemoryGuard.WarnTotalBytes {
		warnings = append(warnings, "启用的规则集预计总大小超过建议值")
	}
	return warnings
}

func normalizeStringIDs(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range in {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeRuleDetour(raw string) string {
	value := strings.TrimSpace(raw)
	switch strings.ToLower(value) {
	case "direct":
		return "Direct"
	case "proxy":
		return "Proxy"
	case "block":
		return "Block"
	default:
		return value
	}
}

func validRemoteRuleKind(kind string) bool {
	switch kind {
	case "geo", "adblock", "custom", "provider":
		return true
	default:
		return false
	}
}

func validRemoteRuleTarget(target string) bool {
	switch target {
	case "direct", "proxy", "block":
		return true
	default:
		return false
	}
}

func validateRemoteRuleURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("must start with http(s)://")
	}
	if parsed.Host == "" {
		return fmt.Errorf("host is required")
	}
	return nil
}

func registeredRuleSetLocalDirs(coreRegistry *coredesc.Registry) ([]string, error) {
	if coreRegistry == nil {
		return nil, fmt.Errorf("core registry is required for local rule-set paths")
	}
	seen := map[string]struct{}{}
	var dirs []string
	for _, descriptor := range coreRegistry.List() {
		for _, raw := range descriptor.ConfigDirs {
			dir := strings.TrimSpace(raw)
			if dir == "" {
				return nil, fmt.Errorf("core descriptor %s has an empty config directory", descriptor.ID)
			}
			if !filepath.IsAbs(dir) {
				return nil, fmt.Errorf("core descriptor %s config directory %q is not absolute", descriptor.ID, dir)
			}
			dir = filepath.Clean(dir)
			if _, ok := seen[dir]; ok {
				continue
			}
			seen[dir] = struct{}{}
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no registered core config directories allow local rule-set paths")
	}
	return dirs, nil
}

func validateLocalRulePath(raw string, allowedDirs []string) error {
	if raw == "" {
		return fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(raw) {
		return fmt.Errorf("must be absolute")
	}
	clean := filepath.Clean(raw)
	for _, dir := range allowedDirs {
		if strings.HasPrefix(clean, dir+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("must be under a registered core config directory: %s", strings.Join(allowedDirs, ", "))
}
