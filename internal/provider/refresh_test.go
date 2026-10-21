package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"smartgate/internal/state"
)

type fakeFetcher struct {
	calls []FetchRequest
	resp  map[string]FetchResponse
	errs  map[string]error
}

func (f *fakeFetcher) Fetch(_ context.Context, req FetchRequest) (FetchResponse, error) {
	f.calls = append(f.calls, req)
	if err := f.errs[req.Provider.ID]; err != nil {
		return FetchResponse{}, err
	}
	return f.resp[req.Provider.ID], nil
}

func TestHTTPFetcherDirectUsesNoProxyByDefault(t *testing.T) {
	fetcher := HTTPFetcher{}
	client, err := fetcher.clientFor(FetchRequest{Provider: state.ProviderSpec{ID: "p1"}})
	if err != nil {
		t.Fatalf("clientFor direct: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected http transport, got %T", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatalf("direct fetch should not inherit environment/system proxy")
	}
}

func TestNormalizeNameserversPrefersNonLoopbackAndDeduplicates(t *testing.T) {
	got := normalizeNameservers([]string{
		"127.0.0.1",
		"::1",
		"192.168.0.68",
		"192.168.0.68:53",
		"2408:8888::8",
		"0.0.0.0",
	})
	want := []string{"192.168.0.68:53", "[2408:8888::8]:53"}
	if len(got) != len(want) {
		t.Fatalf("nameservers len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("nameserver[%d] = %q, want %q; all=%#v", i, got[i], want[i], got)
		}
	}
}

func TestNameserversFromResolvFilesReadsOpenWrtAutoConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf.auto")
	if err := os.WriteFile(path, []byte("# Interface wan\nnameserver 192.168.0.68\nsearch lan\nnameserver 2408:8888::8\n"), 0o644); err != nil {
		t.Fatalf("write resolv: %v", err)
	}
	got := nameserversFromResolvFiles(filepath.Join(dir, "missing"), path)
	want := []string{"192.168.0.68", "2408:8888::8"}
	if len(got) != len(want) {
		t.Fatalf("nameservers len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("nameserver[%d] = %q, want %q; all=%#v", i, got[i], want[i], got)
		}
	}
}

func TestEffectiveSubscriptionURLAcceptsRemoteProfileDeepLink(t *testing.T) {
	raw := "sing-box://import-remote-profile?url=https%3A%2F%2Fexample.com%2Fsub%3Ftoken%3Dredacted&name=Example"
	got, err := EffectiveSubscriptionURL(raw)
	if err != nil {
		t.Fatalf("effective subscription URL: %v", err)
	}
	if got != "https://example.com/sub?token=redacted" {
		t.Fatalf("effective URL = %q", got)
	}
}

func TestNormalizeProviderSpecAcceptsRemoteProfileDeepLink(t *testing.T) {
	_, err := NormalizeProviderSpec(state.ProviderSpec{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      TypeSubscription,
		Enabled:   true,
		URL:       "sing-box://import-remote-profile?url=https%3A%2F%2Fexample.com%2Fsub&name=Example",
		FetchMode: FetchDirect,
	})
	if err != nil {
		t.Fatalf("normalize provider: %v", err)
	}
}

