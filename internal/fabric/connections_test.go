package fabric

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"smartgate/internal/state"
)

type connectionRunner struct {
	output string
	calls  [][]string
}

func (runner *connectionRunner) Run(_ context.Context, argv []string) (string, error) {
	runner.calls = append(runner.calls, append([]string(nil), argv...))
	return runner.output, nil
}

func TestParseConntrackListSupportsExtendedIPv4IPv6TCPUDPAndDeduplicates(t *testing.T) {
	tcpLine := "ipv4 2 tcp 6 431999 ESTABLISHED src=192.168.1.20 dst=93.184.216.34 sport=54321 dport=443 packets=4 bytes=512 src=93.184.216.34 dst=192.168.1.20 sport=443 dport=54321 packets=6 bytes=2048 [ASSURED] mark=0x1207 use=1 id=101"
	udpLine := "ipv6 10 udp 17 29 src=fd00::2 dst=2606:4700:4700::1111 sport=53000 dport=53 packets=1 bytes=64 src=2606:4700:4700::1111 dst=fd00::2 sport=53 dport=53000 packets=1 bytes=96 mark=0x1207 use=1 id=102"
	connections, err := ParseConntrackList(strings.Join([]string{tcpLine, tcpLine, udpLine}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 2 {
		t.Fatalf("connections = %+v", connections)
	}
	tcp := connections[0]
	if tcp.Protocol != "tcp" || tcp.State != "ESTABLISHED" || tcp.SourceIP != "192.168.1.20" || tcp.DestinationIP != "93.184.216.34" {
		t.Fatalf("unexpected TCP tuple: %+v", tcp)
	}
	if tcp.SourcePort != "54321" || tcp.DestinationPort != "443" || tcp.ReplySourcePort != "443" || tcp.ReplyDestPort != "54321" {
		t.Fatalf("unexpected TCP ports: %+v", tcp)
	}
	if tcp.Upload != 512 || tcp.Download != 2048 || tcp.ID == "" {
		t.Fatalf("unexpected TCP counters: %+v", tcp)
	}
	udp := connections[1]
	if udp.Protocol != "udp" || udp.State != "" || udp.SourceIP != "fd00::2" || udp.DestinationIP != "2606:4700:4700::1111" {
		t.Fatalf("unexpected UDP tuple: %+v", udp)
	}
	if udp.ReplySourceIP != "2606:4700:4700::1111" || udp.ReplyDestIP != "fd00::2" || udp.Upload != 64 || udp.Download != 96 || udp.ID == tcp.ID {
		t.Fatalf("unexpected UDP reply/counters: %+v", udp)
	}
}

func TestParseConntrackListRejectsMalformedEntries(t *testing.T) {
	tests := map[string]string{
		"unsupported protocol": "ipv4 2 icmp 1 10 src=192.0.2.1 dst=192.0.2.2",
		"missing destination":  "tcp 6 10 ESTABLISHED src=192.0.2.1 sport=1234 dport=443",
		"invalid source IP":    "tcp 6 10 ESTABLISHED src=not-an-ip dst=192.0.2.2 sport=1234 dport=443",
		"invalid port":         "udp 17 10 src=192.0.2.1 dst=192.0.2.2 sport=0 dport=53",
		"invalid bytes":        "udp 17 10 src=192.0.2.1 dst=192.0.2.2 sport=1234 dport=53 bytes=nope src=192.0.2.2 dst=192.0.2.1 sport=53 dport=1234 bytes=1",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConntrackList(input); err == nil {
				t.Fatalf("expected error for %q", input)
			}
		})
	}
}

func TestShellConnectionTrackerListUsesMarkedExtendedOutput(t *testing.T) {
	runner := &connectionRunner{output: "tcp 6 10 ESTABLISHED src=192.0.2.1 dst=192.0.2.2 sport=1234 dport=443 src=192.0.2.2 dst=192.0.2.1 sport=443 dport=1234"}
	dispatch := testConnectionDispatch()
	connections, err := (ShellConnectionTracker{Runner: runner}).List(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 1 {
		t.Fatalf("connections = %+v", connections)
	}
	expected := []string{"conntrack", "-L", "-m", "mark", "--mark", "0x00001207/0x0000ffff", "-o", "id,extended"}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], expected) {
		t.Fatalf("argv = %+v", runner.calls)
	}
}

func TestShellConnectionTrackerCloseOneUsesExactTupleAndMark(t *testing.T) {
	runner := &connectionRunner{}
	dispatch := testConnectionDispatch()
	connection := TrackedConnection{
		Protocol: "tcp", SourceIP: "192.0.2.1", DestinationIP: "2001:db8::2",
		SourcePort: "23456", DestinationPort: "443",
	}
	closed, err := (ShellConnectionTracker{Runner: runner}).CloseOne(context.Background(), dispatch, connection)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d", closed)
	}
	expected := []string{
		"conntrack", "-D", "-p", "tcp", "-s", "192.0.2.1", "-d", "2001:db8::2",
		"-m", "mark", "--mark", "0x00001207/0x0000ffff", "--sport", "23456", "--dport", "443",
	}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], expected) {
		t.Fatalf("argv = %+v", runner.calls)
	}
}

func testConnectionDispatch() state.PoolDispatchGraph {
	return state.PoolDispatchGraph{Pool: "ai", Binding: "binding-ai", Generation: 7, Mark: 0x1207, MarkMask: 0xffff}
}
