package state

import (
	"time"

	"smartgate/internal/smartnode"
)

type ExecutionGraph struct {
	Workers       []WorkerTarget `json:"workers"`
	NodeExecutors []NodeExecutor `json:"node_executors"`
	PoolBindings  []PoolBinding  `json:"pool_bindings"`
	Fabric        FabricGraph    `json:"fabric"`
}

type WorkerTarget struct {
	ID           string             `json:"id"`
	Core         smartnode.CoreID   `json:"core"`
	Version      string             `json:"version,omitempty"`
	Control      WorkerControl      `json:"control"`
	Routes       []WorkerRoute      `json:"routes"`
	Inbounds     []WorkerInbound    `json:"inbounds"`
	ConfigSHA256 string             `json:"config_sha256,omitempty"`
	Resources    WorkerResourcePlan `json:"resources"`
}

type WorkerControl struct {
	Listen string `json:"listen"`
	Port   int    `json:"port"`
	Kind   string `json:"kind"`
}

type WorkerInbound struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Pool       string `json:"pool"`
	Route      string `json:"route"`
	Lane       int    `json:"lane"`
	Binding    string `json:"binding"`
	Generation uint64 `json:"generation"`
	Listen     string `json:"listen"`
	Port       int    `json:"port"`
	Network    string `json:"network"`
}

type WorkerRoute struct {
	ID         string   `json:"id"`
	Pool       string   `json:"pool"`
	Lane       int      `json:"lane"`
	Selector   string   `json:"selector"`
	Candidates []string `json:"candidates"`
	Selected   string   `json:"selected"`
}

type WorkerResourcePlan struct {
	EstimatedRSSKB int `json:"estimated_rss_kb,omitempty"`
	EstimatedFD    int `json:"estimated_fd,omitempty"`
	Ports          int `json:"ports,omitempty"`
	ConfigBytes    int `json:"config_bytes,omitempty"`
}

type NodeExecutor struct {
	ID        string           `json:"id"`
	SmartNode string           `json:"smart_node"`
	Revision  int              `json:"revision"`
	Core      smartnode.CoreID `json:"core"`
	Worker    string           `json:"worker"`
}

type PoolBinding struct {
	ID         string        `json:"id"`
	Pool       string        `json:"pool"`
	Target     BindingTarget `json:"target"`
	Generation uint64        `json:"generation"`
	Since      time.Time     `json:"since"`
}

type BindingTarget struct {
	Kind      string           `json:"kind"`
	SmartNode string           `json:"smart_node,omitempty"`
	Executor  string           `json:"executor,omitempty"`
	Core      smartnode.CoreID `json:"core,omitempty"`
	Worker    string           `json:"worker,omitempty"`
}

type FabricGraph struct {
	Takeover       FabricTakeoverGraph  `json:"takeover"`
	DNS            FabricDNSGraph       `json:"dns"`
	Rules          []FabricRuleGraph    `json:"rules"`
	RuleSets       []RuleSetSnapshotRef `json:"rule_sets,omitempty"`
	FallbackPool   string               `json:"fallback_pool"`
	Dispatch       []PoolDispatchGraph  `json:"dispatch"`
	RuleSetsPinned bool                 `json:"-"`
}

type FabricTakeoverGraph struct {
	Enabled bool `json:"enabled"`
	TCP     bool `json:"tcp"`
	UDP     bool `json:"udp"`
	IPv4    bool `json:"ipv4"`
	IPv6    bool `json:"ipv6"`
}

type FabricDNSGraph struct {
	Listen             string                    `json:"listen"`
	RedirectLAN53      bool                      `json:"redirect_lan_53"`
	RejectHTTPSRecords bool                      `json:"reject_https_records"`
	BlockDoT853        bool                      `json:"block_dot_853"`
	PoolSets           map[string]FabricPoolSets `json:"pool_sets"`
}

type FabricPoolSets struct {
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`
}

type FabricRuleGraph struct {
	ID       string                `json:"id"`
	Source   string                `json:"source"`
	Kind     string                `json:"kind"`
	Value    string                `json:"value"`
	Target   string                `json:"target"`
	Priority int                   `json:"priority"`
	IPSets   *FabricRuleIPSetNames `json:"ip_sets,omitempty"`
}

type FabricRuleIPSetNames struct {
	IPv4             string `json:"ipv4"`
	IPv6             string `json:"ipv6"`
	DefinitionPath   string `json:"definition_path"`
	DefinitionSHA256 string `json:"definition_sha256"`
}

type RuleSetSnapshotRef struct {
	ID                string               `json:"id"`
	SourceFingerprint string               `json:"source_fingerprint"`
	ContentSHA256     string               `json:"content_sha256"`
	MatcherSHA256     string               `json:"matcher_sha256"`
	ContentPath       string               `json:"content_path"`
	MatcherPath       string               `json:"matcher_path"`
	Format            string               `json:"format"`
	SourceType        string               `json:"source_type"`
	Size              int64                `json:"size"`
	FetchedAt         time.Time            `json:"fetched_at"`
	Stats             RuleSetSnapshotStats `json:"stats"`
}

type RuleSetSnapshotStats struct {
	DomainExact   int `json:"domain_exact"`
	DomainSuffix  int `json:"domain_suffix"`
	DomainKeyword int `json:"domain_keyword"`
	DomainRegex   int `json:"domain_regex"`
	IPCIDR        int `json:"ip_cidr"`
}

type PoolDispatchGraph struct {
	Pool       string `json:"pool"`
	Binding    string `json:"binding"`
	Generation uint64 `json:"generation"`
	Lane       int    `json:"lane"`
	Route      string `json:"route,omitempty"`
	Inbound    string `json:"inbound,omitempty"`
	DNSInbound string `json:"dns_inbound,omitempty"`
	Mark       uint32 `json:"mark"`
	MarkMask   uint32 `json:"mark_mask"`
	Table      int    `json:"table"`
	Port       int    `json:"port,omitempty"`
	DNSPort    int    `json:"dns_port,omitempty"`
	Target     string `json:"target"`
}
