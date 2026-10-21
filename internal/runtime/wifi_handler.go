package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"smartgate/internal/netgate"
)

const defaultWiFiInterface = "wlan0"

var safeWiFiSection = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type wifiAPI struct {
	runner netgate.Runner
}

type wifiInterfacePayload struct {
	Interface string `json:"interface"`
}

type wifiConfigPayload struct {
	Confirm    bool   `json:"confirm"`
	Section    string `json:"section"`
	SSID       string `json:"ssid"`
	Encryption string `json:"encryption"`
	Key        string `json:"key"`
	Disabled   *bool  `json:"disabled"`
}

type wifiRadioView struct {
	ID         string          `json:"id"`
	Up         bool            `json:"up"`
	Pending    bool            `json:"pending"`
	Disabled   bool            `json:"disabled"`
	Band       string          `json:"band,omitempty"`
	Channel    string          `json:"channel,omitempty"`
	HTMode     string          `json:"htmode,omitempty"`
	Country    string          `json:"country,omitempty"`
	TxPower    int             `json:"txpower,omitempty"`
	Interfaces []wifiIfaceView `json:"interfaces"`
}

type wifiIfaceView struct {
	Section    string `json:"section"`
	Ifname     string `json:"ifname,omitempty"`
	SSID       string `json:"ssid,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Encryption string `json:"encryption,omitempty"`
	Disabled   bool   `json:"disabled"`
	Network    string `json:"network,omitempty"`
	Band       string `json:"band,omitempty"`
	Channel    string `json:"channel,omitempty"`
	HTMode     string `json:"htmode,omitempty"`
}

type wifiStatusView struct {
	Radios     []wifiRadioView `json:"radios"`
	Interfaces []wifiIfaceView `json:"interfaces"`
}

type wifiConfigNetworkView struct {
	Section       string `json:"section"`
	Device        string `json:"device,omitempty"`
	SSID          string `json:"ssid,omitempty"`
	Mode          string `json:"mode,omitempty"`
	Encryption    string `json:"encryption,omitempty"`
	KeyConfigured bool   `json:"key_configured"`
	Disabled      bool   `json:"disabled"`
	Network       string `json:"network,omitempty"`
	Band          string `json:"band,omitempty"`
	Channel       string `json:"channel,omitempty"`
	HTMode        string `json:"htmode,omitempty"`
	Country       string `json:"country,omitempty"`
}

type wifiConfigDeviceView struct {
	Section  string `json:"section"`
	Band     string `json:"band,omitempty"`
	Channel  string `json:"channel,omitempty"`
	HTMode   string `json:"htmode,omitempty"`
	Country  string `json:"country,omitempty"`
	TxPower  string `json:"txpower,omitempty"`
	Disabled bool   `json:"disabled"`
}

type wifiConfigView struct {
	Networks []wifiConfigNetworkView `json:"networks"`
	Devices  []wifiConfigDeviceView  `json:"devices"`
}

type wifiScanNetwork struct {
	BSSID      string `json:"bssid"`
	SSID       string `json:"ssid"`
	Channel    int    `json:"channel"`
	SignalDBM  int    `json:"signal_dbm"`
	Quality    string `json:"quality"`
	Encryption string `json:"encryption"`
}

type wifiClientView struct {
	MAC        string `json:"mac"`
	Interface  string `json:"interface,omitempty"`
	SSID       string `json:"ssid,omitempty"`
	SignalDBM  int    `json:"signal_dbm"`
	Hostname   string `json:"hostname,omitempty"`
	IP         string `json:"ip,omitempty"`
	RxRate     int64  `json:"rx_rate,omitempty"`
	TxRate     int64  `json:"tx_rate,omitempty"`
	RxBytes    int64  `json:"rx_bytes,omitempty"`
	TxBytes    int64  `json:"tx_bytes,omitempty"`
	Authorized bool   `json:"authorized,omitempty"`
	Raw        string `json:"-"`
}

type wifiApplyResult struct {
	Section string   `json:"section"`
	Status  string   `json:"status"`
	Changed []string `json:"changed"`
}

type wifiLease struct {
	MAC      string
	IP       string
	Hostname string
}

func registerWiFiHandlers(mux *http.ServeMux, cfg Config) {
	runner := cfg.SystemRunner
	if runner == nil {
		runner = netgate.ShellRunner{}
	}
	api := wifiAPI{runner: runner}
	mux.HandleFunc("/api/wifi/status", api.status)
	mux.HandleFunc("/api/wifi/scan", api.scan)
	mux.HandleFunc("/api/wifi/clients", api.clients)
	mux.HandleFunc("/api/wifi/config", api.config)
}

func (api wifiAPI) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	result := api.run(r, []string{"ubus", "call", "network.wireless", "status"})
	if !result.OK {
		writeAPIErrorCode(w, http.StatusBadGateway, "wifi_status_failed", "wireless status query failed", nil)
		return
	}
	radios, ifaces, _ := parseWirelessStatus(result.Output)
	writeAPIData(w, http.StatusOK, wifiStatusView{
		Radios:     radios,
		Interfaces: ifaces,
	})
}

func (api wifiAPI) scan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	iface, ok := decodeWiFiInterface(w, r)
	if !ok {
		return
	}
	if iface == "" || strings.EqualFold(iface, "auto") || iface == defaultWiFiInterface {
		if discovered := api.defaultScanInterface(r); discovered != "" {
			iface = discovered
		} else if iface == "" || strings.EqualFold(iface, "auto") {
			writeAPIError(w, http.StatusBadRequest, "no wireless interface available for scan")
			return
		}
	}
	if err := validateWiFiInterface(iface); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	result := api.run(r, []string{"iwinfo", iface, "scan"})
	if !result.OK {
		writeAPIErrorCode(w, http.StatusBadGateway, "wifi_scan_failed", "wireless scan failed", map[string]any{"interface": iface})
		return
	}
	networks := parseWiFiScan(result.Output)
	sort.SliceStable(networks, func(i, j int) bool {
		if networks[i].SignalDBM == networks[j].SignalDBM {
			return networks[i].SSID < networks[j].SSID
		}
		return networks[i].SignalDBM > networks[j].SignalDBM
	})
	writeAPIData(w, http.StatusOK, map[string]any{
		"interface": iface,
		"networks":  networks,
	})
}

func (api wifiAPI) clients(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	iface := strings.TrimSpace(r.URL.Query().Get("interface"))
	if iface == "" || strings.EqualFold(iface, "all") {
		iface = "all"
	} else if err := validateWiFiInterface(iface); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	targets, ssidByIface, _, _ := api.clientTargets(r, iface)
	if len(targets) == 0 {
		// Keep a structured empty response rather than hard-failing on default wlan0.
		writeAPIData(w, http.StatusOK, map[string]any{
			"interface":  iface,
			"interfaces": []string{},
			"clients":    []wifiClientView{},
		})
		return
	}

	leases := parseDHCPLeases(api.run(r, []string{"cat", "/tmp/dhcp.leases"}).Output)
	clients := make([]wifiClientView, 0)
	for _, target := range targets {
		hostapd := api.run(r, []string{"ubus", "call", "hostapd." + target, "get_clients"})
		if hostapd.OK && strings.TrimSpace(hostapd.Output) != "" {
			parsed := parseHostapdClients(hostapd.Output, target, ssidByIface[target], leases)
			if len(parsed) > 0 || looksLikeHostapdClientsJSON(hostapd.Output) {
				clients = append(clients, parsed...)
				continue
			}
		}
		assoc := api.run(r, []string{"iwinfo", target, "assoclist"})
		if assoc.OK {
			parsed := parseWiFiClients(assoc.Output, target, ssidByIface[target], leases)
			clients = append(clients, parsed...)
		}
	}

	sort.SliceStable(clients, func(i, j int) bool {
		if clients[i].SignalDBM == clients[j].SignalDBM {
			return clients[i].MAC < clients[j].MAC
		}
		return clients[i].SignalDBM > clients[j].SignalDBM
	})

	writeAPIData(w, http.StatusOK, map[string]any{
		"interface":  iface,
		"interfaces": targets,
		"clients":    clients,
	})
}

func (api wifiAPI) config(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		result := api.run(r, []string{"uci", "show", "wireless"})
		if !result.OK {
			writeAPIErrorCode(w, http.StatusBadGateway, "wifi_config_read_failed", "wireless configuration could not be read", nil)
			return
		}
		sections := parseWiFiUCI(result.Output)
		writeAPIData(w, http.StatusOK, wifiConfigView{
			Networks: buildWiFiConfigNetworks(sections),
			Devices:  buildWiFiConfigDevices(sections),
		})
	case http.MethodPut:
		var payload wifiConfigPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		if !payload.Confirm {
			writeAPIError(w, http.StatusBadRequest, "confirm must be true")
			return
		}
		results, changed, err := api.applyConfig(r, payload)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, result := range results {
			if !result.OK {
				writeAPIErrorCode(w, http.StatusBadGateway, "wifi_config_apply_failed", "wireless configuration apply failed", map[string]any{"section": payload.Section, "changed": changed})
				return
			}
		}
		writeAPIData(w, http.StatusAccepted, wifiApplyResult{Section: payload.Section, Status: "ok", Changed: changed})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api wifiAPI) applyConfig(r *http.Request, payload wifiConfigPayload) ([]systemCommandResult, []string, error) {
	section := strings.TrimSpace(payload.Section)
	if !safeWiFiSection.MatchString(section) {
		return nil, nil, fmt.Errorf("section must be a safe UCI section id")
	}
	var commands [][]string
	changed := make([]string, 0, 4)
	if payload.SSID != "" {
		commands = append(commands, []string{"uci", "set", "wireless." + section + ".ssid=" + payload.SSID})
		changed = append(changed, "ssid")
	}
	if payload.Encryption != "" {
		commands = append(commands, []string{"uci", "set", "wireless." + section + ".encryption=" + payload.Encryption})
		changed = append(changed, "encryption")
	}
	if payload.Key != "" {
		commands = append(commands, []string{"uci", "set", "wireless." + section + ".key=" + payload.Key})
		changed = append(changed, "key")
	}
	if payload.Disabled != nil {
		value := "0"
		if *payload.Disabled {
			value = "1"
		}
		commands = append(commands, []string{"uci", "set", "wireless." + section + ".disabled=" + value})
		changed = append(changed, "disabled")
	}
	if len(commands) == 0 {
		return nil, nil, fmt.Errorf("at least one wifi field is required")
	}
	commands = append(commands, []string{"uci", "commit", "wireless"}, []string{"wifi", "reload"})
	results := make([]systemCommandResult, 0, len(commands))
	for _, command := range commands {
		result := api.run(r, command)
		results = append(results, result)
		if !result.OK {
			break
		}
	}
	return results, changed, nil
}

func (api wifiAPI) run(r *http.Request, argv []string) systemCommandResult {
	runner := api.runner
	if runner == nil {
		runner = netgate.ShellRunner{}
	}
	output, err := runner.Run(r.Context(), argv)
	result := systemCommandResult{OK: err == nil, Argv: append([]string(nil), argv...), Output: strings.TrimSpace(output)}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func (api wifiAPI) defaultScanInterface(r *http.Request) string {
	result := api.run(r, []string{"ubus", "call", "network.wireless", "status"})
	if !result.OK {
		return ""
	}
	_, ifaces, _ := parseWirelessStatus(result.Output)
	for _, iface := range ifaces {
		if iface.Ifname != "" && !iface.Disabled {
			return iface.Ifname
		}
	}
	for _, iface := range ifaces {
		if iface.Ifname != "" {
			return iface.Ifname
		}
	}
	return ""
}

func (api wifiAPI) clientTargets(r *http.Request, iface string) (targets []string, ssidByIface map[string]string, raw string, result systemCommandResult) {
	ssidByIface = map[string]string{}
	result = api.run(r, []string{"ubus", "call", "network.wireless", "status"})
	raw = result.Output
	if result.OK {
		_, ifaces, _ := parseWirelessStatus(result.Output)
		for _, item := range ifaces {
			if item.Ifname == "" {
				continue
			}
			ssidByIface[item.Ifname] = item.SSID
			if iface == "all" || item.Ifname == iface {
				targets = append(targets, item.Ifname)
			}
		}
	}
	if iface != "all" && len(targets) == 0 {
		// Explicit interface request even if wireless status is incomplete.
		targets = []string{iface}
	}
	sort.Strings(targets)
	return targets, ssidByIface, raw, result
}

func decodeWiFiInterface(w http.ResponseWriter, r *http.Request) (string, bool) {
	var payload wifiInterfacePayload
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return "", false
		}
	}
	iface := strings.TrimSpace(payload.Interface)
	if iface == "" {
		iface = defaultWiFiInterface
	}
	if strings.EqualFold(iface, "auto") {
		return "auto", true
	}
	if err := validateWiFiInterface(iface); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return iface, true
}

func validateWiFiInterface(iface string) error {
	if !safeWiFiSection.MatchString(iface) {
		return fmt.Errorf("interface must be a safe interface name")
	}
	return nil
}

func parseWirelessStatus(raw string) ([]wifiRadioView, []wifiIfaceView, any) {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return []wifiRadioView{}, []wifiIfaceView{}, map[string]any{}
	}
	root, ok := parsed.(map[string]any)
	if !ok {
		return []wifiRadioView{}, []wifiIfaceView{}, parsed
	}
	radioIDs := make([]string, 0, len(root))
	for id := range root {
		radioIDs = append(radioIDs, id)
	}
	sort.Strings(radioIDs)

	radios := make([]wifiRadioView, 0, len(radioIDs))
	ifaces := make([]wifiIfaceView, 0)
	for _, id := range radioIDs {
		radioMap, ok := root[id].(map[string]any)
		if !ok {
			continue
		}
		config, _ := radioMap["config"].(map[string]any)
		radio := wifiRadioView{
			ID:       id,
			Up:       asBool(radioMap["up"]),
			Pending:  asBool(radioMap["pending"]),
			Disabled: asBool(radioMap["disabled"]) || asBool(config["disabled"]),
			Band:     asString(config["band"]),
			Channel:  asString(config["channel"]),
			HTMode:   asString(config["htmode"]),
			Country:  asString(config["country"]),
			TxPower:  asInt(config["txpower"]),
		}
		list, _ := radioMap["interfaces"].([]any)
		for _, item := range list {
			ifaceMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ifaceCfg, _ := ifaceMap["config"].(map[string]any)
			iface := wifiIfaceView{
				Section:    asString(ifaceMap["section"]),
				Ifname:     asString(ifaceMap["ifname"]),
				SSID:       asString(ifaceCfg["ssid"]),
				Mode:       asString(ifaceCfg["mode"]),
				Encryption: asString(ifaceCfg["encryption"]),
				Disabled:   asBool(ifaceCfg["disabled"]),
				Network:    firstString(ifaceCfg["network"]),
				Band:       radio.Band,
				Channel:    radio.Channel,
				HTMode:     radio.HTMode,
			}
			radio.Interfaces = append(radio.Interfaces, iface)
			ifaces = append(ifaces, iface)
		}
		if radio.Interfaces == nil {
			radio.Interfaces = []wifiIfaceView{}
		}
		radios = append(radios, radio)
	}
	return radios, ifaces, parsed
}

func parseWiFiUCI(raw string) map[string]map[string]string {
	sections := map[string]map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "wireless.") {
			continue
		}
		left, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		parts := strings.Split(left, ".")
		if len(parts) == 2 {
			section := parts[1]
			if sections[section] == nil {
				sections[section] = map[string]string{}
			}
			sections[section][".type"] = trimUCIValue(value)
			continue
		}
		if len(parts) < 3 {
			continue
		}
		section, key := parts[1], strings.Join(parts[2:], ".")
		if sections[section] == nil {
			sections[section] = map[string]string{}
		}
		sections[section][key] = trimUCIValue(value)
	}
	return sections
}

func buildWiFiConfigDevices(sections map[string]map[string]string) []wifiConfigDeviceView {
	devices := []wifiConfigDeviceView{}
	ids := make([]string, 0, len(sections))
	for id := range sections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		section := sections[id]
		if section[".type"] != "wifi-device" && !strings.HasPrefix(id, "radio") {
			// Prefer explicit type; fall back for older dumps that omit section type.
			if section["type"] == "" && section["band"] == "" && section["path"] == "" {
				continue
			}
			if section[".type"] == "wifi-iface" || section["ssid"] != "" || section["mode"] != "" {
				continue
			}
		}
		if section[".type"] == "wifi-iface" {
			continue
		}
		if section[".type"] != "wifi-device" && section["type"] == "" && section["band"] == "" {
			continue
		}
		devices = append(devices, wifiConfigDeviceView{
			Section:  id,
			Band:     section["band"],
			Channel:  section["channel"],
			HTMode:   section["htmode"],
			Country:  section["country"],
			TxPower:  section["txpower"],
			Disabled: section["disabled"] == "1" || section["disabled"] == "true",
		})
	}
	return devices
}

func buildWiFiConfigNetworks(sections map[string]map[string]string) []wifiConfigNetworkView {
	devices := map[string]map[string]string{}
	for id, section := range sections {
		if section[".type"] == "wifi-device" || (section[".type"] == "" && (section["band"] != "" || section["path"] != "" || section["type"] != "")) {
			if section[".type"] != "wifi-iface" && section["ssid"] == "" {
				devices[id] = section
			}
		}
	}
	networks := []wifiConfigNetworkView{}
	ids := make([]string, 0, len(sections))
	for id := range sections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		section := sections[id]
		isIface := section[".type"] == "wifi-iface" || section["ssid"] != "" || section["mode"] != ""
		if !isIface || section[".type"] == "wifi-device" {
			continue
		}
		deviceID := section["device"]
		device := devices[deviceID]
		networks = append(networks, wifiConfigNetworkView{
			Section:       id,
			Device:        deviceID,
			SSID:          section["ssid"],
			Mode:          section["mode"],
			Encryption:    section["encryption"],
			KeyConfigured: strings.TrimSpace(section["key"]) != "",
			Disabled:      section["disabled"] == "1" || section["disabled"] == "true",
			Network:       section["network"],
			Band:          device["band"],
			Channel:       device["channel"],
			HTMode:        device["htmode"],
			Country:       device["country"],
		})
	}
	return networks
}

func trimUCIValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "'")
	value = strings.Trim(value, `"`)
	return value
}

