package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"smartgate/internal/lockset"
	"smartgate/internal/state"
)

type dnsAPI struct {
	store *state.Store
	locks *lockset.LockSet
}

type dnsPayload struct {
	ClassificationMode string   `json:"classification_mode"`
	Strategy           string   `json:"strategy"`
	FakeIPEnabled      *bool    `json:"fakeip_enabled"`
	FakeIPRange        string   `json:"fakeip_range"`
	Final              string   `json:"final"`
	DirectServers      []string `json:"direct_servers"`
	ProxyServers       []string `json:"proxy_servers"`
	FallbackServers    []string `json:"fallback_servers"`
}

type dnsView struct {
	state.DNSSpec
	ClassificationMode string `json:"classification_mode"`
}

const dnsClassificationMode = "real_ip_nft_set"

func registerDNSHandlers(mux *http.ServeMux, store *state.Store, locks *lockset.LockSet) {
	api := dnsAPI{store: store, locks: locks}
	mux.HandleFunc("/api/dns", api.dns)
}

func (api dnsAPI) dns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		spec, err := api.loadSpec("dns-read")
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAPIData(w, http.StatusOK, makeDNSView(spec.DNS))
	case http.MethodPut:
		var payload dnsPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		if payload.ClassificationMode != "" && payload.ClassificationMode != dnsClassificationMode {
			writeAPIErrorCode(w, http.StatusBadRequest, "validation_failed", "classification_mode must be real_ip_nft_set", nil)
			return
		}
		if payload.FakeIPEnabled != nil && *payload.FakeIPEnabled {
			writeAPIErrorCode(w, http.StatusUnprocessableEntity, "fakeip_not_supported", "Fake-IP is not supported by the SmartGate V3 data plane", nil)
			return
		}
		dns, err := normalizeDNSSpec(payload)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := api.saveDNS("dns-write", dns); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAPIData(w, http.StatusOK, makeDNSView(dns))
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api dnsAPI) loadSpec(name string) (state.SpecState, error) {
	if api.locks != nil {
		session := api.locks.Session(name)
		release, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return state.SpecState{}, err
		}
		defer release()
	}
	return api.store.LoadSpec()
}

func (api dnsAPI) saveDNS(name string, dns state.DNSSpec) error {
	if api.locks != nil {
		session := api.locks.Session(name)
		release, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		defer release()
	}
	spec, err := api.store.LoadSpec()
	if err != nil {
		return err
	}
	spec.DNS = dns
	_, err = api.store.SaveSpec(spec)
	return err
}

func normalizeDNSSpec(payload dnsPayload) (state.DNSSpec, error) {
	dns := state.DefaultDNSSpec()
	if payload.Strategy != "" {
		dns.Strategy = strings.ToLower(strings.TrimSpace(payload.Strategy))
	}
	dns.FakeIPEnabled = false
	dns.FakeIPRange = ""
	if payload.Final != "" {
		dns.Final = strings.ToLower(strings.TrimSpace(payload.Final))
	}
	if payload.DirectServers != nil {
		dns.DirectServers = payload.DirectServers
	}
	if payload.ProxyServers != nil {
		dns.ProxyServers = payload.ProxyServers
	}
	if payload.FallbackServers != nil {
		dns.FallbackServers = payload.FallbackServers
	}

	if !validDNSStrategy(dns.Strategy) {
		return dns, fmt.Errorf("strategy must be prefer_ipv4, prefer_ipv6, ipv4_only, or ipv6_only")
	}
	if dns.Final != "direct" && dns.Final != "proxy" && dns.Final != "fallback" {
		return dns, fmt.Errorf("final must be direct, proxy, or fallback")
	}
	var err error
	dns.DirectServers, err = normalizeDNSServers(dns.DirectServers)
	if err != nil {
		return dns, err
	}
	dns.ProxyServers, err = normalizeDNSServers(dns.ProxyServers)
	if err != nil {
		return dns, err
	}
	dns.FallbackServers, err = normalizeDNSServers(dns.FallbackServers)
	if err != nil {
		return dns, err
	}
	if len(dns.DirectServers) == 0 {
		return dns, fmt.Errorf("direct_servers must contain at least one server")
	}
	if dns.Final == "proxy" && len(dns.ProxyServers) == 0 {
		return dns, fmt.Errorf("proxy_servers must contain at least one server when final is proxy")
	}
	if dns.Final == "fallback" && len(dns.FallbackServers) == 0 {
		return dns, fmt.Errorf("fallback_servers must contain at least one server when final is fallback")
	}
	return dns, nil
}

func makeDNSView(dns state.DNSSpec) dnsView {
	return dnsView{DNSSpec: state.EffectiveDNSSpec(dns), ClassificationMode: dnsClassificationMode}
}

func normalizeDNSServers(raw []string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	for _, item := range raw {
		value := strings.TrimSpace(item)
		if value == "" {
			continue
		}
		if strings.ContainsAny(value, " \t\r\n") {
			return nil, fmt.Errorf("dns server %q must not contain whitespace", value)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func validDNSStrategy(strategy string) bool {
	switch strategy {
	case "prefer_ipv4", "prefer_ipv6", "ipv4_only", "ipv6_only":
		return true
	default:
		return false
	}
}
