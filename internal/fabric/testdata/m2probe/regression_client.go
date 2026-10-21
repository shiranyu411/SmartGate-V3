package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"time"
)

func runRegressionHTTP(args []string) {
	flags := flag.NewFlagSet("regression-http", flag.ExitOnError)
	target := flags.String("url", "https://www.gstatic.com/generate_204", "test URL")
	socks := flags.String("socks", "", "optional Worker SOCKS address")
	dnsServer := flags.String("dns", "", "optional client DNS server")
	hold := flags.Bool("hold", false, "wait for an established connection to close")
	expected := flags.Int("status", 204, "expected HTTP status")
	expectedNode := flags.String("node", "", "expected fixture node")
	_ = flags.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if *dnsServer != "" {
		dialer.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, *dnsServer)
		}}
	}
	var dial func(context.Context, string, string) (net.Conn, error) = dialer.DialContext
	if *socks != "" {
		socksDialer, err := proxy.SOCKS5("tcp", *socks, nil, dialer)
		if err != nil {
			fatalf("create SOCKS client: %v", err)
		}
		dial = socksDialer.(proxy.ContextDialer).DialContext
	}
	transport := &http.Transport{Proxy: nil, DialContext: dial, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, *target, nil)
	if err != nil {
		fatalf("HTTP request: %v", err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		fatalf("verified HTTP request: %v", err)
	}
	defer response.Body.Close()
	node := response.Header.Get("X-SmartGate-Fixture-Node")
	if response.StatusCode != *expected || (*expectedNode != "" && node != *expectedNode) {
		fatalf("HTTP status=%d node=%s expected_status=%d expected_node=%s", response.StatusCode, node, *expected, *expectedNode)
	}
	if !*hold {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		fmt.Printf("HTTP status=%d node=%s tls_verified=%t\n", response.StatusCode, node, response.TLS != nil)
		return
	}
	fmt.Printf("HOLD_READY node=%s tls_verified=%t\n", node, response.TLS != nil)
	started := time.Now()
	_, err = io.Copy(io.Discard, response.Body)
	if ctx.Err() != nil {
		fatalf("old connection did not close before deadline: %v", ctx.Err())
	}
	fmt.Printf("HOLD_CLOSED elapsed_ms=%d error=%v\n", time.Since(started).Milliseconds(), err)
}

func runRegressionTCP(args []string) {
	flags := flag.NewFlagSet("regression-tcp", flag.ExitOnError)
	address := flags.String("address", "9.9.9.9:853", "TCP destination")
	refused := flags.Bool("expect-refused", false, "expect blocking")
	_ = flags.Parse(args)
	started := time.Now()
	conn, err := net.DialTimeout("tcp", *address, 2*time.Second)
	if conn != nil {
		_ = conn.Close()
	}
	if *refused {
		if err == nil || time.Since(started) > time.Second {
			fatalf("TCP was not rejected promptly: %v", err)
		}
		fmt.Println("TCP blocked promptly")
	} else if err != nil {
		fatalf("TCP connect: %v", err)
	} else {
		fmt.Println("TCP connected")
	}
}
