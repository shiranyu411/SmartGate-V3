package provider

import (
	"fmt"
	"net/url"
	"strings"
)

func EffectiveSubscriptionURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "http" || scheme == "https" {
		if u.Host == "" {
			return "", fmt.Errorf("http(s) URL host is required")
		}
		return raw, nil
	}
	if scheme == remoteProfileScheme {
		return effectiveRemoteProfileURL(u)
	}
	return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
}

func effectiveRemoteProfileURL(u *url.URL) (string, error) {
	action := strings.Trim(strings.ToLower(u.Host+u.Path), "/")
	if action != "import-remote-profile" {
		return "", fmt.Errorf("unsupported remote profile action %q", action)
	}
	remote := strings.TrimSpace(u.Query().Get("url"))
	if remote == "" {
		return "", fmt.Errorf("remote profile link missing url parameter")
	}
	ru, err := url.Parse(remote)
	if err != nil {
		return "", fmt.Errorf("invalid remote profile url parameter: %w", err)
	}
	if ru.Scheme != "http" && ru.Scheme != "https" {
		return "", fmt.Errorf("remote profile url parameter must be http(s)")
	}
	if ru.Host == "" {
		return "", fmt.Errorf("remote profile url host is required")
	}
	return remote, nil
}
