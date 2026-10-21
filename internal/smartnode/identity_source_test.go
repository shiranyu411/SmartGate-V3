package smartnode

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestIdentitySourceKeepsPlaintextOutsideNodeIdentityAndResolvesReferences(t *testing.T) {
	outbound := map[string]any{
		"type": "vless", "tag": "node", "server": "EXAMPLE.com.", "server_port": 443,
		"uuid": "uuid-secret", "password": "password-secret", "method": "aes-128-gcm",
		"future_field": map[string]any{"enabled": true},
		"transport":    map[string]any{"type": "xhttp", "path": "/api", "host": "CDN.EXAMPLE", "mode": "auto"},
		"tls": map[string]any{
			"enabled": true, "server_name": "EDGE.EXAMPLE", "certificate": "certificate-secret", "disable_sni": true,
			"reality": map[string]any{"enabled": true, "public_key": "public-key-secret", "short_id": "abcd", "spider_x": "/"},
		},
	}
	source, err := IdentitySourceFromRaw(RawNode{ProviderID: "p1", RawTag: "node", Outbound: outbound})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(source.Identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, plaintext := range []string{"uuid-secret", "password-secret", "certificate-secret", "public-key-secret", "aes-128-gcm", "auto"} {
		if strings.Contains(string(body), plaintext) {
			t.Fatalf("NodeIdentity contains plaintext %q: %s", plaintext, body)
		}
	}

	secrets, err := source.ResolveSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if secrets["uuid"] != "uuid-secret" || secrets["password"] != "password-secret" {
		t.Fatalf("unexpected resolved secrets: %+v", secrets)
	}
	var method string
	if found, err := source.ResolveExtra("method", &method); err != nil || !found || method != "aes-128-gcm" {
		t.Fatalf("resolve method: found=%t value=%q err=%v", found, method, err)
	}
	var publicKey string
	if err := source.ResolveReference(source.Identity.Security.PublicKeyRef, &publicKey); err != nil || publicKey != "public-key-secret" {
		t.Fatalf("resolve public key: value=%q err=%v", publicKey, err)
	}
	var certificate string
	if err := source.ResolveReference(source.Identity.Security.CertificateRef, &certificate); err != nil || certificate != "certificate-secret" {
		t.Fatalf("resolve certificate: value=%q err=%v", certificate, err)
	}

	wantReasons := []string{
		"field.future_field_not_assessed",
		"security.disable_sni_not_assessed",
		"security.reality_spider_x_not_assessed",
		"transport.mode_not_assessed",
	}
	if got := UnassessedReasons(source.Identity); !reflect.DeepEqual(got, wantReasons) {
		t.Fatalf("unassessed reasons = %v, want %v", got, wantReasons)
	}
}

func TestIdentitySourceRejectsMissingReference(t *testing.T) {
	source, err := IdentitySourceFromRaw(RawNode{
		ProviderID: "p1",
		RawTag:     "node",
		Outbound:   map[string]any{"type": "vless", "tag": "node", "server": "example.com", "server_port": 443, "uuid": "uuid-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	delete(source.References, source.Identity.SecretRef)
	if _, err := source.ResolveSecrets(); err == nil {
		t.Fatal("missing secret reference was accepted")
	}
}

func TestIdentitySourceRejectsUnserializableProtectedValue(t *testing.T) {
	_, err := IdentitySourceFromRaw(RawNode{
		ProviderID: "p1",
		RawTag:     "node",
		Outbound: map[string]any{
			"type": "vmess", "tag": "node", "server": "example.com", "server_port": 443,
			"uuid": func() {},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "collect identity references") {
		t.Fatalf("unserializable protected value was silently discarded: %v", err)
	}
}
