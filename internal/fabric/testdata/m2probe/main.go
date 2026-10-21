package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"

	"smartgate/internal/fabric"
	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

const (
	singBoxPort = 16001
	xrayPort    = 17001

	ipTransparent          = 19
	ipRecvOriginalDst      = 20
	ipv6RecvOriginalDst    = 74
	ipv6Transparent        = 75
	originalDstControlSize = 256
)

type probeEvent struct {
	Worker   string `json:"worker"`
	Network  string `json:"network"`
	Payload  string `json:"payload"`
	Original string `json:"original"`
}

type probeResolver struct{}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: m2probe {export|worker-export|listen|send|dns-serve|dns-worker-serve|dns-upstream-serve|dns-query|echo-serve|roundtrip|load|wait-tcp}")
	}
	switch os.Args[1] {
	case "clock-ticks", "cpu-count", "unix-nano":
		runSystemMetric(os.Args[1])
	case "regression-http":
		runRegressionHTTP(os.Args[2:])
	case "regression-tcp":
		runRegressionTCP(os.Args[2:])
	case "regression-fixture":
		runRegressionFixture(os.Args[2:])
	case "export":
		runExport(os.Args[2:])
	case "worker-export":
		runWorkerExport(os.Args[2:])
	case "listen":
		runListen(os.Args[2:])
	case "send":
		runSend(os.Args[2:])
	case "dns-serve":
		runDNSServe(os.Args[2:])
	case "dns-worker-serve":
		runDNSWorkerServe(os.Args[2:])
	case "dns-upstream-serve":
		runDNSUpstreamServe(os.Args[2:])
	case "dns-query":
		runDNSQuery(os.Args[2:])
	case "echo-serve":
		runEchoServe(os.Args[2:])
	case "roundtrip":
		runRoundTrip(os.Args[2:])
	case "load":
		runLoad(os.Args[2:])
	case "wait-tcp":
		runWaitTCP(os.Args[2:])
	default:
		fatalf("unknown mode %q", os.Args[1])
	}
}

func runExport(args []string) {
	flags := flag.NewFlagSet("export", flag.ExitOnError)
	output := flags.String("output", "", "output nft file")
	_ = flags.Parse(args)
	if strings.TrimSpace(*output) == "" {
		fatalf("export output is required")
	}
	plan, err := fabric.BuildDispatchDryRunPlan(
		"m2-kernel-smoke",
		netgate.NewOwnerRegistry("smartgate"),
		probeGraph(),
		fabric.DispatchPlanOptions{LANDevice: "sglan0", DNSListenPort: 1053, ManagementPort: 3000},
	)
	if err != nil {
		fatalf("build Fabric plan: %v", err)
	}
	if len(plan.Steps) == 0 || len(plan.Steps[0].Commands) == 0 {
		fatalf("Fabric plan did not contain a reconcile command")
	}
	argv := plan.Steps[0].Commands[0].Argv
	if len(argv) == 0 {
		fatalf("Fabric reconcile command is empty")
	}
	reconcile := argv[len(argv)-1]
	const startMarker = "cat >\"$table_file\" <<'SMARTGATE_NFT'\n"
	const endMarker = "\nSMARTGATE_NFT\n"
	start := strings.Index(reconcile, startMarker)
	if start < 0 {
		fatalf("Fabric reconcile script is missing nft start marker")
	}
	start += len(startMarker)
	end := strings.Index(reconcile[start:], endMarker)
	if end < 0 {
		fatalf("Fabric reconcile script is missing nft end marker")
	}
	nftScript := strings.ReplaceAll(reconcile[start:start+end], "__SMARTGATE_LAN_DEVICE__", "sglan0") + "\n"
	if err := os.WriteFile(*output, []byte(nftScript), 0o600); err != nil {
		fatalf("write nft script: %v", err)
	}
	fmt.Println(*output)
}

