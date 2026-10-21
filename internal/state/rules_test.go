package state

import (
	"fmt"
	"testing"
)

func TestGeoDefaultAdBlockUsesThreeSwitchesWithoutLegacySetList(t *testing.T) {
	for _, master := range []bool{false, true} {
		for _, ad := range []bool{false, true} {
			for _, item := range []bool{false, true} {
				t.Run(fmt.Sprintf("master=%t/ad=%t/item=%t", master, ad, item), func(t *testing.T) {
					settings := DefaultRemoteRuleSettings()
					settings.Enabled = master
					settings.AdBlock.Enabled = ad
					settings.AdBlock.Sets = []string{"unrelated-legacy-id"}
					for i := range settings.Sets {
						if settings.Sets[i].ID == "geosite-category-ads" {
							settings.Sets[i].Enabled = item
						}
					}
					for _, rule := range DesiredRuleCatalog(SpecState{RemoteRules: settings}) {
						if rule.ID == "geo-default-geosite-category-ads" {
							if rule.Enabled != (master && ad && item) || rule.PoolID != "block" {
								t.Fatalf("unexpected adblock route: %+v", rule)
							}
							return
						}
					}
					t.Fatal("adblock rule missing from desired catalog")
				})
			}
		}
	}
}