func parseWiFiScan(raw string) []wifiScanNetwork {
	networks := []wifiScanNetwork{}
	var current *wifiScanNetwork
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Cell ") {
			if current != nil {
				networks = append(networks, *current)
			}
			current = &wifiScanNetwork{}
			if _, addr, ok := strings.Cut(trimmed, "Address:"); ok {
				current.BSSID = strings.TrimSpace(addr)
			}
			continue
		}
		if current == nil {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "ESSID:"):
			ssid := trimQuoted(strings.TrimPrefix(trimmed, "ESSID:"))
			if strings.EqualFold(ssid, "unknown") {
				ssid = ""
			}
			current.SSID = ssid
		case strings.Contains(trimmed, "Channel:"):
			current.Channel = parseFieldInt(trimmed, "Channel:")
		case strings.Contains(trimmed, "Signal:"):
			current.SignalDBM = parseFieldInt(trimmed, "Signal:")
		case strings.Contains(trimmed, "Quality:"):
			current.Quality = firstToken(afterToken(trimmed, "Quality:"))
		case strings.HasPrefix(trimmed, "Encryption:"):
			current.Encryption = strings.TrimSpace(strings.TrimPrefix(trimmed, "Encryption:"))
		}
	}
	if current != nil {
		networks = append(networks, *current)
	}
	return networks
}

