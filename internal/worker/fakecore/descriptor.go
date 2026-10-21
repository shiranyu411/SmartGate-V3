package fakecore

import (
	"fmt"
	"sort"
	"strings"

	"smartgate/internal/coredesc"
	"smartgate/internal/smartnode"
)

const CoreID smartnode.CoreID = "fakecore"

func (adapter *Adapter) Descriptor() coredesc.Descriptor {
	reasonTexts := map[string]string{
		"protocol.fooprotocol_not_supported": "FakeCore 未启用 fooprotocol",
	}
	return coredesc.Descriptor{
		ID:          CoreID,
		DisplayName: "FakeCore",
		Version:     "dev-noop-1",
		Control:     coredesc.Control{Kind: "noop", Listen: "127.0.0.1", Port: 11090},
		Ports:       coredesc.Ports{TProxyBase: 21000, DNSBase: 22000},
		Resources:   coredesc.Resources{EstimatedRSSKB: 4 * 1024},
		Process: coredesc.Process{
			Binary:      "/usr/bin/fakecore",
			StartArgs:   func(configPath string) []string { return []string{"run", "-c", configPath} },
			CheckArgs:   func(configPath string) []string { return []string{"check", "-c", configPath} },
			ProcessName: "fakecore",
		},
		Capability: coredesc.Capability{
			Evaluator: func(identity smartnode.NodeIdentity, unassessed []string) smartnode.CoreCapability {
				return evaluateDescriptorCapability(identity, unassessed, reasonTexts)
			},
			ReasonTexts: reasonTexts,
		},
		ConfigDirs: []string{"/etc/smartgate/fakecore"},
	}
}

func evaluateDescriptorCapability(identity smartnode.NodeIdentity, unassessed []string, reasonTexts map[string]string) smartnode.CoreCapability {
	status := smartnode.CapabilitySupported
	reasons := []string{}
	addUnsupported := func(reason string) {
		status = smartnode.CapabilityUnsupportedByCore
		reasons = append(reasons, reason)
	}
	addDegraded := func(reason string) {
		if status == smartnode.CapabilitySupported {
			status = smartnode.CapabilityDegraded
		}
		reasons = append(reasons, reason)
	}

	if !descriptorContains([]string{"fooprotocol", "tuic", "hysteria2", "vless"}, identity.Protocol) {
		addUnsupported("protocol." + descriptorReasonPart(identity.Protocol) + "_not_supported")
	}
	transport := strings.ToLower(identity.Transport.Type)
	if transport == "" {
		transport = "tcp"
	}
	if !descriptorContains([]string{"tcp", "ws", "grpc", "quic"}, transport) {
		addUnsupported("transport." + descriptorReasonPart(transport) + "_not_supported")
	}
	if identity.Security.Type != "none" && identity.Security.Type != "tls" && identity.Security.Type != "reality" {
		addUnsupported("security." + descriptorReasonPart(identity.Security.Type) + "_not_supported")
	}
	for _, reason := range unassessed {
		addDegraded(reason)
	}

	sort.Strings(reasons)
	reasons = uniqueDescriptorReasons(reasons)
	texts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		text, exists := reasonTexts[reason]
		if !exists {
			text = fmt.Sprintf("%s 无法完整执行: %s", CoreID, reason)
		}
		texts = append(texts, text)
	}
	return smartnode.CoreCapability{Status: status, Reasons: reasons, ReasonTexts: texts}
}

func descriptorContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func descriptorReasonPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' {
			builder.WriteRune(character)
			continue
		}
		builder.WriteByte('_')
	}
	return strings.Trim(builder.String(), "_")
}

func uniqueDescriptorReasons(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
