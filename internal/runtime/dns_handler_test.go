package runtime

import (
	"net/http"
	"strings"
	"testing"

	"smartgate/internal/lockset"
	"smartgate/internal/state"
)

func TestDNSHandlerGetsDefaultsAndSavesNormalizedSpec(t *testing.T) {
	server, store := newDNSTestServer(t)

	getResp := serve(t, server, http.MethodGet, "/api/dns", nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("get dns status = %d body=%s", getResp.Code, getResp.Body.String())
	}
	getData := decodeEnvelopeData(t, getResp.Body.Bytes())
	if getData["strategy"] != "prefer_ipv4" || getData["fakeip_enabled"] != false || getData["classification_mode"] != dnsClassificationMode {
		t.Fatalf("unexpected default dns data: %+v", getData)
	}

	disableFakeIP := false
	putResp := serve(t, server, http.MethodPut, "/api/dns", mustJSON(t, dnsPayload{
		Strategy:        "IPV4_ONLY",
		FakeIPEnabled:   &disableFakeIP,
		FakeIPRange:     "198.18.0.0/15",
		Final:           "proxy",
		DirectServers:   []string{" 1.1.1.1 ", "1.1.1.1"},
		ProxyServers:    []string{"https://dns.google/dns-query"},
		FallbackServers: []string{"tls://8.8.8.8"},
	}))
	if putResp.Code != http.StatusOK {
		t.Fatalf("put dns status = %d body=%s", putResp.Code, putResp.Body.String())
	}

	spec, err := store.LoadSpec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if spec.DNS.Strategy != "ipv4_only" || spec.DNS.FakeIPEnabled {
		t.Fatalf("dns was not normalized/saved: %+v", spec.DNS)
	}
	if spec.DNS.Final != "proxy" || len(spec.DNS.DirectServers) != 1 || spec.DNS.DirectServers[0] != "1.1.1.1" {
		t.Fatalf("dns servers were not normalized/saved: %+v", spec.DNS)
	}
}

func TestDNSHandlerRejectsFakeIP(t *testing.T) {
	server, _ := newDNSTestServer(t)
	enableFakeIP := true
	resp := serve(t, server, http.MethodPut, "/api/dns", mustJSON(t, dnsPayload{
		FakeIPEnabled: &enableFakeIP,
		DirectServers: []string{"1.1.1.1"},
	}))
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected unsupported fake-ip response, got %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "fakeip_not_supported") {
		t.Fatalf("missing stable fake-ip error code: %s", resp.Body.String())
	}
}

func TestDNSHandlerRejectsFinalWithoutMatchingServerGroup(t *testing.T) {
	server, _ := newDNSTestServer(t)

	resp := serve(t, server, http.MethodPut, "/api/dns", mustJSON(t, dnsPayload{
		Final:         "proxy",
		DirectServers: []string{"1.1.1.1"},
	}))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func newDNSTestServer(t *testing.T) (*Server, *state.Store) {
	t.Helper()
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	locks := lockset.New()
	store := state.NewStore(layout)
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Layout:     layout,
		Locks:      locks,
		Store:      store,
		Status:     NewStatus("127.0.0.1:3000", state.RuntimeState{}),
	})
	return server, store
}
