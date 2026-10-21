package fabric

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

const DefaultDNSListen = "[::]:1053"

type DNSResolver interface {
	Exchange(context.Context, string, *dns.Msg) (*dns.Msg, error)
}

type AddressSetWriter interface {
	Add(context.Context, string, netip.Addr, time.Duration) error
}

type DNSConfig struct {
	Listen             string
	RejectHTTPSRecords bool
	Classifier         *Classifier
	Resolver           DNSResolver
	SetWriter          AddressSetWriter
	MinTTL             time.Duration
	MaxTTL             time.Duration
}

type DNSMetrics struct {
	Queries          uint64            `json:"queries"`
	Failures         uint64            `json:"failures"`
	RejectedHTTPS    uint64            `json:"rejected_https"`
	Classified       uint64            `json:"classified"`
	Fallback         uint64            `json:"fallback"`
	SetWrites        uint64            `json:"set_writes"`
	SetWriteFailures uint64            `json:"set_write_failures"`
	ByTarget         map[string]uint64 `json:"by_target"`
}

type DNSServer struct {
	config DNSConfig
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.RWMutex
	udp      *dns.Server
	tcp      *dns.Server
	addr     string
	serveErr chan error
	targets  map[string]uint64
	hosts    *dnsHostIndex

	queries          atomic.Uint64
	failures         atomic.Uint64
	rejectedHTTPS    atomic.Uint64
	classified       atomic.Uint64
	fallback         atomic.Uint64
	setWrites        atomic.Uint64
	setWriteFailures atomic.Uint64
}

func NewDNSServer(config DNSConfig) (*DNSServer, error) {
	config, err := validateDNSConfig(config)
	if err != nil {
		return nil, err
	}
	return &DNSServer{config: config, serveErr: make(chan error, 2), targets: map[string]uint64{}, hosts: newDNSHostIndex()}, nil
}

func validateDNSConfig(config DNSConfig) (DNSConfig, error) {
	if config.Classifier == nil {
		return DNSConfig{}, fmt.Errorf("DNS classifier is required")
	}
	if config.Resolver == nil {
		return DNSConfig{}, fmt.Errorf("DNS resolver is required")
	}
	if config.SetWriter == nil {
		return DNSConfig{}, fmt.Errorf("DNS nft set writer is required")
	}
	if strings.TrimSpace(config.Listen) == "" {
		config.Listen = DefaultDNSListen
	}
	if config.MinTTL <= 0 {
		config.MinTTL = time.Second
	}
	if config.MaxTTL <= 0 {
		config.MaxTTL = 24 * time.Hour
	}
	if config.MaxTTL < config.MinTTL {
		return DNSConfig{}, fmt.Errorf("DNS max TTL must be at least min TTL")
	}
	return config, nil
}

func (server *DNSServer) Reconfigure(config DNSConfig) error {
	config, err := validateDNSConfig(config)
	if err != nil {
		return err
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.udp == nil || server.tcp == nil {
		return fmt.Errorf("DNS server is not running")
	}
	if normalizeListenAddress(config.Listen) != normalizeListenAddress(server.config.Listen) {
		return fmt.Errorf("DNS listen address change requires a service restart")
	}
	server.config = config
	return nil
}

func (server *DNSServer) Start(ctx context.Context) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.udp != nil || server.tcp != nil {
		return fmt.Errorf("DNS server is already running")
	}
	host, port, err := net.SplitHostPort(server.config.Listen)
	if err != nil {
		return fmt.Errorf("parse DNS listen address: %w", err)
	}
	udpConn, err := net.ListenPacket("udp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("listen DNS UDP: %w", err)
	}
	boundPort := udpConn.LocalAddr().(*net.UDPAddr).Port
	tcpListener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(boundPort)))
	if err != nil {
		_ = udpConn.Close()
		return fmt.Errorf("listen DNS TCP: %w", err)
	}
	server.ctx, server.cancel = context.WithCancel(ctx)
	server.addr = net.JoinHostPort(host, strconv.Itoa(boundPort))
	server.udp = &dns.Server{PacketConn: udpConn, Handler: server}
	server.tcp = &dns.Server{Listener: tcpListener, Handler: server}
	go server.serve(server.udp)
	go server.serve(server.tcp)
	go func(runCtx context.Context) {
		<-runCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}(server.ctx)
	return nil
}

func (server *DNSServer) Wait() error {
	err := <-server.serveErr
	if err == nil || errors.Is(err, net.ErrClosed) || strings.Contains(strings.ToLower(err.Error()), "server closed") {
		return nil
	}
	return err
}

