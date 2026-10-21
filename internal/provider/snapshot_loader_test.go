package provider

import (
	"errors"
	"os"
	"testing"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func TestSnapshotLastGoodLoaderLoadsCapturedProvider(t *testing.T) {
	source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{
		ProviderID: "provider-a",
		RawTag:     "node-a",
		Outbound:   map[string]any{"type": "direct", "tag": "node-a", "server": "example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	loader, err := NewSnapshotLastGoodLoader([]state.LastGoodSnapshot{{
		ProviderID: "provider-a",
		Identities: []smartnode.IdentitySource{source},
	}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := loader.Load("provider-a")
	if err != nil {
		t.Fatal(err)
	}
	if env.ProviderID != "provider-a" || env.Count != 1 || env.Identities[0].RawTag != "node-a" {
		t.Fatalf("unexpected snapshot envelope: %+v", env)
	}
	if _, err := loader.Load("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing provider error = %v, want os.ErrNotExist", err)
	}
}

func TestSnapshotLastGoodLoaderRejectsDuplicateProvider(t *testing.T) {
	_, err := NewSnapshotLastGoodLoader([]state.LastGoodSnapshot{{ProviderID: "provider-a"}, {ProviderID: "provider-a"}})
	if err == nil {
		t.Fatal("expected duplicate provider snapshot to fail")
	}
}
