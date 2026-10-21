package worker

import "smartgate/internal/smartnode"

func init() {
	supported := func(smartnode.NodeIdentity, []string) smartnode.CoreCapability {
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported, Reasons: []string{}, ReasonTexts: []string{}}
	}
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: smartnode.CoreID("sing-box"), Evaluate: supported},
		{Core: smartnode.CoreID("xray"), Evaluate: supported},
	})
}