func TestSubscriptionUserAgentPrefersProviderOverride(t *testing.T) {
	cases := []struct {
		name string
		spec state.ProviderSpec
		want string
	}{
		{name: "default", spec: state.ProviderSpec{URL: "https://example.com/sub"}, want: "SmartGate/2.0"},
		{name: "remote profile deep link", spec: state.ProviderSpec{URL: "sing-box://import-remote-profile?url=https%3A%2F%2Fexample.com%2Fsub"}, want: "sing-box/1.12.0"},
		{name: "override plain url", spec: state.ProviderSpec{URL: "https://example.com/sub", UserAgent: "clash-verge/v2.5.2"}, want: "clash-verge/v2.5.2"},
		{name: "override beats deep link default", spec: state.ProviderSpec{URL: "sing-box://import-remote-profile?url=https%3A%2F%2Fexample.com%2Fsub", UserAgent: "mihomo/v1.19.2"}, want: "mihomo/v1.19.2"},
		{name: "whitespace-only override falls back", spec: state.ProviderSpec{URL: "https://example.com/sub", UserAgent: "   "}, want: "SmartGate/2.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subscriptionUserAgent(tc.spec); got != tc.want {
				t.Fatalf("subscriptionUserAgent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHTTPFetcherSendsConfiguredUserAgent(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	fetcher := HTTPFetcher{Client: server.Client()}
	for _, spec := range []state.ProviderSpec{
		{ID: "default", URL: server.URL + "/sub"},
		{ID: "override", URL: server.URL + "/sub", UserAgent: "clash-verge/v2.5.2"},
	} {
		if _, err := fetcher.Fetch(context.Background(), FetchRequest{Provider: spec}); err != nil {
			t.Fatalf("fetch %s: %v", spec.ID, err)
		}
	}
	want := []string{"SmartGate/2.0", "clash-verge/v2.5.2"}
	if len(seen) != len(want) {
		t.Fatalf("requests = %d, want %d", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("request[%d] User-Agent = %q, want %q", i, seen[i], want[i])
		}
	}
}

func TestNormalizeProviderSpecUserAgent(t *testing.T) {
	base := state.ProviderSpec{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      TypeSubscription,
		Enabled:   true,
		URL:       "https://example.com/sub",
		FetchMode: FetchDirect,
	}

	t.Run("trims and keeps printable ascii", func(t *testing.T) {
		spec := base
		spec.UserAgent = "  clash-verge/v2.5.2  "
		got, err := NormalizeProviderSpec(spec)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		wantUA := "clash-verge/v2.5.2"
		if got.UserAgent != wantUA {
			t.Fatalf("UserAgent = %q", got.UserAgent)
		}
	})

	t.Run("rejects header injection and non-ascii", func(t *testing.T) {
		for name, ua := range map[string]string{
			"crlf":      "clash\r\nX-Injected: 1",
			"newline":   "clash\nverge",
			"tab":       "clash\tverge",
			"non-ascii": "客户端/1.0",
			"del":       "clash\x7f",
			"too long":  strings.Repeat("a", 257),
		} {
			t.Run(name, func(t *testing.T) {
				spec := base
				spec.UserAgent = ua
				if _, err := NormalizeProviderSpec(spec); err == nil {
					t.Fatalf("expected user agent %q to be rejected", ua)
				}
			})
		}
	})

	t.Run("accepts max length", func(t *testing.T) {
		spec := base
		spec.UserAgent = strings.Repeat("a", 256)
		if _, err := NormalizeProviderSpec(spec); err != nil {
			t.Fatalf("normalize: %v", err)
		}
	})

	t.Run("cleared for non-subscription types", func(t *testing.T) {
		dir := t.TempDir()
		localPath := filepath.Join(dir, "nodes.json")
		if err := os.WriteFile(localPath, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write local file: %v", err)
		}
		for _, spec := range []state.ProviderSpec{
			{ID: "m", Name: "Manual", Type: TypeManual, Enabled: true, UserAgent: "clash-verge/v2.5.2"},
			{ID: "t", Name: "Temporary", Type: TypeTemporary, Enabled: true, UserAgent: "clash-verge/v2.5.2"},
			{ID: "l", Name: "Local", Type: TypeLocalFile, Enabled: true, FilePath: localPath, UserAgent: "clash-verge/v2.5.2"},
		} {
			t.Run(string(spec.Type), func(t *testing.T) {
				got, err := NormalizeProviderSpec(spec)
				if err != nil {
					t.Fatalf("normalize: %v", err)
				}
				if got.UserAgent != "" {
					t.Fatalf("UserAgent should be cleared for %s, got %q", spec.Type, got.UserAgent)
				}
			})
		}
	})
}

func TestEffectiveSubscriptionURLRejectsUnsafeRemoteProfileDeepLink(t *testing.T) {
	for _, raw := range []string{
		"sing-box://import-remote-profile?name=missing-url",
		"sing-box://import-remote-profile?url=file%3A%2F%2F%2Ftmp%2Fnodes.json",
		"sing-box://other-action?url=https%3A%2F%2Fexample.com%2Fsub",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := EffectiveSubscriptionURL(raw); err == nil {
				t.Fatal("expected URL to be rejected")
			}
		})
	}
}

func TestHTTPFetcherViaBootstrapRequiresLoopbackProxy(t *testing.T) {
	boot := state.ProviderSpec{ID: "boot"}
	req := FetchRequest{Provider: state.ProviderSpec{ID: "p1"}, ViaBootstrap: &boot}

	if _, err := (HTTPFetcher{}).clientFor(req); err == nil {
		t.Fatal("expected via_bootstrap without proxy URL to fail")
	}

	for _, raw := range []string{
		"http://127.0.0.1:7890",
		"http://localhost:7890",
		"http://[::1]:7890",
	} {
		t.Run(raw, func(t *testing.T) {
			client, err := (HTTPFetcher{BootstrapProxyURL: raw}).clientFor(req)
			if err != nil {
				t.Fatalf("clientFor via_bootstrap: %v", err)
			}
			transport, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("expected http transport, got %T", client.Transport)
			}
			if transport.Proxy == nil {
				t.Fatal("via_bootstrap should configure an explicit HTTP proxy")
			}
			proxyURL, err := transport.Proxy(&http.Request{URL: mustURL(t, "https://subscription.example/list")})
			if err != nil {
				t.Fatalf("proxy lookup: %v", err)
			}
			if proxyURL.String() != raw {
				t.Fatalf("proxy URL = %q, want %q", proxyURL.String(), raw)
			}
		})
	}
}

