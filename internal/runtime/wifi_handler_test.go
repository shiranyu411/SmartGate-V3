package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestWiFiConfigReadParsesUCISections(t *testing.T) {
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"uci", "show", "wireless"}): {output: strings.Join([]string{
			"wireless.radio0=wifi-device",
			"wireless.radio0.type='mac80211'",
			"wireless.radio0.band='2g'",
			"wireless.radio0.channel='1'",
			"wireless.radio0.htmode='HE20'",
			"wireless.radio0.country='CN'",
			"wireless.radio0.disabled='0'",
			"wireless.default_radio0=wifi-iface",
			"wireless.default_radio0.device='radio0'",
			"wireless.default_radio0.network='lan'",
			"wireless.default_radio0.mode='ap'",
			"wireless.default_radio0.ssid='SmartGate'",
			"wireless.default_radio0.encryption='psk2'",
			"wireless.default_radio0.key='secret-not-for-prod'",
			"wireless.default_radio0.disabled='0'",
			"wireless.radio1=wifi-device",
			"wireless.radio1.band='5g'",
			"wireless.radio1.channel='36'",
			"wireless.default_radio1=wifi-iface",
			"wireless.default_radio1.device='radio1'",
			"wireless.default_radio1.ssid='SmartGate-5G'",
			"wireless.default_radio1.encryption='psk2'",
			"wireless.default_radio1.mode='ap'",
		}, "\n")},
	}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodGet, "/api/wifi/config", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("wifi config = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if _, exists := data["sections"]; exists || strings.Contains(resp.Body.String(), "secret-not-for-prod") {
		t.Fatalf("wifi config leaked key: %s", resp.Body.String())
	}

	networks := data["networks"].([]any)
	if len(networks) != 2 {
		t.Fatalf("expected 2 networks, got %+v", networks)
	}
	n0 := networks[0].(map[string]any)
	if n0["ssid"] != "SmartGate" || n0["band"] != "2g" || n0["device"] != "radio0" {
		t.Fatalf("unexpected network[0]: %+v", n0)
	}
	if n0["key_configured"] != true {
		t.Fatalf("wifi key configured state missing: %+v", n0)
	}
	devices := data["devices"].([]any)
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %+v", devices)
	}
}

func TestWiFiConfigWriteRequiresConfirmAndRunsUCICommands(t *testing.T) {
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodPut, "/api/wifi/config", mustJSON(t, wifiConfigPayload{
		Section: "default_radio0",
		SSID:    "SmartGate",
	}))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected confirm failure, got %d body=%s", resp.Code, resp.Body.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("wifi config should not run without confirm: %+v", runner.calls)
	}

	disabled := false
	resp = serve(t, server, http.MethodPut, "/api/wifi/config", mustJSON(t, wifiConfigPayload{
		Confirm:    true,
		Section:    "default_radio0",
		SSID:       "SmartGate",
		Encryption: "psk2",
		Key:        "password123",
		Disabled:   &disabled,
	}))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("wifi config apply = %d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "password123") || strings.Contains(resp.Body.String(), "argv") || strings.Contains(resp.Body.String(), "output") {
		t.Fatalf("wifi apply response leaked command data: %s", resp.Body.String())
	}
	want := [][]string{
		{"uci", "set", "wireless.default_radio0.ssid=SmartGate"},
		{"uci", "set", "wireless.default_radio0.encryption=psk2"},
		{"uci", "set", "wireless.default_radio0.key=password123"},
		{"uci", "set", "wireless.default_radio0.disabled=0"},
		{"uci", "commit", "wireless"},
		{"wifi", "reload"},
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("unexpected command count: got %+v want %+v", runner.calls, want)
	}
	for i := range want {
		if systemTestKey(runner.calls[i]) != systemTestKey(want[i]) {
			t.Fatalf("command %d = %+v want %+v", i, runner.calls[i], want[i])
		}
	}
}

