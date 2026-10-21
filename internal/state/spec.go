package state

import (
	"strings"

	"smartgate/internal/smartnode"
)

type SpecState struct {
	SchemaVersion int                            `json:"schema_version"`
	Settings      SettingsSpec                   `json:"settings"`
	DNS           DNSSpec                        `json:"dns"`
	Providers     []ProviderSpec                 `json:"providers,omitempty"`
	SmartNodes    map[string]smartnode.SmartNode `json:"smart_nodes,omitempty"`
	BusinessPools []BusinessPoolSpec             `json:"business_pools"`
	Rules         RuleCatalog                    `json:"rules"`
	RemoteRules   RemoteRuleSettings             `json:"remote_rules,omitempty"`
	Fabric        FabricSpec                     `json:"fabric"`
	Scheduler     []SchedulerTaskSpec            `json:"scheduler,omitempty"`
}

type SettingsSpec struct {
	ListenLAN            string `json:"listen_lan"`
	AllowWAN             bool   `json:"allow_wan"`
	DiscardProviderRules bool   `json:"discard_provider_rules"`
	GatewayMode          string `json:"gateway_mode"`
	OverlayGuardEnabled  bool   `json:"overlay_guard_enabled"`
}

const (
	GatewayModeOn     = "on"
	GatewayModeSafe   = "safe"
	GatewayModeOff    = "off"
	GatewayModeGlobal = "global"
)

func ValidGatewayMode(mode string) bool {
	switch mode {
	case GatewayModeOn, GatewayModeSafe, GatewayModeOff, GatewayModeGlobal:
		return true
	default:
		return false
	}
}

func EffectiveGatewayMode(mode string) string {
	if ValidGatewayMode(mode) {
		return mode
	}
	return GatewayModeOn
}

type ProviderSpec struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	Enabled         bool     `json:"enabled"`
	FetchMode       string   `json:"fetch_mode,omitempty"`
	Roles           []string `json:"roles,omitempty"`
	Bootstrap       string   `json:"bootstrap,omitempty"`
	URL             string   `json:"url,omitempty"`
	FilePath        string   `json:"file_path,omitempty"`
	UserAgent       string   `json:"user_agent,omitempty"`
	Priority        int      `json:"priority,omitempty"`
	LastGoodID      string   `json:"last_good_id,omitempty"`
	IncludeKeywords []string `json:"include_keywords,omitempty"`
	ExcludeKeywords []string `json:"exclude_keywords,omitempty"`
}

type BusinessPoolSpec struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Role          string     `json:"role,omitempty"`
	NodeIDs       []string   `json:"node_ids"`
	Policy        PoolPolicy `json:"policy"`
	BuiltinOrigin string     `json:"builtin_origin,omitempty"`
}

type PoolPolicy struct {
	Cutover              string `json:"cutover"`
	GracefulDrainSeconds int    `json:"graceful_drain_seconds"`
	NoCandidate          string `json:"no_candidate"`
	AutoFailover         bool   `json:"auto_failover"`
}

const (
	CutoverStrict   = "strict"
	CutoverGraceful = "graceful"

	NoCandidateBlock  = "block"
	NoCandidateDirect = "direct"
	NoCandidateHold   = "hold"
)

type FabricSpec struct {
	Takeover FabricTakeoverSpec `json:"takeover"`
	DNS      FabricDNSSpec      `json:"dns"`
}

type FabricTakeoverSpec struct {
	Enabled bool `json:"enabled"`
	TCP     bool `json:"tcp"`
	UDP     bool `json:"udp"`
	IPv4    bool `json:"ipv4"`
	IPv6    bool `json:"ipv6"`
}

type FabricDNSSpec struct {
	Listen             string `json:"listen"`
	RedirectLAN53      bool   `json:"redirect_lan_53"`
	RejectHTTPSRecords bool   `json:"reject_https_records"`
	BlockDoT853        bool   `json:"block_dot_853"`
}

type RuleSpec struct {
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	IPCIDR        []string `json:"ip_cidr,omitempty"`
}

type RemoteRuleSettings struct {
	Enabled        bool            `json:"enabled"`
	DefaultFormat  string          `json:"default_format"`
	DownloadDetour string          `json:"download_detour"`
	UpdateInterval string          `json:"update_interval"`
	MemoryGuard    RuleMemoryGuard `json:"memory_guard"`
	Sets           []RemoteRuleSet `json:"sets,omitempty"`
	AdBlock        AdBlockSettings `json:"ad_block"`
}

