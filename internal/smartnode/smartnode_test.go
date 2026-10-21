package smartnode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	matrixCoreA = CoreID("core-a")
	matrixCoreB = CoreID("core-b")
)

func TestReconcileRenameKeepsIdentityAndRevision(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	first, _, err := Reconcile(nil, []RawNode{rawVLESS("p1", "old-name", "node.example", "secret")}, now)
	if err != nil {
		t.Fatal(err)
	}
	id := onlyID(t, first)
	secondRaw := rawVLESS("p1", "new-name", "node.example", "secret")
	secondRaw.SeenAt = now.Add(time.Hour)
	second, report, err := Reconcile(first, []RawNode{secondRaw}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if onlyID(t, second) != id {
		t.Fatalf("rename changed SmartNode id: %s", onlyID(t, second))
	}
	if second[id].Revision != 1 {
		t.Fatalf("rename changed revision: %d", second[id].Revision)
	}
	if len(second[id].Aliases) != 2 || len(report.MetadataOnly) != 1 {
		t.Fatalf("rename should add metadata alias only: node=%+v report=%+v", second[id], report)
	}
}

func TestReconcileSemanticChangeIncrementsRevision(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	first, _, err := Reconcile(nil, []RawNode{rawVLESS("p1", "node", "one.example", "secret")}, now)
	if err != nil {
		t.Fatal(err)
	}
	id := onlyID(t, first)
	second, report, err := Reconcile(first, []RawNode{rawVLESS("p1", "node", "two.example", "secret")}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := second[id]; !ok {
		t.Fatalf("semantic revision lost stable id %s", id)
	}
	if second[id].Revision != 2 || second[id].Identity.Server != "two.example" {
		t.Fatalf("revision not advanced: %+v", second[id])
	}
	if len(report.Revised) != 1 || report.Revised[0] != id {
		t.Fatalf("unexpected revision report: %+v", report)
	}
}

func TestReconcileMergesProvidersByConnectionSemantics(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	catalog, _, err := Reconcile(nil, []RawNode{
		rawVLESS("p1", "alpha", "node.example", "secret"),
		rawVLESS("p2", "beta", "node.example", "secret"),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	id := onlyID(t, catalog)
	if len(catalog[id].Aliases) != 2 {
		t.Fatalf("expected two aliases, got %+v", catalog[id].Aliases)
	}
}

func TestNormalizeDoesNotPersistPlaintextSecrets(t *testing.T) {
	raw := rawVLESS("p1", "node", "node.example", "very-secret-uuid")
	raw.Outbound["tls"] = map[string]any{
		"enabled": true,
		"reality": map[string]any{
			"enabled":    true,
			"public_key": "reality-public-key",
			"short_id":   "abcd",
		},
	}
	node, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"very-secret-uuid", "reality-public-key"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("serialized SmartNode leaked %q: %s", secret, body)
		}
	}
	if !strings.HasPrefix(node.Identity.SecretRef, "sha256:") || !strings.HasPrefix(node.Identity.Security.PublicKeyRef, "sha256:") {
		t.Fatalf("expected hashed refs, got %+v", node.Identity)
	}
}

func TestCapabilityMatrixIsCoreSpecific(t *testing.T) {
	raw := rawVLESS("p1", "node", "node.example", "secret")
	raw.Outbound["transport"] = map[string]any{"type": "xhttp", "path": "/api"}
	node, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if node.Capability[matrixCoreA].Status != CapabilityDegraded {
		t.Fatalf("expected sing-box degraded, got %+v", node.Capability[matrixCoreA])
	}
	if node.Capability[matrixCoreB].Status != CapabilitySupported {
		t.Fatalf("expected xray supported, got %+v", node.Capability[matrixCoreB])
	}

	hy2 := RawNode{ProviderID: "p1", RawTag: "hy2", Outbound: map[string]any{
		"type": "hysteria2", "tag": "hy2", "server": "hy.example", "server_port": 443, "password": "secret",
	}}
	hy2Node, err := Normalize(hy2)
	if err != nil {
		t.Fatal(err)
	}
	if hy2Node.Capability[matrixCoreB].Status != CapabilityUnsupportedByCore {
		t.Fatalf("expected xray unsupported for hysteria2, got %+v", hy2Node.Capability[matrixCoreB])
	}
}

func TestCapabilityMatrixJSONMatchesLegacyStructBytes(t *testing.T) {
	primary := CoreCapability{Status: CapabilityDegraded, Reasons: []string{"transport.xhttp_semantics_incomplete"}, ReasonTexts: []string{"sing-box 无法完整表达该 XHTTP 传输语义"}}
	secondary := CoreCapability{Status: CapabilitySupported, Reasons: []string{}}
	legacyPrimaryCore := CoreID("sing-box")
	legacySecondaryCore := CoreID("xray")
	legacy := struct {
		Primary   CoreCapability `json:"sing-box"`
		Secondary CoreCapability `json:"xray"`
	}{Primary: primary, Secondary: secondary}
	dynamic := CapabilityMatrix{legacyPrimaryCore: primary, legacySecondaryCore: secondary}
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	dynamicJSON, err := json.Marshal(dynamic)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dynamicJSON, legacyJSON) {
		t.Fatalf("capability JSON changed\nlegacy=%s\ndynamic=%s", legacyJSON, dynamicJSON)
	}
	var decodedLegacy CapabilityMatrix
	if err := json.Unmarshal(legacyJSON, &decodedLegacy); err != nil {
		t.Fatal(err)
	}
	reencodedLegacy, err := json.Marshal(decodedLegacy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reencodedLegacy, legacyJSON) {
		t.Fatalf("legacy capability JSON changed after round trip\nlegacy=%s\nroundtrip=%s", legacyJSON, reencodedLegacy)
	}

	var decoded CapabilityMatrix
	if err := json.Unmarshal([]byte(`{"core-c":{"status":"supported","reasons":[]},"sing-box":{"status":"degraded","reasons":[]},"xray":{"status":"supported","reasons":[]}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 3 || decoded["core-c"].Status != CapabilitySupported {
		t.Fatalf("dynamic capability key was not decoded: %+v", decoded)
	}
}

func TestCloneCatalogPreservesDynamicCapabilities(t *testing.T) {
	source := map[string]SmartNode{
		"node": {Capability: CapabilityMatrix{
			"core-c": {Status: CapabilityDegraded, Reasons: []string{"reason.one"}, ReasonTexts: []string{"Reason one"}},
		}},
	}
	cloned := cloneCatalog(source)
	capability, exists := cloned["node"].Capability["core-c"]
	if !exists || capability.Status != CapabilityDegraded || len(capability.Reasons) != 1 {
		t.Fatalf("dynamic capability was not cloned: %+v", cloned)
	}
	capability.Reasons[0] = "changed"
	if source["node"].Capability["core-c"].Reasons[0] != "reason.one" {
		t.Fatal("cloned capability shares its reasons slice with the source")
	}
}

func TestNormalizeFailsWhenCoreEvaluatorsAreNotConfigured(t *testing.T) {
	previous := coreEvaluators
	coreEvaluators = nil
	defer func() { coreEvaluators = previous }()
	_, err := Normalize(rawVLESS("p1", "node", "node.example", "secret"))
	if err == nil || err.Error() != "smartnode core evaluators are not configured" {
		t.Fatalf("unexpected normalize error: %v", err)
	}
}

func rawVLESS(providerID, tag, server, uuid string) RawNode {
	return RawNode{ProviderID: providerID, RawTag: tag, Outbound: map[string]any{
		"type": "vless", "tag": tag, "server": server, "server_port": 443, "uuid": uuid,
	}}
}

func onlyID(t *testing.T, catalog map[string]SmartNode) string {
	t.Helper()
	if len(catalog) != 1 {
		t.Fatalf("expected one SmartNode, got %d: %+v", len(catalog), catalog)
	}
	for id := range catalog {
		return id
	}
	return ""
}
