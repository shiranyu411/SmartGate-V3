package smartnode

import "time"

type CoreID string

const (
	CapabilitySupported         = "supported"
	CapabilityUnsupportedByCore = "unsupported_by_core"
	CapabilityDegraded          = "degraded"
)

type SmartNode struct {
	ID         string           `json:"id"`
	Revision   int              `json:"revision"`
	Identity   NodeIdentity     `json:"identity"`
	Aliases    []NodeAlias      `json:"aliases"`
	CoreChoice CoreID           `json:"core_choice,omitempty"`
	Capability CapabilityMatrix `json:"capability"`
}

type NodeIdentity struct {
	Protocol  string            `json:"protocol"`
	Server    string            `json:"server"`
	Port      int               `json:"port"`
	SecretRef string            `json:"secret_ref,omitempty"`
	Transport Transport         `json:"transport"`
	Security  Security          `json:"security"`
	Extra     map[string]string `json:"extra,omitempty"`
}

type Transport struct {
	Type        string `json:"type"`
	Path        string `json:"path,omitempty"`
	Host        string `json:"host,omitempty"`
	ServiceName string `json:"service_name,omitempty"`
}

type Security struct {
	Type             string   `json:"type"`
	SNI              string   `json:"sni,omitempty"`
	ALPN             []string `json:"alpn,omitempty"`
	Fingerprint      string   `json:"fingerprint,omitempty"`
	Flow             string   `json:"flow,omitempty"`
	PublicKeyRef     string   `json:"public_key_ref,omitempty"`
	ShortID          string   `json:"short_id,omitempty"`
	SkipCertVerify   bool     `json:"skip_cert_verify,omitempty"`
	CertificateRef   string   `json:"certificate_ref,omitempty"`
	ClientCertRef    string   `json:"client_certificate_ref,omitempty"`
	ClientKeyRef     string   `json:"client_key_ref,omitempty"`
	ECHConfigListRef string   `json:"ech_config_list_ref,omitempty"`
}

type NodeAlias struct {
	ProviderID string    `json:"provider_id"`
	RawTag     string    `json:"raw_tag"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
}

type CapabilityMatrix map[CoreID]CoreCapability

type CoreCapability struct {
	Status      string   `json:"status"`
	Reasons     []string `json:"reasons"`
	ReasonTexts []string `json:"reason_texts,omitempty"`
}

type RawNode struct {
	ProviderID string
	RawTag     string
	Outbound   map[string]any
	SeenAt     time.Time
}

// IdentityInput is the catalog boundary after provider parsing. Outbound
// maps are intentionally absent; legacy callers use RawNode until their
// snapshot has been promoted to this input.
type IdentityInput struct {
	ProviderID string
	RawTag     string
	Identity   NodeIdentity
	SeenAt     time.Time
}

type ReconcileReport struct {
	Added        []string `json:"added,omitempty"`
	Revised      []string `json:"revised,omitempty"`
	MetadataOnly []string `json:"metadata_only,omitempty"`
	Removed      []string `json:"removed,omitempty"`
}

func (matrix CapabilityMatrix) ForCore(core CoreID) (CoreCapability, bool) {
	capability, exists := matrix[core]
	return capability, exists
}
