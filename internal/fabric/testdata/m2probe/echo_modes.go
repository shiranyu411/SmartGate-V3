package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type echoEvent struct {
	Network string `json:"network"`
	Payload string `json:"payload"`
	Local   string `json:"local"`
	Remote  string `json:"remote"`
}

type lockedWriter struct {
	mu sync.Mutex
}

func newLockedWriter() *lockedWriter { return &lockedWriter{} }

func (writer *lockedWriter) Write(body []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return os.Stdout.Write(body)
}

func runEchoServe(args []string) {
	flags := flag.NewFlagSet("echo-serve", flag.ExitOnError)
	count := flags.Int("count", 8, "round trips to serve")
	timeout := flags.Duration("timeout", 30*time.Second, "serve timeout")
	quiet := flags.Bool("quiet", false, "suppress per-request event output")
	_ = flags.Parse(args)
	if *count < 1 {
		fatalf("echo-serve count must be positive")
	}
	endpoints := []struct {
		network string
		address string
	}{
		{network: "tcp4", address: "8.8.4.4:18080"},
		{network: "tcp4", address: "9.9.9.9:18080"},
		{network: "tcp6", address: "[2001:4860:4860::8844]:18080"},
		{network: "tcp6", address: "[2001:db8:9::9]:18080"},
		{network: "udp4", address: "8.8.4.4:443"},
		{network: "udp4", address: "9.9.9.9:443"},
		{network: "udp6", address: "[2001:4860:4860::8844]:443"},
		{network: "udp6", address: "[2001:db8:9::9]:443"},
	}
	events := make(chan echoEvent, *count)
	errorsCh := make(chan error, len(endpoints))
	closers := make([]io.Closer, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.network[:3] == "tcp" {
			listener, err := net.Listen(endpoint.network, endpoint.address)
			if err != nil {
				closeAll(closers)
				fatalf("listen echo %s %s: %v", endpoint.network, endpoint.address, err)
			}
			closers = append(closers, listener)
			go serveTCPEcho(listener, endpoint.network, events, errorsCh)
			continue
		}
		connection, err := net.ListenPacket(endpoint.network, endpoint.address)
		if err != nil {
			closeAll(closers)
			fatalf("listen echo %s %s: %v", endpoint.network, endpoint.address, err)
		}
		closers = append(closers, connection)
		go serveUDPEcho(connection, endpoint.network, events, errorsCh)
	}
	defer closeAll(closers)
	fmt.Println("READY")
	encoder := json.NewEncoder(newLockedWriter())
	timer := time.NewTimer(*timeout)
	defer timer.Stop()
	for received := 0; received < *count; {
		select {
		case event := <-events:
			if !*quiet {
				if err := encoder.Encode(event); err != nil {
					fatalf("encode echo event: %v", err)
				}
			}
			received++
		case err := <-errorsCh:
			fatalf("serve echo: %v", err)
		case <-timer.C:
			fatalf("echo server timed out after %d/%d round trips", received, *count)
		}
	}
	if *quiet {
		fmt.Printf("SERVED %d\n", *count)
	}
}