func parseWiFiClients(raw, iface, ssid string, leases map[string]wifiLease) []wifiClientView {
	clients := []wifiClientView{}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 || !strings.Contains(fields[0], ":") {
			continue
		}
		mac := strings.ToLower(fields[0])
		signal := 0
		if idx := strings.Index(trimmed, "dBm"); idx > 0 {
			// take the signed number immediately before the first dBm token
			before := strings.TrimSpace(trimmed[:idx])
			parts := strings.Fields(before)
			if len(parts) > 0 {
				signal, _ = strconv.Atoi(parts[len(parts)-1])
			}
		}
		client := wifiClientView{
			MAC:       mac,
			Interface: iface,
			SSID:      ssid,
			SignalDBM: signal,
			Raw:       trimmed,
		}
		if lease, ok := leases[mac]; ok {
			client.IP = lease.IP
			client.Hostname = lease.Hostname
		}
		clients = append(clients, client)
	}
	return clients
}

func looksLikeHostapdClientsJSON(raw string) bool {
	return strings.Contains(raw, `"clients"`)
}

func parseHostapdClients(raw, iface, ssid string, leases map[string]wifiLease) []wifiClientView {
	var payload struct {
		Clients map[string]map[string]any `json:"clients"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil
	}
	macs := make([]string, 0, len(payload.Clients))
	for mac := range payload.Clients {
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	clients := make([]wifiClientView, 0, len(macs))
	for _, mac := range macs {
		info := payload.Clients[mac]
		normalized := strings.ToLower(mac)
		client := wifiClientView{
			MAC:        normalized,
			Interface:  iface,
			SSID:       ssid,
			SignalDBM:  asInt(info["signal"]),
			Authorized: asBool(info["authorized"]) || asBool(info["assoc"]),
		}
		if rate, ok := info["rate"].(map[string]any); ok {
			client.RxRate = asInt64(rate["rx"])
			client.TxRate = asInt64(rate["tx"])
		}
		if bytesMap, ok := info["bytes"].(map[string]any); ok {
			client.RxBytes = asInt64(bytesMap["rx"])
			client.TxBytes = asInt64(bytesMap["tx"])
		}
		if lease, ok := leases[normalized]; ok {
			client.IP = lease.IP
			client.Hostname = lease.Hostname
		}
		clients = append(clients, client)
	}
	return clients
}

func parseDHCPLeases(raw string) map[string]wifiLease {
	leases := map[string]wifiLease{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 {
			continue
		}
		mac := strings.ToLower(fields[1])
		host := fields[3]
		if host == "*" {
			host = ""
		}
		leases[mac] = wifiLease{MAC: mac, IP: fields[2], Hostname: host}
	}
	return leases
}

func parseFieldInt(line, token string) int {
	value := line
	if token != "" {
		value = afterToken(line, token)
	}
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(fields[0]))
	return n
}

func firstToken(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func afterToken(line, token string) string {
	_, after, ok := strings.Cut(line, token)
	if !ok {
		return ""
	}
	return strings.TrimSpace(after)
}

func trimQuoted(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"`)
	return strings.Trim(value, "'")
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "1" || strings.EqualFold(t, "true") || strings.EqualFold(t, "yes") || strings.EqualFold(t, "on")
	case float64:
		return t != 0
	case int:
		return t != 0
	default:
		return false
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	default:
		return 0
	}
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	case json.Number:
		n, _ := t.Int64()
		return n
	default:
		return 0
	}
}

func firstString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		if len(t) == 0 {
			return ""
		}
		return asString(t[0])
	default:
		return asString(v)
	}
}
