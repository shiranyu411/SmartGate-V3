package fabric

import (
	"context"
	"github.com/miekg/dns"
	"net"
	"smartgate/internal/state"
	"testing"
	"time"
)

func geoCNParityClassifier(t *testing.T, mode string, override bool) *Classifier {
	t.Helper()
	spec := state.DefaultSpecState()
	spec.Settings.GatewayMode = mode
	spec.BusinessPools[0].ID = "renamed-general"
	for i := range spec.RemoteRules.Sets {
		spec.RemoteRules.Sets[i].Enabled = spec.RemoteRules.Sets[i].ID == "geosite-cn"
	}
	spec.RemoteRules.Enabled = true
	spec.Rules = append(spec.Rules, state.Rule{ID: "custom-direct", Source: state.RuleSourceUser, Kind: RuleDomainSuffix, Value: "custom.example", PoolID: TargetDirect, Enabled: true})
	if override {
		spec.Rules = append(spec.Rules, state.Rule{ID: "user-asset", Source: state.RuleSourceUser, Kind: RuleDomainSuffix, Value: "gstatic.com", PoolID: TargetDirect, Enabled: true})
	}
	rules, fallback, err := RulesFromSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	pools := []string{"renamed-general", "ai", "streaming", "banking", "crypto"}
	classifier, err := newClassifier(rules, fallback, pools, map[string]RuleSetMatcher{"geosite-cn": {DomainExact: []string{"www.gstatic.com", "gemini.google.com", "cn.example"}}})
	if err != nil {
		t.Fatal(err)
	}
	return classifier
}

func TestFabricGeoCNProxySafetyPreservesBusinessAndExplicitRules(t *testing.T) {
	classifier := geoCNParityClassifier(t, state.GatewayModeOn, false)
	for domain, want := range map[string]string{"www.gstatic.com": "renamed-general", "gemini.google.com": "ai", "cn.example": TargetDirect, "custom.example": TargetDirect} {
		if got := classifier.ClassifyDomain(domain); got.Target != want {
			t.Fatalf("%s routed to %s want %s", domain, got.Target, want)
		}
	}
	if got := geoCNParityClassifier(t, state.GatewayModeOn, true).ClassifyDomain("www.gstatic.com"); got.Target != TargetDirect {
		t.Fatalf("explicit user choice was overridden: %+v", got)
	}
}

func TestFabricGlobalModeOmitsSavedDirectAndGeoDirect(t *testing.T) {
	classifier := geoCNParityClassifier(t, state.GatewayModeGlobal, false)
	for domain, want := range map[string]string{"www.gstatic.com": "renamed-general", "cn.example": "renamed-general", "custom.example": "renamed-general", "gemini.google.com": "ai", "router.lan": TargetDirect} {
		if got := classifier.ClassifyDomain(domain); got.Target != want {
			t.Fatalf("%s routed to %s want %s", domain, got.Target, want)
		}
	}
}

func TestFabricGeoCNProxySafetyDNSUsesProxyPoolOverUDPAndTCP(t *testing.T) {
	classifier := geoCNParityClassifier(t, state.GatewayModeOn, false)
	server, err := NewDNSServer(DNSConfig{Listen: "127.0.0.1:0", Classifier: classifier, SetWriter: &MemorySetWriter{}, Resolver: resolverFunc(func(_ context.Context, target string, request *dns.Msg) (*dns.Msg, error) {
		response := new(dns.Msg)
		response.SetReply(request)
		ip := net.ParseIP("203.0.113.9")
		if target == TargetDirect {
			ip = net.ParseIP("127.0.0.1")
		}
		response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: ip}}
		return response, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	for _, network := range []string{"udp", "tcp"} {
		question := new(dns.Msg)
		question.SetQuestion("www.gstatic.com.", dns.TypeA)
		answer, _, err := (&dns.Client{Net: network, Timeout: time.Second}).Exchange(question, server.Addr())
		if err != nil || len(answer.Answer) != 1 || !answer.Answer[0].(*dns.A).A.Equal(net.ParseIP("203.0.113.9")) {
			t.Fatalf("%s used polluted direct DNS: %v %v", network, answer, err)
		}
	}
}
