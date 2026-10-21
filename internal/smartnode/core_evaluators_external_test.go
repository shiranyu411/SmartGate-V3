package smartnode_test

import (
	"smartgate/internal/smartnode"
)

func init() {
	primary := func(identity smartnode.NodeIdentity, unassessed []string) smartnode.CoreCapability {
		if identity.Transport.Type == "xhttp" || len(unassessed) > 0 {
			return smartnode.CoreCapability{Status: smartnode.CapabilityDegraded, Reasons: append([]string(nil), unassessed...)}
		}
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported}
	}
	secondary := func(identity smartnode.NodeIdentity, unassessed []string) smartnode.CoreCapability {
		if identity.Protocol == "hysteria2" {
			return smartnode.CoreCapability{Status: smartnode.CapabilityUnsupportedByCore, Reasons: []string{"protocol.unsupported"}}
		}
		if len(unassessed) > 0 {
			return smartnode.CoreCapability{Status: smartnode.CapabilityDegraded, Reasons: append([]string(nil), unassessed...)}
		}
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported}
	}
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: "core-a", Evaluate: primary},
		{Core: "core-b", Evaluate: secondary},
	})
}