func TestWiFiStatusReturnsRadiosAndInterfaces(t *testing.T) {
	statusJSON := `{"radio0":{"up":true,"pending":false,"disabled":false,"config":{"band":"2g","channel":"1","htmode":"HE20","country":"CN","txpower":20},"interfaces":[{"section":"default_radio0","ifname":"phy0-ap0","config":{"ssid":"example-wifi","mode":"ap","encryption":"psk2","network":["lan"]}}]},"radio1":{"up":true,"pending":false,"disabled":false,"config":{"band":"5g","channel":"36","htmode":"HE80","country":"CN","txpower":20},"interfaces":[{"section":"default_radio1","ifname":"phy1-ap0","config":{"ssid":"example-wifi","mode":"ap","encryption":"psk2","network":["lan"]}}]}}`
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"ubus", "call", "network.wireless", "status"}): {output: statusJSON},
	}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodGet, "/api/wifi/status", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("wifi status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	radios := data["radios"].([]any)
	ifaces := data["interfaces"].([]any)
	if len(radios) != 2 {
		t.Fatalf("expected 2 radios, got %+v", radios)
	}
	if len(ifaces) != 2 {
		t.Fatalf("expected 2 interfaces, got %+v", ifaces)
	}
	iface0 := ifaces[0].(map[string]any)
	if iface0["ifname"] != "phy0-ap0" || iface0["ssid"] != "example-wifi" || iface0["band"] != "2g" {
		t.Fatalf("unexpected iface0: %+v", iface0)
	}
	radio1 := radios[1].(map[string]any)
	if radio1["id"] != "radio1" || radio1["up"] != true {
		t.Fatalf("unexpected radio1: %+v", radio1)
	}
}

func TestWiFiScanAutoPicksDiscoveredInterface(t *testing.T) {
	statusJSON := `{"radio0":{"up":true,"config":{"band":"2g","channel":"1"},"interfaces":[{"section":"default_radio0","ifname":"phy0-ap0","config":{"ssid":"example-wifi","mode":"ap"}}]}}`
	scanOut := `Cell 01 - Address: 02:00:00:00:00:0C
          ESSID: "Cafe"
          Mode: Master  Channel: 6
          Signal: -42 dBm  Quality: 60/70
          Encryption: WPA2 PSK (CCMP)
Cell 02 - Address: 02:00:00:00:00:0a
          ESSID: "WeakNet"
          Mode: Master  Channel: 11
          Signal: -80 dBm  Quality: 20/70
          Encryption: mixed WPA/WPA2 PSK (TKIP, CCMP)`
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"ubus", "call", "network.wireless", "status"}): {output: statusJSON},
		systemTestKey([]string{"iwinfo", "phy0-ap0", "scan"}):                 {output: scanOut},
	}}
	server := newSystemTestServer(t, runner)

	// empty interface / default wlan0 should auto-resolve
	for _, body := range []any{
		map[string]any{},
		map[string]any{"interface": "auto"},
		map[string]any{"interface": "wlan0"},
	} {
		resp := serve(t, server, http.MethodPost, "/api/wifi/scan", mustJSON(t, body))
		if resp.Code != http.StatusOK {
			t.Fatalf("wifi scan = %d body=%s payload=%+v", resp.Code, resp.Body.String(), body)
		}
		data := decodeEnvelopeData(t, resp.Body.Bytes())
		if data["interface"] != "phy0-ap0" {
			t.Fatalf("expected auto iface phy0-ap0, got %+v for payload %+v", data["interface"], body)
		}
		networks := data["networks"].([]any)
		if len(networks) != 2 {
			t.Fatalf("expected 2 networks sorted, got %+v", networks)
		}
		// stronger signal first
		if networks[0].(map[string]any)["ssid"] != "Cafe" {
			t.Fatalf("expected Cafe first by signal, got %+v", networks)
		}
	}
}

