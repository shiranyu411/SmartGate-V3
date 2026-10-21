package state

import (
	"encoding/json"
	"os"
	"testing"

	"smartgate/internal/smartnode"
)

func TestDefaultCoreMigrationPersistsAndPreservesExplicitChoices(t *testing.T) {
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{{Core: "primary", Default: true}})
	t.Cleanup(func() { smartnode.ConfigureCoreEvaluators(nil) })
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	spec := DefaultSpecState()
	spec.SmartNodes = map[string]smartnode.SmartNode{
		"old":      {ID: "old", Revision: 3},
		"explicit": {ID: "explicit", Revision: 2, CoreChoice: "secondary"},
	}
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(layout.SpecPath("spec"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(layout)
	if err := store.PersistDefaultCoreChoices(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(layout.SpecPath("spec"))
	if err != nil {
		t.Fatal(err)
	}
	var saved SpecState
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.SmartNodes["old"].CoreChoice != "primary" || saved.SmartNodes["explicit"].CoreChoice != "secondary" || saved.SmartNodes["old"].Revision != 3 {
		t.Fatalf("incorrect persisted migration: %+v", saved.SmartNodes)
	}
	if err := store.PersistDefaultCoreChoices(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(layout.SpecPath("spec"))
	if string(after) != string(raw) {
		t.Fatal("repeated migration changed saved config")
	}
	saved.SmartNodes["later"] = smartnode.SmartNode{ID: "later", Revision: 1}
	if _, err := store.SaveSpec(saved); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(layout).LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.SmartNodes["later"].CoreChoice != "primary" {
		t.Fatal("save/reload lost default choice")
	}
}
