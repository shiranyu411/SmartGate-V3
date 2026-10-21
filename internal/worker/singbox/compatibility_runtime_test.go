package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
)

// Run with SMARTGATE_SINGBOX_BIN for each upgrade target. All peers and DNS
// are local; Linux TPROXY/UDP coverage lives in m2-worker-smoke.sh.
func TestRealCoreBootstrapSelectorRejectAndRestart(t *testing.T) {
	binary := os.Getenv("SMARTGATE_SINGBOX_BIN")
	if binary == "" {
		t.Skip("SMARTGATE_SINGBOX_BIN is required for real-core compatibility")
	}
	port := func() int {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().(*net.TCPAddr).Port
	}
	peerPort, socksPort, controlPort, unknownPort := port(), port(), port(), port()
	start := func(name string, config any) func() {
		dir := t.TempDir()
		body, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(dir, "core.log")
		log, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "run", "-c", path)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		stop := func() { once.Do(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = log.Close() }) }
		t.Cleanup(func() {
			stop()
			if t.Failed() {
				output, _ := os.ReadFile(logPath)
				t.Logf("%s: %s", name, output)
			}
		})
		return stop
	}
	start("peer", map[string]any{
		"inbounds":  []map[string]any{{"type": "socks", "listen": "127.0.0.1", "listen_port": peerPort}},
		"outbounds": []map[string]any{{"type": "direct", "tag": "direct"}},
	})
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	dnsServer := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		answer := new(dns.Msg).SetReply(request)
		for _, question := range request.Question {
			if question.Name == "peer.smartgate.test." && question.Qtype == dns.TypeA {
				queries.Add(1)
				answer.Answer = append(answer.Answer, &dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(127, 0, 0, 1)})
			}
		}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = dnsServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = dnsServer.Shutdown() })
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	request := singBoxRequest()
	request.Worker.Inbounds = request.Worker.Inbounds[1:]
	request.Worker.Inbounds[0].Port = socksPort
	request.Worker.Control.Port = controlPort
	request.Identities["node-general"] = singBoxIdentitySource("peer", map[string]any{"type": "socks", "server": "peer.smartgate.test", "server_port": peerPort})
	second := request.Executors[0]
	second.ID = "exec-second"
	request.Executors = append(request.Executors, second)
	request.Worker.Routes[0].Candidates = append(request.Worker.Routes[0].Candidates, second.ID)
	config, _, err := BuildConfig(request)
	if err != nil {
		t.Fatal(err)
	}
	config.DNS["servers"].([]map[string]any)[0]["server"] = "127.0.0.1"
	config.DNS["servers"].([]map[string]any)[0]["server_port"] = packet.LocalAddr().(*net.UDPAddr).Port
	config.Inbounds = append(config.Inbounds, map[string]any{"type": "socks", "tag": "unrouted", "listen": "127.0.0.1", "listen_port": unknownPort})
	client := clashClient{baseURL: fmt.Sprintf("http://127.0.0.1:%d", controlPort)}
	wait := func(selected string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if actual, err := client.Selected(context.Background(), "route-general-0"); err == nil && actual == selected {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("selector did not become %s", selected)
	}
	fetch := func(port int) error {
		dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil, &net.Dialer{Timeout: time.Second})
		if err != nil {
			return err
		}
		transport := &http.Transport{Dial: dialer.Dial, DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		response, err := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Get(target.URL)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			return fmt.Errorf("HTTP status %d", response.StatusCode)
		}
		return nil
	}
	stop := start("worker", config)
	wait("exec-general")
	if err := fetch(socksPort); err != nil {
		t.Fatal(err)
	}
	if queries.Load() == 0 {
		t.Fatal("proxy hostname was not resolved through the explicit bootstrap DNS")
	}
	if err := fetch(unknownPort); err == nil {
		t.Fatal("unmatched inbound escaped the terminal reject rule")
	}
	if err := client.Select(context.Background(), "route-general-0", "exec-second"); err != nil {
		t.Fatal(err)
	}
	wait("exec-second")
	if err := fetch(socksPort); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Delay(context.Background(), "exec-second", target.URL, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Connections(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop()
	// SmartGate rebuilds selectors from durable selection on restart.
	request.Worker.Routes[0].Selected = "exec-second"
	rebuilt, _, err := BuildConfig(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, outbound := range rebuilt.Outbounds {
		if outbound["type"] == "selector" {
			for _, active := range config.Outbounds {
				if active["tag"] == outbound["tag"] {
					active["default"] = outbound["default"]
				}
			}
		}
	}
	start("restarted worker", config)
	wait("exec-second")
	if err := fetch(socksPort); err != nil {
		t.Fatal(err)
	}
}
