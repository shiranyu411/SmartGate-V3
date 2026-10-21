package selection

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ProxyController interface {
	Select(ctx context.Context, selectorTag, outboundTag string) error
	Delay(ctx context.Context, outboundTag, testURL string, timeout time.Duration) (DelayResult, error)
	Connections(ctx context.Context) (map[string]any, error)
	CloseConnections(ctx context.Context) error
	CloseConnection(ctx context.Context, connectionID string) error
}

// ProxySelectionReader exposes the selector value currently active in the
// controller. Transactions use it instead of trusting a caller-provided source.
type ProxySelectionReader interface {
	Selected(ctx context.Context, selectorTag string) (string, error)
}

type DelayResult struct {
	DelayMS int    `json:"delay_ms"`
	URL     string `json:"url"`
}

// CloseConnectionsMatchingSelector closes live connections whose chain
// includes selectorTag. Individual close failures do not stop later closes.
func CloseConnectionsMatchingSelector(ctx context.Context, control ProxyController, selectorTag string) (int, error) {
	return closeConnectionsMatching(ctx, control, selectorTag, "")
}

// CloseConnectionsMatchingSelectorAndOutbound closes live connections whose
// chain includes both selectorTag and outboundTag.
func CloseConnectionsMatchingSelectorAndOutbound(ctx context.Context, control ProxyController, selectorTag, outboundTag string) (int, error) {
	return closeConnectionsMatching(ctx, control, selectorTag, outboundTag)
}

func CountConnectionsMatchingSelector(ctx context.Context, control ProxyController, selectorTag string) (int, error) {
	if control == nil {
		return 0, fmt.Errorf("proxy control is not configured")
	}
	selectorTag = strings.TrimSpace(selectorTag)
	if selectorTag == "" {
		return 0, fmt.Errorf("selector tag is required")
	}
	payload, err := control.Connections(ctx)
	if err != nil {
		return 0, err
	}
	return len(connectionIDsMatchingChainTags(payload, selectorTag)), nil
}

var selectorRebindDelays = []time.Duration{300 * time.Millisecond, 1200 * time.Millisecond}

// SetSelectorRebindDelaysForTest overrides residual rebind delays and returns
// a function that restores the previous delays.
func SetSelectorRebindDelaysForTest(delays []time.Duration) func() {
	previous := append([]time.Duration(nil), selectorRebindDelays...)
	if delays == nil {
		selectorRebindDelays = nil
	} else {
		selectorRebindDelays = append([]time.Duration(nil), delays...)
	}
	return func() {
		selectorRebindDelays = previous
	}
}

// ScheduleSelectorConnectionRebind closes matching connections immediately
// and schedules residual sweeps without making rebind failure transactional.
func ScheduleSelectorConnectionRebind(control ProxyController, selectorTag, oldOutbound string) {
	if control == nil {
		return
	}
	selectorTag = strings.TrimSpace(selectorTag)
	oldOutbound = strings.TrimSpace(oldOutbound)
	if selectorTag == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = CloseConnectionsMatchingSelector(ctx, control, selectorTag)
	cancel()

	delays := append([]time.Duration(nil), selectorRebindDelays...)
	if len(delays) == 0 {
		return
	}
	go func() {
		for _, delay := range delays {
			if delay > 0 {
				time.Sleep(delay)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if oldOutbound != "" {
				_, _ = CloseConnectionsMatchingSelectorAndOutbound(ctx, control, selectorTag, oldOutbound)
			} else {
				_, _ = CloseConnectionsMatchingSelector(ctx, control, selectorTag)
			}
			cancel()
		}
	}()
}

func closeConnectionsMatching(ctx context.Context, control ProxyController, selectorTag, outboundTag string) (int, error) {
	if control == nil {
		return 0, fmt.Errorf("proxy control is not configured")
	}
	selectorTag = strings.TrimSpace(selectorTag)
	if selectorTag == "" {
		return 0, fmt.Errorf("selector tag is required")
	}
	outboundTag = strings.TrimSpace(outboundTag)
	payload, err := control.Connections(ctx)
	if err != nil {
		return 0, err
	}
	ids := connectionIDsMatchingChainTags(payload, selectorTag, outboundTag)
	if len(ids) == 0 {
		return 0, nil
	}
	var (
		closed int
		errs   []string
	)
	for _, id := range ids {
		if err := control.CloseConnection(ctx, id); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		closed++
	}
	if len(errs) > 0 {
		return closed, fmt.Errorf("closed %d/%d matching connections for %s; errors: %s", closed, len(ids), selectorTag, strings.Join(errs, "; "))
	}
	return closed, nil
}

func connectionIDsMatchingChainTag(payload map[string]any, chainTag string) []string {
	return connectionIDsMatchingChainTags(payload, chainTag, "")
}

func connectionIDsMatchingChainTags(payload map[string]any, requiredTags ...string) []string {
	if payload == nil {
		return nil
	}
	tags := make([]string, 0, len(requiredTags))
	for _, tag := range requiredTags {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		return nil
	}
	raw, ok := payload["connections"]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		conn, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if !connectionChainsContainAll(conn["chains"], tags...) {
			continue
		}
		id := strings.TrimSpace(fmt.Sprint(conn["id"]))
		if id == "" || id == "<nil>" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func connectionChainsContain(raw any, want string) bool {
	return connectionChainsContainAll(raw, want)
}

func connectionChainsContainAll(raw any, want ...string) bool {
	if len(want) == 0 {
		return false
	}
	present := map[string]struct{}{}
	switch typed := raw.(type) {
	case []any:
		for _, item := range typed {
			present[strings.TrimSpace(fmt.Sprint(item))] = struct{}{}
		}
	case []string:
		for _, item := range typed {
			present[strings.TrimSpace(item)] = struct{}{}
		}
	default:
		return false
	}
	for _, tag := range want {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := present[tag]; !ok {
			return false
		}
	}
	return true
}
