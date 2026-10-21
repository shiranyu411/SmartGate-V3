package fabric

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
)

// DNS writes must not spawn nft, which loads the router's full ruleset even
// for a single element. Netlink addresses only the already-owned set.
func addNativeSetElement(ctx context.Context, family, table, name string, address netip.Addr, ttl time.Duration) (bool, error) {
	var tableFamily nftables.TableFamily
	switch family {
	case "inet":
		tableFamily = nftables.TableFamilyINet
	case "ip":
		tableFamily = nftables.TableFamilyIPv4
	case "ip6":
		tableFamily = nftables.TableFamilyIPv6
	default:
		return true, fmt.Errorf("unsupported nft family %q", family)
	}
	deadline := time.Now().Add(time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	conn, err := nftables.New(nftables.WithSockOptions(func(socket *netlink.Conn) error {
		return socket.SetDeadline(deadline)
	}))
	if err != nil {
		return true, err
	}
	set, err := conn.GetSetByName(&nftables.Table{Family: tableFamily, Name: table}, name)
	if err != nil {
		return true, err
	}
	// Fail closed while an upgrade is replacing the old interval-based sets.
	if set.Interval || !set.HasTimeout || set.KeyType.Bytes != uint32(len(address.AsSlice())) {
		return true, fmt.Errorf("nft set %s is not a timed address set", name)
	}
	if err := conn.SetAddElements(set, []nftables.SetElement{{Key: address.AsSlice(), Timeout: ttl}}); err != nil {
		return true, err
	}
	return true, conn.Flush()
}
