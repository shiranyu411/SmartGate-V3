package fabric

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"smartgate/internal/state"
)

type DNSUpstream interface {
	Exchange(context.Context, *dns.Msg) (*dns.Msg, error)
}

type NetworkDNSUpstream struct {
	Address string
	Network string
	Timeout time.Duration
	Dial    DialContext
}

func (upstream NetworkDNSUpstream) Exchange(ctx context.Context, request *dns.Msg) (*dns.Msg, error) {
	address := strings.TrimSpace(upstream.Address)
	if address == "" {
		return nil, fmt.Errorf("DNS upstream address is required")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(strings.Trim(address, "[]"), "53")
	}
	network := strings.ToLower(strings.TrimSpace(upstream.Network))
	if network == "" {
		network = "udp"
	}
	timeout := upstream.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if upstream.Dial != nil {
		return exchangeDNSConn(ctx, upstream.Dial, network, address, "", timeout, request)
	}
	client := &dns.Client{Net: network, Timeout: timeout}
	response, _, err := client.ExchangeContext(ctx, request, address)
	if err != nil {
		return nil, err
	}
	if response.Truncated && network == "udp" {
		client.Net = "tcp"
		response, _, err = client.ExchangeContext(ctx, request, address)
	}
	return response, err
}

type DialContext func(context.Context, string, string) (net.Conn, error)

type SOCKS5Dialer struct {
	ProxyAddress string
	Timeout      time.Duration
}

