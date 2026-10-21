package xray

import (
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
	runner := &versionProbeRunner{output: "Xray 26.3.27 (Xray, Penetrates Everything.) d2758a0\n"}
	adapter := &Adapter{Runtime: &worker.FileRuntime{Runner: runner}, Binary: "/opt/xray"}
	version, err := adapter.ProbeVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "26.3.27" || !reflect.DeepEqual(runner.command, []string{"/opt/xray", "version"}) {
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

func TestDescriptorCoversXrayMetadataAndMatchesCurrentCapabilities(t *testing.T) {
	descriptor := (&Adapter{}).Descriptor()
	if descriptor.ID != smartnode.CoreID("xray") || descriptor.DisplayName != "Xray" {
		t.Fatalf("unexpected identity metadata: %+v", descriptor)
	}
	if descriptor.Control != (coredesc.Control{Kind: "grpc", Listen: "127.0.0.1", Port: 10085}) {
		t.Fatalf("unexpected control metadata: %+v", descriptor.Control)
	}
	if descriptor.Ports != (coredesc.Ports{TProxyBase: 17000, DNSBase: 19000}) || descriptor.Resources.EstimatedRSSKB != 40960 {
		t.Fatalf("unexpected port or resource metadata: ports=%+v resources=%+v", descriptor.Ports, descriptor.Resources)
	}
	if descriptor.Process.Binary != "/usr/bin/xray" || descriptor.Process.ServiceUnit != "" || descriptor.Process.ProcessName != "xray" {
		t.Fatalf("unexpected process metadata: %+v", descriptor.Process)
	}
	if got := descriptor.Process.StartArgs("/tmp/worker.json"); !reflect.DeepEqual(got, []string{"run", "-config", "/tmp/worker.json"}) {
		t.Fatalf("unexpected start args: %v", got)
	}
	if got := descriptor.Process.CheckArgs("/tmp/worker.json"); !reflect.DeepEqual(got, []string{"run", "-test", "-config", "/tmp/worker.json"}) {
		t.Fatalf("unexpected check args: %v", got)
	}
	if descriptor.WorkerID(1) != "w_xr_1" || len(descriptor.ConfigDirs) != 0 {
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
				Status:      smartnode.CapabilityDegraded,
				Reasons:     []string{"identity.extra.foo_unassessed"},
				ReasonTexts: []string{"xray 无法完整执行: identity.extra.foo_unassessed"},
			},
		},
		{
			identity: smartnode.NodeIdentity{Protocol: "hysteria2", Transport: smartnode.Transport{Type: "quic"}, Security: smartnode.Security{Type: "tls"}},
			want: smartnode.CoreCapability{
				Status:      smartnode.CapabilityUnsupportedByCore,
				Reasons:     []string{"protocol.hysteria2_not_supported"},
				ReasonTexts: []string{"xray 无法完整执行: protocol.hysteria2_not_supported"},
			},
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
					"xray 无法完整执行: security.future_security_not_supported",
					"xray 无法完整执行: transport.future_transport_not_supported",
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

func TestBuildConfigExpressesVLESSRealityXHTTPAndBinding(t *testing.T) {
	request := xrayRequest()
	config, sensitive, err := BuildConfig(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		`"protocol":"dokodemo-door"`, `"followRedirect":true`, `"tproxy":"tproxy"`,
		`"protocol":"vless"`, `"security":"reality"`, `"xhttpSettings"`,
		`"balancerTag":"route-ai-1"`, `"RoutingService"`, `"domainStrategy":"AsIs"`, `"listen":"127.0.0.1"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Xray config missing %q: %s", required, text)
		}
	}
	if !contains(sensitive, "ai-secret-uuid") || !contains(sensitive, "public-key-secret") {
		t.Fatalf("sensitive values were not collected for redaction: %v", sensitive)
	}
}

func TestBuildConfigRejectsIncompleteReality(t *testing.T) {
	request := xrayRequest()
	source := request.Identities["node-ai"]
	source.Identity.Security.PublicKeyRef = ""
	request.Identities["node-ai"] = source
	if _, _, err := BuildConfig(request); err == nil {
		t.Fatal("expected incomplete REALITY config to be rejected")
	}
}

func TestBuildConfigRejectsUnsupportedProtocol(t *testing.T) {
	request := xrayRequest()
	source := request.Identities["node-ai"]
	source.Identity.Protocol = "hysteria2"
	request.Identities["node-ai"] = source
	if _, _, err := BuildConfig(request); err == nil {
		t.Fatal("expected unsupported Xray protocol to be rejected")
	}
}

func xrayRequest() worker.CompileRequest {
	return worker.CompileRequest{
		Worker: state.WorkerTarget{
			ID: "w_xr_1", Core: smartnode.CoreID("xray"),
			Control: state.WorkerControl{Listen: "127.0.0.1", Port: 10085, Kind: "grpc"},
			Routes:  []state.WorkerRoute{{ID: "route-ai-1", Pool: "ai", Lane: 1, Selector: "route-ai-1", Candidates: []string{"exec-ai"}, Selected: "exec-ai"}},
			Inbounds: []state.WorkerInbound{
				{ID: "in-ai", Kind: "tproxy", Pool: "ai", Route: "route-ai-1", Lane: 1, Binding: "bind-ai", Generation: 4, Listen: "::", Port: 17000, Network: "tcp,udp"},
				{ID: "dns-ai", Kind: "socks", Pool: "ai", Route: "route-ai-1", Lane: 1, Binding: "bind-ai", Generation: 4, Listen: "127.0.0.1", Port: 19000, Network: "tcp"},
			},
		},
		Executors: []state.NodeExecutor{{ID: "exec-ai", SmartNode: "node-ai", Revision: 2, Core: smartnode.CoreID("xray"), Worker: "w_xr_1"}},
		Bindings: []state.PoolBinding{{
			ID: "bind-ai", Pool: "ai", Generation: 4,
			Target: state.BindingTarget{Kind: "node", SmartNode: "node-ai", Executor: "exec-ai", Core: smartnode.CoreID("xray"), Worker: "w_xr_1"},
		}},
		Identities: map[string]smartnode.IdentitySource{"node-ai": xrayIdentitySource("node-ai", map[string]any{
			"type": "vless", "tag": "node-ai", "server": "ai.example", "server_port": 443, "uuid": "ai-secret-uuid", "flow": "xtls-rprx-vision",
			"transport": map[string]any{"type": "xhttp", "path": "/api", "host": "cdn.example", "mode": "auto"},
			"tls": map[string]any{
				"enabled": true, "server_name": "edge.example", "utls": map[string]any{"fingerprint": "chrome"},
				"reality": map[string]any{"enabled": true, "public_key": "public-key-secret", "short_id": "abcd"},
			},
		})},
	}
}

func xrayIdentitySource(tag string, outbound map[string]any) smartnode.IdentitySource {
	source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{ProviderID: "p1", RawTag: tag, Outbound: outbound})
	if err != nil {
		panic(err)
	}
	return source
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
