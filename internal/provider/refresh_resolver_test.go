package provider

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type stubConn struct{ net.Conn }

func (stubConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (stubConn) Write([]byte) (int, error)        { return 0, net.ErrClosed }
func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (stubConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

func TestDialDirectResolverServersFallsBack(t *testing.T) {
	var tried []string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		tried = append(tried, address)
		if address == "127.0.0.1:1" {
			return nil, errors.New("connection refused")
		}
		return stubConn{}, nil
	}
	ctx := context.Background()
	conn, err := dialDirectResolverServersWith(ctx, "udp", []string{"127.0.0.1:1", "127.0.0.1:53"}, dial)
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	_ = conn.Close()
	if len(tried) != 2 || tried[0] != "127.0.0.1:1" || tried[1] != "127.0.0.1:53" {
		t.Fatalf("unexpected dial order: %v", tried)
	}
}

func TestDialDirectResolverServersAllFail(t *testing.T) {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("refused: " + address)
	}
	_, err := dialDirectResolverServersWith(context.Background(), "udp", []string{"127.0.0.1:1", "127.0.0.1:2"}, dial)
	if err == nil {
		t.Fatal("expected error when all nameservers refuse")
	}
}