func TestParseLoopbackHTTPProxyURLRejectsUnsafeURLs(t *testing.T) {
	for _, raw := range []string{
		"http://192.168.1.1:7890",
		"http://10.0.0.1:7890",
		"socks5://127.0.0.1:7890",
		"http://user:pass@127.0.0.1:7890",
		"http://127.0.0.1:7890/proxy",
		"http://127.0.0.1:7890?x=1",
		"http://127.0.0.1:7890#frag",
		"http://localhost.evil:7890",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseLoopbackHTTPProxyURL(raw); err == nil {
				t.Fatal("expected proxy URL to be rejected")
			}
		})
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return u
}

func TestRefreshAllSavesLastGoodAndPreservesOnFailure(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	lastGood := NewLastGoodStore(layout)
	if _, err := lastGood.Save("p1", []map[string]any{{"type": "vmess", "tag": "old", "server": "old.example"}}); err != nil {
		t.Fatalf("seed last-good: %v", err)
	}
	fetcher := &fakeFetcher{
		resp: map[string]FetchResponse{
			"p1": {StatusCode: http.StatusOK, Body: []byte(`[{"type":"unknown-proxy","tag":"bad","server":"bad.example"}]`)},
		},
		errs: map[string]error{},
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: FetchDirect,
	}}
	result, err := NewRefreshService(lastGood, fetcher).RefreshAll(context.Background(), spec)
	if err != nil {
		t.Fatalf("refresh all: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != "stale" {
		t.Fatalf("expected stale result, got %+v", result)
	}
	env, err := lastGood.Load("p1")
	if err != nil {
		t.Fatalf("load last-good: %v", err)
	}
	if env.Identities[0].RawTag != "old" {
		t.Fatalf("last-good should remain old, got %+v", env.Identities)
	}
}

func TestRefreshAllMarksFailureWithoutLastGoodAsFailed(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	fetcher := &fakeFetcher{
		resp: map[string]FetchResponse{
			"p1": {StatusCode: http.StatusForbidden, Body: []byte(`forbidden`)},
		},
		errs: map[string]error{},
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: FetchDirect,
	}}
	result, err := NewRefreshService(NewLastGoodStore(layout), fetcher).RefreshAll(context.Background(), spec)
	if err != nil {
		t.Fatalf("refresh all: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != "failed" {
		t.Fatalf("expected failed result, got %+v", result)
	}
}

func TestRefreshAllParsesSubscriptionUserinfo(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	fetcher := &fakeFetcher{
		resp: map[string]FetchResponse{
			"p1": {
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Subscription-Userinfo": []string{"upload=1024; download=2048; total=8192; expire=1799999999"},
				},
				Body: []byte(`[{"type":"vmess","tag":"node-1","server":"example.com"}]`),
			},
		},
		errs: map[string]error{},
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: FetchDirect,
	}}
	lastGood := NewLastGoodStore(layout)
	result, err := NewRefreshService(lastGood, fetcher).RefreshAll(context.Background(), spec)
	if err != nil {
		t.Fatalf("refresh all: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Subscription == nil {
		t.Fatalf("expected subscription info in refresh result, got %+v", result)
	}
	if result.Results[0].Subscription.UsedBytes != 3072 || result.Results[0].Subscription.RemainingBytes != 5120 {
		t.Fatalf("unexpected refresh subscription info: %+v", result.Results[0].Subscription)
	}
	env, err := lastGood.Load("p1")
	if err != nil {
		t.Fatalf("load last-good: %v", err)
	}
	if env.Subscription == nil || env.Subscription.TotalBytes != 8192 || env.Subscription.ExpireUnix != 1799999999 {
		t.Fatalf("subscription info was not persisted: %+v", env.Subscription)
	}
}

