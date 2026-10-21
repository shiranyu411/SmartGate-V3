package singbox_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"smartgate/internal/smartnode"
	"smartgate/internal/worker/singbox"
	"smartgate/internal/worker/xray"
)

func TestCapabilityMatrixJSONMatchesBaselineFixtures(t *testing.T) {
	primary := (&singbox.Adapter{}).Descriptor()
	secondary := (&xray.Adapter{}).Descriptor()
	smartnode.ConfigureCoreEvaluators([]smartnode.CoreEvaluator{
		{Core: primary.ID, Evaluate: primary.Capability.Evaluator},
		{Core: secondary.ID, Evaluate: secondary.Capability.Evaluator},
	})
	defer smartnode.ConfigureCoreEvaluators(nil)

	fixtures := []struct {
		name      string
		raw       smartnode.RawNode
		primary   smartnode.CoreCapability
		secondary smartnode.CoreCapability
	}{
		{
			name: "vless_xhttp",
			raw: smartnode.RawNode{ProviderID: "p1", RawTag: "node", Outbound: map[string]any{
				"type": "vless", "tag": "node", "server": "node.example", "server_port": 443, "uuid": "secret",
				"transport": map[string]any{"type": "xhttp", "path": "/api"},
			}},
			primary: smartnode.CoreCapability{
				Status:      smartnode.CapabilityDegraded,
				Reasons:     []string{"transport.xhttp_semantics_incomplete"},
				ReasonTexts: []string{"sing-box 无法完整表达该 XHTTP 传输语义"},
			},
			secondary: smartnode.CoreCapability{
				Status:      smartnode.CapabilitySupported,
				Reasons:     []string{},
				ReasonTexts: []string{},
			},
		},
		{
			name: "hysteria2",
			raw: smartnode.RawNode{ProviderID: "p1", RawTag: "hy2", Outbound: map[string]any{
				"type": "hysteria2", "tag": "hy2", "server": "hy.example", "server_port": 443, "password": "secret",
			}},
			primary: smartnode.CoreCapability{
				Status:      smartnode.CapabilitySupported,
				Reasons:     []string{},
				ReasonTexts: []string{},
			},
			secondary: smartnode.CoreCapability{
				Status:      smartnode.CapabilityUnsupportedByCore,
				Reasons:     []string{"protocol.hysteria2_not_supported"},
				ReasonTexts: []string{"xray 无法完整执行: protocol.hysteria2_not_supported"},
			},
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			node, err := smartnode.Normalize(fixture.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := node.Capability[primary.ID]; !reflect.DeepEqual(got, fixture.primary) {
				t.Fatalf("primary capability changed\ngot=%+v\nwant=%+v", got, fixture.primary)
			}
			if got := node.Capability[secondary.ID]; !reflect.DeepEqual(got, fixture.secondary) {
				t.Fatalf("secondary capability changed\ngot=%+v\nwant=%+v", got, fixture.secondary)
			}

			legacy := struct {
				Primary   smartnode.CoreCapability `json:"sing-box"`
				Secondary smartnode.CoreCapability `json:"xray"`
			}{Primary: fixture.primary, Secondary: fixture.secondary}
			legacyJSON, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			currentJSON, err := json.Marshal(node.Capability)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(currentJSON, legacyJSON) {
				t.Fatalf("capability JSON changed\ncurrent=%s\nlegacy=%s", currentJSON, legacyJSON)
			}
		})
	}
}