func runRoundTrip(args []string) {
	flags := flag.NewFlagSet("roundtrip", flag.ExitOnError)
	network := flags.String("network", "tcp4", "tcp4, tcp6, udp4, or udp6")
	destination := flags.String("destination", "", "destination address")
	payload := flags.String("payload", "probe", "payload")
	timeout := flags.Duration("timeout", 5*time.Second, "round-trip timeout")
	_ = flags.Parse(args)
	if *destination == "" {
		fatalf("roundtrip destination is required")
	}
	connection, err := net.DialTimeout(*network, *destination, *timeout)
	if err != nil {
		fatalf("dial roundtrip destination: %v", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(*timeout))
	if _, err := connection.Write([]byte(*payload)); err != nil {
		fatalf("write roundtrip payload: %v", err)
	}
	buffer := make([]byte, 4096)
	read, err := connection.Read(buffer)
	if err != nil {
		fatalf("read roundtrip response: %v", err)
	}
	var event echoEvent
	if err := json.Unmarshal(buffer[:read], &event); err != nil {
		fatalf("decode roundtrip response: %v", err)
	}
	if event.Payload != *payload {
		fatalf("roundtrip payload mismatch: got %q want %q", event.Payload, *payload)
	}
	body, err := json.Marshal(event)
	if err != nil {
		fatalf("encode roundtrip response: %v", err)
	}
	fmt.Printf("ROUNDTRIP %s\n", body)
}

func runLoad(args []string) {
	flags := flag.NewFlagSet("load", flag.ExitOnError)
	network := flags.String("network", "udp4", "udp4 or udp6")
	destination := flags.String("destination", "", "destination address")
	prefix := flags.String("payload-prefix", "load", "payload prefix")
	count := flags.Int("count", 1000, "total round trips")
	concurrency := flags.Int("concurrency", 4, "parallel UDP associations")
	timeout := flags.Duration("timeout", 5*time.Second, "per-request timeout")
	_ = flags.Parse(args)
	if (*network != "udp4" && *network != "udp6") || *destination == "" || *count < 1 || *concurrency < 1 {
		fatalf("load requires udp4/udp6, a destination, and positive count/concurrency")
	}
	if *concurrency > *count {
		*concurrency = *count
	}
	started := time.Now()
	errorsCh := make(chan error, *concurrency)
	var completed atomic.Uint64
	var waitGroup sync.WaitGroup
	base := *count / *concurrency
	remainder := *count % *concurrency
	for workerIndex := 0; workerIndex < *concurrency; workerIndex++ {
		requests := base
		if workerIndex < remainder {
			requests++
		}
		waitGroup.Add(1)
		go func(index, requests int) {
			defer waitGroup.Done()
			connection, err := net.DialTimeout(*network, *destination, *timeout)
			if err != nil {
				errorsCh <- fmt.Errorf("worker %d dial: %w", index, err)
				return
			}
			defer connection.Close()
			buffer := make([]byte, 4096)
			for requestIndex := 0; requestIndex < requests; requestIndex++ {
				payload := fmt.Sprintf("%s-%d-%d", *prefix, index, requestIndex)
				_ = connection.SetDeadline(time.Now().Add(*timeout))
				if _, err := connection.Write([]byte(payload)); err != nil {
					errorsCh <- fmt.Errorf("worker %d write %d: %w", index, requestIndex, err)
					return
				}
				read, err := connection.Read(buffer)
				if err != nil {
					errorsCh <- fmt.Errorf("worker %d read %d: %w", index, requestIndex, err)
					return
				}
				var event echoEvent
				if err := json.Unmarshal(buffer[:read], &event); err != nil {
					errorsCh <- fmt.Errorf("worker %d decode %d: %w", index, requestIndex, err)
					return
				}
				if event.Payload != payload {
					errorsCh <- fmt.Errorf("worker %d payload mismatch at %d", index, requestIndex)
					return
				}
				completed.Add(1)
			}
		}(workerIndex, requests)
	}
	waitGroup.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			fatalf("load failed after %d/%d requests: %v", completed.Load(), *count, err)
		}
	}
	duration := time.Since(started)
	result := map[string]any{
		"network": *network, "destination": *destination, "requests": completed.Load(),
		"concurrency": *concurrency, "duration_ms": duration.Milliseconds(),
		"requests_per_second": float64(completed.Load()) / duration.Seconds(),
	}
	body, err := json.Marshal(result)
	if err != nil {
		fatalf("encode load result: %v", err)
	}
	fmt.Printf("LOAD %s\n", body)
}

func runWaitTCP(args []string) {
	flags := flag.NewFlagSet("wait-tcp", flag.ExitOnError)
	address := flags.String("address", "", "TCP address")
	timeout := flags.Duration("timeout", 10*time.Second, "wait timeout")
	_ = flags.Parse(args)
	if *address == "" {
		fatalf("wait-tcp address is required")
	}
	deadline := time.Now().Add(*timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", *address, 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			fmt.Printf("READY %s\n", *address)
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	fatalf("TCP address %s did not become ready: %v", *address, lastErr)
}

func serveTCPEcho(listener net.Listener, network string, events chan<- echoEvent, errorsCh chan<- error) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				errorsCh <- err
			}
			return
		}
		go handleTCPEcho(connection, network, events, errorsCh)
	}
}

func handleTCPEcho(connection net.Conn, network string, events chan<- echoEvent, errorsCh chan<- error) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	buffer := make([]byte, 4096)
	read, err := connection.Read(buffer)
	if err != nil {
		errorsCh <- err
		return
	}
	event := echoEvent{Network: network, Payload: string(buffer[:read]), Local: connection.LocalAddr().String(), Remote: connection.RemoteAddr().String()}
	body, err := json.Marshal(event)
	if err != nil {
		errorsCh <- err
		return
	}
	body = append(body, '\n')
	if _, err := connection.Write(body); err != nil {
		errorsCh <- err
		return
	}
	events <- event
}

func serveUDPEcho(connection net.PacketConn, network string, events chan<- echoEvent, errorsCh chan<- error) {
	buffer := make([]byte, 4096)
	for {
		read, remote, err := connection.ReadFrom(buffer)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				errorsCh <- err
			}
			return
		}
		event := echoEvent{Network: network, Payload: string(buffer[:read]), Local: connection.LocalAddr().String(), Remote: remote.String()}
		body, err := json.Marshal(event)
		if err != nil {
			errorsCh <- err
			return
		}
		if _, err := connection.WriteTo(body, remote); err != nil {
			errorsCh <- err
			return
		}
		events <- event
	}
}

func closeAll(closers []io.Closer) {
	for _, closer := range closers {
		_ = closer.Close()
	}
}