func TestRefreshAllParsesSubscriptionInfoFromNoticeNodes(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	fetcher := &fakeFetcher{
		resp: map[string]FetchResponse{
			"p1": {
				StatusCode: http.StatusOK,
				Body: []byte(`[
					{"type":"anytls","tag":"剩余流量：66.88 GB","server":"notice.invalid","server_port":1,"password":"p"},
					{"type":"anytls","tag":"套餐到期：2027-01-01","server":"notice.invalid","server_port":1,"password":"p"},
					{"type":"trojan","tag":"node-1","server":"node.example","server_port":443,"password":"p"}
				]`),
			},
		},
		errs: map[string]error{},
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:        "p1",
		Name:      "Provider 1",
		Type:      TypeSubscription,
		Enabled:   true,
		URL:       "https://p1.example/sub",
		FetchMode: FetchDirect,
	}}
	lastGood := NewLastGoodStore(layout)
	result, err := NewRefreshService(lastGood, fetcher).RefreshAll(context.Background(), spec)
	if err != nil {
		t.Fatalf("refresh all: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Subscription == nil {
		t.Fatalf("expected subscription info in refresh result, got %+v", result)
	}
	if result.Results[0].Subscription.RemainingBytes == 0 || result.Results[0].Subscription.ExpireUnix != 1798761600 {
		t.Fatalf("unexpected refresh subscription info: %+v", result.Results[0].Subscription)
	}
	env, err := lastGood.Load("p1")
	if err != nil {
		t.Fatalf("load last-good: %v", err)
	}
	if env.Subscription == nil || env.Subscription.RemainingBytes == 0 || env.Subscription.ExpireUnix != 1798761600 {
		t.Fatalf("subscription info was not persisted: %+v", env.Subscription)
	}
}

func TestRefreshAllUsesBootstrapLastGoodBeforeViaBootstrap(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	lastGood := NewLastGoodStore(layout)
	fetcher := &fakeFetcher{
		resp: map[string]FetchResponse{
			"boot": {StatusCode: http.StatusOK, Body: []byte(`[{"type":"vmess","tag":"boot-node","server":"boot.example"}]`)},
			"p1":   {StatusCode: http.StatusOK, Body: []byte(`[{"type":"trojan","tag":"p1-node","server":"p1.example"}]`)},
		},
		errs: map[string]error{},
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "p1", Name: "Provider 1", Type: TypeSubscription, Enabled: true, URL: "https://p1.example/sub", FetchMode: FetchViaBootstrap, Bootstrap: "boot", Priority: 20},
		{ID: "boot", Name: "Bootstrap", Type: TypeSubscription, Enabled: true, URL: "https://boot.example/sub", FetchMode: FetchDirect, Priority: 10},
	}
	result, err := NewRefreshService(lastGood, fetcher).RefreshAll(context.Background(), spec)
	if err != nil {
		t.Fatalf("refresh all: %v", err)
	}
	if len(result.Results) != 2 || result.Results[0].ProviderID != "boot" || result.Results[1].ProviderID != "p1" {
		t.Fatalf("unexpected refresh order/result: %+v", result)
	}
	if len(fetcher.calls) != 2 || fetcher.calls[1].ViaBootstrap == nil || fetcher.calls[1].ViaBootstrap.ID != "boot" {
		t.Fatalf("via_bootstrap fetch did not receive bootstrap provider: %+v", fetcher.calls)
	}
	if !lastGood.Exists("boot") || !lastGood.Exists("p1") {
		t.Fatalf("expected both last-good files to exist")
	}
}

func TestRefreshAllLoadsLocalFileProvider(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	filePath := filepath.Join(layout.DataDir, "nodes.json")
	if err := os.WriteFile(filePath, []byte(`[{"type":"trojan","tag":"local-1","server":"local.example","server_port":443}]`), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	lastGood := NewLastGoodStore(layout)
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID:       "local",
		Name:     "Local",
		Type:     TypeLocalFile,
		Enabled:  true,
		FilePath: filePath,
	}}

	result, err := NewRefreshService(lastGood, nil).RefreshAll(context.Background(), spec)
	if err != nil {
		t.Fatalf("refresh all: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != "ok" || result.Results[0].Count != 1 {
		t.Fatalf("unexpected local-file refresh result: %+v", result)
	}
	env, err := lastGood.Load("local")
	if err != nil {
		t.Fatalf("load local last-good: %v", err)
	}
	if env.Count != 1 || env.Identities[0].RawTag != "local-1" || len(env.Outbounds) != 0 {
		t.Fatalf("unexpected local last-good: %+v", env)
	}
}

