package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"smartgate/internal/coredesc"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type versionProbeRunner struct {
	output  string
	command []string
}

func (runner *versionProbeRunner) Run(_ context.Context, command []string) (string, error) {
	runner.command = append([]string(nil), command...)
	return runner.output, nil
}

func TestProbeVersionUsesAdapterBinaryAndPublishesDescriptorVersion(t *testing.T) {
	runner := &versionProbeRunner{output: "sing-box version 1.12.4\nEnvironment: go1.26.1 linux/arm64\n"}
	adapter := &Adapter{Runtime: &worker.FileRuntime{Runner: runner}}
	version, err := adapter.ProbeVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.12.4" || !reflect.DeepEqual(runner.command, []string{"/usr/bin/sing-box", "version"}) {
		t.Fatalf("version=%q command=%v", version, runner.command)
	}
	adapter.Version = version
	if adapter.Descriptor().Version != version {
		t.Fatalf("descriptor version=%q want %q", adapter.Descriptor().Version, version)
	}
	runner.output = "unexpected"
	if _, err := adapter.ProbeVersion(context.Background()); err == nil {
		t.Fatal("malformed version output was accepted")
	}
}

func TestDescriptorCoversSingBoxMetadataAndMatchesCurrentCapabilities(t *testing.T) {
	descriptor := (&Adapter{}).Descriptor()
	if descriptor.ID != smartnode.CoreID("sing-box") || descriptor.DisplayName != "sing-box" {
		t.Fatalf("unexpected identity metadata: %+v", descriptor)
	}
	if descriptor.Control != (coredesc.Control{Kind: "clash", Listen: "127.0.0.1", Port: 9090}) {
		t.Fatalf("unexpected control metadata: %+v", descriptor.Control)
	}
	if descriptor.Ports != (coredesc.Ports{TProxyBase: 16000, DNSBase: 18000}) || descriptor.Resources.EstimatedRSSKB != 32768 {
		t.Fatalf("unexpected port or resource metadata: ports=%+v resources=%+v", descriptor.Ports, descriptor.Resources)
	}
	if descriptor.Process.Binary != "/usr/bin/sing-box" || descriptor.Process.ServiceUnit != "sing-box" || descriptor.Process.ProcessName != "sing-box" {
		t.Fatalf("unexpected process metadata: %+v", descriptor.Process)
	}
	if got := descriptor.Process.StartArgs("/tmp/worker.json"); !reflect.DeepEqual(got, []string{"run", "-c", "/tmp/worker.json"}) {
		t.Fatalf("unexpected start args: %v", got)
	}
	if got := descriptor.Process.CheckArgs("/tmp/worker.json"); !reflect.DeepEqual(got, []string{"check", "-c", "/tmp/worker.json"}) {
		t.Fatalf("unexpected check args: %v", got)
	}
	if descriptor.WorkerID(1) != "w_sb_1" || !reflect.DeepEqual(descriptor.ConfigDirs, []string{"/etc/sing-box"}) {
		t.Fatalf("unexpected naming or directory metadata: worker=%q dirs=%v", descriptor.WorkerID(1), descriptor.ConfigDirs)
	}

	fixtures := []struct {
		identity   smartnode.NodeIdentity
		unassessed []string
		want       smartnode.CoreCapability
	}{
		{
			identity:   smartnode.NodeIdentity{Protocol: "vless", Transport: smartnode.Transport{Type: "xhttp"}, Security: smartnode.Security{Type: "reality"}},
			unassessed: []string{"identity.extra.foo_unassessed"},
			want: smartnode.CoreCapability{
				Status:  smartnode.CapabilityDegraded,
				Reasons: []string{"identity.extra.foo_unassessed", "transport.xhttp_semantics_incomplete"},
				ReasonTexts: []string{
					"sing-box 无法完整执行: identity.extra.foo_unassessed",
					"sing-box 无法完整表达该 XHTTP 传输语义",
				},
			},
		},
		{
			identity: smartnode.NodeIdentity{Protocol: "hysteria2", Transport: smartnode.Transport{Type: "quic"}, Security: smartnode.Security{Type: "tls"}},
			want:     smartnode.CoreCapability{Status: smartnode.CapabilitySupported, Reasons: []string{}, ReasonTexts: []string{}},
		},
		{
			identity: smartnode.NodeIdentity{Protocol: "trojan", Transport: smartnode.Transport{Type: "future transport"}, Security: smartnode.Security{Type: "future security", Flow: "vision"}},
			want: smartnode.CoreCapability{
				Status: smartnode.CapabilityUnsupportedByCore,
				Reasons: []string{
					"security.flow_requires_vless",
					"security.future_security_not_supported",
					"transport.future_transport_not_supported",
				},
				ReasonTexts: []string{
					"flow 仅允许用于 VLESS 节点",
					"sing-box 无法完整执行: security.future_security_not_supported",
					"sing-box 无法完整执行: transport.future_transport_not_supported",
				},
			},
		},
	}
	for _, fixture := range fixtures {
		got := descriptor.Capability.Evaluator(fixture.identity, fixture.unassessed)
		if !reflect.DeepEqual(got, fixture.want) {
			t.Fatalf("capability drift for %+v: got=%+v want=%+v", fixture.identity, got, fixture.want)
		}
	}
}

