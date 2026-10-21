package commitlane

import (
	"context"
	"errors"
	"os"
	"testing"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/provider"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

func TestLastGoodTxnSaveManyWritesUnderCommitLane(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	txn := NewLastGoodTxn(
		New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")),
		provider.NewLastGoodStore(layout),
	)
	outbounds := []map[string]any{{"type": "vmess", "tag": "node-1", "server": "example.com"}}
	identity, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{ProviderID: "p1", RawTag: "node-1", Outbound: outbounds[0]})
	if err != nil {
		t.Fatal(err)
	}

	saved, err := txn.SaveMany(context.Background(), []provider.LastGoodUpdate{{
		ProviderID: "p1",
		Identities: []smartnode.IdentitySource{identity},
	}})
	if err != nil {
		t.Fatalf("save many: %v", err)
	}
	if saved["p1"].Count != 1 {
		t.Fatalf("unexpected saved envelope: %+v", saved)
	}
	env, err := provider.NewLastGoodStore(layout).Load("p1")
	if err != nil {
		t.Fatalf("load last-good: %v", err)
	}
	if len(env.Outbounds) != 0 || len(env.Identities) != 1 || env.Identities[0].RawTag != "node-1" {
		t.Fatalf("unexpected last-good: %+v", env)
	}
}

func TestLastGoodTxnSaveManyBlocksDuringRescue(t *testing.T) {
	layout := state.Layout{
		DataDir:    t.TempDir(),
		RuntimeDir: t.TempDir(),
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := os.WriteFile(layout.RescueStatusPath(), []byte(`{"active":true}`), 0o644); err != nil {
		t.Fatalf("write rescue flag: %v", err)
	}
	txn := NewLastGoodTxn(
		New(lockset.New(), layout, netgate.NewOwnerRegistry("smartgate")),
		provider.NewLastGoodStore(layout),
	)
	outbound := map[string]any{"type": "vmess", "tag": "node-1", "server": "example.com"}
	identity, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{ProviderID: "p1", RawTag: "node-1", Outbound: outbound})
	if err != nil {
		t.Fatal(err)
	}

	_, err = txn.SaveMany(context.Background(), []provider.LastGoodUpdate{{
		ProviderID: "p1",
		Identities: []smartnode.IdentitySource{identity},
	}})
	if !errors.Is(err, ErrRescueActive) {
		t.Fatalf("expected ErrRescueActive, got %v", err)
	}
	if provider.NewLastGoodStore(layout).Exists("p1") {
		t.Fatal("last-good should not be written during rescue")
	}
}
