package singbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type singBoxMigrationFixture struct {
	name     string
	outbound map[string]any
}

func TestIdentityMigrationPreservesSingBoxWorkerConfigAcrossProtocols(t *testing.T) {
	for _, fixture := range singBoxMigrationFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			raw := cloneMigrationMap(fixture.outbound)
			request := singBoxRequest()
			request.Identities["node-general"] = singBoxIdentitySource(raw["tag"].(string), raw)

			actual, _, err := BuildConfig(request)
			if err != nil {
				t.Fatalf("compile IR config: %v", err)
			}
			actualOutbound, err := outboundByTag(actual.Outbounds, "exec-general")
			if err != nil {
				t.Fatal(err)
			}
			legacy := legacySingBoxOutbound(raw, "exec-general")
			if !jsonEqual(actualOutbound, legacy) {
				actualBody, _ := json.Marshal(actualOutbound)
				legacyBody, _ := json.Marshal(legacy)
				t.Fatalf("IR outbound differs from legacy direct translation\nactual=%s\nlegacy=%s", actualBody, legacyBody)
			}

			legacyConfig := actual
			legacyConfig.Outbounds = append([]map[string]any(nil), actual.Outbounds...)
			for index, outbound := range legacyConfig.Outbounds {
				if outbound["tag"] == "exec-general" {
					legacyConfig.Outbounds[index] = legacy
					break
				}
			}
			actualBody, err := json.Marshal(actual)
			if err != nil {
				t.Fatal(err)
			}
			legacyBody, err := json.Marshal(legacyConfig)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actualBody, legacyBody) {
				t.Fatalf("IR worker config differs from legacy config for %s", fixture.name)
			}
		})
	}
}

func TestIdentityMigrationConfigsPassSingBoxCheck(t *testing.T) {
	binary := os.Getenv("SMARTGATE_SINGBOX_BIN")
	if binary == "" {
		t.Skip("SMARTGATE_SINGBOX_BIN is required for the real-core migration check")
	}
	for _, fixture := range singBoxMigrationFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			raw := cloneMigrationMap(fixture.outbound)
			request := singBoxRequest()
			request.Identities["node-general"] = singBoxIdentitySource(raw["tag"].(string), raw)
			config, _, err := BuildConfig(request)
			if err != nil {
				t.Fatalf("compile config: %v", err)
			}
			body, err := json.MarshalIndent(config, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), fixture.name+".json")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "check", "-c", path)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("sing-box check failed: %v\n%s", err, output)
			}
		})
	}
}

func singBoxMigrationFixtures() []singBoxMigrationFixture {
	return []singBoxMigrationFixture{
		{
			name: "anytls",
			outbound: map[string]any{
				"type": "anytls", "tag": "anytls-source", "server": "anytls.example", "server_port": 443,
				"password": "anytls-password", "tls": map[string]any{"enabled": true, "server_name": "anytls.example"},
			},
		},
		{
			name: "shadowsocks",
			outbound: map[string]any{
				"type": "shadowsocks", "tag": "ss-source", "server": "ss.example", "server_port": 8388,
				"method": "aes-128-gcm", "password": "ss-password",
			},
		},
		{
			name: "vmess",
			outbound: map[string]any{
				"type": "vmess", "tag": "vmess-source", "server": "vmess.example", "server_port": 443,
				"uuid": "11111111-1111-4111-8111-111111111111", "alter_id": 0, "security": "auto",
				"transport": map[string]any{"type": "ws", "path": "/vmess", "headers": map[string]any{"Host": "edge.vmess.example"}},
				"tls":       map[string]any{"enabled": true, "server_name": "vmess.example", "utls": map[string]any{"enabled": true, "fingerprint": "chrome"}},
			},
		},
		{
			name: "vless",
			outbound: map[string]any{
				"type": "vless", "tag": "vless-source", "server": "vless.example", "server_port": 443,
				"uuid": "22222222-2222-4222-8222-222222222222", "packet_encoding": "xudp", "flow": "xtls-rprx-vision",
				"transport": map[string]any{"type": "grpc", "service_name": "vless-service"},
				"tls": map[string]any{
					"enabled": true, "server_name": "vless.example", "utls": map[string]any{"enabled": true, "fingerprint": "chrome"},
					"reality": map[string]any{"enabled": true, "public_key": "njK6EAS5uNMSnGSZdMlS4792Ruy0VTfuYmZT0pSi3x8", "short_id": "abcd"},
				},
			},
		},
		{
			name: "trojan",
			outbound: map[string]any{
				"type": "trojan", "tag": "trojan-source", "server": "trojan.example", "server_port": 443,
				"password": "trojan-password", "tls": map[string]any{"enabled": true, "server_name": "trojan.example"},
			},
		},
		{
			name: "hysteria",
			outbound: map[string]any{
				"type": "hysteria", "tag": "hysteria-source", "server": "hysteria.example", "server_port": 443,
				"auth_str": "hysteria-auth", "up_mbps": 20, "down_mbps": 100, "obfs": "obfs-secret",
				"tls": map[string]any{"enabled": true, "server_name": "hysteria.example"},
			},
		},
		{
			name: "hysteria2",
			outbound: map[string]any{
				"type": "hysteria2", "tag": "hysteria2-source", "server": "hysteria2.example", "server_port": 443,
				"password": "hysteria2-password", "up_mbps": 20, "down_mbps": 100,
				"obfs": map[string]any{"type": "salamander", "password": "salamander-password"},
				"tls":  map[string]any{"enabled": true, "server_name": "hysteria2.example"},
			},
		},
		{
			name: "tuic",
			outbound: map[string]any{
				"type": "tuic", "tag": "tuic-source", "server": "tuic.example", "server_port": 443,
				"uuid": "33333333-3333-4333-8333-333333333333", "password": "tuic-password", "congestion_control": "bbr",
				"tls": map[string]any{"enabled": true, "server_name": "tuic.example"},
			},
		},
		{
			name: "socks",
			outbound: map[string]any{
				"type": "socks", "tag": "socks-source", "server": "socks.example", "server_port": 1080,
				"username": "socks-user", "password": "socks-password",
			},
		},
		{
			name: "http",
			outbound: map[string]any{
				"type": "http", "tag": "http-source", "server": "http.example", "server_port": 8080,
				"username": "http-user", "password": "http-password",
			},
		},
	}
}

func legacySingBoxOutbound(raw map[string]any, executorID string) map[string]any {
	outbound := cloneMigrationMap(raw)
	delete(outbound, "detour")
	delete(outbound, "domain_resolver")
	delete(outbound, "multiplex")
	outbound["tag"] = executorID
	return outbound
}

func cloneMigrationMap(input map[string]any) map[string]any {
	body, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	var output map[string]any
	if err := json.Unmarshal(body, &output); err != nil {
		panic(err)
	}
	return output
}

func outboundByTag(outbounds []map[string]any, tag string) (map[string]any, error) {
	for _, outbound := range outbounds {
		if outbound["tag"] == tag {
			return outbound, nil
		}
	}
	return nil, fmt.Errorf("outbound %s not found", tag)
}

func jsonEqual(left, right any) bool {
	leftBody, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightBody, err := json.Marshal(right)
	return err == nil && bytes.Equal(leftBody, rightBody)
}