func TestDescriptorRegistrationDoesNotChangeCompiledConfigBytes(t *testing.T) {
	request := singBoxRequest()
	before, _, err := BuildConfig(request)
	if err != nil {
		t.Fatal(err)
	}
	registry := coredesc.NewRegistry()
	if err := registry.Register((&Adapter{}).Descriptor()); err != nil {
		t.Fatal(err)
	}
	after, _, err := BuildConfig(request)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("descriptor registration changed config bytes\nbefore=%s\nafter=%s", beforeJSON, afterJSON)
	}
}

func TestBuildConfigRemovesGatewayOwnershipAndScopesExecutors(t *testing.T) {
	request := singBoxRequest()
	config, _, err := BuildConfig(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, forbidden := range []string{`"type":"block"`, `"final":"Block"`, `"type":"tun"`, `"auto_route"`, `"hijack-dns"`, "node-xray"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("worker config retained gateway or foreign executor field %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{`"type":"tproxy"`, `"type":"socks"`, `"interrupt_exist_connections":true`, `"tag":"exec-general"`, `"action":"reject"`, `"default_domain_resolver":{"server":"node-bootstrap","strategy":"prefer_ipv4"}`, `"external_controller":"127.0.0.1:9090"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("worker config missing %q: %s", required, text)
		}
	}
	rules := config.Route["rules"].([]map[string]any)
	if !reflect.DeepEqual(rules[len(rules)-1], map[string]any{"action": "reject"}) {
		t.Fatal("unmatched traffic must hit the terminal reject rule")
	}
	bootstrap := config.DNS["servers"].([]map[string]any)
	if len(bootstrap) != 1 || bootstrap[0]["type"] != "udp" || bootstrap[0]["server"] != "223.5.5.5" {
		t.Fatal("node bootstrap must use independent direct DNS")
	}
	for _, outbound := range config.Outbounds {
		if outbound["tag"] == "exec-general" {
			if _, exists := outbound["multiplex"]; exists {
				t.Fatalf("worker outbound retained provider multiplex settings: %+v", outbound)
			}
		}
	}
	networks, ok := config.Inbounds[0]["network"].([]string)
	if !ok || len(networks) != 2 || networks[0] != "tcp" || networks[1] != "udp" {
		t.Fatalf("tproxy networks were not compiled for sing-box: %#v", config.Inbounds[0]["network"])
	}
}

func TestBuildConfigRejectsNonLoopbackControl(t *testing.T) {
	request := singBoxRequest()
	request.Worker.Control.Listen = "0.0.0.0"
	if _, _, err := BuildConfig(request); err == nil {
		t.Fatal("expected non-loopback control API to be rejected")
	}
}

func singBoxRequest() worker.CompileRequest {
	return worker.CompileRequest{
		Worker: state.WorkerTarget{
			ID: "w_sb_1", Core: smartnode.CoreID("sing-box"),
			Control: state.WorkerControl{Listen: "127.0.0.1", Port: 9090, Kind: "clash"},
			Routes:  []state.WorkerRoute{{ID: "route-general-0", Pool: "general", Lane: 0, Selector: "route-general-0", Candidates: []string{"exec-general"}, Selected: "exec-general"}},
			Inbounds: []state.WorkerInbound{
				{ID: "in-general", Kind: "tproxy", Pool: "general", Route: "route-general-0", Lane: 0, Binding: "bind-general", Generation: 1, Listen: "::", Port: 16000, Network: "tcp,udp"},
				{ID: "dns-general", Kind: "socks", Pool: "general", Route: "route-general-0", Lane: 0, Binding: "bind-general", Generation: 1, Listen: "127.0.0.1", Port: 18000, Network: "tcp"},
			},
		},
		Executors: []state.NodeExecutor{{ID: "exec-general", SmartNode: "node-general", Revision: 1, Core: smartnode.CoreID("sing-box"), Worker: "w_sb_1"}},
		Bindings: []state.PoolBinding{{
			ID: "bind-general", Pool: "general", Generation: 1,
			Target: state.BindingTarget{Kind: "node", SmartNode: "node-general", Executor: "exec-general", Core: smartnode.CoreID("sing-box"), Worker: "w_sb_1"},
		}},
		Identities: map[string]smartnode.IdentitySource{
			"node-general": singBoxIdentitySource("provider-tag", map[string]any{"type": "vless", "tag": "provider-tag", "server": "general.example", "server_port": 443, "uuid": "secret-general", "multiplex": map[string]any{"enabled": true}}),
			"node-xray":    singBoxIdentitySource("xray-tag", map[string]any{"type": "vless", "tag": "xray-tag", "server": "ai.example", "server_port": 443, "uuid": "secret-ai"}),
		},
	}
}

func singBoxIdentitySource(tag string, outbound map[string]any) smartnode.IdentitySource {
	source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{ProviderID: "p1", RawTag: tag, Outbound: outbound})
	if err != nil {
		panic(err)
	}
	return source
}
