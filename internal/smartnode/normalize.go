package smartnode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

var secretKeys = []string{
	"uuid",
	"password",
	"username",
	"auth",
	"auth_str",
	"token",
	"private_key",
	"pre_shared_key",
	"psk",
}

var extraKeys = []string{
	"alter_id",
	"congestion_control",
	"down_mbps",
	"encryption",
	"method",
	"multiplex",
	"network",
	"obfs",
	"packet_encoding",
	"plugin",
	"plugin_opts",
	"security",
	"udp_over_tcp",
	"udp_relay_mode",
	"up_mbps",
}

var knownTopLevelKeys = func() map[string]struct{} {
	keys := []string{
		"type", "tag", "server", "server_port", "tls", "transport", "flow",
		"domain_resolver", "detour",
	}
	keys = append(keys, secretKeys...)
	keys = append(keys, extraKeys...)
	out := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		out[key] = struct{}{}
	}
	return out
}()

type normalizedNode struct {
	Node       SmartNode
	Digest     string
	Unassessed []string
}

type normalizedIdentity struct {
	Identity   NodeIdentity
	Digest     string
	ProviderID string
	RawTag     string
	SeenAt     time.Time
	Unassessed []string
}

func Normalize(raw RawNode) (SmartNode, error) {
	normalized, err := normalize(raw)
	return normalized.Node, err
}

func normalize(raw RawNode) (normalizedNode, error) {
	normalized, err := normalizeIdentity(raw)
	if err != nil {
		return normalizedNode{}, err
	}
	node := SmartNode{
		ID:         "sn_" + normalized.Digest[:16],
		Revision:   1,
		CoreChoice: DefaultCoreChoice(),
		Identity:   normalized.Identity,
		Aliases: []NodeAlias{{
			ProviderID: normalized.ProviderID,
			RawTag:     normalized.RawTag,
			FirstSeen:  normalized.SeenAt,
			LastSeen:   normalized.SeenAt,
		}},
	}
	capability, err := EvaluateCapability(normalized.Identity, normalized.Unassessed)
	if err != nil {
		return normalizedNode{}, err
	}
	node.Capability = capability
	return normalizedNode{Node: node, Digest: normalized.Digest, Unassessed: normalized.Unassessed}, nil
}

func normalizeIdentity(raw RawNode) (normalizedIdentity, error) {
	protocol := strings.ToLower(stringValue(raw.Outbound["type"]))
	server := normalizeServer(stringValue(raw.Outbound["server"]))
	port := intValue(raw.Outbound["server_port"])
	rawTag := strings.TrimSpace(raw.RawTag)
	if rawTag == "" {
		rawTag = stringValue(raw.Outbound["tag"])
	}
	if strings.TrimSpace(raw.ProviderID) == "" {
		return normalizedIdentity{}, fmt.Errorf("smartnode provider id is required")
	}
	if rawTag == "" {
		return normalizedIdentity{}, fmt.Errorf("smartnode alias tag is required")
	}
	if protocol == "" {
		return normalizedIdentity{}, fmt.Errorf("smartnode protocol is required")
	}
	if server == "" {
		return normalizedIdentity{}, fmt.Errorf("smartnode server is required")
	}
	if port < 0 || port > 65535 {
		return normalizedIdentity{}, fmt.Errorf("smartnode port must be between 0 and 65535")
	}

	transport, transportExtra, transportUnassessed := normalizeTransport(raw.Outbound)
	security, securityExtra, securityUnassessed := normalizeSecurity(raw.Outbound)
	extra, topLevelUnassessed := normalizeExtra(raw.Outbound)
	for key, value := range transportExtra {
		extra[key] = value
	}
	for key, value := range securityExtra {
		extra[key] = value
	}
	if len(extra) == 0 {
		extra = nil
	}

	identity := NodeIdentity{
		Protocol:  protocol,
		Server:    server,
		Port:      port,
		SecretRef: secretRef(raw.Outbound),
		Transport: transport,
		Security:  security,
		Extra:     extra,
	}
	digest := IdentityDigest(identity)
	seenAt := raw.SeenAt.UTC()
	if seenAt.IsZero() {
		seenAt = time.Now().UTC()
	}
	unassessed := append(transportUnassessed, securityUnassessed...)
	unassessed = append(unassessed, topLevelUnassessed...)
	if port == 0 {
		unassessed = append(unassessed, "identity.port_missing")
	}
	sort.Strings(unassessed)
	unassessed = uniqueStrings(unassessed)
	return normalizedIdentity{
		Identity: identity, Digest: digest, ProviderID: strings.TrimSpace(raw.ProviderID),
		RawTag: rawTag, SeenAt: seenAt, Unassessed: unassessed,
	}, nil
}

func IdentityDigest(identity NodeIdentity) string {
	body, _ := json.Marshal(identity)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func MaskRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	prefix := ""
	value := ref
	if before, after, ok := strings.Cut(ref, ":"); ok {
		prefix = before + ":"
		value = after
	}
	if len(value) <= 4 {
		return prefix + "****"
	}
	return prefix + "****" + value[len(value)-4:]
}

