package smartnode

import "fmt"

type CoreEvaluator struct {
	Core     CoreID
	Default  bool
	Evaluate func(NodeIdentity, []string) CoreCapability
}

var coreEvaluators []CoreEvaluator

func ConfigureCoreEvaluators(evaluators []CoreEvaluator) {
	coreEvaluators = evaluators
}

// DefaultCoreChoice is product policy supplied by the composition root. It
// never replaces an explicit choice or chooses a core by registry order.
func DefaultCoreChoice() CoreID {
	for _, evaluator := range coreEvaluators {
		if evaluator.Default {
			return evaluator.Core
		}
	}
	return ""
}

// WithDefaultCoreChoices fills missing choices without mutating the caller's
// map. Reconciliation, persistent Spec and recovered captures share this rule.
func WithDefaultCoreChoices(nodes map[string]SmartNode) (map[string]SmartNode, bool) {
	core := DefaultCoreChoice()
	if core == "" {
		return nodes, false
	}
	var out map[string]SmartNode
	for id, node := range nodes {
		if node.CoreChoice != "" {
			continue
		}
		if out == nil {
			out = make(map[string]SmartNode, len(nodes))
			for key, existing := range nodes {
				out[key] = existing
			}
		}
		node.CoreChoice = core
		out[id] = node
	}
	if out == nil {
		return nodes, false
	}
	return out, true
}

func RegisteredCoreIDs() []CoreID {
	cores := make([]CoreID, 0, len(coreEvaluators))
	for _, evaluator := range coreEvaluators {
		cores = append(cores, evaluator.Core)
	}
	return cores
}

func ValidCore(core CoreID) bool {
	for _, evaluator := range coreEvaluators {
		if evaluator.Core == core {
			return true
		}
	}
	return false
}

func EvaluateCapability(identity NodeIdentity, unassessed []string) (CapabilityMatrix, error) {
	if len(coreEvaluators) == 0 {
		return nil, fmt.Errorf("smartnode core evaluators are not configured")
	}
	matrix := make(CapabilityMatrix, len(coreEvaluators))
	for _, evaluator := range coreEvaluators {
		matrix[evaluator.Core] = evaluator.Evaluate(identity, unassessed)
	}
	return matrix, nil
}
