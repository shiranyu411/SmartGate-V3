package worker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"smartgate/internal/smartnode"
)

type batchProbeAdapter struct {
	*fakeAdapter
	requests []CompileRequest
}

func (a *batchProbeAdapter) Descriptor() Descriptor {
	d := a.fakeAdapter.Descriptor()
	d.Control.Kind = "http"
	d.Process.Binary, _ = os.Executable()
	d.Process.CheckArgs = func(path string) []string { return []string{"-test.run=^TestNodeProbeChild$", "--", "check", path} }
	d.Process.StartArgs = func(path string) []string { return []string{"-test.run=^TestNodeProbeChild$", "--", "serve", path} }
	return d
}
func (a *batchProbeAdapter) Compile(request CompileRequest) (CandidateConfig, error) {
	a.requests = append(a.requests, request)
	return CandidateConfig{Value: request}, nil
}

// A child process exercises actual config/check/start/kill and independent
// loopback SOCKS routes, without relying on an installed production core.
func TestNodeProbeChild(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		return
	}
	if args[1] == "check" {
		os.Exit(0)
	}
	body, err := os.ReadFile(args[2])
	if err != nil {
		os.Exit(2)
	}
	var request CompileRequest
	if json.Unmarshal(body, &request) != nil {
		os.Exit(3)
	}
	for _, inbound := range request.Worker.Inbounds {
		listener, err := net.Listen("tcp", net.JoinHostPort(inbound.Listen, fmt.Sprint(inbound.Port)))
		if err != nil {
			os.Exit(4)
		}
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go serveProbeSOCKS(conn)
			}
		}()
	}
	select {}
}
func serveProbeSOCKS(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}
	var host string
	switch request[3] {
	case 1:
		raw := make([]byte, 4)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	case 3:
		raw := make([]byte, 1)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		name := make([]byte, int(raw[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		host = string(name)
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port))), time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	go io.Copy(upstream, conn)
	_, _ = io.Copy(conn, upstream)
}

func TestNodeProbeBatchMeasuresUnboundNodesAndCleansProcesses(t *testing.T) {
	a := &batchProbeAdapter{fakeAdapter: &fakeAdapter{core: "batch-a"}}
	b := &batchProbeAdapter{fakeAdapter: &fakeAdapter{core: "batch-b"}}
	manager, err := NewManager(a, b)
	if err != nil {
		t.Fatal(err)
	}
	var targets []NodeProbeTarget
	for i := 0; i < 30; i++ {
		core := smartnode.CoreID("batch-a")
		if i%2 == 1 {
			core = "batch-b"
		}
		targets = append(targets, NodeProbeTarget{Node: smartnode.SmartNode{ID: fmt.Sprintf("node-%d", i), Revision: 2, CoreChoice: core}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	batch, err := manager.StartNodeProbeBatch(ctx, targets)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	if len(batch.processes) != 2 || len(batch.inbounds) != 30 {
		t.Fatalf("incorrect batch shape: processes=%d routes=%d", len(batch.processes), len(batch.inbounds))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := batch.Delay(ctx, id, server.URL, time.Second); err != nil {
				t.Errorf("%s: %v", id, err)
			}
		}(target.Node.ID)
	}
	wg.Wait()
	for _, adapter := range []*batchProbeAdapter{a, b} {
		if len(adapter.requests) != 1 || len(adapter.requests[0].Executors) != 15 || len(adapter.installed) != 0 || len(adapter.activated) != 0 {
			t.Fatalf("batch modified live worker: %+v", adapter)
		}
		for _, route := range adapter.requests[0].Worker.Routes {
			if len(route.Candidates) != 1 || route.Candidates[0] != route.Selected {
				t.Fatal("probe route is not isolated")
			}
		}
	}
	directory := batch.directory
	batch.Close()
	batch.Close()
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("temporary config remains: %v", err)
	}
	for _, inbound := range batch.inbounds {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(inbound.Listen, fmt.Sprint(inbound.Port)), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatal("probe listener remains after close")
		}
	}
	if len(nodeProbeSlot) != 0 {
		t.Fatal("probe slot leaked")
	}
}

func TestNodeProbeBatchSetupFailureReleasesSlot(t *testing.T) {
	manager, err := NewManager(&fakeAdapter{core: "unsupported"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		_, err := manager.StartNodeProbeBatch(ctx, []NodeProbeTarget{{Node: smartnode.SmartNode{ID: "node", CoreChoice: "unsupported"}}})
		if err == nil || !strings.Contains(err.Error(), "no isolated probe compiler") {
			t.Fatalf("unexpected setup error: %v", err)
		}
		if len(nodeProbeSlot) != 0 {
			t.Fatal("failed setup leaked slot")
		}
	}
}
