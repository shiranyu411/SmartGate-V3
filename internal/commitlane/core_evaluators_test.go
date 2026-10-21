package commitlane

import (
	"smartgate/internal/coredesc"
	"smartgate/internal/planner"
	"smartgate/internal/smartnode"
)

func configureTestCoreEvaluators() {
	primary, secondary := commitTestDescriptors()
	registry := coredesc.NewRegistry()
	if err := registry.Register(primary); err != nil {
		panic(err)
	}
	if err := registry.Register(secondary); err != nil {
		panic(err)
	}
	planner.ConfigureCoreRegistry(registry)
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: primary.ID, Evaluate: primary.Capability.Evaluator},
		{Core: secondary.ID, Evaluate: secondary.Capability.Evaluator},
	})
}

func commitTestDescriptors() (coredesc.Descriptor, coredesc.Descriptor) {
	primaryEvaluator := func(identity smartnode.NodeIdentity, unassessed []string) smartnode.CoreCapability {
		if identity.Transport.Type == "xhttp" || len(unassessed) > 0 {
			return smartnode.CoreCapability{Status: smartnode.CapabilityDegraded, Reasons: append([]string(nil), unassessed...)}
		}
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported}
	}
	secondaryEvaluator := func(identity smartnode.NodeIdentity, unassessed []string) smartnode.CoreCapability {
		if identity.Protocol == "hysteria2" {
			return smartnode.CoreCapability{Status: smartnode.CapabilityUnsupportedByCore, Reasons: []string{"protocol.unsupported"}}
		}
		if len(unassessed) > 0 {
			return smartnode.CoreCapability{Status: smartnode.CapabilityDegraded, Reasons: append([]string(nil), unassessed...)}
		}
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported}
	}
	return coredesc.Descriptor{
			ID: "sing-box", DisplayName: "Primary", Control: coredesc.Control{Kind: "clash", Listen: "127.0.0.1", Port: 9090},
			Ports: coredesc.Ports{TProxyBase: 16000, DNSBase: 18000}, Resources: coredesc.Resources{EstimatedRSSKB: 32 * 1024},
			Capability: coredesc.Capability{Evaluator: primaryEvaluator}, Naming: coredesc.Naming{LegacyWorkerAlias: "w_sb_1"},
		}, coredesc.Descriptor{
			ID: "xray", DisplayName: "Secondary", Control: coredesc.Control{Kind: "grpc", Listen: "127.0.0.1", Port: 10085},
			Ports: coredesc.Ports{TProxyBase: 17000, DNSBase: 19000}, Resources: coredesc.Resources{EstimatedRSSKB: 40 * 1024},
			Capability: coredesc.Capability{Evaluator: secondaryEvaluator}, Naming: coredesc.Naming{LegacyWorkerAlias: "w_xr_1"},
		}
}

func init() {
	configureTestCoreEvaluators()
}
