package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"smartgate/internal/fabric"
	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

type dnsUpstreamEvent struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Remote string `json:"remote"`
	Answer string `json:"answer"`
}

type probeDNSUpstreamHandler struct {
	events chan<- dnsUpstreamEvent
	count  atomic.Uint64
}

func runDNSWorkerServe(args []string) {
	flags := flag.NewFlagSet("dns-worker-serve", flag.ExitOnError)
	count := flags.Int("count", 1, "queries to serve")
	timeout := flags.Duration("timeout", 20*time.Second, "serve timeout")
	listen := flags.String("listen", "[::]:1053", "embedded DNS listen address")
	upstream := flags.String("upstream", "198.51.100.53:5353", "DNS upstream address")
	_ = flags.Parse(args)
	if *count < 1 {
		fatalf("dns-worker-serve count must be positive")
	}
	graph := probeGraph()
	graph.DNS.Listen = *listen
	spec := state.DefaultSpecState()
	address := "tcp://" + *upstream
	spec.DNS = state.DNSSpec{Final: "proxy", DirectServers: []string{address}, ProxyServers: []string{address}}
	resolver, err := fabric.BuildDNSUpstreamRouter(spec, graph)
	if err != nil {
		fatalf("build Worker DNS upstream router: %v", err)
	}
	classifier, err := fabric.ClassifierFromGraph(graph)
	if err != nil {
		fatalf("build DNS classifier: %v", err)
	}
	nftWriter := fabric.NftSetWriterForGraph(netgate.ShellRunner{}, graph)
	server, err := fabric.NewDNSServer(fabric.DNSConfig{
		Listen: *listen, RejectHTTPSRecords: true, Classifier: classifier,
		Resolver: resolver, SetWriter: &fabric.ExclusiveSetWriter{Next: nftWriter},
	})
	if err != nil {
		fatalf("build Worker-backed DNS server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		fatalf("start Worker-backed DNS server: %v", err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	fmt.Printf("READY %s\n", server.Addr())
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		metrics := server.Metrics()
		if metrics.Queries >= uint64(*count) && metrics.SetWrites+metrics.Failures >= uint64(*count) {
			body, err := json.Marshal(metrics)
			if err != nil {
				fatalf("encode Worker DNS metrics: %v", err)
			}
			fmt.Printf("METRICS %s\n", body)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fatalf("Worker-backed DNS server timed out: %+v", server.Metrics())
}

func runDNSUpstreamServe(args []string) {
	flags := flag.NewFlagSet("dns-upstream-serve", flag.ExitOnError)
	count := flags.Int("count", 1, "queries to serve")
	timeout := flags.Duration("timeout", 20*time.Second, "serve timeout")
	listen := flags.String("listen", "198.51.100.53:5353", "upstream DNS listen address")
	_ = flags.Parse(args)
	if *count < 1 {
		fatalf("dns-upstream-serve count must be positive")
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fatalf("listen upstream DNS: %v", err)
	}
	events := make(chan dnsUpstreamEvent, *count)
	handler := &probeDNSUpstreamHandler{events: events}
	server := &dns.Server{Listener: listener, Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ActivateAndServe() }()
	fmt.Printf("READY %s\n", listener.Addr())
	encoder := json.NewEncoder(newLockedWriter())
	timer := time.NewTimer(*timeout)
	defer timer.Stop()
	for received := 0; received < *count; {
		select {
		case event := <-events:
			if err := encoder.Encode(event); err != nil {
				fatalf("encode upstream DNS event: %v", err)
			}
			received++
		case err := <-serveErr:
			fatalf("serve upstream DNS: %v", err)
		case <-timer.C:
			fatalf("upstream DNS timed out after %d/%d queries", received, *count)
		}
	}
	_ = server.Shutdown()
}

func (handler *probeDNSUpstreamHandler) ServeDNS(writer dns.ResponseWriter, request *dns.Msg) {
	response := new(dns.Msg)
	response.SetReply(request)
	if request == nil || len(request.Question) != 1 {
		response.SetRcode(request, dns.RcodeFormatError)
		_ = writer.WriteMsg(response)
		return
	}
	question := request.Question[0]
	answer := probeDNSAnswer(question)
	if answer == nil {
		response.Rcode = dns.RcodeNameError
	} else {
		response.Answer = []dns.RR{answer}
	}
	if err := writer.WriteMsg(response); err != nil {
		return
	}
	answerText := "NXDOMAIN"
	if answer != nil {
		answerText = answer.String()
	}
	handler.count.Add(1)
	handler.events <- dnsUpstreamEvent{
		Name: question.Name, Type: dns.TypeToString[question.Qtype], Remote: writer.RemoteAddr().String(), Answer: answerText,
	}
}

func probeDNSAnswer(question dns.Question) dns.RR {
	name := strings.ToLower(dns.Fqdn(question.Name))
	header := dns.RR_Header{Name: question.Name, Class: dns.ClassINET, Ttl: 60}
	switch name {
	case "probe.general.example.":
		switch question.Qtype {
		case dns.TypeA:
			header.Rrtype = dns.TypeA
			return &dns.A{Hdr: header, A: net.ParseIP("8.8.4.4")}
		case dns.TypeAAAA:
			header.Rrtype = dns.TypeAAAA
			return &dns.AAAA{Hdr: header, AAAA: net.ParseIP("2001:4860:4860::8844")}
		}
	case "probe.ai.example.":
		switch question.Qtype {
		case dns.TypeA:
			header.Rrtype = dns.TypeA
			return &dns.A{Hdr: header, A: net.ParseIP("9.9.9.9")}
		case dns.TypeAAAA:
			header.Rrtype = dns.TypeAAAA
			return &dns.AAAA{Hdr: header, AAAA: net.ParseIP("2001:db8:9::9")}
		}
	}
	return nil
}
