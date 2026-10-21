package smartnode

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// IdentitySource is the protected compiler input paired with a NodeIdentity.
// References may contain credentials and must never be exposed through public
// node views, logs, diagnostics, or dry-run summaries.
type IdentitySource struct {
	RawTag     string                     `json:"raw_tag"`
	Identity   NodeIdentity               `json:"identity"`
	References map[string]json.RawMessage `json:"references,omitempty"`
}

func NormalizeWithSource(raw RawNode) (SmartNode, IdentitySource, error) {
	normalized, err := normalize(raw)
	if err != nil {
		return SmartNode{}, IdentitySource{}, err
	}
	source, err := identitySource(raw.Outbound, normalized.Node.Identity, normalized.Node.Aliases[0].RawTag)
	if err != nil {
		return SmartNode{}, IdentitySource{}, err
	}
	return normalized.Node, source, nil
}

// IdentitySourceFromRaw lifts a protected provider object into the platform IR
// without evaluating any core. It is used while reading legacy LastGood data,
// including startup recovery before Adapter evaluators are configured.
func IdentitySourceFromRaw(raw RawNode) (IdentitySource, error) {
	normalized, err := normalizeIdentity(raw)
	if err != nil {
		return IdentitySource{}, err
	}
	return identitySource(raw.Outbound, normalized.Identity, normalized.RawTag)
}

func (source IdentitySource) ResolveReference(ref string, target any) error {
	if ref == "" {
		return fmt.Errorf("identity reference is required")
	}
	body, ok := source.References[ref]
	if !ok {
		return fmt.Errorf("identity reference %s is missing", ref)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode identity reference %s: %w", ref, err)
	}
	return nil
}

func (source IdentitySource) ResolveExtra(key string, target any) (bool, error) {
	ref, ok := source.Identity.Extra[key]
	if !ok {
		return false, nil
	}
	return true, source.ResolveReference(ref, target)
}

func (source IdentitySource) ResolveSecrets() (map[string]any, error) {
	if source.Identity.SecretRef == "" {
		return map[string]any{}, nil
	}
	secrets := map[string]any{}
	if err := source.ResolveReference(source.Identity.SecretRef, &secrets); err != nil {
		return nil, err
	}
	return secrets, nil
}

// UnassessedReasons reconstructs the capability inputs encoded by the IR.
// Unknown source fields are retained in Extra and therefore remain visible to
// every registered core evaluator after LastGood is reloaded.
func UnassessedReasons(identity NodeIdentity) []string {
	reasons := make([]string, 0, len(identity.Extra)+1)
	if identity.Port == 0 {
		reasons = append(reasons, "identity.port_missing")
	}
	for key := range identity.Extra {
		switch {
		case strings.HasPrefix(key, "transport."):
			reasons = append(reasons, key+"_not_assessed")
		case strings.HasPrefix(key, "tls."):
			reasons = append(reasons, "security."+strings.TrimPrefix(key, "tls.")+"_not_assessed")
		case strings.HasPrefix(key, "reality."):
			reasons = append(reasons, "security.reality_"+strings.TrimPrefix(key, "reality.")+"_not_assessed")
		case strings.HasPrefix(key, "field."):
			reasons = append(reasons, key+"_not_assessed")
		}
	}
	sort.Strings(reasons)
	return uniqueStrings(reasons)
}

func identitySource(outbound map[string]any, identity NodeIdentity, rawTag string) (IdentitySource, error) {
	allReferences := map[string]json.RawMessage{}
	if err := collectReferenceValues(outbound, allReferences); err != nil {
		return IdentitySource{}, fmt.Errorf("collect identity references: %w", err)
	}
	references := map[string]json.RawMessage{}
	for _, ref := range identity.Extra {
		body, ok := allReferences[ref]
		if !ok {
			return IdentitySource{}, fmt.Errorf("identity extra reference %s is missing", ref)
		}
		references[ref] = body
	}
	for _, ref := range []string{
		identity.Security.PublicKeyRef,
		identity.Security.CertificateRef,
		identity.Security.ClientCertRef,
		identity.Security.ClientKeyRef,
		identity.Security.ECHConfigListRef,
	} {
		if ref == "" {
			continue
		}
		body, ok := allReferences[ref]
		if !ok {
			return IdentitySource{}, fmt.Errorf("identity security reference %s is missing", ref)
		}
		references[ref] = body
	}
	if identity.SecretRef != "" {
		body, err := json.Marshal(secretValues(outbound))
		if err != nil {
			return IdentitySource{}, err
		}
		references[identity.SecretRef] = body
	}
	if len(references) == 0 {
		references = nil
	}
	return IdentitySource{RawTag: rawTag, Identity: identity, References: references}, nil
}

func collectReferenceValues(value any, references map[string]json.RawMessage) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	references[hashValue(value)] = body
	if text, ok := value.(string); ok && text != "" {
		references[refValue(text)] = body
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			if err := collectReferenceValues(item, references); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := collectReferenceValues(item, references); err != nil {
				return err
			}
		}
	case []string:
		for _, item := range typed {
			if err := collectReferenceValues(item, references); err != nil {
				return err
			}
		}
	}
	return nil
}

func secretValues(outbound map[string]any) map[string]any {
	secrets := map[string]any{}
	for _, key := range secretKeys {
		if value, ok := outbound[key]; ok && value != nil && stringValue(value) != "" {
			secrets[key] = value
		}
	}
	if obfs := mapValue(outbound["obfs"]); len(obfs) > 0 {
		if value, ok := obfs["password"]; ok && stringValue(value) != "" {
			secrets["obfs.password"] = value
		}
	}
	return secrets
}
