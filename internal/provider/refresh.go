package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"smartgate/internal/state"
)

type FetchRequest struct {
	Provider     state.ProviderSpec
	ViaBootstrap *state.ProviderSpec
}

type FetchResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

type Fetcher interface {
	Fetch(ctx context.Context, req FetchRequest) (FetchResponse, error)
}

type HTTPFetcher struct {
	Client            *http.Client
	BootstrapProxyURL string
}

const DefaultBootstrapProxyURL = "http://127.0.0.1:2080"

var remoteProfileScheme = "sing-box"

func (f HTTPFetcher) Fetch(ctx context.Context, req FetchRequest) (FetchResponse, error) {
	client, err := f.clientFor(req)
	if err != nil {
		return FetchResponse{}, err
	}
	effectiveURL, err := EffectiveSubscriptionURL(req.Provider.URL)
	if err != nil {
		return FetchResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, effectiveURL, nil)
	if err != nil {
		return FetchResponse{}, err
	}
	httpReq.Header.Set("User-Agent", subscriptionUserAgent(req.Provider))
	resp, err := client.Do(httpReq)
	if err != nil {
		return FetchResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxSubscriptionBodyBytes+1))
	if err != nil {
		return FetchResponse{}, err
	}
	return FetchResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

func (f HTTPFetcher) clientFor(req FetchRequest) (*http.Client, error) {
	if req.ViaBootstrap == nil {
		if f.Client != nil {
			return f.Client, nil
		}
		transport := defaultHTTPTransport()
		transport.Proxy = nil
		return &http.Client{Timeout: 90 * time.Second, Transport: transport}, nil
	}
	if f.BootstrapProxyURL == "" {
		return nil, fmt.Errorf("via_bootstrap fetch requires loopback HTTP proxy URL")
	}
	proxyURL, err := parseLoopbackHTTPProxyURL(f.BootstrapProxyURL)
	if err != nil {
		return nil, err
	}
	transport := defaultHTTPTransport()
	transport.Proxy = http.ProxyURL(proxyURL)
	return &http.Client{Timeout: 90 * time.Second, Transport: transport}, nil
}

func defaultHTTPTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport := base.Clone()
		transport.DialContext = directResolverDialer().DialContext
		return transport
	}
	return &http.Transport{DialContext: directResolverDialer().DialContext}
}

func directResolverDialer() *net.Dialer {
	servers := directResolverServers()
	return &net.Dialer{
		Timeout: 30 * time.Second,
		Resolver: &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				proto := "udp"
				if strings.HasPrefix(network, "tcp") {
					proto = "tcp"
				}
				return dialDirectResolverServers(ctx, proto, servers)
			},
		},
	}
}

// dialDirectResolverServers tries each nameserver until one accepts the dial.
func dialDirectResolverServers(ctx context.Context, network string, servers []string) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	return dialDirectResolverServersWith(ctx, network, servers, d.DialContext)
}

func dialDirectResolverServersWith(ctx context.Context, network string, servers []string, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	var lastErr error
	for _, server := range servers {
		conn, err := dial(ctx, network, server)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no direct resolver nameservers configured")
}

func directResolverServers() []string {
	servers := nameserversFromResolvFiles(
		"/tmp/resolv.conf.d/resolv.conf.auto",
		"/etc/resolv.conf",
	)
	servers = append(servers, "223.5.5.5", "119.29.29.29")
	return normalizeNameservers(servers)
}

func nameserversFromResolvFiles(files ...string) []string {
	var servers []string
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" {
				servers = append(servers, fields[1])
			}
		}
	}
	return servers
}

func normalizeNameservers(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	servers := make([]string, 0, len(raw))
	for _, item := range raw {
		host := strings.TrimSpace(item)
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		addr := net.JoinHostPort(ip.String(), "53")
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		servers = append(servers, addr)
	}
	if len(servers) == 0 {
		return []string{"223.5.5.5:53"}
	}
	return servers
}

func subscriptionUserAgent(p state.ProviderSpec) string {
	if ua := strings.TrimSpace(p.UserAgent); ua != "" {
		return ua
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.URL)), remoteProfileScheme+"://") {
		return remoteProfileScheme + "/1.12.0"
	}
	return "SmartGate/2.0"
}

func parseLoopbackHTTPProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid bootstrap proxy URL: %w", err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("bootstrap proxy URL must use http scheme")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("bootstrap proxy URL must not include credentials, path, query, or fragment")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("bootstrap proxy URL host is required")
	}
	if host == "localhost" {
		return u, nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("bootstrap proxy URL must point to loopback, got %q", host)
	}
	return u, nil
}

