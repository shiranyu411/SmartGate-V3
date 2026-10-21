package smartnode

import (
	"testing"
	"time"
)

func TestDefaultCoreSurvivesCatalogRefreshWithoutReplacingExplicitChoice(t *testing.T) {
	previous := coreEvaluators
	t.Cleanup(func() { ConfigureCoreEvaluators(previous) })
	ConfigureCoreEvaluators([]CoreEvaluator{
		{Core: matrixCoreA, Default: true, Evaluate: func(NodeIdentity, []string) CoreCapability { return CoreCapability{Status: CapabilitySupported} }},
		{Core: matrixCoreB, Evaluate: func(NodeIdentity, []string) CoreCapability { return CoreCapability{Status: CapabilitySupported} }},
	})
	now := time.Now().UTC()
	first, _, err := Reconcile(nil, []RawNode{rawVLESS("p1", "node", "first.example", "fixture-secret")}, now)
	if err != nil {
		t.Fatal(err)
	}
	id := onlyID(t, first)
	if first[id].CoreChoice != matrixCoreA {
		t.Fatalf("new node lacks default: %+v", first[id])
	}
	node := first[id]
	node.CoreChoice = matrixCoreB
	first[id] = node
	revised, _, err := Reconcile(first, []RawNode{rawVLESS("p1", "node", "changed.example", "fixture-secret")}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if revised[id].CoreChoice != matrixCoreB || revised[id].Revision != 2 {
		t.Fatalf("refresh changed explicit choice: %+v", revised[id])
	}
	input := IdentityInput{ProviderID: "p2", RawTag: "new-ir", Identity: NodeIdentity{Protocol: "socks", Server: "ir.example", Port: 1080}}
	imported, _, err := ReconcileIdentityInputs(nil, []IdentityInput{input}, now)
	if err != nil {
		t.Fatal(err)
	}
	if imported[onlyID(t, imported)].CoreChoice != matrixCoreA {
		t.Fatal("IR import lacks default")
	}
	node = revised[id]
	node.CoreChoice = ""
	revised[id] = node
	filled, changed := WithDefaultCoreChoices(revised)
	if !changed || filled[id].CoreChoice != matrixCoreA || revised[id].CoreChoice != "" {
		t.Fatal("legacy defaults missing or input mutated")
	}
}
