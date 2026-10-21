package scheduler

import (
	"smartgate/internal/smartnode"
)

func init() {
	supported := func(identity smartnode.NodeIdentity, _ []string) smartnode.CoreCapability {
		if identity.Protocol == "naive" {
			return smartnode.CoreCapability{Status: smartnode.CapabilityUnsupportedByCore}
		}
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported}
	}
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: "sing-box", Evaluate: supported},
		{Core: "xray", Evaluate: supported},
	})
}