type RefreshService struct {
	lastGood LastGoodRepository
	fetcher  Fetcher
}

type RefreshPlan struct {
	Steps []RefreshStep `json:"steps"`
}

type RefreshStep struct {
	ProviderID  string `json:"provider_id"`
	FetchMode   string `json:"fetch_mode"`
	BootstrapID string `json:"bootstrap_id,omitempty"`
}

type RefreshResult struct {
	Plan    RefreshPlan         `json:"plan"`
	Results []RefreshStepResult `json:"results"`
	Changed bool                `json:"changed"`
}

type RefreshStepResult struct {
	ProviderID        string            `json:"provider_id"`
	Status            string            `json:"status"`
	Changed           bool              `json:"changed"`
	MetadataOnly      bool              `json:"metadata_only,omitempty"`
	Count             int               `json:"count,omitempty"`
	Subscription      *SubscriptionInfo `json:"subscription,omitempty"`
	SubscriptionError string            `json:"subscription_error,omitempty"`
	Error             string            `json:"error,omitempty"`
}

func NewRefreshService(lastGood LastGoodRepository, fetcher Fetcher) *RefreshService {
	if fetcher == nil {
		fetcher = HTTPFetcher{}
	}
	return &RefreshService{lastGood: lastGood, fetcher: fetcher}
}

func (s *RefreshService) Plan(spec state.SpecState) (RefreshPlan, error) {
	normalized, err := ValidateSpecProviders(spec)
	if err != nil {
		return RefreshPlan{}, err
	}
	providers := append([]state.ProviderSpec(nil), normalized.Providers...)
	sort.SliceStable(providers, func(i, j int) bool {
		if providers[i].FetchMode == providers[j].FetchMode {
			if providers[i].Priority == providers[j].Priority {
				return providers[i].ID < providers[j].ID
			}
			return providers[i].Priority < providers[j].Priority
		}
		return providers[i].FetchMode == FetchDirect
	})
	plan := RefreshPlan{}
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		switch p.Type {
		case TypeSubscription:
			plan.Steps = append(plan.Steps, RefreshStep{
				ProviderID:  p.ID,
				FetchMode:   p.FetchMode,
				BootstrapID: p.Bootstrap,
			})
		case TypeLocalFile:
			plan.Steps = append(plan.Steps, RefreshStep{
				ProviderID: p.ID,
				FetchMode:  TypeLocalFile,
			})
		}
	}
	return plan, nil
}

