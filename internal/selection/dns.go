package selection

import (
	"net"
	"net/url"
	"strings"

	"smartgate/internal/state"
)

func DNSForGatewayMode(dns state.DNSSpec, gatewayMode string, proxyAvailable bool) state.DNSSpec {
	effectiveDNS := state.EffectiveDNSSpec(dns)
	if state.EffectiveGatewayMode(gatewayMode) != state.GatewayModeGlobal || !proxyAvailable {
		return effectiveDNS
	}
	if len(effectiveDNS.ProxyServers) == 0 {
		effectiveDNS.ProxyServers = []string{"https://cloudflare-dns.com/dns-query"}
	}
	effectiveDNS.Final = "proxy"
	return effectiveDNS
}

// EffectiveDNSForGatewayMode returns gateway-mode defaults with proxy-final
// resolver safety applied.
func EffectiveDNSForGatewayMode(dns state.DNSSpec, gatewayMode string, proxyAvailable bool) state.DNSSpec {
	return SanitizeDNSForProxyFinal(DNSForGatewayMode(dns, gatewayMode, proxyAvailable))
}

func SanitizeDNSForProxyFinal(dns state.DNSSpec) state.DNSSpec {
	if dns.Final != "proxy" {
		return dns
	}
	if !HasUnsafeLocalDNSResolver(dns.DirectServers) {
		return dns
	}
	dns.DirectServers = []string{
		"1.1.1.1",
		"1.0.0.1",
	}
	return dns
}

func HasUnsafeLocalDNSResolver(servers []string) bool {
	for _, item := range servers {
		if dnsServerIsUnsafeLocalResolver(item) {
			return true
		}
	}
	return false
}

func dnsServerIsUnsafeLocalResolver(raw string) bool {
	value := strings.TrimSpace(raw)
	if value == "" {
		return false
	}
	if strings.EqualFold(value, "dhcp://auto") {
		return true
	}
	host := value
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		host = parsed.Hostname()
	} else if splitHost, _, err := net.SplitHostPort(value); err == nil {
		host = splitHost
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
