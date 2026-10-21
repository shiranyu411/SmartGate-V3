package state

import "testing"

func TestEffectiveRemoteRuleSettingsPreservesEnabledFalse(t *testing.T) {
	got := EffectiveRemoteRuleSettings(RemoteRuleSettings{Enabled: false})
	if got.Enabled {
		t.Fatalf("enabled:false input must not become default Enabled:true, got %+v", got)
	}
	// Catalog defaults should still fill so callers keep a usable set list.
	if len(got.Sets) == 0 {
		t.Fatalf("expected default sets catalog when bare disable, got empty")
	}
}

func TestEffectiveRemoteRuleSettingsEmptyEnabledTrueUsesDefaults(t *testing.T) {
	got := EffectiveRemoteRuleSettings(RemoteRuleSettings{Enabled: true})
	if !got.Enabled {
		t.Fatalf("enabled:true bare input should keep defaults on")
	}
	if got.DefaultFormat == "" || len(got.Sets) == 0 {
		t.Fatalf("expected full defaults, got %+v", got)
	}
}
