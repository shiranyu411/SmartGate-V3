package worker

import (
	"fmt"
	"strings"

	"smartgate/internal/smartnode"
)

type Descriptor struct {
	ID          smartnode.CoreID
	DisplayName string
	Version     string
	Control     DescriptorControl
	Ports       DescriptorPorts
	Resources   DescriptorResources
	Process     DescriptorProcess
	Capability  DescriptorCapability
	Naming      DescriptorNaming
	ConfigDirs  []string
}

type DescriptorControl struct {
	Kind   string
	Listen string
	Port   int
}

type DescriptorPorts struct {
	TProxyBase int
	DNSBase    int
}

type DescriptorResources struct {
	EstimatedRSSKB int
}

type DescriptorProcess struct {
	Binary      string
	StartArgs   func(configPath string) []string
	CheckArgs   func(configPath string) []string
	ServiceUnit string
	ProcessName string
}

type CapabilityEvaluator func(smartnode.NodeIdentity, []string) smartnode.CoreCapability

type DescriptorCapability struct {
	Evaluator   CapabilityEvaluator
	ReasonTexts map[string]string
}

type DescriptorNaming struct {
	LegacyWorkerAlias string
}

func (descriptor Descriptor) WorkerID(index int) string {
	if index == 1 && descriptor.Naming.LegacyWorkerAlias != "" {
		return descriptor.Naming.LegacyWorkerAlias
	}
	return fmt.Sprintf("w_%s_%d", sanitizeCoreID(descriptor.ID), index)
}

func sanitizeCoreID(core smartnode.CoreID) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(string(core)) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' {
			builder.WriteRune(character)
			continue
		}
		builder.WriteByte('_')
	}
	return strings.Trim(builder.String(), "_")
}