func (server *DNSServer) Shutdown(ctx context.Context) error {
	server.mu.Lock()
	udpServer := server.udp
	tcpServer := server.tcp
	cancel := server.cancel
	server.udp = nil
	server.tcp = nil
	server.cancel = nil
	server.mu.Unlock()

	var errs []error
	for _, current := range []*dns.Server{udpServer, tcpServer} {
		if current == nil {
			continue
		}
		if err := current.ShutdownContext(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
			if strings.Contains(strings.ToLower(err.Error()), "server not started") {
				if current.PacketConn != nil {
					_ = current.PacketConn.Close()
				}
				if current.Listener != nil {
					_ = current.Listener.Close()
				}
				continue
			}
			errs = append(errs, err)
		}
	}
	if cancel != nil {
		cancel()
	}
	return errors.Join(errs...)
}

func (server *DNSServer) Addr() string {
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.addr
}

func (server *DNSServer) Metrics() DNSMetrics {
	server.mu.RLock()
	byTarget := make(map[string]uint64, len(server.targets))
	for target, count := range server.targets {
		byTarget[target] = count
	}
	server.mu.RUnlock()
	return DNSMetrics{
		Queries: server.queries.Load(), Failures: server.failures.Load(), RejectedHTTPS: server.rejectedHTTPS.Load(),
		Classified: server.classified.Load(), Fallback: server.fallback.Load(), SetWrites: server.setWrites.Load(),
		SetWriteFailures: server.setWriteFailures.Load(), ByTarget: byTarget,
	}
}

func (server *DNSServer) ServeDNS(writer dns.ResponseWriter, request *dns.Msg) {
	config := server.configSnapshot()
	server.queries.Add(1)
	if request == nil || len(request.Question) != 1 {
		server.failures.Add(1)
		_ = writer.WriteMsg(errorReply(request, dns.RcodeFormatError))
		return
	}
	question := request.Question[0]
	if config.RejectHTTPSRecords && (question.Qtype == dns.TypeHTTPS || question.Qtype == dns.TypeSVCB) {
		server.rejectedHTTPS.Add(1)
		_ = writer.WriteMsg(errorReply(request, dns.RcodeRefused))
		return
	}
	match := config.Classifier.ClassifyDomain(question.Name)
	server.recordMatch(match)
	if match.Target == TargetBlock {
		_ = writer.WriteMsg(errorReply(request, dns.RcodeNameError))
		return
	}
	ctx := server.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := config.Resolver.Exchange(ctx, match.Target, request.Copy())
	if err != nil || response == nil {
		server.failures.Add(1)
		_ = writer.WriteMsg(errorReply(request, dns.RcodeServerFailure))
		return
	}
	response.Id = request.Id
	if match.Target != TargetBlock {
		if err := server.writeAnswerSets(ctx, config, match.Target, response); err != nil {
			server.failures.Add(1)
			server.setWriteFailures.Add(1)
			_ = writer.WriteMsg(errorReply(request, dns.RcodeServerFailure))
			return
		}
	}
	if err := writer.WriteMsg(response); err != nil {
		server.failures.Add(1)
	} else if remote := writer.RemoteAddr(); remote != nil {
		if host, _, err := net.SplitHostPort(remote.String()); err == nil {
			if client, err := netip.ParseAddr(host); err == nil {
				server.hosts.remember(client, question.Name, response, time.Now())
			}
		}
	}
}

func (server *DNSServer) serve(current *dns.Server) {
	err := current.ActivateAndServe()
	select {
	case server.serveErr <- err:
	default:
	}
}

func (server *DNSServer) recordMatch(match Match) {
	if match.Fallback {
		server.fallback.Add(1)
	} else {
		server.classified.Add(1)
	}
	server.mu.Lock()
	server.targets[match.Target]++
	server.mu.Unlock()
}

func (server *DNSServer) writeAnswerSets(ctx context.Context, config DNSConfig, poolID string, response *dns.Msg) error {
	for _, answer := range response.Answer {
		var address netip.Addr
		switch record := answer.(type) {
		case *dns.A:
			parsed, ok := netip.AddrFromSlice(record.A)
			if !ok {
				continue
			}
			address = parsed.Unmap()
		case *dns.AAAA:
			parsed, ok := netip.AddrFromSlice(record.AAAA)
			if !ok {
				continue
			}
			address = parsed
		default:
			continue
		}
		ttl := time.Duration(answer.Header().Ttl) * time.Second
		if ttl < config.MinTTL {
			ttl = config.MinTTL
		}
		if ttl > config.MaxTTL {
			ttl = config.MaxTTL
		}
		if err := config.SetWriter.Add(ctx, poolID, address, ttl); err != nil {
			return fmt.Errorf("write DNS classification for pool %s: %w", poolID, err)
		}
		server.setWrites.Add(1)
	}
	return nil
}

func (server *DNSServer) configSnapshot() DNSConfig {
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.config
}

func normalizeListenAddress(value string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return strings.TrimSpace(value)
	}
	return net.JoinHostPort(host, port)
}

func errorReply(request *dns.Msg, rcode int) *dns.Msg {
	reply := new(dns.Msg)
	if request != nil {
		reply.SetRcode(request, rcode)
		return reply
	}
	reply.Response = true
	reply.Rcode = rcode
	return reply
}