func TestWiFiScanAndClientsReturnStructuredViews(t *testing.T) {
	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"iwinfo", "wlan1", "scan"}): {output: `Cell 01 - Address: 02:00:00:00:00:0C
          ESSID: "Cafe"
          Mode: Master  Channel: 6
          Signal: -42 dBm  Quality: 60/70
          Encryption: WPA2 PSK (CCMP)`},
		systemTestKey([]string{"ubus", "call", "network.wireless", "status"}):   {output: `{"radio0":{"up":true,"config":{"band":"5g"},"interfaces":[{"section":"default_radio0","ifname":"wlan1","config":{"ssid":"Lab"}}]}}`},
		systemTestKey([]string{"ubus", "call", "hostapd.wlan1", "get_clients"}): {err: errors.New("Object not found")},
		systemTestKey([]string{"iwinfo", "wlan1", "assoclist"}):                 {output: "02:00:00:00:00:0a  -50 dBm / -95 dBm (SNR 45)  10 ms ago\n"},
		systemTestKey([]string{"cat", "/tmp/dhcp.leases"}):                      {output: "1710000000 02:00:00:00:00:0a 192.168.1.50 phone *\n"},
	}}
	server := newSystemTestServer(t, runner)

	scanResp := serve(t, server, http.MethodPost, "/api/wifi/scan", mustJSON(t, wifiInterfacePayload{Interface: "wlan1"}))
	if scanResp.Code != http.StatusOK {
		t.Fatalf("wifi scan = %d body=%s", scanResp.Code, scanResp.Body.String())
	}
	scanData := decodeEnvelopeData(t, scanResp.Body.Bytes())
	networks := scanData["networks"].([]any)
	if len(networks) != 1 || networks[0].(map[string]any)["ssid"] != "Cafe" {
		t.Fatalf("unexpected scan networks: %+v", networks)
	}

	clientsResp := serve(t, server, http.MethodGet, "/api/wifi/clients?interface=wlan1", nil)
	if clientsResp.Code != http.StatusOK {
		t.Fatalf("wifi clients = %d body=%s", clientsResp.Code, clientsResp.Body.String())
	}
	clientsData := decodeEnvelopeData(t, clientsResp.Body.Bytes())
	clients := clientsData["clients"].([]any)
	if len(clients) != 1 {
		t.Fatalf("unexpected clients: %+v", clients)
	}
	c0 := clients[0].(map[string]any)
	if !strings.EqualFold(c0["mac"].(string), "02:00:00:00:00:0a") {
		t.Fatalf("unexpected clients: %+v", clients)
	}
	if c0["hostname"] != "phone" || c0["ip"] != "192.168.1.50" {
		t.Fatalf("expected dhcp enrich, got %+v", c0)
	}
	if int(c0["signal_dbm"].(float64)) != -50 {
		t.Fatalf("expected signal -50, got %+v", c0["signal_dbm"])
	}
	if c0["ssid"] != "Lab" || c0["interface"] != "wlan1" {
		t.Fatalf("expected ssid/interface enrichment: %+v", c0)
	}
}

