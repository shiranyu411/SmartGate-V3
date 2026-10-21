package worker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"smartgate/internal/state"
	"sync/atomic"
	"testing"
	"time"
)

func TestRouteVerificationConfirmsIndependentTarget(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveProbeSOCKS(conn)
		}
	}()
	inbound := state.WorkerInbound{Kind: "socks", Listen: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer primary.Close()
	var hits atomic.Int32
	alternate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(204) }))
	defer alternate.Close()
	if err := verifySOCKSRouteTargets(context.Background(), inbound, []string{primary.URL, alternate.URL}, time.Second); err != nil || hits.Load() != 1 {
		t.Fatalf("alternate did not confirm route: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifySOCKSRouteTargets(ctx, inbound, []string{primary.URL, alternate.URL}, time.Second); err != context.Canceled || hits.Load() != 1 {
		t.Fatalf("canceled verification retried: %v", err)
	}
}

func TestLatencyProbeAcceptsOriginStatusWithoutWeakeningTLSOrManualChecks(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveProbeSOCKS(conn)
		}
	}()
	inbound := state.WorkerInbound{Kind: "socks", Listen: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer origin.Close()
	if err := MeasureSOCKSRoute(context.Background(), inbound, origin.URL, time.Second); err != nil {
		t.Fatalf("origin status fabricated connectivity failure: %v", err)
	}
	if err := VerifySOCKSRoute(context.Background(), inbound, origin.URL, time.Second); err == nil {
		t.Fatal("explicit manual health check lost its HTTP status validation")
	}
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer untrusted.Close()
	if err := MeasureSOCKSRoute(context.Background(), inbound, untrusted.URL, time.Second); err == nil {
		t.Fatal("latency probe accepted an untrusted certificate")
	}
}
