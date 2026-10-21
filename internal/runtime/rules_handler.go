package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"smartgate/internal/lockset"
	"smartgate/internal/opid"
	"smartgate/internal/state"
)

type rulesAPI struct {
	store *state.Store
	locks *lockset.LockSet
}

type rulesPutPayload struct {
	Rules state.RuleCatalog `json:"rules"`
}

type ruleConflictResolvePayload struct {
	RuleKey  string `json:"rule_key"`
	KeepPool string `json:"keep_pool"`
	OpID     string `json:"op_id"`
}

type rulesView struct {
	Rules     state.RuleCatalog    `json:"rules"`
	Conflicts []state.RuleConflict `json:"conflicts"`
}

func registerRulesHandlers(mux *http.ServeMux, store *state.Store, locks *lockset.LockSet) {
	api := rulesAPI{store: store, locks: locks}
	mux.HandleFunc("/api/rules", api.rules)
	mux.HandleFunc("/api/rules/conflicts", api.conflicts)
	mux.HandleFunc("/api/rules/resolve-conflict", api.resolveConflict)
}

func (api rulesAPI) rules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		spec, err := api.loadSpec("rules-read")
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		view, err := buildRulesView(spec)
		if err != nil {
			writeAPIErrorCode(w, http.StatusUnprocessableEntity, "rules_invalid", err.Error(), nil)
			return
		}
		writeAPIData(w, http.StatusOK, view)
	case http.MethodPut:
		var payload rulesPutPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", "invalid json", nil)
			return
		}
		view, err := api.replaceRules(payload.Rules)
		if err != nil {
			writeAPIErrorCode(w, http.StatusUnprocessableEntity, "rules_invalid", err.Error(), nil)
			return
		}
		writeAPIData(w, http.StatusOK, view)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api rulesAPI) conflicts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	spec, err := api.loadSpec("rules-conflicts-read")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	conflicts, err := state.RuleConflicts(spec)
	if err != nil {
		writeAPIErrorCode(w, http.StatusUnprocessableEntity, "rules_invalid", err.Error(), nil)
		return
	}
	writeAPIData(w, http.StatusOK, conflicts)
}

func (api rulesAPI) resolveConflict(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload ruleConflictResolvePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", "invalid json", nil)
		return
	}
	if _, err := opid.Clean(payload.OpID); err != nil {
		writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", err.Error(), nil)
		return
	}
	payload.RuleKey = strings.TrimSpace(payload.RuleKey)
	payload.KeepPool = strings.TrimSpace(payload.KeepPool)
	if payload.RuleKey == "" || payload.KeepPool == "" {
		writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", "rule_key and keep_pool are required", nil)
		return
	}
	view, err := api.resolve(payload)
	if err != nil {
		writeAPIErrorCode(w, http.StatusUnprocessableEntity, "rules_invalid", err.Error(), nil)
		return
	}
	writeAPIData(w, http.StatusOK, view)
}

func (api rulesAPI) replaceRules(rules state.RuleCatalog) (rulesView, error) {
	var view rulesView
	err := api.withSpecWrite("rules-write", func(spec *state.SpecState) error {
		normalized, _, err := state.ValidateRuleCatalog(rules, spec.BusinessPools)
		if err != nil {
			return err
		}
		if err := validateSystemRuleUpdate(*spec, normalized); err != nil {
			return err
		}
		spec.Rules = normalized
		var buildErr error
		view, buildErr = buildRulesView(*spec)
		return buildErr
	})
	return view, err
}

func (api rulesAPI) resolve(payload ruleConflictResolvePayload) (rulesView, error) {
	var view rulesView
	err := api.withSpecWrite("rules-resolve-"+payload.OpID, func(spec *state.SpecState) error {
		conflicts, err := state.RuleConflicts(*spec)
		if err != nil {
			return err
		}
		conflict, found := findRuleConflict(conflicts, payload.RuleKey)
		if !found {
			view, err = buildRulesView(*spec)
			return err
		}
		if !containsRuleTarget(conflict.Pools, payload.KeepPool) {
			return fmt.Errorf("keep_pool %q is not an owner of conflict %q", payload.KeepPool, payload.RuleKey)
		}
		out := make(state.RuleCatalog, 0, len(spec.Rules))
		for _, rule := range spec.Rules {
			if rule.Source == state.RuleSourceUser && rule.Enabled && state.RuleKey(rule.Kind, rule.Value) == payload.RuleKey && rule.PoolID != payload.KeepPool {
				continue
			}
			out = append(out, rule)
		}
		spec.Rules = out
		var buildErr error
		view, buildErr = buildRulesView(*spec)
		return buildErr
	})
	return view, err
}

func findRuleConflict(conflicts []state.RuleConflict, ruleKey string) (state.RuleConflict, bool) {
	for _, conflict := range conflicts {
		if conflict.RuleKey == ruleKey {
			return conflict, true
		}
	}
	return state.RuleConflict{}, false
}

func containsRuleTarget(targets []string, target string) bool {
	for _, current := range targets {
		if current == target {
			return true
		}
	}
	return false
}

func (api rulesAPI) withSpecWrite(name string, mutate func(*state.SpecState) error) error {
	if api.store == nil {
		return fmt.Errorf("state store is not configured")
	}
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
	if err := mutate(&spec); err != nil {
		return err
	}
	_, err = api.store.SaveSpec(spec)
	return err
}

func (api rulesAPI) loadSpec(name string) (state.SpecState, error) {
	if api.store == nil {
		return state.SpecState{}, fmt.Errorf("state store is not configured")
	}
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

func buildRulesView(spec state.SpecState) (rulesView, error) {
	rules, conflicts, err := state.ValidateRuleCatalog(state.DesiredRuleCatalog(spec), spec.BusinessPools)
	if err != nil {
		return rulesView{}, err
	}
	return rulesView{Rules: state.EffectiveRuleCatalog(rules), Conflicts: conflicts}, nil
}

func validateSystemRuleUpdate(spec state.SpecState, incoming state.RuleCatalog) error {
	expected := state.SystemRuleByID(spec)
	seen := map[string]struct{}{}
	for _, rule := range incoming {
		if rule.Source == state.RuleSourceUser {
			continue
		}
		current, ok := expected[rule.ID]
		if !ok {
			return fmt.Errorf("system rule %q is not recognized", rule.ID)
		}
		seen[rule.ID] = struct{}{}
		if rule.Source != current.Source || rule.Kind != current.Kind || rule.Value != current.Value || rule.PoolID != current.PoolID {
			return fmt.Errorf("system rule %q content and ownership are immutable", rule.ID)
		}
	}
	for id := range expected {
		if _, ok := seen[id]; !ok {
			return fmt.Errorf("system rule %q cannot be deleted", id)
		}
	}
	return nil
}

func validRuleTarget(target string, pools []state.BusinessPoolSpec) bool {
	if target == "direct" || target == "block" {
		return true
	}
	for _, pool := range state.EffectiveBusinessPools(pools) {
		if pool.ID == target {
			return true
		}
	}
	return false
}
