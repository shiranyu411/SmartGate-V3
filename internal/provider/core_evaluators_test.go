package provider

import "smartgate/internal/smartnode"

var providerTestCores = []smartnode.CoreID{"sing-box", "xray"}

func init() {
	evaluator := func(protocols ...string) func(smartnode.NodeIdentity, []string) smartnode.CoreCapability {
		supported := make(map[string]struct{}, len(protocols))
		for _, protocol := range protocols {
			supported[protocol] = struct{}{}
		}
		return func(identity smartnode.NodeIdentity, _ []string) smartnode.CoreCapability {
			if _, exists := supported[identity.Protocol]; exists {
				return smartnode.CoreCapability{Status: smartnode.CapabilitySupported, Reasons: []string{}, ReasonTexts: []string{}}
			}
			return smartnode.CoreCapability{Status: smartnode.CapabilityUnsupportedByCore, Reasons: []string{"protocol_not_supported"}, ReasonTexts: []string{"protocol not supported"}}
		}
	}
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: providerTestCores[0], Evaluate: evaluator("anytls", "shadowsocks", "vmess", "vless", "trojan", "hysteria", "hysteria2", "tuic", "socks", "http")},
		{Core: providerTestCores[1], Evaluate: evaluator("shadowsocks", "vmess", "vless", "trojan", "socks", "http")},
	})
}