func runListen(args []string) {
	flags := flag.NewFlagSet("listen", flag.ExitOnError)
	count := flags.Int("count", 1, "events to capture")
	timeout := flags.Duration("timeout", 15*time.Second, "capture timeout")
	_ = flags.Parse(args)
	if *count < 1 {
		fatalf("listen count must be positive")
	}

	type listenerSpec struct {
		network string
		address string
		worker  string
	}
	specs := []listenerSpec{
		{network: "udp4", address: fmt.Sprintf("0.0.0.0:%d", singBoxPort), worker: "w_sb_1"},
		{network: "udp6", address: fmt.Sprintf("[::]:%d", singBoxPort), worker: "w_sb_1"},
		{network: "udp4", address: fmt.Sprintf("0.0.0.0:%d", xrayPort), worker: "w_xr_1"},
		{network: "udp6", address: fmt.Sprintf("[::]:%d", xrayPort), worker: "w_xr_1"},
	}
	listeners := make([]*net.UDPConn, 0, len(specs))
	events := make(chan probeEvent, *count)
	errorsCh := make(chan error, len(specs))
	for _, spec := range specs {
		listener, err := listenTransparentUDP(spec.network, spec.address)
		if err != nil {
			closeUDPListeners(listeners)
			fatalf("listen %s for %s: %v", spec.network, spec.worker, err)
		}
		listeners = append(listeners, listener)
		go readProbeEvents(listener, spec.network, spec.worker, events, errorsCh)
	}
	defer closeUDPListeners(listeners)
	fmt.Println("READY")
	encoder := json.NewEncoder(os.Stdout)
	timer := time.NewTimer(*timeout)
	defer timer.Stop()
	for received := 0; received < *count; {
		select {
		case event := <-events:
			if err := encoder.Encode(event); err != nil {
				fatalf("encode event: %v", err)
			}
			received++
		case err := <-errorsCh:
			fatalf("read transparent packet: %v", err)
		case <-timer.C:
			fatalf("timed out after receiving %d/%d events", received, *count)
		}
	}
}

func runSend(args []string) {
	flags := flag.NewFlagSet("send", flag.ExitOnError)
	network := flags.String("network", "udp4", "udp4 or udp6")
	destination := flags.String("destination", "", "destination address")
	payload := flags.String("payload", "probe", "payload")
	count := flags.Int("count", 1, "packet count")
	_ = flags.Parse(args)
	if *network != "udp4" && *network != "udp6" {
		fatalf("unsupported send network %q", *network)
	}
	if *destination == "" || *count < 1 {
		fatalf("send destination and positive count are required")
	}
	remote, err := net.ResolveUDPAddr(*network, *destination)
	if err != nil {
		fatalf("resolve destination: %v", err)
	}
	connection, err := net.DialUDP(*network, nil, remote)
	if err != nil {
		fatalf("dial destination: %v", err)
	}
	defer connection.Close()
	for index := 0; index < *count; index++ {
		current := *payload
		if *count > 1 {
			current = fmt.Sprintf("%s-%d", *payload, index)
		}
		if _, err := connection.Write([]byte(current)); err != nil {
			fatalf("write packet %d: %v", index, err)
		}
	}
	fmt.Printf("sent %d\n", *count)
}

