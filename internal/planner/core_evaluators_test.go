package planner

import (
	"smartgate/internal/coredesc"
	"smartgate/internal/smartnode"
)

var testPrimaryCore smartnode.CoreID
var testSecondaryCore smartnode.CoreID

func init() {
	primary, secondary := plannerTestDescriptors()
	testPrimaryCore = primary.ID
	testSecondaryCore = secondary.ID
	registry := coredesc.NewRegistry()
	if err := registry.Register(primary); err != nil {
		panic(err)
	}
	if err := registry.Register(secondary); err != nil {
		panic(err)
	}
	ConfigureCoreRegistry(registry)
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: primary.ID, Evaluate: primary.Capability.Evaluator},
		{Core: secondary.ID, Evaluate: secondary.Capability.Evaluator},
	})
}

func plannerTestDescriptors() (coredesc.Descriptor, coredesc.Descriptor) {
	supported := func(smartnode.NodeIdentity, []string) smartnode.CoreCapability {
		return smartnode.CoreCapability{Status: smartnode.CapabilitySupported}
	}
	return coredesc.Descriptor{
			ID: "sing-box", DisplayName: "Primary", Control: coredesc.Control{Kind: "clash", Listen: "127.0.0.1", Port: 9090},
			Ports: coredesc.Ports{TProxyBase: 16000, DNSBase: 18000}, Resources: coredesc.Resources{EstimatedRSSKB: 32 * 1024},
			Capability: coredesc.Capability{Evaluator: supported}, Naming: coredesc.Naming{LegacyWorkerAlias: "w_sb_1"},
		}, coredesc.Descriptor{
			ID: "xray", DisplayName: "Secondary", Control: coredesc.Control{Kind: "grpc", Listen: "127.0.0.1", Port: 10085},
			Ports: coredesc.Ports{TProxyBase: 17000, DNSBase: 19000}, Resources: coredesc.Resources{EstimatedRSSKB: 40 * 1024},
			Capability: coredesc.Capability{Evaluator: supported}, Naming: coredesc.Naming{LegacyWorkerAlias: "w_xr_1"},
		}
}