func (s *RefreshService) RefreshAll(ctx context.Context, spec state.SpecState) (RefreshResult, error) {
	plan, err := s.Plan(spec)
	if err != nil {
		return RefreshResult{}, err
	}
	result := RefreshResult{Plan: plan}
	providers := providerMap(spec.Providers)
	available := map[string]bool{}
	var updates []LastGoodUpdate
	for _, step := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		p := providers[step.ProviderID]
		if p.Type == TypeLocalFile {
			stepResult, update := s.refreshLocalFile(p)
			if update != nil {
				updates = append(updates, *update)
			}
			result.Results = append(result.Results, stepResult)
			continue
		}
		var via *state.ProviderSpec
		if step.FetchMode == FetchViaBootstrap {
			if !available[step.BootstrapID] && !s.lastGood.Exists(step.BootstrapID) {
				result.Results = append(result.Results, RefreshStepResult{
					ProviderID: step.ProviderID,
					Status:     "failed",
					Error:      "bootstrap provider has no last-good",
				})
				continue
			}
			bootstrap := providers[step.BootstrapID]
			via = &bootstrap
		}
		stepResult, update := s.refreshOne(ctx, p, via)
		if stepResult.Status == "ok" || stepResult.Status == "unchanged" {
			available[p.ID] = true
		}
		if update != nil {
			updates = append(updates, *update)
		}
		result.Results = append(result.Results, stepResult)
	}
	if err := s.saveRefreshUpdates(ctx, updates, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (s *RefreshService) RefreshOne(ctx context.Context, spec state.SpecState, providerID string) (RefreshResult, error) {
	normalized, err := ValidateSpecProviders(spec)
	if err != nil {
		return RefreshResult{}, err
	}
	providers := providerMap(normalized.Providers)
	p, ok := providers[providerID]
	if !ok {
		return RefreshResult{}, fmt.Errorf("provider %s does not exist", providerID)
	}
	if !p.Enabled || (p.Type != TypeSubscription && p.Type != TypeLocalFile) {
		return RefreshResult{}, fmt.Errorf("provider %s is not an enabled refreshable provider", providerID)
	}
	if p.Type == TypeLocalFile {
		step := RefreshStep{ProviderID: p.ID, FetchMode: TypeLocalFile}
		stepResult, update := s.refreshLocalFile(p)
		result := RefreshResult{Plan: RefreshPlan{Steps: []RefreshStep{step}}, Results: []RefreshStepResult{stepResult}}
		if update != nil {
			if err := s.saveRefreshUpdates(ctx, []LastGoodUpdate{*update}, &result); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	var via *state.ProviderSpec
	if p.FetchMode == FetchViaBootstrap {
		if !s.lastGood.Exists(p.Bootstrap) {
			return RefreshResult{}, fmt.Errorf("provider %s bootstrap %s has no last-good", providerID, p.Bootstrap)
		}
		bootstrap := providers[p.Bootstrap]
		via = &bootstrap
	}
	step := RefreshStep{ProviderID: p.ID, FetchMode: p.FetchMode, BootstrapID: p.Bootstrap}
	stepResult, update := s.refreshOne(ctx, p, via)
	result := RefreshResult{Plan: RefreshPlan{Steps: []RefreshStep{step}}, Results: []RefreshStepResult{stepResult}}
	if update != nil {
		if err := s.saveRefreshUpdates(ctx, []LastGoodUpdate{*update}, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *RefreshService) fetchWithRetry(ctx context.Context, req FetchRequest) (FetchResponse, error) {
	var lastErr error
	// One immediate attempt + one short retry for transient bootstrap/proxy flakes.
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return FetchResponse{}, err
		}
		if attempt > 0 {
			timer := time.NewTimer(1500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return FetchResponse{}, ctx.Err()
			case <-timer.C:
			}
		}
		resp, err := s.fetcher.Fetch(ctx, req)
		if err == nil {
			// Retry once on gateway/upstream flakiness that often clears quickly.
			if attempt == 0 && (resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout) {
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				continue
			}
			return resp, nil
		}
		lastErr = err
		if !isTransientFetchError(err) {
			return FetchResponse{}, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("provider fetch failed")
	}
	return FetchResponse{}, lastErr
}

func isTransientFetchError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"tls handshake timeout",
		"i/o timeout",
		"timeout",
		"connection reset",
		"connection refused",
		"broken pipe",
		"eof",
		"temporary failure",
		"server misbehaving",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func (s *RefreshService) refreshOne(ctx context.Context, p state.ProviderSpec, via *state.ProviderSpec) (RefreshStepResult, *LastGoodUpdate) {
	resp, err := s.fetchWithRetry(ctx, FetchRequest{Provider: p, ViaBootstrap: via})
	if err != nil {
		return s.failedRefreshResult(p.ID, safeRefreshError(err)), nil
	}
	if resp.StatusCode == http.StatusNotModified {
		return RefreshStepResult{ProviderID: p.ID, Status: "unchanged"}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.failedRefreshResult(p.ID, fmt.Sprintf("服务器返回 HTTP %d", resp.StatusCode)), nil
	}
	now := time.Now()
	subscription, subscriptionErr := ParseSubscriptionUserinfoHeader(resp.Header, now)
	parsed, err := ParseSubscriptionPayload(p.ID, resp.Body)
	if err != nil {
		return s.failedRefreshResult(p.ID, safeRefreshError(err)), nil
	}
	if subscription == nil && subscriptionErr == nil {
		subscription = parsed.Subscription
	}
	update := &LastGoodUpdate{ProviderID: p.ID, Subscription: subscription, Identities: parsed.Identities}
	result := RefreshStepResult{ProviderID: p.ID, Status: "ok", Count: len(parsed.Identities), Subscription: subscription}
	if subscriptionErr != nil {
		result.SubscriptionError = safeRefreshError(subscriptionErr)
	}
	return result, update
}

func (s *RefreshService) refreshLocalFile(p state.ProviderSpec) (RefreshStepResult, *LastGoodUpdate) {
	file, err := os.Open(p.FilePath)
	if err != nil {
		return s.failedRefreshResult(p.ID, "本地订阅文件无法打开"), nil
	}
	body, readErr := io.ReadAll(io.LimitReader(file, MaxSubscriptionBodyBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return s.failedRefreshResult(p.ID, "本地订阅文件无法读取"), nil
	}
	if closeErr != nil {
		return s.failedRefreshResult(p.ID, "本地订阅文件无法关闭"), nil
	}
	if len(body) > MaxSubscriptionBodyBytes {
		return s.failedRefreshResult(p.ID, fmt.Sprintf("本地订阅文件过大：%d > %d", len(body), MaxSubscriptionBodyBytes)), nil
	}
	parsed, err := ParseSubscriptionPayload(p.ID, body)
	if err != nil {
		return s.failedRefreshResult(p.ID, safeRefreshError(err)), nil
	}
	update := &LastGoodUpdate{ProviderID: p.ID, Identities: parsed.Identities}
	return RefreshStepResult{ProviderID: p.ID, Status: "ok", Count: len(parsed.Identities)}, update
}

func safeRefreshError(err error) string {
	if err == nil {
		return ""
	}
	return RefreshErrorText(err.Error())
}

// RefreshErrorText converts provider-generated and legacy refresh errors into
// API-facing Chinese text without retaining URLs or credentials.
func RefreshErrorText(raw string) string {
	message := strings.TrimSpace(raw)
	if message == "" {
		return "订阅刷新失败"
	}
	if strings.Contains(strings.ToLower(message), "url") || strings.Contains(message, "://") {
		return "订阅刷新失败"
	}
	if strings.IndexFunc(message, func(character rune) bool {
		return unicode.Is(unicode.Han, character)
	}) >= 0 {
		return message
	}
	lower := strings.ToLower(message)
	var status int
	if _, err := fmt.Sscanf(message, "HTTP %d", &status); err == nil && status > 0 {
		return fmt.Sprintf("服务器返回 HTTP %d", status)
	}
	switch {
	case strings.Contains(lower, "subscription-userinfo"):
		return "订阅流量信息格式无效"
	case strings.Contains(lower, "local provider file could not be opened"):
		return "本地订阅文件无法打开"
	case strings.Contains(lower, "local provider file could not be read"):
		return "本地订阅文件无法读取"
	case strings.Contains(lower, "local provider file could not be closed"):
		return "本地订阅文件无法关闭"
	case strings.Contains(lower, "too large") || strings.Contains(lower, "exceeds"):
		return "订阅内容过大"
	case strings.Contains(lower, "body is empty"):
		return "订阅内容为空"
	case strings.Contains(lower, "looks like html"):
		return "订阅地址返回了网页内容"
	case strings.Contains(lower, "no supported node identities"):
		return "订阅中没有可支持的节点"
	case strings.Contains(lower, "does not contain outbounds") || strings.Contains(lower, "neither supported json"):
		return "订阅内容格式不受支持"
	case strings.Contains(lower, "x509") || strings.Contains(lower, "certificate"):
		return "订阅服务器证书校验失败"
	case strings.Contains(lower, "tls handshake"):
		return "订阅服务器 TLS 握手失败"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return "请求订阅超时"
	case strings.Contains(lower, "no such host") || strings.Contains(lower, "server misbehaving") || strings.Contains(lower, "resolver"):
		return "订阅服务器域名解析失败"
	case strings.Contains(lower, "connection refused"):
		return "订阅服务器拒绝连接"
	case strings.Contains(lower, "connection reset") || strings.Contains(lower, "broken pipe") || lower == "eof" || strings.HasSuffix(lower, ": eof"):
		return "订阅连接意外中断"
	case strings.Contains(lower, "context canceled"):
		return "订阅刷新已取消"
	default:
		return "订阅刷新失败"
	}
}

func (s *RefreshService) failedRefreshResult(providerID, reason string) RefreshStepResult {
	status := "failed"
	if s.lastGood.Exists(providerID) {
		status = "stale"
	}
	return RefreshStepResult{ProviderID: providerID, Status: status, Error: reason}
}

func (s *RefreshService) saveRefreshUpdates(ctx context.Context, updates []LastGoodUpdate, result *RefreshResult) error {
	if len(updates) == 0 {
		return nil
	}
	envs, err := s.lastGood.SaveMany(ctx, updates)
	if err != nil {
		return err
	}
	for i := range result.Results {
		env, ok := envs[result.Results[i].ProviderID]
		if !ok {
			continue
		}
		result.Results[i].Changed = true
		result.Results[i].MetadataOnly = env.MetadataOnly
		result.Results[i].Count = env.Count
		result.Results[i].Subscription = env.Subscription
		result.Changed = true
	}
	return nil
}

func providerMap(providers []state.ProviderSpec) map[string]state.ProviderSpec {
	out := make(map[string]state.ProviderSpec, len(providers))
	for _, p := range providers {
		out[p.ID] = p
	}
	return out
}