func normalizeServer(raw string) string {
	host := strings.TrimSpace(raw)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func normalizeTransport(outbound map[string]any) (Transport, map[string]string, []string) {
	transportMap := mapValue(outbound["transport"])
	typ := strings.ToLower(stringValue(transportMap["type"]))
	if typ == "" {
		typ = strings.ToLower(stringValue(outbound["network"]))
	}
	if typ == "" {
		typ = "tcp"
	}
	if typ == "http" {
		typ = "httpupgrade"
	}
	host := stringValue(transportMap["host"])
	if host == "" {
		headers := mapValue(transportMap["headers"])
		for key, value := range headers {
			if strings.EqualFold(key, "host") {
				host = firstString(value)
				break
			}
		}
	}
	transport := Transport{
		Type:        typ,
		Path:        stringValue(transportMap["path"]),
		Host:        strings.ToLower(host),
		ServiceName: firstNonEmpty(stringValue(transportMap["service_name"]), stringValue(transportMap["serviceName"])),
	}
	extra := map[string]string{}
	unassessed := []string{}
	known := map[string]struct{}{
		"type": {}, "path": {}, "host": {}, "headers": {}, "service_name": {}, "serviceName": {},
	}
	for key, value := range transportMap {
		if _, ok := known[key]; ok {
			continue
		}
		extra["transport."+key] = hashValue(value)
		unassessed = append(unassessed, "transport."+sanitizeReasonPart(key)+"_not_assessed")
	}
	return transport, extra, unassessed
}

func normalizeSecurity(outbound map[string]any) (Security, map[string]string, []string) {
	tlsMap := mapValue(outbound["tls"])
	realityMap := mapValue(tlsMap["reality"])
	utlsMap := mapValue(tlsMap["utls"])
	typ := "none"
	if boolValue(tlsMap["enabled"]) || len(tlsMap) > 0 {
		typ = "tls"
	}
	if boolValue(realityMap["enabled"]) || stringValue(realityMap["public_key"]) != "" {
		typ = "reality"
	}
	security := Security{
		Type:           typ,
		SNI:            strings.ToLower(firstNonEmpty(stringValue(tlsMap["server_name"]), stringValue(tlsMap["serverName"]))),
		ALPN:           stringSlice(tlsMap["alpn"]),
		Fingerprint:    strings.ToLower(firstNonEmpty(stringValue(utlsMap["fingerprint"]), stringValue(tlsMap["fingerprint"]))),
		Flow:           strings.ToLower(stringValue(outbound["flow"])),
		PublicKeyRef:   refValue(stringValue(realityMap["public_key"])),
		ShortID:        strings.ToLower(firstNonEmpty(stringValue(realityMap["short_id"]), stringValue(realityMap["shortId"]))),
		SkipCertVerify: boolValue(tlsMap["insecure"]),
		CertificateRef: refValue(stringValue(tlsMap["certificate"])),
		ClientCertRef:  refValue(stringValue(tlsMap["client_certificate"])),
		ClientKeyRef:   refValue(stringValue(tlsMap["client_key"])),
	}
	sort.Strings(security.ALPN)
	if len(security.ALPN) == 0 {
		security.ALPN = nil
	}
	extra := map[string]string{}
	unassessed := []string{}
	knownTLS := map[string]struct{}{
		"enabled": {}, "server_name": {}, "serverName": {}, "alpn": {}, "utls": {}, "reality": {},
		"fingerprint": {}, "insecure": {}, "certificate": {}, "client_certificate": {}, "client_key": {},
	}
	for key, value := range tlsMap {
		if _, ok := knownTLS[key]; ok {
			continue
		}
		extra["tls."+key] = hashValue(value)
		unassessed = append(unassessed, "security."+sanitizeReasonPart(key)+"_not_assessed")
	}
	knownReality := map[string]struct{}{"enabled": {}, "public_key": {}, "short_id": {}, "shortId": {}}
	for key, value := range realityMap {
		if _, ok := knownReality[key]; ok {
			continue
		}
		extra["reality."+key] = hashValue(value)
		unassessed = append(unassessed, "security.reality_"+sanitizeReasonPart(key)+"_not_assessed")
	}
	return security, extra, unassessed
}

func normalizeExtra(outbound map[string]any) (map[string]string, []string) {
	extra := map[string]string{}
	for _, key := range extraKeys {
		if value, ok := outbound[key]; ok && value != nil {
			extra[key] = hashValue(value)
		}
	}
	unassessed := []string{}
	for key, value := range outbound {
		if _, ok := knownTopLevelKeys[key]; ok {
			continue
		}
		extra["field."+key] = hashValue(value)
		unassessed = append(unassessed, "field."+sanitizeReasonPart(key)+"_not_assessed")
	}
	return extra, unassessed
}

func secretRef(outbound map[string]any) string {
	secrets := map[string]string{}
	for _, key := range secretKeys {
		if value, ok := outbound[key]; ok && value != nil && stringValue(value) != "" {
			secrets[key] = hashValue(value)
		}
	}
	if obfs := mapValue(outbound["obfs"]); len(obfs) > 0 {
		if value := stringValue(obfs["password"]); value != "" {
			secrets["obfs.password"] = hashValue(value)
		}
	}
	if len(secrets) == 0 {
		return ""
	}
	body, _ := json.Marshal(secrets)
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func refValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hashValue(value any) string {
	body, _ := json.Marshal(normalizeJSONValue(value))
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizeJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeJSONValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = normalizeJSONValue(item)
		}
		return out
	case string:
		return strings.TrimSpace(typed)
	default:
		return value
	}
}

func mapValue(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if typed, ok := value.(string); ok {
		return strings.TrimSpace(typed)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(typed))
		return parsed
	default:
		return 0
	}
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, _ := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed
	default:
		return false
	}
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		out := append([]string(nil), typed...)
		for index := range out {
			out[index] = strings.TrimSpace(out[index])
		}
		return uniqueStrings(out)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if value := stringValue(item); value != "" {
				out = append(out, value)
			}
		}
		return uniqueStrings(out)
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{strings.TrimSpace(typed)}
	default:
		return nil
	}
}

func firstString(value any) string {
	values := stringSlice(value)
	if len(values) == 0 {
		return stringValue(value)
	}
	return values[0]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
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

func sanitizeReasonPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteByte('_')
	}
	return strings.Trim(builder.String(), "_")
}