func TestWiFiClientsAllInterfacesPreferHostapd(t *testing.T) {
	statusJSON := `{"radio0":{"up":true,"config":{"band":"2g","channel":"1"},"interfaces":[{"section":"default_radio0","ifname":"phy0-ap0","config":{"ssid":"example-wifi","mode":"ap"}}]},"radio1":{"up":true,"config":{"band":"5g","channel":"36"},"interfaces":[{"section":"default_radio1","ifname":"phy1-ap0","config":{"ssid":"example-wifi","mode":"ap"}}]}}`
	hostapd0 := `{"freq":2412,"clients":{"02:00:00:00:00:0d":{"auth":true,"assoc":true,"authorized":true,"signal":-48,"rate":{"rx":144400,"tx":86670},"bytes":{"rx":1200,"tx":3400}}}}`
	hostapd1 := `{"freq":5180,"clients":{"02:00:00:00:00:0b":{"auth":true,"assoc":true,"authorized":true,"signal":-55,"rate":{"rx":200000,"tx":150000},"bytes":{"rx":900,"tx":800}}}}`
	leases := "1710000000 02:00:00:00:00:0d 192.168.1.191 midea_ac_0403 *\n1710000001 02:00:00:00:00:0b 192.168.1.20 laptop *\n"

	runner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"ubus", "call", "network.wireless", "status"}):      {output: statusJSON},
		systemTestKey([]string{"ubus", "call", "hostapd.phy0-ap0", "get_clients"}): {output: hostapd0},
		systemTestKey([]string{"ubus", "call", "hostapd.phy1-ap0", "get_clients"}): {output: hostapd1},
		systemTestKey([]string{"cat", "/tmp/dhcp.leases"}):                         {output: leases},
	}}
	server := newSystemTestServer(t, runner)

	resp := serve(t, server, http.MethodGet, "/api/wifi/clients", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("wifi clients all = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if data["interface"] != "all" {
		t.Fatalf("expected interface=all default, got %+v", data["interface"])
	}
	ifaces, _ := data["interfaces"].([]any)
	if len(ifaces) != 2 {
		t.Fatalf("expected scanned interfaces phy0/phy1, got %+v", ifaces)
	}
	clients := data["clients"].([]any)
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients across bands, got %+v", clients)
	}
	// stronger signal first (-48 before -55)
	first := clients[0].(map[string]any)
	if first["mac"] != "02:00:00:00:00:0d" || first["hostname"] != "midea_ac_0403" || first["ip"] != "192.168.1.191" {
		t.Fatalf("unexpected first client: %+v", first)
	}
	if first["interface"] != "phy0-ap0" || first["ssid"] != "example-wifi" {
		t.Fatalf("expected iface/ssid on client: %+v", first)
	}
	if int(first["signal_dbm"].(float64)) != -48 {
		t.Fatalf("signal: %+v", first["signal_dbm"])
	}
	// ensure hostapd rates were parsed
	if int64(first["rx_rate"].(float64)) != 144400 {
		t.Fatalf("rx_rate: %+v", first["rx_rate"])
	}

	// empty hostapd still ok with zero clients on one radio
	emptyRunner := &fakeSystemRunner{responses: map[string]fakeSystemResponse{
		systemTestKey([]string{"ubus", "call", "network.wireless", "status"}): {output: `{}`},
		systemTestKey([]string{"cat", "/tmp/dhcp.leases"}):                    {output: ""},
	}}
	emptyServer := newSystemTestServer(t, emptyRunner)
	emptyResp := serve(t, emptyServer, http.MethodGet, "/api/wifi/clients?interface=all", nil)
	if emptyResp.Code != http.StatusOK {
		t.Fatalf("empty clients = %d", emptyResp.Code)
	}
	emptyData := decodeEnvelopeData(t, emptyResp.Body.Bytes())
	if len(emptyData["clients"].([]any)) != 0 {
		t.Fatalf("expected empty clients: %+v", emptyData)
	}
}

func TestParseWirelessStatusAndHostapdHelpers(t *testing.T) {
	raw := `{"radio0":{"up":true,"config":{"band":"2g","channel":"6","txpower":18},"interfaces":[{"section":"default_radio0","ifname":"phy0-ap0","config":{"ssid":"Home","mode":"ap","encryption":"psk2","disabled":false,"network":"lan"}}]}}`
	radios, ifaces, parsed := parseWirelessStatus(raw)
	if parsed == nil || len(radios) != 1 || len(ifaces) != 1 {
		t.Fatalf("parseWirelessStatus failed: radios=%+v ifaces=%+v", radios, ifaces)
	}
	if ifaces[0].Ifname != "phy0-ap0" || ifaces[0].Network != "lan" {
		t.Fatalf("unexpected iface parse: %+v", ifaces[0])
	}

	hostapd := `{"clients":{"02:00:00:00:00:0C":{"signal":-40,"authorized":true,"rate":{"rx":1,"tx":2},"bytes":{"rx":3,"tx":4}}}}`
	leases := parseDHCPLeases("1 02:00:00:00:00:0c 10.0.0.2 host *\n")
	clients := parseHostapdClients(hostapd, "phy0-ap0", "Home", leases)
	if len(clients) != 1 || clients[0].Hostname != "host" || clients[0].IP != "10.0.0.2" || clients[0].SignalDBM != -40 {
		t.Fatalf("hostapd parse: %+v", clients)
	}

	// ensure JSON round-trip of status view fields stays stable for FE
	view := wifiStatusView{Radios: radios, Interfaces: ifaces}
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ifname":"phy0-ap0"`) {
		t.Fatalf("json missing ifname: %s", b)
	}
}
