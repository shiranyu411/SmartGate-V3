package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// This fixture terminates proxy requests locally. A VM-only CA keeps normal
// TLS certificate verification enabled for the public probe hostnames.
func runRegressionFixture(args []string) {
	flags := flag.NewFlagSet("regression-fixture", flag.ExitOnError)
	listen := flags.String("listen", "127.0.0.1:13080", "SOCKS fixture address")
	certPath := flags.String("tls-cert", "", "test TLS certificate")
	keyPath := flags.String("tls-key", "", "test TLS private key")
	modePath := flags.String("mode-file", "", "failure controls JSON file")
	dnsListen := flags.String("dns-listen", "127.0.0.1:15353", "DNS fixture address")
	_ = flags.Parse(args)
	cert, err := tls.LoadX509KeyPair(*certPath, *keyPath)
	if err != nil {
		fatalf("load fixture certificate: %v", err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) { _ = w.WriteMsg(regressionDNSAnswer(r, true)) })
	for _, network := range []string{"udp", "tcp"} {
		go func(network string) {
			server := &dns.Server{Addr: *dnsListen, Net: network, Handler: handler}
			if err := server.ListenAndServe(); err != nil {
				fatalf("fixture DNS: %v", err)
			}
		}(network)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fatalf("fixture listen: %v", err)
	}
	fmt.Printf("READY regression-fixture socks=%s dns=%s\n", *listen, *dnsListen)
	for {
		conn, err := listener.Accept()
		if err != nil {
			fatalf("fixture accept: %v", err)
		}
		go serveRegressionSOCKS(conn, config, *modePath)
	}
}

func regressionDNSAnswer(request *dns.Msg, polluted bool) *dns.Msg {
	answer := new(dns.Msg)
	answer.SetReply(request)
	if len(request.Question) != 1 {
		return answer
	}
	q := request.Question[0]
	switch q.Qtype {
	case dns.TypeA:
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: regressionFixtureIPv4(q.Name, polluted)}}
	case dns.TypeAAAA:
		answer.Answer = []dns.RR{&dns.AAAA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 30}, AAAA: net.ParseIP("2606:4700:4700::1111")}}
	}
	return answer
}

func serveRegressionSOCKS(conn net.Conn, config *tls.Config, modePath string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	auth := byte(0)
	for _, method := range methods {
		if method == 2 {
			auth = 2
		}
	}
	if _, err := conn.Write([]byte{5, auth}); err != nil {
		return
	}
	username := "anonymous"
	if auth == 2 {
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		name := make([]byte, int(head[1]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		username = string(name)
		one := make([]byte, 1)
		if _, err := io.ReadFull(conn, one); err != nil {
			return
		}
		password := make([]byte, int(one[0]))
		if _, err := io.ReadFull(conn, password); err != nil {
			return
		}
		if string(password) != "fixture-secret" {
			_, _ = conn.Write([]byte{1, 1})
			return
		}
		if _, err := conn.Write([]byte{1, 0}); err != nil {
			return
		}
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
	case 4:
		raw := make([]byte, 16)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	case 3:
		one := make([]byte, 1)
		if _, err := io.ReadFull(conn, one); err != nil {
			return
		}
		raw := make([]byte, int(one[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = string(raw)
	default:
		return
	}
	_ = host
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)
	if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	var stream net.Conn = conn
	if port == 443 || port == 853 {
		secured := tls.Server(conn, config)
		if secured.Handshake() != nil {
			return
		}
		stream = secured
	}
	req, err := http.ReadRequest(bufio.NewReader(stream))
	if err != nil {
		return
	}
	defer req.Body.Close()
	if strings.HasPrefix(req.URL.Path, "/dns-query") {
		var raw []byte
		if req.Method == http.MethodGet {
			raw, _ = base64.RawURLEncoding.DecodeString(req.URL.Query().Get("dns"))
		} else {
			raw, _ = io.ReadAll(io.LimitReader(req.Body, 65536))
		}
		message := new(dns.Msg)
		if message.Unpack(raw) != nil {
			return
		}
		body, _ := regressionDNSAnswer(message, false).Pack()
		writeRegressionResponse(stream, 200, "application/dns-message", body, username)
		return
	}
	if req.URL.Path == "/hold" {
		if _, err := fmt.Fprintf(stream, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nX-SmartGate-Fixture-Node: %s\r\n\r\n", username); err != nil {
			return
		}
		for {
			if _, err := io.WriteString(stream, "1\r\nx\r\n"); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	var modes struct {
		DelayMS     int      `json:"delay_ms"`
		PrimaryFail []string `json:"primary_fail"`
		AllFail     []string `json:"all_fail"`
	}
	if modePath != "" {
		body, _ := os.ReadFile(modePath)
		_ = json.Unmarshal(body, &modes)
	}
	contains := func(items []string) bool {
		for _, item := range items {
			if item == username || item == "*" {
				return true
			}
		}
		return false
	}
	if modes.DelayMS > 0 {
		time.Sleep(time.Duration(modes.DelayMS) * time.Millisecond)
	}
	status := 204
	if strings.Contains(req.Host, "cloudflare.com") {
		status = 200
	}
	if contains(modes.AllFail) || (contains(modes.PrimaryFail) && strings.Contains(req.Host, "gstatic.com")) {
		status = 503
	}
	if len(modes.PrimaryFail)+len(modes.AllFail) > 0 {
		fmt.Printf("FIXTURE node=%s host=%s status=%d\n", username, req.Host, status)
	}
	if status == 503 {
		return
	}
	body := []byte{}
	if status != 204 {
		body = []byte("fixture-node=" + username + "\n")
	}
	writeRegressionResponse(stream, status, "text/plain", body, username)
}

func writeRegressionResponse(writer io.Writer, status int, contentType string, body []byte, node string) {
	response := http.Response{StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)), Close: true}
	response.Header.Set("Content-Type", contentType)
	response.Header.Set("X-SmartGate-Fixture-Node", node)
	_ = response.Write(writer)
}

func regressionFixtureIPv4(name string, polluted bool) net.IP {
	if polluted && (strings.Contains(name, "gstatic.com") || strings.Contains(name, "google.com")) {
		return net.ParseIP("127.0.0.1")
	}
	return net.ParseIP("9.9.9.9")
}
