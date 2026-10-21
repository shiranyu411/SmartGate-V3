package xray

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"smartgate/internal/state"
)

type xrayMigrationFixture struct {
	name     string
	outbound map[string]any
}

func TestIdentityMigrationPreservesXrayWorkerConfigAcrossSupportedProtocols(t *testing.T) {
	for _, fixture := range xrayMigrationFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			raw := cloneXrayMigrationMap(fixture.outbound)
			request := xrayRequest()
			request.Identities["node-ai"] = xrayIdentitySource(raw["tag"].(string), raw)
			actual, _, err := BuildConfig(request)
			if err != nil {
				t.Fatalf("compile IR config: %v", err)
			}
			actualOutbound, err := xrayOutboundByTag(actual.Outbounds, "exec-ai")
			if err != nil {
				t.Fatal(err)
			}
			legacy := legacyXrayOutbound(request.Executors[0], raw)
			if !xrayJSONEqual(actualOutbound, legacy) {
				actualBody, _ := json.Marshal(actualOutbound)
				legacyBody, _ := json.Marshal(legacy)
				t.Fatalf("IR outbound differs from legacy direct translation\nactual=%s\nlegacy=%s", actualBody, legacyBody)
			}

			legacyConfig := actual
			legacyConfig.Outbounds = append([]map[string]any(nil), actual.Outbounds...)
			for index, outbound := range legacyConfig.Outbounds {
				if outbound["tag"] == "exec-ai" {
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

func TestIdentityMigrationConfigsPassXrayTest(t *testing.T) {
	binary := os.Getenv("SMARTGATE_XRAY_BIN")
	if binary == "" {
		t.Skip("SMARTGATE_XRAY_BIN is required for the real-core migration check")
	}
	for _, fixture := range xrayMigrationFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			raw := cloneXrayMigrationMap(fixture.outbound)
			request := xrayRequest()
			request.Identities["node-ai"] = xrayIdentitySource(raw["tag"].(string), raw)
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
			cmd := exec.Command(binary, "run", "-test", "-config", path)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("xray config test failed: %v\n%s", err, output)
			}
		})
	}
}

func TestXrayIdentityCompilerRejectsTheFourSingBoxOnlyProtocols(t *testing.T) {
	for _, protocol := range []string{"anytls", "hysteria", "hysteria2", "tuic"} {
		t.Run(protocol, func(t *testing.T) {
			request := xrayRequest()
			raw := map[string]any{"type": protocol, "tag": protocol + "-source", "server": "example.com", "server_port": 443}
			request.Identities["node-ai"] = xrayIdentitySource(protocol+"-source", raw)
			if _, _, err := BuildConfig(request); err == nil {
				t.Fatalf("Xray accepted unsupported protocol %s", protocol)
			}
		})
	}
}

func legacyXrayOutbound(executor state.NodeExecutor, raw map[string]any) map[string]any {
	protocol := strings.ToLower(legacyXrayStringValue(raw["type"]))
	server := legacyXrayStringValue(raw["server"])
	port := legacyXrayIntValue(raw["server_port"])
	settings := map[string]any{}
	switch protocol {
	case "vless":
		id := legacyXrayStringValue(raw["uuid"])
		user := map[string]any{"id": id, "encryption": legacyXrayFirstNonEmpty(legacyXrayStringValue(raw["encryption"]), "none")}
		if flow := legacyXrayStringValue(raw["flow"]); flow != "" {
			user["flow"] = flow
		}
		settings["vnext"] = []map[string]any{{"address": server, "port": port, "users": []map[string]any{user}}}
	case "vmess":
		id := legacyXrayStringValue(raw["uuid"])
		user := map[string]any{"id": id, "alterId": legacyXrayIntValue(raw["alter_id"]), "security": legacyXrayFirstNonEmpty(legacyXrayStringValue(raw["security"]), "auto")}
		settings["vnext"] = []map[string]any{{"address": server, "port": port, "users": []map[string]any{user}}}
	case "trojan":
		settings["servers"] = []map[string]any{{"address": server, "port": port, "password": legacyXrayStringValue(raw["password"])}}
	case "shadowsocks":
		method := legacyXrayFirstNonEmpty(legacyXrayStringValue(raw["method"]), legacyXrayStringValue(raw["cipher"]))
		settings["servers"] = []map[string]any{{"address": server, "port": port, "method": method, "password": legacyXrayStringValue(raw["password"])}}
	case "socks", "http":
		serverConfig := map[string]any{"address": server, "port": port}
		username := legacyXrayStringValue(raw["username"])
		password := legacyXrayStringValue(raw["password"])
		if username != "" || password != "" {
			serverConfig["users"] = []map[string]any{{"user": username, "pass": password}}
		}
		settings["servers"] = []map[string]any{serverConfig}
	}
	stream := legacyXrayStreamSettings(executor.ID, raw)
	return map[string]any{
		"tag": executor.ID, "protocol": protocol, "settings": settings, "streamSettings": stream,
		"mux": map[string]any{"enabled": false},
	}
}

