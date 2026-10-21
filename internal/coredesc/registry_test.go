package coredesc

import (
	"testing"

	"smartgate/internal/smartnode"
)

func TestRegistryRejectsDuplicateRegistration(t *testing.T) {
	registry := NewRegistry()
	descriptor := testDescriptor("alpha", 1000)
	if err := registry.Register(descriptor); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(descriptor); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}
}

func TestRegistryRejectsPortOverlap(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testDescriptor("alpha", 1000)); err != nil {
		t.Fatal(err)
	}
	overlap := testDescriptor("beta", 2000)
	overlap.Ports.TProxyBase = 1002
	if err := registry.Register(overlap); err == nil {
		t.Fatal("expected port overlap to fail")
	}
}

func TestRegistryLookupAndListUseStableCoreOrder(t *testing.T) {
	registry := NewRegistry()
	for _, descriptor := range []Descriptor{testDescriptor("beta", 2000), testDescriptor("alpha", 1000)} {
		if err := registry.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	descriptor, exists := registry.Lookup("beta")
	if !exists || descriptor.DisplayName != "beta display" {
		t.Fatalf("unexpected lookup result: exists=%v descriptor=%+v", exists, descriptor)
	}
	listed := registry.List()
	if len(listed) != 2 || listed[0].ID != "alpha" || listed[1].ID != "beta" {
		t.Fatalf("registry list is not stable: %+v", listed)
	}
	if got := listed[0].WorkerID(1); got != listed[0].Naming.LegacyWorkerAlias {
		t.Fatalf("legacy worker alias was not retained: %q", got)
	}
	wantDerived := "w_" + string(listed[0].ID) + "_2"
	if got := listed[0].WorkerID(2); got != wantDerived {
		t.Fatalf("derived worker ID is incorrect: %q", got)
	}
}

func testDescriptor(id smartnode.CoreID, portBase int) Descriptor {
	return Descriptor{
		ID: id, DisplayName: string(id) + " display",
		Control: Control{Port: portBase},
		Ports:   Ports{TProxyBase: portBase + 1, DNSBase: portBase + 2},
		Naming:  Naming{LegacyWorkerAlias: "w_legacy_1"},
	}
}
