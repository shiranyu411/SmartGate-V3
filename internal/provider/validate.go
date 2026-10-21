package provider

import (
	"fmt"
	"strings"

	"smartgate/internal/opid"
	"smartgate/internal/state"
)

const (
	TypeSubscription = "subscription"
	TypeManual       = "manual"
	TypeLocalFile    = "local-file"
	TypeTemporary    = "temporary"

	FetchDirect       = "direct"
	FetchViaBootstrap = "via_bootstrap"
)

func NormalizeProviderSpec(p state.ProviderSpec) (state.ProviderSpec, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.Type = strings.TrimSpace(p.Type)
	p.FetchMode = strings.TrimSpace(p.FetchMode)
	p.Bootstrap = strings.TrimSpace(p.Bootstrap)
	p.URL = strings.TrimSpace(p.URL)
	p.FilePath = strings.TrimSpace(p.FilePath)
	p.UserAgent = strings.TrimSpace(p.UserAgent)
	p.Roles = normalizeRoles(p.Roles)
	p.IncludeKeywords = normalizeKeywords(p.IncludeKeywords)
	p.ExcludeKeywords = normalizeKeywords(p.ExcludeKeywords)

	if p.ID == "" {
		return p, fmt.Errorf("provider id is required")
	}
	if err := ValidateProviderID(p.ID); err != nil {
		return p, err
	}
	if p.Name == "" {
		return p, fmt.Errorf("provider %s name is required", p.ID)
	}
	if p.Priority == 0 {
		p.Priority = 100
	}
	if p.Type == "" {
		p.Type = TypeManual
	}
	if p.FetchMode == "" && p.Type == TypeSubscription {
		p.FetchMode = FetchDirect
	}

	switch p.Type {
	case TypeSubscription:
		if _, err := EffectiveSubscriptionURL(p.URL); err != nil {
			return p, fmt.Errorf("provider %s subscription URL is invalid: %w", p.ID, err)
		}
		if err := validateUserAgent(p.UserAgent); err != nil {
			return p, fmt.Errorf("provider %s user agent is invalid: %w", p.ID, err)
		}
	case TypeManual, TypeTemporary:
		p.URL = ""
		p.FilePath = ""
		p.UserAgent = ""
	case TypeLocalFile:
		p.UserAgent = ""
		if p.FilePath == "" {
			return p, fmt.Errorf("provider %s local-file path is required", p.ID)
		}
	default:
		return p, fmt.Errorf("provider %s has unknown type %q", p.ID, p.Type)
	}

	if p.FetchMode != "" && p.FetchMode != FetchDirect && p.FetchMode != FetchViaBootstrap {
		return p, fmt.Errorf("provider %s has unknown fetch mode %q", p.ID, p.FetchMode)
	}
	if p.FetchMode == FetchViaBootstrap && p.Bootstrap == "" {
		return p, fmt.Errorf("provider %s via_bootstrap requires bootstrap provider", p.ID)
	}
	if p.Bootstrap == p.ID && p.Bootstrap != "" {
		return p, fmt.Errorf("provider %s cannot bootstrap itself", p.ID)
	}
	return p, nil
}

func ValidateProviderID(id string) error {
	if _, err := opid.Clean(id); err != nil {
		return fmt.Errorf("provider id %q is unsafe: %w", id, err)
	}
	return nil
}

// validateUserAgent keeps the value safe to place in an HTTP header:
// printable ASCII only, no header injection, bounded length.
func validateUserAgent(ua string) error {
	if ua == "" {
		return nil
	}
	if len(ua) > 256 {
		return fmt.Errorf("must be at most 256 characters")
	}
	for _, r := range ua {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("must contain printable ASCII only")
		}
	}
	return nil
}

func ValidateSpecProviders(spec state.SpecState) (state.SpecState, error) {
	seen := make(map[string]struct{}, len(spec.Providers))
	normalized := make([]state.ProviderSpec, 0, len(spec.Providers))
	for _, p := range spec.Providers {
		np, err := NormalizeProviderSpec(p)
		if err != nil {
			return spec, err
		}
		if _, ok := seen[np.ID]; ok {
			return spec, fmt.Errorf("duplicate provider id %q", np.ID)
		}
		seen[np.ID] = struct{}{}
		normalized = append(normalized, np)
	}
	spec.Providers = normalized
	return validateBootstrapGraph(spec)
}

func validateBootstrapGraph(spec state.SpecState) (state.SpecState, error) {
	providers := make(map[string]state.ProviderSpec, len(spec.Providers))
	for _, p := range spec.Providers {
		providers[p.ID] = p
	}

	visiting := map[string]bool{}
	visited := map[string]bool{}
	var dfs func(string) error
	dfs = func(id string) error {
		if visited[id] {
			return nil
		}
		if visiting[id] {
			return fmt.Errorf("bootstrap dependency cycle detected at provider %s", id)
		}
		visiting[id] = true
		p := providers[id]
		if p.FetchMode == FetchViaBootstrap {
			target, ok := providers[p.Bootstrap]
			if !ok {
				return fmt.Errorf("provider %s references missing bootstrap provider %s", p.ID, p.Bootstrap)
			}
			if target.ID == p.ID {
				return fmt.Errorf("provider %s cannot bootstrap itself", p.ID)
			}
			if err := dfs(target.ID); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}

	for _, p := range spec.Providers {
		if err := dfs(p.ID); err != nil {
			return spec, err
		}
	}
	return spec, nil
}

func normalizeRoles(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		role := strings.TrimSpace(strings.ToLower(raw))
		if role == "" {
			continue
		}
		if _, ok := seen[role]; ok {
			continue
		}
		seen[role] = struct{}{}
		out = append(out, role)
	}
	return out
}

func normalizeKeywords(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		value := strings.ToLower(strings.TrimSpace(raw))
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