func runDNSServe(args []string) {
	flags := flag.NewFlagSet("dns-serve", flag.ExitOnError)
	count := flags.Int("count", 1, "queries to serve")
	timeout := flags.Duration("timeout", 15*time.Second, "serve timeout")
	_ = flags.Parse(args)
	graph := probeGraph()
	classifier, err := fabric.ClassifierFromGraph(graph)
	if err != nil {
		fatalf("build DNS classifier: %v", err)
	}
	nftWriter := fabric.NftSetWriterForGraph(netgate.ShellRunner{}, graph)
	server, err := fabric.NewDNSServer(fabric.DNSConfig{
		Listen: "127.0.0.1:1053", RejectHTTPSRecords: true,
		Classifier: classifier, Resolver: probeResolver{}, SetWriter: &fabric.ExclusiveSetWriter{Next: nftWriter},
	})
	if err != nil {
		fatalf("build DNS server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		fatalf("start DNS server: %v", err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	fmt.Printf("READY %s\n", server.Addr())
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		metrics := server.Metrics()
		if metrics.Queries >= uint64(*count) && metrics.SetWrites+metrics.Failures >= uint64(*count) {
			body, err := json.Marshal(metrics)
			if err != nil {
				fatalf("encode DNS metrics: %v", err)
			}
			fmt.Printf("METRICS %s\n", body)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fatalf("DNS server timed out: %+v", server.Metrics())
}

func runDNSQuery(args []string) {
	flags := flag.NewFlagSet("dns-query", flag.ExitOnError)
	network := flags.String("network", "udp", "udp or tcp")
	serverAddress := flags.String("server", "127.0.0.1:1053", "DNS server address")
	name := flags.String("name", "probe.ai.example.", "query name")
	recordType := flags.String("type", "A", "A, AAAA, HTTPS or SVCB")
	expectRcode := flags.Int("expect-rcode", dns.RcodeSuccess, "expected response code")
	expectAnswers := flags.Int("expect-answers", 1, "expected answer count")
	_ = flags.Parse(args)
	questionType, ok := dns.StringToType[strings.ToUpper(*recordType)]
	if !ok {
		fatalf("unsupported DNS type %q", *recordType)
	}
	request := new(dns.Msg)
	request.SetQuestion(dns.Fqdn(*name), questionType)
	response, _, err := (&dns.Client{Net: *network, Timeout: 2 * time.Second}).Exchange(request, *serverAddress)
	if err != nil {
		fatalf("DNS query: %v", err)
	}
	if response.Rcode != *expectRcode || len(response.Answer) != *expectAnswers {
		fatalf("unexpected DNS response rcode=%d answers=%d", response.Rcode, len(response.Answer))
	}
	if len(response.Answer) > 0 {
		fmt.Printf("ANSWER %s\n", response.Answer[0].String())
	} else {
		fmt.Printf("DNS rcode=%d answers=0\n", response.Rcode)
	}
}

func (probeResolver) Exchange(_ context.Context, target string, request *dns.Msg) (*dns.Msg, error) {
	if target != "ai" {
		return nil, fmt.Errorf("unexpected DNS target %s", target)
	}
	if request == nil || len(request.Question) != 1 {
		return nil, errors.New("invalid DNS request")
	}
	question := request.Question[0]
	response := new(dns.Msg)
	response.SetReply(request)
	switch question.Qtype {
	case dns.TypeA:
		response.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("9.9.9.9"),
		}}
	case dns.TypeAAAA:
		response.Answer = []dns.RR{&dns.AAAA{
			Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60},
			AAAA: net.ParseIP("2001:4860:4860::8888"),
		}}
	default:
		return nil, fmt.Errorf("unsupported DNS query type %d", question.Qtype)
	}
	return response, nil
}

func listenTransparentUDP(network, address string) (*net.UDPConn, error) {
	listenConfig := net.ListenConfig{Control: func(_ string, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fileDescriptor uintptr) {
			fd := int(fileDescriptor)
			if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
				socketErr = err
				return
			}
			if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20); err != nil {
				socketErr = err
				return
			}
			switch network {
			case "udp4":
				if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, ipTransparent, 1); err != nil {
					socketErr = err
					return
				}
				socketErr = syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, ipRecvOriginalDst, 1)
			case "udp6":
				if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1); err != nil {
					socketErr = err
					return
				}
				if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, ipv6Transparent, 1); err != nil {
					socketErr = err
					return
				}
				socketErr = syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, ipv6RecvOriginalDst, 1)
			default:
				socketErr = fmt.Errorf("unsupported listen network %s", network)
			}
		}); err != nil {
			return err
		}
		return socketErr
	}}
	packetConnection, err := listenConfig.ListenPacket(context.Background(), network, address)
	if err != nil {
		return nil, err
	}
	udpConnection, ok := packetConnection.(*net.UDPConn)
	if !ok {
		_ = packetConnection.Close()
		return nil, fmt.Errorf("unexpected packet connection type %T", packetConnection)
	}
	return udpConnection, nil
}