func (dialer SOCKS5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS5 worker DNS supports TCP only")
	}
	proxyAddress := strings.TrimSpace(dialer.ProxyAddress)
	if proxyAddress == "" {
		return nil, fmt.Errorf("SOCKS5 proxy address is required")
	}
	timeout := dialer.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	connection, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = connection.Close()
		}
	}()
	deadline := time.Now().Add(timeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	_ = connection.SetDeadline(deadline)
	if _, err := connection.Write([]byte{5, 1, 0}); err != nil {
		return nil, err
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(connection, response); err != nil {
		return nil, err
	}
	if response[0] != 5 || response[1] != 0 {
		return nil, fmt.Errorf("SOCKS5 worker rejected no-auth negotiation")
	}
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse SOCKS5 destination: %w", err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid SOCKS5 destination port")
	}
	request := []byte{5, 1, 0}
	if parsed, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if parsed.Is4() {
			request = append(request, 1)
			request = append(request, parsed.AsSlice()...)
		} else {
			request = append(request, 4)
			request = append(request, parsed.AsSlice()...)
		}
	} else {
		if len(host) == 0 || len(host) > 255 {
			return nil, fmt.Errorf("invalid SOCKS5 destination host")
		}
		request = append(request, 3, byte(len(host)))
		request = append(request, host...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := connection.Write(request); err != nil {
		return nil, err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(connection, header); err != nil {
		return nil, err
	}
	if header[0] != 5 || header[1] != 0 {
		return nil, fmt.Errorf("SOCKS5 worker connect failed with code %d", header[1])
	}
	if err := discardSOCKSAddress(connection, header[3]); err != nil {
		return nil, err
	}
	_ = connection.SetDeadline(time.Time{})
	succeeded = true
	return connection, nil
}

type DoHUpstream struct {
	URL     string
	Dial    DialContext
	Timeout time.Duration
}

func (upstream DoHUpstream) Exchange(ctx context.Context, request *dns.Msg) (*dns.Msg, error) {
	endpoint, err := url.Parse(strings.TrimSpace(upstream.URL))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		return nil, fmt.Errorf("invalid DoH URL")
	}
	body, err := request.Pack()
	if err != nil {
		return nil, err
	}
	timeout := upstream.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.Hostname()}}
	defer transport.CloseIdleConnections()
	if upstream.Dial != nil {
		transport.DialContext = upstream.Dial
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Accept", "application/dns-message")
	httpRequest.Header.Set("Content-Type", "application/dns-message")
	response, err := client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("DoH upstream returned status %d", response.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil {
		return nil, err
	}
	if len(responseBody) > 64*1024 {
		return nil, fmt.Errorf("DoH response exceeds 65536 bytes")
	}
	dnsResponse := new(dns.Msg)
	if err := dnsResponse.Unpack(responseBody); err != nil {
		return nil, err
	}
	return dnsResponse, nil
}

type StaticDNSResponse struct {
	Rcode int
}

func (upstream StaticDNSResponse) Exchange(_ context.Context, request *dns.Msg) (*dns.Msg, error) {
	response := new(dns.Msg)
	response.SetRcode(request, upstream.Rcode)
	return response, nil
}

type DNSUpstreamRouter struct {
	mu        sync.RWMutex
	byTarget  map[string][]DNSUpstream
	counters  map[string]*atomic.Uint64
	strictMap bool
}

func NewDNSUpstreamRouter(byTarget map[string][]DNSUpstream) (*DNSUpstreamRouter, error) {
	router := &DNSUpstreamRouter{byTarget: map[string][]DNSUpstream{}, counters: map[string]*atomic.Uint64{}, strictMap: true}
	for target, upstreams := range byTarget {
		target = strings.TrimSpace(target)
		if target == "" {
			return nil, fmt.Errorf("DNS upstream target is required")
		}
		if len(upstreams) == 0 {
			return nil, fmt.Errorf("DNS target %s has no upstreams", target)
		}
		for _, upstream := range upstreams {
			if upstream == nil {
				return nil, fmt.Errorf("DNS target %s has a nil upstream", target)
			}
		}
		router.byTarget[target] = append([]DNSUpstream(nil), upstreams...)
		router.counters[target] = &atomic.Uint64{}
	}
	return router, nil
}

func (router *DNSUpstreamRouter) Exchange(ctx context.Context, target string, request *dns.Msg) (*dns.Msg, error) {
	router.mu.RLock()
	upstreams := router.byTarget[target]
	counter := router.counters[target]
	router.mu.RUnlock()
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("DNS target %s has no configured upstream", target)
	}
	start := uint64(0)
	if counter != nil {
		start = counter.Add(1) - 1
	}
	var errs []error
	for offset := range upstreams {
		upstream := upstreams[(int(start)+offset)%len(upstreams)]
		response, err := upstream.Exchange(ctx, request.Copy())
		if err == nil {
			return response, nil
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("all DNS upstreams failed for %s: %w", target, errorsJoin(errs))
}

func errorsJoin(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	message := make([]string, 0, len(errs))
	for _, err := range errs {
		message = append(message, err.Error())
	}
	return fmt.Errorf("%s", strings.Join(message, "; "))
}

func BuildDNSUpstreamRouter(spec state.SpecState, graph state.FabricGraph) (*DNSUpstreamRouter, error) {
	dnsSpec := state.EffectiveDNSSpec(spec.DNS)
	if state.EffectiveGatewayMode(spec.Settings.GatewayMode) == state.GatewayModeGlobal {
		dnsSpec.Final = "proxy"
		if len(dnsSpec.ProxyServers) == 0 {
			dnsSpec.ProxyServers = []string{"https://cloudflare-dns.com/dns-query"}
		}
		if containsUnsafeResolver(dnsSpec.DirectServers) {
			dnsSpec.DirectServers = []string{"1.1.1.1", "1.0.0.1"}
		}
	}
	direct, err := upstreamsForAddresses(dnsSpec.DirectServers, nil)
	if err != nil {
		return nil, fmt.Errorf("build direct DNS upstreams: %w", err)
	}
	if len(direct) == 0 {
		return nil, fmt.Errorf("at least one direct DNS upstream is required")
	}
	byTarget := map[string][]DNSUpstream{TargetDirect: direct}
	for _, dispatch := range graph.Dispatch {
		switch dispatch.Target {
		case TargetBlock:
			byTarget[dispatch.Pool] = []DNSUpstream{StaticDNSResponse{Rcode: dns.RcodeNameError}}
		case TargetDirect:
			byTarget[dispatch.Pool] = append([]DNSUpstream(nil), direct...)
		default:
			if dispatch.DNSPort < 1 || dispatch.DNSPort > 65535 {
				return nil, fmt.Errorf("pool %s has invalid worker DNS port", dispatch.Pool)
			}
			addresses := dnsSpec.ProxyServers
			if len(addresses) == 0 {
				addresses = dnsSpec.DirectServers
			}
			dialer := SOCKS5Dialer{ProxyAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(dispatch.DNSPort))}
			upstreams, err := upstreamsForAddresses(addresses, dialer.DialContext)
			if err != nil {
				return nil, fmt.Errorf("build pool %s DNS upstreams: %w", dispatch.Pool, err)
			}
			byTarget[dispatch.Pool] = upstreams
		}
	}
	return NewDNSUpstreamRouter(byTarget)
}

func upstreamsForAddresses(addresses []string, dial DialContext) ([]DNSUpstream, error) {
	upstreams := make([]DNSUpstream, 0, len(addresses))
	for _, raw := range addresses {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err == nil && parsed.Scheme != "" {
			switch strings.ToLower(parsed.Scheme) {
			case "https":
				upstreams = append(upstreams, DoHUpstream{URL: value, Dial: dial})
			case "tls":
				address := parsed.Host
				if parsed.Port() == "" {
					address = net.JoinHostPort(parsed.Hostname(), "853")
				}
				upstreams = append(upstreams, tlsDNSUpstream{Address: address, ServerName: parsed.Hostname(), Dial: dial})
			case "tcp", "udp":
				address := parsed.Host
				if parsed.Port() == "" {
					address = net.JoinHostPort(parsed.Hostname(), "53")
				}
				network := parsed.Scheme
				if dial != nil {
					network = "tcp"
				}
				upstreams = append(upstreams, NetworkDNSUpstream{Address: address, Network: network, Dial: dial})
			case "dhcp":
				return nil, fmt.Errorf("dhcp:// DNS sources require resolved addresses before worker routing")
			default:
				return nil, fmt.Errorf("unsupported DNS upstream scheme %q", parsed.Scheme)
			}
			continue
		}
		network := "udp"
		if dial != nil {
			network = "tcp"
		}
		upstreams = append(upstreams, NetworkDNSUpstream{Address: value, Network: network, Dial: dial})
	}
	return upstreams, nil
}

type tlsDNSUpstream struct {
	Address    string
	ServerName string
	Dial       DialContext
	Timeout    time.Duration
}

func (upstream tlsDNSUpstream) Exchange(ctx context.Context, request *dns.Msg) (*dns.Msg, error) {
	timeout := upstream.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dial := upstream.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	return exchangeDNSConn(ctx, dial, "tcp", upstream.Address, upstream.ServerName, timeout, request)
}

func exchangeDNSConn(ctx context.Context, dial DialContext, network, address, tlsServerName string, timeout time.Duration, request *dns.Msg) (*dns.Msg, error) {
	connection, err := dial(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	deadline := time.Now().Add(timeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	_ = connection.SetDeadline(deadline)
	if tlsServerName != "" {
		tlsConnection := tls.Client(connection, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: tlsServerName})
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		connection = tlsConnection
	}
	dnsConnection := &dns.Conn{Conn: connection}
	if err := dnsConnection.WriteMsg(request); err != nil {
		return nil, err
	}
	return dnsConnection.ReadMsg()
}

func discardSOCKSAddress(reader io.Reader, addressType byte) error {
	length := 0
	switch addressType {
	case 1:
		length = net.IPv4len
	case 4:
		length = net.IPv6len
	case 3:
		value := []byte{0}
		if _, err := io.ReadFull(reader, value); err != nil {
			return err
		}
		length = int(value[0])
	default:
		return fmt.Errorf("SOCKS5 worker returned invalid address type %d", addressType)
	}
	buffer := make([]byte, length+2)
	_, err := io.ReadFull(reader, buffer)
	return err
}

func containsUnsafeResolver(addresses []string) bool {
	for _, raw := range addresses {
		value := strings.TrimSpace(raw)
		if value == "" || strings.EqualFold(value, "dhcp://auto") {
			return true
		}
		host := value
		if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
			host = parsed.Hostname()
		} else if splitHost, _, err := net.SplitHostPort(value); err == nil {
			host = splitHost
		}
		address, err := netip.ParseAddr(strings.Trim(host, "[]"))
		if err != nil {
			continue
		}
		if address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() {
			return true
		}
	}
	return false
}
