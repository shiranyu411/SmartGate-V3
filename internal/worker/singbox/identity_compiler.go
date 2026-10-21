package singbox

import (
	"fmt"
	"sort"
	"strings"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func compileIdentityOutbound(executor state.NodeExecutor, source smartnode.IdentitySource) (map[string]any, []string, error) {
	identity := source.Identity
	if identity.Protocol == "" {
		return nil, nil, fmt.Errorf("executor %s protocol is required", executor.ID)
	}
	if identity.Server == "" || identity.Port < 1 || identity.Port > 65535 {
		return nil, nil, fmt.Errorf("executor %s has invalid server endpoint", executor.ID)
	}
	outbound := map[string]any{
		"type":        identity.Protocol,
		"tag":         executor.ID,
		"server":      identity.Server,
		"server_port": identity.Port,
	}

	secrets, err := source.ResolveSecrets()
	if err != nil {
		return nil, nil, fmt.Errorf("executor %s: %w", executor.ID, err)
	}
	for key, value := range secrets {
		if key != "obfs.password" {
			outbound[key] = value
		}
	}

	transportExtra := map[string]any{}
	tlsExtra := map[string]any{}
	realityExtra := map[string]any{}
	extraKeys := make([]string, 0, len(identity.Extra))
	for key := range identity.Extra {
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		value, err := resolveIdentityExtra(source, key)
		if err != nil {
			return nil, nil, fmt.Errorf("executor %s: %w", executor.ID, err)
		}
		switch {
		case key == "multiplex":
		case strings.HasPrefix(key, "transport."):
			transportExtra[strings.TrimPrefix(key, "transport.")] = value
		case strings.HasPrefix(key, "tls."):
			tlsExtra[strings.TrimPrefix(key, "tls.")] = value
		case strings.HasPrefix(key, "reality."):
			realityExtra[strings.TrimPrefix(key, "reality.")] = value
		case strings.HasPrefix(key, "field."):
			outbound[strings.TrimPrefix(key, "field.")] = value
		default:
			outbound[key] = value
		}
	}

	if transport := compileIdentityTransport(identity.Transport, transportExtra); transport != nil {
		outbound["transport"] = transport
	}
	security, err := compileIdentitySecurity(source, tlsExtra, realityExtra)
	if err != nil {
		return nil, nil, fmt.Errorf("executor %s: %w", executor.ID, err)
	}
	if security != nil {
		outbound["tls"] = security
	}
	if identity.Security.Flow != "" {
		outbound["flow"] = identity.Security.Flow
	}
	return outbound, sensitiveValues(outbound), nil
}

func resolveIdentityExtra(source smartnode.IdentitySource, key string) (any, error) {
	var value any
	found, err := source.ResolveExtra(key, &value)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("identity extra %s is missing", key)
	}
	return value, nil
}

func compileIdentityTransport(identity smartnode.Transport, extra map[string]any) map[string]any {
	typ := identity.Type
	if typ == "" {
		typ = "tcp"
	}
	if typ == "tcp" && identity.Path == "" && identity.Host == "" && identity.ServiceName == "" && len(extra) == 0 {
		return nil
	}
	transport := map[string]any{"type": typ}
	if identity.Path != "" {
		transport["path"] = identity.Path
	}
	if identity.Host != "" {
		if typ == "ws" {
			transport["headers"] = map[string]any{"Host": identity.Host}
		} else {
			transport["host"] = identity.Host
		}
	}
	if identity.ServiceName != "" {
		transport["service_name"] = identity.ServiceName
	}
	for key, value := range extra {
		transport[key] = value
	}
	return transport
}

func compileIdentitySecurity(source smartnode.IdentitySource, tlsExtra, realityExtra map[string]any) (map[string]any, error) {
	identity := source.Identity.Security
	if identity.Type == "none" && identity.SNI == "" && len(identity.ALPN) == 0 && identity.Fingerprint == "" &&
		!identity.SkipCertVerify && identity.PublicKeyRef == "" && identity.CertificateRef == "" &&
		identity.ClientCertRef == "" && identity.ClientKeyRef == "" && identity.ECHConfigListRef == "" &&
		len(tlsExtra) == 0 && len(realityExtra) == 0 {
		return nil, nil
	}
	tls := map[string]any{"enabled": true}
	if identity.SNI != "" {
		tls["server_name"] = identity.SNI
	}
	if len(identity.ALPN) > 0 {
		tls["alpn"] = append([]string(nil), identity.ALPN...)
	}
	if identity.Fingerprint != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": identity.Fingerprint}
	}
	if identity.SkipCertVerify {
		tls["insecure"] = true
	}
	for key, ref := range map[string]string{
		"certificate":        identity.CertificateRef,
		"client_certificate": identity.ClientCertRef,
		"client_key":         identity.ClientKeyRef,
	} {
		if ref == "" {
			continue
		}
		var value any
		if err := source.ResolveReference(ref, &value); err != nil {
			return nil, err
		}
		tls[key] = value
	}
	for key, value := range tlsExtra {
		tls[key] = value
	}
	if identity.Type == "reality" || identity.PublicKeyRef != "" || len(realityExtra) > 0 {
		reality := map[string]any{"enabled": true}
		if identity.PublicKeyRef != "" {
			var publicKey string
			if err := source.ResolveReference(identity.PublicKeyRef, &publicKey); err != nil {
				return nil, err
			}
			reality["public_key"] = publicKey
		}
		if identity.ShortID != "" {
			reality["short_id"] = identity.ShortID
		}
		for key, value := range realityExtra {
			reality[key] = value
		}
		tls["reality"] = reality
	}
	if identity.ECHConfigListRef != "" {
		var config any
		if err := source.ResolveReference(identity.ECHConfigListRef, &config); err != nil {
			return nil, err
		}
		tls["ech"] = map[string]any{"enabled": true, "config": config}
	}
	return tls, nil
}
