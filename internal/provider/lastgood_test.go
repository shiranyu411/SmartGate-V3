package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func TestLastGoodStoreRoundTripAndRejectsUnsafeProviderID(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewLastGoodStore(layout)
	outbounds := []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}}
	if _, err := store.Save("provider-1", outbounds); err != nil {
		t.Fatalf("save last-good: %v", err)
	}
	env, err := store.Load("provider-1")
	if err != nil {
		t.Fatalf("load last-good: %v", err)
	}
	if env.ProviderID != "provider-1" || env.Count != 1 {
		t.Fatalf("unexpected last-good envelope: %+v", env)
	}
	if len(env.Identities) != 1 || env.Identities[0].RawTag != "node-1" || env.Identities[0].Identity.Protocol != "vmess" {
		t.Fatalf("last-good did not persist identity: %+v", env.Identities)
	}
	body, err := os.ReadFile(filepath.Join(layout.LastGoodDir(), "provider-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &encoded); err != nil {
		t.Fatal(err)
	}
	if len(encoded["identities"]) == 0 || len(encoded["outbounds"]) != 0 {
		t.Fatalf("new last-good file must contain identities without legacy outbounds: %s", body)
	}
	if _, err := store.Save("../escape", outbounds); err == nil {
		t.Fatal("expected unsafe provider id to be rejected")
	}
}

func TestLastGoodStoreLoadsLegacyEnvelopeWithoutIdentities(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
  "provider_id": "provider-1",
  "saved_at": "2026-08-04T00:00:00Z",
  "count": 1,
  "outbounds": [{"type":"vmess","tag":"node-1","server":"example.com","server_port":443,"uuid":"secret"}]
}`)
	if err := os.WriteFile(filepath.Join(layout.LastGoodDir(), "provider-1.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := NewLastGoodStore(layout).Load("provider-1")
	if err != nil {
		t.Fatalf("load legacy envelope: %v", err)
	}
	if len(env.Identities) != 1 || env.Identities[0].Identity.Protocol != "vmess" || env.Identities[0].Identity.SecretRef == "" {
		t.Fatalf("legacy envelope was not promoted to protected identity input: %+v", env.Identities)
	}
}

func TestLastGoodStoreRoundTripsExplicitEmptyALPN(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	outbound := map[string]any{
		"type": "vmess", "tag": "node-1", "server": "example.com", "server_port": 443,
		"tls": map[string]any{"enabled": true, "alpn": []any{}},
	}
	if _, err := NewLastGoodStore(layout).Save("provider-1", []map[string]any{outbound}); err != nil {
		t.Fatalf("save explicit empty ALPN: %v", err)
	}
	if _, err := NewLastGoodStore(layout).Load("provider-1"); err != nil {
		t.Fatalf("load explicit empty ALPN: %v", err)
	}
}

func TestLastGoodStoreRejectsMissingIdentityTag(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	_, err := NewLastGoodStore(layout).Save("provider-1", []map[string]any{{
		"type": "vmess", "server": "example.com", "server_port": 443,
	}})
	if err == nil || !strings.Contains(err.Error(), "alias tag is required") {
		t.Fatalf("missing tag was converted into an identity alias: %v", err)
	}
}

func TestLastGoodStoreRejectsTamperedDualEmitIdentity(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	outbound := map[string]any{
		"type": "vmess", "tag": "node-1", "server": "example.com", "server_port": 443, "uuid": "secret",
	}
	identities, err := identitySourcesFromOutbounds("provider-1", []map[string]any{outbound})
	if err != nil {
		t.Fatal(err)
	}
	identities[0].Identity.Server = "tampered.example"
	body, err := json.Marshal(LastGoodEnvelope{
		ProviderID: "provider-1",
		Count:      1,
		Identities: identities,
		Outbounds:  []map[string]any{outbound},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.LastGoodDir(), "provider-1.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLastGoodStore(layout).Load("provider-1"); err == nil || !strings.Contains(err.Error(), "does not match outbound semantics") {
		t.Fatalf("tampered dual-emit identity was accepted: %v", err)
	}
}

func TestLastGoodStoreRejectsExplicitEmptyIdentities(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
  "provider_id": "provider-1",
  "identities": [],
  "outbounds": [{"type":"vmess","tag":"node-1","server":"example.com"}]
}`)
	if err := os.WriteFile(filepath.Join(layout.LastGoodDir(), "provider-1.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLastGoodStore(layout).Load("provider-1"); err == nil || !strings.Contains(err.Error(), "identities count 0 does not match outbounds count 1") {
		t.Fatalf("explicit empty identities was treated as legacy omission: %v", err)
	}
}