func readProbeEvents(connection *net.UDPConn, network, worker string, events chan<- probeEvent, errorsCh chan<- error) {
	payload := make([]byte, 2048)
	control := make([]byte, originalDstControlSize)
	for {
		payloadBytes, controlBytes, flags, _, err := connection.ReadMsgUDP(payload, control)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				errorsCh <- err
			}
			return
		}
		if flags&syscall.MSG_CTRUNC != 0 {
			errorsCh <- errors.New("original destination control data was truncated")
			return
		}
		original, err := parseOriginalDestination(network, control[:controlBytes])
		if err != nil {
			errorsCh <- err
			return
		}
		events <- probeEvent{Worker: worker, Network: network, Payload: string(payload[:payloadBytes]), Original: original.String()}
	}
}

func parseOriginalDestination(network string, control []byte) (netip.AddrPort, error) {
	messages, err := syscall.ParseSocketControlMessage(control)
	if err != nil {
		return netip.AddrPort{}, err
	}
	for _, message := range messages {
		switch network {
		case "udp4":
			if message.Header.Level != syscall.IPPROTO_IP || message.Header.Type != ipRecvOriginalDst || len(message.Data) < 8 {
				continue
			}
			address := netip.AddrFrom4([4]byte{message.Data[4], message.Data[5], message.Data[6], message.Data[7]})
			return netip.AddrPortFrom(address, binary.BigEndian.Uint16(message.Data[2:4])), nil
		case "udp6":
			if message.Header.Level != syscall.IPPROTO_IPV6 || message.Header.Type != ipv6RecvOriginalDst || len(message.Data) < 24 {
				continue
			}
			var rawAddress [16]byte
			copy(rawAddress[:], message.Data[8:24])
			return netip.AddrPortFrom(netip.AddrFrom16(rawAddress), binary.BigEndian.Uint16(message.Data[2:4])), nil
		}
	}
	return netip.AddrPort{}, fmt.Errorf("%s packet did not include original destination", network)
}

func closeUDPListeners(listeners []*net.UDPConn) {
	for _, listener := range listeners {
		_ = listener.Close()
	}
}

func probeGraph() state.FabricGraph {
	return state.FabricGraph{
		Takeover: state.FabricTakeoverGraph{Enabled: true, TCP: true, UDP: true, IPv4: true, IPv6: true},
		DNS: state.FabricDNSGraph{
			Listen: "[::]:1053", RedirectLAN53: true, RejectHTTPSRecords: true, BlockDoT853: true,
			PoolSets: map[string]state.FabricPoolSets{
				"general":           {IPv4: "sg_general_v4", IPv6: "sg_general_v6"},
				"ai":                {IPv4: "sg_ai_v4", IPv6: "sg_ai_v6"},
				fabric.TargetDirect: {IPv4: "sg_direct_v4", IPv6: "sg_direct_v6"},
				fabric.TargetBlock:  {IPv4: "sg_block_v4", IPv6: "sg_block_v6"},
			},
		},
		Rules: []state.FabricRuleGraph{
			{ID: "ai-domain", Source: "user", Kind: fabric.RuleDomainSuffix, Value: "ai.example", Target: "ai", Priority: 500},
			{ID: "ai-v4", Source: "user", Kind: fabric.RuleIPCIDR, Value: "203.0.113.0/24", Target: "ai", Priority: 500},
			{ID: "ai-v6", Source: "user", Kind: fabric.RuleIPCIDR, Value: "2606:4700:4700::/48", Target: "ai", Priority: 500},
		},
		FallbackPool: "general",
		Dispatch: []state.PoolDispatchGraph{
			{Pool: "general", Binding: "pb_general", Generation: 1, Mark: 0x50000100, MarkMask: 0xffffff00, Table: 1001, Port: singBoxPort, DNSPort: 16002, Target: "w_sb_1"},
			{Pool: "ai", Binding: "pb_ai", Generation: 1, Mark: 0x50000200, MarkMask: 0xffffff00, Table: 1002, Port: xrayPort, DNSPort: 17002, Target: "w_xr_1"},
			{Pool: fabric.TargetDirect, Binding: "pb_direct", Generation: 1, Mark: 0x50000300, MarkMask: 0xffffff00, Table: 1003, Target: fabric.TargetDirect},
			{Pool: fabric.TargetBlock, Binding: "pb_block", Generation: 1, Mark: 0x50000400, MarkMask: 0xffffff00, Table: 1004, Target: fabric.TargetBlock},
		},
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