func legacyXrayStreamSettings(executorID string, raw map[string]any) map[string]any {
	transport := legacyXrayMapValue(raw["transport"])
	network := strings.ToLower(legacyXrayFirstNonEmpty(legacyXrayStringValue(transport["type"]), legacyXrayStringValue(raw["network"]), "tcp"))
	if network == "http" {
		network = "httpupgrade"
	}
	stream := map[string]any{"network": network, "sockopt": map[string]any{"tcpFastOpen": false}}
	switch network {
	case "tcp":
	case "ws":
		settings := map[string]any{"path": legacyXrayFirstNonEmpty(legacyXrayStringValue(transport["path"]), "/")}
		if host := legacyXrayTransportHost(transport); host != "" {
			settings["headers"] = map[string]any{"Host": host}
		}
		stream["wsSettings"] = settings
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": legacyXrayFirstNonEmpty(legacyXrayStringValue(transport["service_name"]), legacyXrayStringValue(transport["serviceName"]))}
	case "httpupgrade":
		settings := map[string]any{"path": legacyXrayFirstNonEmpty(legacyXrayStringValue(transport["path"]), "/")}
		if host := legacyXrayTransportHost(transport); host != "" {
			settings["host"] = host
		}
		stream["httpupgradeSettings"] = settings
	case "xhttp":
		settings := map[string]any{"path": legacyXrayFirstNonEmpty(legacyXrayStringValue(transport["path"]), "/")}
		if host := legacyXrayTransportHost(transport); host != "" {
			settings["host"] = host
		}
		if mode := legacyXrayStringValue(transport["mode"]); mode != "" {
			settings["mode"] = mode
		}
		stream["xhttpSettings"] = settings
	case "quic":
		stream["quicSettings"] = map[string]any{}
	}
	tlsConfig := legacyXrayMapValue(raw["tls"])
	reality := legacyXrayMapValue(tlsConfig["reality"])
	if legacyXrayBoolValue(reality["enabled"]) || legacyXrayStringValue(reality["public_key"]) != "" {
		settings := map[string]any{
			"serverName": legacyXrayFirstNonEmpty(legacyXrayStringValue(tlsConfig["server_name"]), legacyXrayStringValue(tlsConfig["serverName"])),
			"publicKey":  legacyXrayStringValue(reality["public_key"]),
			"fingerprint": legacyXrayFirstNonEmpty(
				legacyXrayStringValue(legacyXrayMapValue(tlsConfig["utls"])["fingerprint"]),
				legacyXrayStringValue(tlsConfig["fingerprint"]), "chrome",
			),
		}
		if shortID := legacyXrayFirstNonEmpty(legacyXrayStringValue(reality["short_id"]), legacyXrayStringValue(reality["shortId"])); shortID != "" {
			settings["shortId"] = shortID
		}
		stream["security"] = "reality"
		stream["realitySettings"] = settings
		return stream
	}
	if legacyXrayBoolValue(tlsConfig["enabled"]) || len(tlsConfig) > 0 {
		settings := map[string]any{
			"serverName":    legacyXrayFirstNonEmpty(legacyXrayStringValue(tlsConfig["server_name"]), legacyXrayStringValue(tlsConfig["serverName"])),
			"allowInsecure": legacyXrayBoolValue(tlsConfig["insecure"]),
		}
		if alpn := legacyXrayStringSlice(tlsConfig["alpn"]); len(alpn) > 0 {
			settings["alpn"] = alpn
		}
		if fingerprint := legacyXrayFirstNonEmpty(legacyXrayStringValue(legacyXrayMapValue(tlsConfig["utls"])["fingerprint"]), legacyXrayStringValue(tlsConfig["fingerprint"])); fingerprint != "" {
			settings["fingerprint"] = fingerprint
		}
		stream["security"] = "tls"
		stream["tlsSettings"] = settings
	}
	return stream
}

func xrayOutboundByTag(outbounds []map[string]any, tag string) (map[string]any, error) {
	for _, outbound := range outbounds {
		if outbound["tag"] == tag {
			return outbound, nil
		}
	}
	return nil, fmt.Errorf("outbound %s not found", tag)
}

func cloneXrayMigrationMap(input map[string]any) map[string]any {
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

func xrayJSONEqual(left, right any) bool {
	leftBody, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightBody, err := json.Marshal(right)
	return err == nil && bytes.Equal(leftBody, rightBody)
}

func legacyXrayMapValue(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func legacyXrayStringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func legacyXrayIntValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(typed))
		return parsed
	default:
		return 0
	}
}

func legacyXrayBoolValue(value any) bool {
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

func legacyXrayStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := legacyXrayStringValue(item); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func legacyXrayTransportHost(transport map[string]any) string {
	if host := legacyXrayStringValue(transport["host"]); host != "" {
		return host
	}
	for key, value := range legacyXrayMapValue(transport["headers"]) {
		if strings.EqualFold(key, "host") {
			return legacyXrayStringValue(value)
		}
	}
	return ""
}

func legacyXrayFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func xrayMigrationFixtures() []xrayMigrationFixture {
	return []xrayMigrationFixture{
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
				"uuid": "22222222-2222-4222-8222-222222222222", "encryption": "none", "flow": "xtls-rprx-vision",
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