func TestLastGoodStorePromotesLegacyUpdateWithoutIdentities(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewLastGoodStore(layout)
	_, err := store.SaveMany(context.Background(), []LastGoodUpdate{{
		ProviderID: "provider-1",
		Outbounds:  []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}},
	}})
	if err != nil {
		t.Fatalf("promote legacy update: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(layout.LastGoodDir(), "provider-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"outbounds"`)) {
		t.Fatalf("promoted update rewrote legacy outbounds: %s", body)
	}
}

func TestNormalizeLastGoodSnapshotsDropsLegacyRepresentation(t *testing.T) {
	normalized, err := NormalizeLastGoodSnapshots([]state.LastGoodSnapshot{{
		ProviderID: "provider-1",
		Outbounds: []map[string]any{{
			"type": "vmess", "tag": "node-1", "server": "example.com",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 1 || len(normalized[0].Identities) != 1 || normalized[0].Identities[0].RawTag != "node-1" {
		t.Fatalf("legacy snapshot was not promoted: %+v", normalized)
	}
	if len(normalized[0].Outbounds) != 0 {
		t.Fatalf("normalized snapshot retained legacy representation: %+v", normalized[0].Outbounds)
	}
}

func TestLastGoodStoreRejectsExplicitEmptyIdentitiesOnRestore(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	_, err := NewLastGoodStore(layout).ReplaceAll(context.Background(), []LastGoodUpdate{{
		ProviderID: "provider-1",
		Identities: []smartnode.IdentitySource{},
		Outbounds:  []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "identities count 0 does not match outbounds count 1") {
		t.Fatalf("explicit empty identity set was promoted during restore: %v", err)
	}
}

func TestLastGoodStoreLoadsLegacyRawArray(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout.LastGoodDir(), "provider-1.json"), []byte(`[{"type":"vmess","tag":"node-1","server":"example.com"}]`), 0o644); err != nil {
		t.Fatalf("write legacy last-good: %v", err)
	}
	env, err := NewLastGoodStore(layout).Load("provider-1")
	if err != nil {
		t.Fatalf("load legacy last-good: %v", err)
	}
	if env.ProviderID != "provider-1" || env.Count != 1 {
		t.Fatalf("unexpected legacy envelope: %+v", env)
	}
}

func TestLastGoodStoreBoundsLoadAllAndOverwritesOversizedExistingFile(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewLastGoodStore(layout)
	path := filepath.Join(layout.LastGoodDir(), "provider-1.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), MaxDryRunLastGoodBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	var limitErr *TopologyResourceLimitError
	if _, err := store.Load("provider-1"); !errors.As(err, &limitErr) {
		t.Fatalf("unbounded Load accepted oversized file: %T %v", err, err)
	}
	if _, err := store.LoadAll(context.Background()); !errors.As(err, &limitErr) {
		t.Fatalf("unbounded LoadAll accepted oversized file: %T %v", err, err)
	}
	outbounds := []map[string]any{{"type": "vmess", "tag": "replacement", "server": "example.com"}}
	if _, err := store.Save("provider-1", outbounds); err != nil {
		t.Fatalf("bounded save could not replace oversized old file: %v", err)
	}
	env, err := store.Load("provider-1")
	if err != nil || len(env.Identities) != 1 || env.Identities[0].RawTag != "replacement" {
		t.Fatalf("replacement was not persisted: env=%+v err=%v", env, err)
	}
	tooMany := make([]map[string]any, MaxDryRunLastGoodNodes+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"type": "vmess", "tag": "node", "server": "example.com"}
	}
	if _, err := store.ReplaceAll(context.Background(), []LastGoodUpdate{{ProviderID: "provider-2", Outbounds: tooMany}}); !errors.As(err, &limitErr) {
		t.Fatalf("oversized ReplaceAll was not rejected before mutation: %T %v", err, err)
	}
	env, err = store.Load("provider-1")
	if err != nil || env.Identities[0].RawTag != "replacement" {
		t.Fatalf("failed ReplaceAll destroyed prior last-good: env=%+v err=%v", env, err)
	}
}

func TestLastGoodStoreRejectsSequentialSavesBeyondAggregateBudget(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewLastGoodStore(layout)
	makeNodes := func(count int, prefix string) []map[string]any {
		out := make([]map[string]any, count)
		for i := range out {
			out[i] = map[string]any{"type": "vmess", "tag": prefix, "server": "example.com"}
		}
		return out
	}
	if _, err := store.Save("provider-1", makeNodes(2000, "first")); err != nil {
		t.Fatalf("save first bounded provider: %v", err)
	}
	var limitErr *TopologyResourceLimitError
	if _, err := store.Save("provider-2", makeNodes(2500, "second")); !errors.As(err, &limitErr) {
		t.Fatalf("sequential save exceeded aggregate budget without rejection: %T %v", err, err)
	}
	if _, err := store.Load("provider-1"); err != nil {
		t.Fatalf("rejected aggregate save damaged prior data: %v", err)
	}
	if _, err := store.Load("provider-2"); !os.IsNotExist(err) {
		t.Fatalf("rejected provider was partially persisted: %v", err)
	}
}

func TestLastGoodStoreUsesExactEncodedBytesForPostSaveBudget(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewLastGoodStore(layout)
	envelope := LastGoodEnvelope{
		ProviderID: "provider-1",
		SavedAt:    time.Now().UTC(),
		Count:      1,
		Outbounds:  []map[string]any{{"type": "vmess", "tag": "first", "server": "example.com", "padding": ""}},
	}
	base, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	padding := MaxDryRunLastGoodBytes - len(base) - 64
	if padding <= 0 {
		t.Fatal("test envelope overhead exceeds budget")
	}
	envelope.Outbounds[0]["padding"] = strings.Repeat("x", padding)
	body, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil || len(body) >= MaxDryRunLastGoodBytes {
		t.Fatalf("construct near-limit envelope: bytes=%d err=%v", len(body), err)
	}
	if err := os.WriteFile(filepath.Join(layout.LastGoodDir(), "provider-1.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	var limitErr *TopologyResourceLimitError
	if _, err := store.Save("provider-2", []map[string]any{{"type": "vmess", "tag": "second", "server": "example.com"}}); !errors.As(err, &limitErr) {
		t.Fatalf("post-save exact byte budget was not enforced: %T %v", err, err)
	}
	if _, err := store.Load("provider-1"); err != nil {
		t.Fatalf("rejected near-boundary save damaged existing data: %v", err)
	}
}

func TestLastGoodStorePruneExceptRemovesOnlyOrphanSafeProviderFiles(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewLastGoodStore(layout)
	outbounds := []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}}
	for _, id := range []string{"keep", "drop"} {
		if _, err := store.Save(id, outbounds); err != nil {
			t.Fatalf("save %s last-good: %v", id, err)
		}
	}
	if err := os.WriteFile(filepath.Join(layout.LastGoodDir(), "unsafe id.json"), []byte(`[]`), 0o644); err != nil {
		t.Fatalf("write unsafe last-good filename: %v", err)
	}

	removed, err := store.PruneExcept(context.Background(), map[string]struct{}{"keep": {}})
	if err != nil {
		t.Fatalf("prune last-good: %v", err)
	}
	if len(removed) != 1 || removed[0] != "drop" {
		t.Fatalf("unexpected removed providers: %+v", removed)
	}
	if !store.Exists("keep") {
		t.Fatal("kept provider last-good should remain")
	}
	if store.Exists("drop") {
		t.Fatal("orphan provider last-good should be removed")
	}
	if _, err := os.Stat(filepath.Join(layout.LastGoodDir(), "unsafe id.json")); err != nil {
		t.Fatalf("unsafe filename should be left untouched: %v", err)
	}
}

func TestHashEffectiveTopologyTracksNodeContentNotSaveTime(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	store := NewLastGoodStore(layout)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:      "provider-1",
		Name:    "Provider 1",
		Type:    TypeManual,
		Enabled: true,
	}}
	first := []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}}
	if _, err := store.Save("provider-1", first); err != nil {
		t.Fatalf("save first last-good: %v", err)
	}
	hash1, err := HashEffectiveTopology(context.Background(), spec, store)
	if err != nil {
		t.Fatalf("hash first topology: %v", err)
	}
	if _, err := store.Save("provider-1", first); err != nil {
		t.Fatalf("save same last-good: %v", err)
	}
	hash2, err := HashEffectiveTopology(context.Background(), spec, store)
	if err != nil {
		t.Fatalf("hash unchanged topology: %v", err)
	}
	if hash1 != hash2 {
		t.Fatalf("same node content should keep topology hash stable: %s != %s", hash1, hash2)
	}
	if _, err := store.Save("provider-1", []map[string]any{{"type": "vmess", "tag": "node-2", "server": "changed.example"}}); err != nil {
		t.Fatalf("save changed last-good: %v", err)
	}
	hash3, err := HashEffectiveTopology(context.Background(), spec, store)
	if err != nil {
		t.Fatalf("hash changed topology: %v", err)
	}
	if hash3 == hash1 {
		t.Fatal("changed node content should change topology hash")
	}
}

func TestParseSubscriptionPayloadRejectsUnsafeOrEmptyPayloads(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(""),
		[]byte("<html>forbidden</html>"),
		[]byte(`[{"type":"unknown-proxy","tag":"bad","server":"example.com"}]`),
	} {
		if _, err := ParseSubscriptionPayload("p1", body); err == nil {
			t.Fatalf("expected payload %q to fail", string(body))
		}
	}
}

func TestParseSubscriptionPayloadExtractsSupportedOutbounds(t *testing.T) {
	body := []byte(`{
		"outbounds": [
			{"type":"selector","tag":"auto","outbounds":["node-1"]},
			{"type":"vmess","tag":"node-1","server":"example.com"},
			{"type":"naive","tag":"unsupported","server":"example.org"}
		]
	}`)
	parsed, err := ParseSubscriptionPayload("p1", body)
	if err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if len(parsed.Identities) != 1 || parsed.Identities[0].RawTag != "node-1" {
		t.Fatalf("unexpected identity subscription: %+v", parsed)
	}
}

func TestParseSubscriptionPayloadAdmissionMatrixPreservesTenProtocolSet(t *testing.T) {
	protocols := []string{"anytls", "shadowsocks", "vmess", "vless", "trojan", "hysteria", "hysteria2", "tuic", "socks", "http"}
	outbounds := make([]map[string]any, 0, len(protocols))
	for _, protocol := range protocols {
		outbounds = append(outbounds, map[string]any{
			"type":        protocol,
			"tag":         "node-" + protocol,
			"server":      "example.com",
			"server_port": 443,
		})
	}
	body, err := json.Marshal(outbounds)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSubscriptionPayload("p1", body)
	if err != nil {
		t.Fatalf("parse ten-protocol payload: %v", err)
	}
	if len(parsed.Identities) != len(protocols) {
		t.Fatalf("provider admission changed protocol set: identities=%d want=%d", len(parsed.Identities), len(protocols))
	}
	secondCoreUnsupported := map[string]bool{"anytls": true, "hysteria": true, "hysteria2": true, "tuic": true}
	for _, source := range parsed.Identities {
		if source.Identity.Protocol == "" {
			t.Fatalf("dual-emitted identity has no protocol: %+v", source)
		}
		matrix, err := smartnode.EvaluateCapability(source.Identity, smartnode.UnassessedReasons(source.Identity))
		if err != nil {
			t.Fatalf("evaluate %s: %v", source.Identity.Protocol, err)
		}
		firstCore := matrix[providerTestCores[0]]
		if firstCore.Status != smartnode.CapabilitySupported {
			t.Fatalf("sing-box admission drift for %s: %+v", source.Identity.Protocol, firstCore)
		}
		secondCore := matrix[providerTestCores[1]]
		wantSecondStatus := smartnode.CapabilitySupported
		if secondCoreUnsupported[source.Identity.Protocol] {
			wantSecondStatus = smartnode.CapabilityUnsupportedByCore
		}
		if secondCore.Status != wantSecondStatus {
			t.Fatalf("xray admission drift for %s: got=%+v want status=%s", source.Identity.Protocol, secondCore, wantSecondStatus)
		}
	}
}

func TestParseSubscriptionPayloadExtractsBase64URIIdentities(t *testing.T) {
	body := []byte("YW55dGxzOi8vcGFzc0BleGFtcGxlLmNvbTo0NDM/c25pPWV4YW1wbGUub3JnJmluc2VjdXJlPTEjQW55VExTLU5vZGUKdmxlc3M6Ly91dWlkQHZsZXNzLmV4YW1wbGU6NDQzP3NlY3VyaXR5PXJlYWxpdHkmc25pPXd3dy5taWNyb3NvZnQuY29tJnBiaz1wdWJsaWMta2V5JnNpZD0xMjM0JmZwPWNocm9tZSNWTEVTUy1Ob2Rl")
	parsed, err := ParseSubscriptionPayload("p1", body)
	if err != nil {
		t.Fatalf("parse URI payload: %v", err)
	}
	if len(parsed.Identities) != 2 {
		t.Fatalf("expected 2 identities, got %+v", parsed)
	}
	if parsed.Identities[0].Identity.Protocol != "anytls" || parsed.Identities[0].RawTag != "AnyTLS-Node" {
		t.Fatalf("unexpected first identity: %+v", parsed.Identities[0])
	}
	if parsed.Identities[0].Identity.Security.Type == "reality" {
		t.Fatalf("first identity should not get reality security: %+v", parsed.Identities[0].Identity.Security)
	}
	if parsed.Identities[1].Identity.Protocol != "vless" || parsed.Identities[1].RawTag != "VLESS-Node" {
		t.Fatalf("unexpected second identity: %+v", parsed.Identities[1])
	}
}

func TestParseSubscriptionPayloadExtractsEcosystemYAMLIdentities(t *testing.T) {
	body := []byte(`
mixed-port: 7890
proxies:
  - name: SS Node
    type: ss
    server: ss.example.com
    port: 8388
    cipher: 2022-blake3-aes-128-gcm
    password: pass
  - name: VLESS Reality
    type: vless
    server: vless.example.com
    port: 443
    uuid: uuid-1
    flow: xtls-rprx-vision
    security: reality
    sni: www.microsoft.com
    client-fingerprint: chrome
    reality-opts:
      public-key: public-key
      short-id: "1234"
  - name: VMess WS
    type: vmess
    server: vmess.example.com
    port: 443
    uuid: uuid-2
    cipher: auto
    tls: true
    network: ws
    ws-opts:
      path: /ws
      headers:
        Host: edge.example.com
  - { name: HY2 Inline, type: hy2, server: hy2.example.com, port: 8443, password: secret, sni: hy2.example.com, obfs: salamander, obfs-password: obfs-pass }
  - { name: AnyTLS Inline, type: anytls, server: anytls.example.com, port: 443, password: any-pass, sni: anytls.example.com }
  - name: unsupported
    type: ssr
    server: ssr.example.com
    port: 443
proxy-groups:
  - name: auto
    type: select
    proxies:
      - SS Node
`)
	parsed, err := ParseSubscriptionPayload("p1", body)
	if err != nil {
		t.Fatalf("parse ecosystem YAML: %v", err)
	}
	if len(parsed.Identities) != 5 {
		t.Fatalf("expected 5 identities, got %+v", parsed)
	}
	if parsed.Identities[0].Identity.Protocol != "shadowsocks" || parsed.Identities[0].RawTag != "SS Node" || parsed.Identities[0].Identity.Port != 8388 {
		t.Fatalf("unexpected first identity: %+v", parsed.Identities[0])
	}
	realitySecurity := parsed.Identities[1].Identity.Security
	if realitySecurity.Type != "reality" || realitySecurity.SNI != "www.microsoft.com" || realitySecurity.ShortID != "1234" || realitySecurity.PublicKeyRef == "" {
		t.Fatalf("unexpected reality security: %+v", realitySecurity)
	}
	transport := parsed.Identities[2].Identity.Transport
	if transport.Type != "ws" || transport.Path != "/ws" || transport.Host != "edge.example.com" {
		t.Fatalf("unexpected transport: %+v", transport)
	}
	var obfs map[string]any
	if ok, err := parsed.Identities[3].ResolveExtra("obfs", &obfs); err != nil || !ok || obfs["type"] != "salamander" || obfs["password"] != "obfs-pass" {
		t.Fatalf("unexpected obfs reference: value=%+v present=%t err=%v", obfs, ok, err)
	}
	secrets, err := parsed.Identities[4].ResolveSecrets()
	if err != nil || parsed.Identities[4].Identity.Protocol != "anytls" || secrets["password"] != "any-pass" {
		t.Fatalf("unexpected final identity: source=%+v secrets=%+v err=%v", parsed.Identities[4], secrets, err)
	}
}

func TestParseSubscriptionPayloadAddsDefaultUTLSForRealityURI(t *testing.T) {
	body := []byte("vless://uuid@vless.example:443?security=reality&sni=www.microsoft.com&pbk=public-key&sid=1234#Reality-Node")
	parsed, err := ParseSubscriptionPayload("p1", body)
	if err != nil {
		t.Fatalf("parse reality URI: %v", err)
	}
	security := parsed.Identities[0].Identity.Security
	if security.Type != "reality" || security.Fingerprint != "chrome" {
		t.Fatalf("reality URI should get default fingerprint, got %+v", security)
	}
}