func TestRefreshLocalFileRejectsOversizedInputBeforeParsing(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(layout.DataDir, "oversized-nodes.json")
	if err := os.WriteFile(filePath, bytes.Repeat([]byte("x"), MaxSubscriptionBodyBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{ID: "local", Name: "Local", Type: TypeLocalFile, Enabled: true, FilePath: filePath}}
	result, err := NewRefreshService(NewLastGoodStore(layout), nil).RefreshAll(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != "failed" || !strings.Contains(result.Results[0].Error, "过大") {
		t.Fatalf("oversized local file was not rejected: %+v", result)
	}
}

func TestRefreshErrorTextLocalizesKnownAndUnknownFailures(t *testing.T) {
	tests := map[string]string{
		"HTTP 503":                   "服务器返回 HTTP 503",
		"subscription body is empty": "订阅内容为空",
		"subscription contains no supported node identities": "订阅中没有可支持的节点",
		"net/http: TLS handshake timeout":                    "订阅服务器 TLS 握手失败",
		"some future provider failure":                       "订阅刷新失败",
		`Get "https://user:pass@example.com/sub"`:            "订阅刷新失败",
	}
	for input, want := range tests {
		if got := RefreshErrorText(input); got != want {
			t.Errorf("RefreshErrorText(%q)=%q want %q", input, got, want)
		}
	}
}

func TestRefreshOneViaBootstrapRequiresBootstrapLastGood(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{
		{ID: "boot", Name: "Bootstrap", Type: TypeSubscription, Enabled: true, URL: "https://boot.example/sub", FetchMode: FetchDirect},
		{ID: "p1", Name: "Provider 1", Type: TypeSubscription, Enabled: true, URL: "https://p1.example/sub", FetchMode: FetchViaBootstrap, Bootstrap: "boot"},
	}
	fetcher := &fakeFetcher{resp: map[string]FetchResponse{}, errs: map[string]error{"p1": errors.New("should not fetch")}}
	_, err := NewRefreshService(NewLastGoodStore(layout), fetcher).RefreshOne(context.Background(), spec, "p1")
	if err == nil {
		t.Fatal("expected missing bootstrap last-good to fail")
	}
	if len(fetcher.calls) != 0 {
		t.Fatalf("provider should not be fetched without bootstrap last-good: %+v", fetcher.calls)
	}
}

func TestRefreshOneRetriesTransientFetchError(t *testing.T) {
	layout := state.Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	lastGood := NewLastGoodStore(layout)
	if _, err := lastGood.Save("p1", []map[string]any{{"type": "vmess", "tag": "old", "server": "old.example"}}); err != nil {
		t.Fatalf("seed last-good: %v", err)
	}
	fetcher := &countingFetcher{
		errs: []error{fmt.Errorf(`Get "https://p1.example/sub": net/http: TLS handshake timeout`), nil},
		resp: []FetchResponse{{}, {StatusCode: http.StatusOK, Body: []byte(`[{"type":"vmess","tag":"new","server":"new.example"}]`)}},
	}
	spec := state.DefaultSpecState()
	spec.Providers = []state.ProviderSpec{{
		ID: "p1", Name: "Provider 1", Type: TypeSubscription, Enabled: true,
		URL: "https://p1.example/sub", FetchMode: FetchDirect,
	}}
	result, err := NewRefreshService(lastGood, fetcher).RefreshOne(context.Background(), spec, "p1")
	if err != nil {
		t.Fatalf("refresh one: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != "ok" || !result.Results[0].Changed {
		t.Fatalf("expected ok after retry, got %+v", result)
	}
	if fetcher.calls != 2 {
		t.Fatalf("expected 2 fetch attempts, got %d", fetcher.calls)
	}
}

type countingFetcher struct {
	calls int
	errs  []error
	resp  []FetchResponse
}

func (f *countingFetcher) Fetch(ctx context.Context, req FetchRequest) (FetchResponse, error) {
	_ = ctx
	_ = req
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return FetchResponse{}, f.errs[i]
	}
	if i < len(f.resp) {
		return f.resp[i], nil
	}
	return FetchResponse{}, fmt.Errorf("unexpected fetch call %d", i)
}
