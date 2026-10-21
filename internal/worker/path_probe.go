package worker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/proxy"

	"smartgate/internal/state"
)

const DefaultRouteProbeURL = "https://www.gstatic.com/generate_204"
const DefaultRouteConfirmURL = "https://cloudflare.com/cdn-cgi/trace"

func VerifySOCKSRouteConfirmed(ctx context.Context, inbound state.WorkerInbound, timeout time.Duration) error {
	return verifySOCKSRouteTargets(ctx, inbound, []string{DefaultRouteProbeURL, DefaultRouteConfirmURL}, timeout)
}

func verifySOCKSRouteTargets(ctx context.Context, inbound state.WorkerInbound, targets []string, timeout time.Duration) error {
	var lastErr error
	for _, target := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := MeasureSOCKSRoute(ctx, inbound, target, timeout); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func VerifySOCKSRoute(ctx context.Context, inbound state.WorkerInbound, testURL string, timeout time.Duration) error {
	return probeSOCKSRoute(ctx, inbound, testURL, timeout, true)
}

// MeasureSOCKSRoute matches core URL-test availability: a completed, verified
// HTTP exchange is measurable even when the origin returns an error status.
func MeasureSOCKSRoute(ctx context.Context, inbound state.WorkerInbound, testURL string, timeout time.Duration) error {
	return probeSOCKSRoute(ctx, inbound, testURL, timeout, false)
}

func probeSOCKSRoute(ctx context.Context, inbound state.WorkerInbound, testURL string, timeout time.Duration, requireSuccess bool) error {
	if inbound.Kind != "socks" {
		return fmt.Errorf("route probe requires a SOCKS inbound")
	}
	if inbound.Listen != "127.0.0.1" && inbound.Listen != "::1" {
		return fmt.Errorf("route probe SOCKS inbound must be loopback")
	}
	if inbound.Port < 1 || inbound.Port > 65535 {
		return fmt.Errorf("route probe SOCKS inbound port is invalid")
	}
	if testURL == "" {
		testURL = DefaultRouteProbeURL
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	base := &net.Dialer{Timeout: timeout}
	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)), nil, base)
	if err != nil {
		return err
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return fmt.Errorf("SOCKS route dialer has no context support")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return contextDialer.DialContext(ctx, network, address)
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, testURL, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	closeErr := response.Body.Close()
	if requireSuccess && (response.StatusCode < 200 || response.StatusCode >= 400) {
		return fmt.Errorf("route probe returned HTTP %d", response.StatusCode)
	}
	return closeErr
}