type RuleMemoryGuard struct {
	MaxEnabledSets  int `json:"max_enabled_sets"`
	MaxTotalBytes   int `json:"max_total_bytes"`
	MaxSetBytes     int `json:"max_set_bytes"`
	WarnTotalBytes  int `json:"warn_total_bytes"`
	WarnEnabledSets int `json:"warn_enabled_sets"`
}

type RemoteRuleSet struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Kind           string   `json:"kind"`
	Target         string   `json:"target"`
	Outbound       string   `json:"outbound,omitempty"`
	Format         string   `json:"format,omitempty"`
	Type           string   `json:"type,omitempty"`
	URL            string   `json:"url"`
	Path           string   `json:"path,omitempty"`
	DownloadDetour string   `json:"download_detour,omitempty"`
	UpdateInterval string   `json:"update_interval,omitempty"`
	EstimatedBytes int      `json:"estimated_bytes,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Description    string   `json:"description,omitempty"`
}

type AdBlockSettings struct {
	Enabled bool     `json:"enabled"`
	Mode    string   `json:"mode"`
	Sets    []string `json:"sets,omitempty"`
}

type DNSSpec struct {
	Strategy        string   `json:"strategy"`
	FakeIPEnabled   bool     `json:"fakeip_enabled"`
	FakeIPRange     string   `json:"fakeip_range"`
	Final           string   `json:"final"`
	DirectServers   []string `json:"direct_servers"`
	ProxyServers    []string `json:"proxy_servers"`
	FallbackServers []string `json:"fallback_servers"`
}

type SchedulerTaskSpec struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Enabled  bool           `json:"enabled"`
	Schedule string         `json:"schedule,omitempty"`
	Spec     map[string]any `json:"spec,omitempty"`
}

const (
	SchedulerTaskProviderRefresh       = "provider_refresh"
	SchedulerTaskProviderRefreshLegacy = "refresh_provider"
	SchedulerTaskRestartService        = "restart_service"
	SchedulerTaskRestartServiceSpecKey = "service"
)

func SchedulerTaskRestartDefaultService() string {
	return "sing-box"
}

func DefaultSpecState() SpecState {
	return SpecState{
		SchemaVersion: 3,
		Settings: SettingsSpec{
			ListenLAN:            "192.168.1.1:3000",
			AllowWAN:             false,
			DiscardProviderRules: true,
			GatewayMode:          GatewayModeOn,
			OverlayGuardEnabled:  true,
		},
		DNS:           DefaultDNSSpec(),
		BusinessPools: DefaultBusinessPools(),
		Rules:         DefaultRules(),
		RemoteRules:   DefaultRemoteRuleSettings(),
		Fabric:        DefaultFabricSpec(),
		Scheduler: []SchedulerTaskSpec{
			{
				ID:       "provider-auto-refresh",
				Name:     "Provider auto refresh",
				Type:     "provider_refresh",
				Enabled:  true,
				Schedule: "@every 4h",
			},
			{
				ID:       "daily-backup",
				Name:     "Daily backup",
				Type:     "backup",
				Enabled:  true,
				Schedule: "0 5 * * *",
			},
			{
				ID:       "nightly-service-restart",
				Name:     "Nightly service restart",
				Type:     SchedulerTaskRestartService,
				Enabled:  false,
				Schedule: "0 4 * * *",
				Spec: map[string]any{
					SchedulerTaskRestartServiceSpecKey: SchedulerTaskRestartDefaultService(),
				},
			},
			{
				ID:       "weekly-router-reboot",
				Name:     "Weekly router reboot",
				Type:     "reboot_router",
				Enabled:  false,
				Schedule: "0 4 * * 0",
			},
			{
				ID:       "node-delay-test",
				Name:     "Node delay test",
				Type:     "delay_test",
				Enabled:  true,
				Schedule: "@every 1h",
			},
			{
				ID:       "failover-watchdog",
				Name:     "Failover watchdog",
				Type:     "failover_watchdog",
				Enabled:  true,
				Schedule: "@every 1m",
			},
		},
	}
}

func NormalizeSchedulerTaskSpec(task SchedulerTaskSpec) SchedulerTaskSpec {
	task.Type = strings.TrimSpace(strings.ToLower(task.Type))
	switch task.Type {
	case SchedulerTaskProviderRefreshLegacy:
		task.Type = SchedulerTaskProviderRefresh
	case legacyRestartServiceTaskType():
		task.Type = SchedulerTaskRestartService
		if task.Spec == nil {
			task.Spec = map[string]any{}
		}
		task.Spec[SchedulerTaskRestartServiceSpecKey] = SchedulerTaskRestartDefaultService()
	}
	return task
}

func legacyRestartServiceTaskType() string {
	return "restart_singbox"
}

func DefaultDNSSpec() DNSSpec {
	return DNSSpec{
		Strategy:        "prefer_ipv4",
		FakeIPEnabled:   false,
		FakeIPRange:     "",
		Final:           "direct",
		DirectServers:   []string{"223.5.5.5", "119.29.29.29"},
		ProxyServers:    []string{},
		FallbackServers: []string{},
	}
}

func RuleKeys() []string {
	return []string{"direct", "generalproxy", "streaming", "ai", "banking", "crypto", "block"}
}

func DefaultBusinessPools() []BusinessPoolSpec {
	return []BusinessPoolSpec{
		{ID: "general", Name: "General", Role: "general", BuiltinOrigin: "general", Policy: TemplatePoolPolicy("general")},
		{ID: "ai", Name: "AI", Role: "ai", BuiltinOrigin: "ai", Policy: TemplatePoolPolicy("ai")},
		{ID: "streaming", Name: "Streaming", Role: "streaming", BuiltinOrigin: "streaming", Policy: TemplatePoolPolicy("streaming")},
		{ID: "banking", Name: "Banking", Role: "banking", BuiltinOrigin: "banking", Policy: TemplatePoolPolicy("banking")},
		{ID: "crypto", Name: "Crypto", Role: "crypto", BuiltinOrigin: "crypto", Policy: TemplatePoolPolicy("crypto")},
	}
}

func EffectiveBusinessPools(in []BusinessPoolSpec) []BusinessPoolSpec {
	if in == nil {
		return DefaultBusinessPools()
	}
	return in
}

func TemplatePoolPolicy(origin string) PoolPolicy {
	switch origin {
	case "general", "streaming":
		return PoolPolicy{Cutover: CutoverGraceful, GracefulDrainSeconds: 30, NoCandidate: NoCandidateDirect, AutoFailover: true}
	case "ai", "banking", "crypto":
		return PoolPolicy{Cutover: CutoverStrict, GracefulDrainSeconds: 30, NoCandidate: NoCandidateBlock, AutoFailover: true}
	default:
		return PoolPolicy{}
	}
}

func DefaultFabricSpec() FabricSpec {
	return FabricSpec{
		Takeover: FabricTakeoverSpec{Enabled: true, TCP: true, UDP: true, IPv4: true, IPv6: true},
		DNS: FabricDNSSpec{
			Listen:             "[::]:1053",
			RedirectLAN53:      true,
			RejectHTTPSRecords: true,
			BlockDoT853:        true,
		},
	}
}

func DefaultRuleSpecs() map[string]RuleSpec {
	return map[string]RuleSpec{
		"direct": {
			DomainSuffix: []string{"home.arpa", "lan", "local"},
		},
		"generalproxy": {},
		"streaming": {
			DomainSuffix: []string{"netflix.com", "disneyplus.com", "youtube.com"},
		},
		"ai": {
			DomainSuffix: []string{
				"anthropic.com",
				"assemblyai.com",
				"beautiful.ai",
				"bolt.new",
				"chat.com",
				"chatgpt.com",
				"claude.ai",
				"claude.com",
				"claudemcpclient.com",
				"claudemcpcontent.com",
				"claudeusercontent.com",
				"clau.de",
				"codeium.com",
				"codeiumdata.com",
				"cohere.com",
				"cursor-cdn.com",
				"cursor.com",
				"cursor.sh",
				"cursorapi.com",
				"deepgram.com",
				"deepinfra.com",
				"deepmind.google",
				"descript.com",
				"dreamstudio.ai",
				"elevenlabs.com",
				"elevenlabs.io",
				"fal.ai",
				"fireworks.ai",
				"gamma.app",
				"gemini.google.com",
				"grok.com",
				"groq.com",
				"groqcloud.com",
				"heygen.com",
				"hf.co",
				"hf.space",
				"huggingface.co",
				"ideogram.ai",
				"jasper.ai",
				"krea.ai",
				"leonardo.ai",
				"lovable.dev",
				"luma.ai",
				"lumalabs.ai",
				"midjourney.com",
				"murf.ai",
				"oaistatic.com",
				"oaistatsig.com",
				"oaiusercontent.com",
				"openai.com",
				"openrouter.ai",
				"otter.ai",
				"perplexity.ai",
				"perplexity.com",
				"phind.com",
				"pika.art",
				"poe.com",
				"pplx.ai",
				"recraft.ai",
				"replicate.com",
				"replicate.delivery",
				"runpod.io",
				"runpod.net",
				"runwayml.com",
				"scite.ai",
				"sora.com",
				"sourcegraph.com",
				"stability.ai",
				"suno.com",
				"tabnine.com",
				"together.ai",
				"together.xyz",
				"udio.com",
				"v0.dev",
				"vercel.com",
				"windsurf.build",
				"windsurf.com",
				"x.ai",
				"you.com",
			},
			DomainKeyword: []string{
				"anthropic",
				"chatgpt",
				"claude",
				"cursor",
				"gemini",
				"grok",
				"openai",
				"perplexity",
			},
		},
		"banking": {
			DomainSuffix: []string{
				"account.hrblock.com",
				"ais.usvisa-info.com",
				"alloy.com",
				"americanexpress.com",
				"amexgiftcard.com",
				"annualcreditreport.com",
				"app.mercury.com",
				"app.ramp.com",
				"apps.irs.gov",
				"auth.accountmanagement.bofa.com",
				"bankofamerica.com",
				"bofa.com",
				"brex.com",
				"capitalone.com",
				"capitaloneshopping.com",
				"card.apple.com",
				"cash.app",
				"ceac.state.gov",
				"chase.com",
				"chexsystems.com",
				"chime.com",
				"citi.com",
				"citibank.com",
				"consumerfinance.gov",
				"creditkarma.com",
				"discover.com",
				"discovercard.com",
				"documentation.jumio.ai",
				"e-verify.gov",
				"engage.wf.com",
				"entrust.com",
				"equifax.com",
				"etoro.com",
				"experian.com",
				"freetaxusa.com",
				"getchime.com",
				"help.venmo.com",
				"hrblock.com",
				"id.me",
				"identitytheft.gov",
				"idnow.io",
				"irs.gov",
				"irs.gov/file-your-taxes-for-free",
				"jpmorgan.com",
				"jpmorganchase.com",
				"jumio.com",
				"lexisnexis.com",
				"login.gov",
				"login.onfido.com",
				"mastercard.com",
				"mastercard.us",
				"mastercardgiftcard.com",
				"members.transunion.com",
				"mercury.com",
				"my.equifax.com",
				"my.plaid.com",
				"my.uscis.gov",
				"myeverify.uscis.gov",
				"myfico.com",
				"onfido.com",
				"online.citi.com",
				"onlinebanking.usbank.com",
				"pay.google.com",
				"paypal.com",
				"paypal.me",
				"plaid.com",
				"portal.jumio.ai",
				"portal.us.transunion.com",
				"ramp.com",
				"revolut.com",
				"robinhood.com",
				"sa4.www4.irs.gov",
				"secure.bankofamerica.com",
				"secure.login.gov",
				"secure.ssa.gov",
				"services.citi.com",
				"socure.com",
				"sofi.com",
				"ss3.experian.com",
				"ssa.gov",
				"state.gov",
				"stripe.com",
				"sumsub.com",
				"support.ramp.com",
				"taxact.com",
				"taxactsoftware.com",
				"transunion.com",
				"travel.state.gov",
				"trulioo.com",
				"turbotax.intuit.com",
				"usa.gov",
				"usbank.com",
				"uscis.gov",
				"ustraveldocs.com",
				"venmo.com",
				"veriff.com",
				"veriffaml.com",
				"visa.com",
				"wallet.google",
				"welcome.wf.com",
				"wellsfargo.com",
				"wellsfargoadvisors.com",
				"wise.com",
				"withpersona.com",
			},
		},
		"crypto": {
			DomainSuffix: []string{
				"accounts.coinbase.com",
				"alchemypay.org",
				"alchemypay.us",
				"app.walletconnect.com",
				"argent.xyz",
				"banxa.com",
				"base.app",
				"binance.com",
				"binance.us",
				"bingx.com",
				"bitcoin.com",
				"bitfinex.com",
				"bitflyer.com",
				"bitget.com",
				"bitgetapp.com",
				"bithumb.com",
				"bitpay.com",
				"bitstamp.net",
				"blockchain.com",
				"bybit.com",
				"bybit.global",
				"bybitglobal.com",
				"coinbase.com",
				"coincheck.com",
				"coinex.com",
				"coinex.network",
				"coingate.com",
				"coinlist.co",
				"coinpayments.net",
				"crypto.com",
				"crypto.sardine.ai",
				"dashboard.moonpay.com",
				"exodus.com",
				"gate.com",
				"gate.io",
				"gemini.com",
				"htx.com",
				"kast.gg",
				"kast.live",
				"kast.xyz",
				"kraken.com",
				"kucoin.com",
				"lbank.com",
				"lbankneo.com",
				"ledger.com",
				"mercuryo.io",
				"metamask.io",
				"mexc.co",
				"mexc.com",
				"moonpay.com",
				"my.mercuryo.io",
				"nowpayments.io",
				"okcoin.com",
				"okx.com",
				"opennode.com",
				"phantom.com",
				"pionex.ai",
				"pionex.com",
				"pionex.us",
				"rabby.io",
				"rainbow.me",
				"ramp.alchemypay.org",
				"rampnetwork.com",
				"river.com",
				"safepal.com",
				"sardine.com",
				"shop.ledger.com",
				"simplex.com",
				"simplex.inc",
				"strike.me",
				"support.alchemypay.org",
				"support.moonpay.com",
				"swanbitcoin.com",
				"tangem.com",
				"transak.com",
				"trezor.io",
				"trustwallet.com",
				"upbit.com",
				"uphold.com",
				"walletconnect.com",
				"walletconnect.network",
				"web3.okx.com",
				"whitebit.com",
				"whitebit.us",
				"zengo.com",
				"zerion.io",
			},
		},
		"block": {},
	}
}

func DefaultRemoteRuleSettings() RemoteRuleSettings {
	return RemoteRuleSettings{
		Enabled:        true,
		DefaultFormat:  "binary",
		DownloadDetour: "Proxy",
		UpdateInterval: "24h",
		MemoryGuard: RuleMemoryGuard{
			MaxEnabledSets:  12,
			MaxTotalBytes:   8 * 1024 * 1024,
			MaxSetBytes:     4 * 1024 * 1024,
			WarnTotalBytes:  5 * 1024 * 1024,
			WarnEnabledSets: 8,
		},
		Sets: []RemoteRuleSet{
			{
				ID:             "geosite-category-ads",
				Name:           "Ads Domain Blocklist",
				Enabled:        false,
				Kind:           "adblock",
				Target:         "block",
				Format:         "binary",
				Type:           "remote",
				URL:            "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-category-ads.srs",
				DownloadDetour: "Proxy",
				UpdateInterval: "24h",
				EstimatedBytes: 2 * 1024 * 1024,
				Tags:           []string{"ads", "block", "geo"},
				Description:    "Standard geo ad-blocking rule-set. Disabled by default and controlled by the ad-block memory guard.",
			},
			{
				ID:             "geosite-cn",
				Name:           "GeoSite CN",
				Enabled:        false,
				Kind:           "geo",
				Target:         "direct",
				Format:         "binary",
				Type:           "remote",
				URL:            "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs",
				DownloadDetour: "Proxy",
				UpdateInterval: "24h",
				EstimatedBytes: 800 * 1024,
				Tags:           []string{"geo", "cn", "direct"},
				Description:    "CN domain direct rule-set for the classic geo three-line behavior.",
			},
			{
				ID:             "geoip-cn",
				Name:           "GeoIP CN",
				Enabled:        false,
				Kind:           "geo",
				Target:         "direct",
				Format:         "binary",
				Type:           "remote",
				URL:            "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs",
				DownloadDetour: "Proxy",
				UpdateInterval: "24h",
				EstimatedBytes: 900 * 1024,
				Tags:           []string{"geo", "cn", "direct"},
				Description:    "CN IP direct rule-set for the classic geo three-line behavior.",
			},
		},
		AdBlock: AdBlockSettings{
			Enabled: false,
			Mode:    "remote",
			Sets:    []string{"geosite-category-ads"},
		},
	}
}

func EffectiveRemoteRuleSettings(in RemoteRuleSettings) RemoteRuleSettings {
	defaults := DefaultRemoteRuleSettings()
	// Empty aside from Enabled: fill catalog defaults, but never flip an
	// explicit/zero Enabled:false into the default master-switch true.
	bare := in.DefaultFormat == "" && in.DownloadDetour == "" && in.UpdateInterval == "" && len(in.Sets) == 0 && in.MemoryGuard == (RuleMemoryGuard{}) && emptyAdBlockSettings(in.AdBlock)
	if bare {
		if in.Enabled {
			return defaults
		}
		out := defaults
		out.Enabled = false
		return out
	}
	if in.DefaultFormat == "" {
		in.DefaultFormat = defaults.DefaultFormat
	}
	if in.DownloadDetour == "" {
		in.DownloadDetour = defaults.DownloadDetour
	}
	if in.UpdateInterval == "" {
		in.UpdateInterval = defaults.UpdateInterval
	}
	if in.MemoryGuard.MaxEnabledSets == 0 {
		in.MemoryGuard.MaxEnabledSets = defaults.MemoryGuard.MaxEnabledSets
	}
	if in.MemoryGuard.MaxTotalBytes == 0 {
		in.MemoryGuard.MaxTotalBytes = defaults.MemoryGuard.MaxTotalBytes
	}
	if in.MemoryGuard.MaxSetBytes == 0 {
		in.MemoryGuard.MaxSetBytes = defaults.MemoryGuard.MaxSetBytes
	}
	if in.MemoryGuard.WarnTotalBytes == 0 {
		in.MemoryGuard.WarnTotalBytes = defaults.MemoryGuard.WarnTotalBytes
	}
	if in.MemoryGuard.WarnEnabledSets == 0 {
		in.MemoryGuard.WarnEnabledSets = defaults.MemoryGuard.WarnEnabledSets
	}
	if len(in.Sets) == 0 {
		in.Sets = defaults.Sets
	} else {
		in.Sets = mergeDefaultRemoteRuleSets(defaults.Sets, in.Sets)
	}
	if in.AdBlock.Mode == "" {
		in.AdBlock.Mode = defaults.AdBlock.Mode
	}
	if len(in.AdBlock.Sets) == 0 {
		in.AdBlock.Sets = defaults.AdBlock.Sets
	}
	return in
}

func mergeDefaultRemoteRuleSets(defaults, current []RemoteRuleSet) []RemoteRuleSet {
	byID := make(map[string]RemoteRuleSet, len(current))
	customOrder := make([]RemoteRuleSet, 0, len(current))
	defaultIDs := make(map[string]struct{}, len(defaults))
	for _, item := range defaults {
		defaultIDs[item.ID] = struct{}{}
	}
	for _, item := range current {
		if isDeprecatedBusinessGeoRuleSet(item.ID) {
			continue
		}
		byID[item.ID] = item
		if _, ok := defaultIDs[item.ID]; !ok {
			customOrder = append(customOrder, item)
		}
	}
	out := make([]RemoteRuleSet, 0, len(defaults)+len(customOrder))
	for _, item := range defaults {
		if existing, ok := byID[item.ID]; ok {
			out = append(out, existing)
			continue
		}
		out = append(out, item)
	}
	out = append(out, customOrder...)
	return out
}

func isDeprecatedBusinessGeoRuleSet(id string) bool {
	switch id {
	case "geosite-category-ai-not-cn",
		"geosite-openai",
		"geosite-anthropic",
		"geosite-google-gemini",
		"geosite-xai",
		"geosite-cursor",
		"geosite-category-media",
		"geosite-category-cryptocurrency",
		"geosite-paypal",
		"geosite-stripe",
		"geosite-wise":
		return true
	default:
		return false
	}
}

func emptyAdBlockSettings(in AdBlockSettings) bool {
	return !in.Enabled && in.Mode == "" && len(in.Sets) == 0
}

func isKnownRuleKey(key string) bool {
	for _, known := range RuleKeys() {
		if key == known {
			return true
		}
	}
	return false
}

func EffectiveDNSSpec(dns DNSSpec) DNSSpec {
	if dns.Strategy == "" && dns.FakeIPRange == "" && dns.Final == "" && len(dns.DirectServers) == 0 && len(dns.ProxyServers) == 0 && len(dns.FallbackServers) == 0 && !dns.FakeIPEnabled {
		return DefaultDNSSpec()
	}
	if dns.Strategy == "" {
		dns.Strategy = "prefer_ipv4"
	}
	dns.FakeIPEnabled = false
	dns.FakeIPRange = ""
	if dns.Final == "" {
		dns.Final = "direct"
	}
	if len(dns.DirectServers) == 0 {
		dns.DirectServers = []string{"223.5.5.5", "119.29.29.29"}
	}
	if dns.ProxyServers == nil {
		dns.ProxyServers = []string{}
	}
	if dns.FallbackServers == nil {
		dns.FallbackServers = []string{}
	}
	return dns
}
