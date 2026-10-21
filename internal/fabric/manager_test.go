package fabric

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"

	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

type managerRunner struct {
	commands int
}

func (runner *managerRunner) Run(_ context.Context, _ []string) (string, error) {
	runner.commands++
	return "ok", nil
}

func TestFabricManagerPrepareApplyAndReconfigureDNS(t *testing.T) {
	upstreamAddress, stopUpstream := startManagerDNSUpstream(t)
	defer stopUpstream()
	graph := managerFabricGraph()
	graph.DNS.Listen = freeDNSListen(t)
	spec := state.DefaultSpecState()
	spec.DNS.DirectServers = []string{upstreamAddress}
	spec.DNS.ProxyServers = []string{upstreamAddress}
	runner := &managerRunner{}
	manager := NewManager(netgate.NewOwnerRegistry("smartgate"), runner)
	prepared, err := manager.Prepare(context.Background(), "fabric-manager", spec, graph)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Apply(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || runner.commands == 0 {
		t.Fatalf("fabric was not applied: result=%+v commands=%d", result, runner.commands)
	}
	if _, ok := manager.Current(); !ok {
		t.Fatal("fabric current graph was not recorded")
	}
	prepared.Graph.DNS.RejectHTTPSRecords = false
	if _, err := manager.Apply(context.Background(), prepared); err != nil {
		t.Fatalf("same-listen DNS reconfigure failed: %v", err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFabricCutoverPreservesMaterializedRuleSets(t *testing.T) {
	for _, kind := range []string{RuleGeoSite, RuleGeoIP} {
		t.Run(kind, func(t *testing.T) {
			store := newRuleSetTestStore(t)
			path := filepath.Join(t.TempDir(), "rules.json")
			body := []byte(`{"version":3,"rules":[{"domain":"fixture.example","ip_cidr":"203.0.113.0/24"}]}`)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			spec := state.DefaultSpecState()
			spec.RemoteRules.Enabled = true
			spec.RemoteRules.Sets = []state.RemoteRuleSet{{ID: "fixture", Enabled: true, Type: "local", Format: "source", Path: path}}
			graph := managerFabricGraph()
			graph.Rules = []state.FabricRuleGraph{{ID: "fixture", Kind: kind, Value: "fixture", Target: TargetDirect}}
			manager := NewManager(nil, &managerRunner{}).WithRuleSetStore(store)
			current, err := manager.Prepare(context.Background(), "initial", spec, graph)
			if err != nil {
				t.Fatal(err)
			}
			manager.current = &current
			// A selector must work immediately after Apply, before any restart
			// has reconstructed the transient snapshot-pinning flag.
			if current.Graph.RuleSetsPinned {
				t.Fatal("initial preparation unexpectedly pins snapshots")
			}
			// The original source may change or disappear between selections.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			target := graph
			target.Rules = append([]state.FabricRuleGraph(nil), graph.Rules...)
			for i := range target.Rules {
				target.Rules[i].IPSets = nil // planner output is not materialized
			}
			target.Dispatch = append([]state.PoolDispatchGraph(nil), graph.Dispatch...)
			target.Dispatch[0].Generation++
			target.Dispatch[0].Binding = "next-binding"
			prepared, err := manager.PrepareCutover(context.Background(), "select", "general", spec, target)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(prepared.Graph.RuleSets, current.Graph.RuleSets) || !reflect.DeepEqual(prepared.Graph.Rules, current.Graph.Rules) {
				t.Fatal("selector changed applied rule-set snapshots or materialization")
			}
			if current.Graph.Dispatch[0].Generation != 1 {
				t.Fatal("preparation mutated the active dispatch")
			}
			manager.current = &prepared
			if _, err := manager.PrepareCompensation(context.Background(), "undo", "general", spec, graph); err != nil {
				t.Fatalf("compensation failed: %v", err)
			}
			target.DNS.RejectHTTPSRecords = false
			if _, err := manager.PrepareCutover(context.Background(), "reject", "general", spec, target); err == nil {
				t.Fatal("selector accepted a change to non-target DNS state")
			}
		})
	}
}

func freeDNSListen(t *testing.T) string {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := packet.LocalAddr().String()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		_ = packet.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		_ = packet.Close()
		t.Fatal(err)
	}
	if err := packet.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func managerFabricGraph() state.FabricGraph {
	return state.FabricGraph{
		Takeover: state.FabricTakeoverGraph{Enabled: true, TCP: true, UDP: true, IPv4: true, IPv6: true},
		DNS: state.FabricDNSGraph{
			Listen: "127.0.0.1:1053", RedirectLAN53: true, RejectHTTPSRecords: true, BlockDoT853: true,
			PoolSets: map[string]state.FabricPoolSets{
				TargetDirect: {IPv4: "direct_4", IPv6: "direct_6"},
				TargetBlock:  {IPv4: "block_4", IPv6: "block_6"},
				"general":    {IPv4: "general_4", IPv6: "general_6"},
			},
		},
		FallbackPool: "general",
		Dispatch: []state.PoolDispatchGraph{{
			Pool: "general", Binding: "binding-general", Generation: 1, Mark: 0x50010001,
			MarkMask: 0xffffffff, Table: 1001, Port: 16000, DNSPort: 18000, Target: TargetDirect,
		}},
	}
}

func startManagerDNSUpstream(t *testing.T) (string, func()) {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		_ = writer.WriteMsg(response)
	})}
	go func() { _ = server.ActivateAndServe() }()
	return packet.LocalAddr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownContext(ctx)
	}
}
