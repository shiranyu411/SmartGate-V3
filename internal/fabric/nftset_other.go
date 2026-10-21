//go:build !linux

package fabric

import (
	"context"
	"net/netip"
	"time"
)

func addNativeSetElement(context.Context, string, string, string, netip.Addr, time.Duration) (bool, error) {
	return false, nil
}
