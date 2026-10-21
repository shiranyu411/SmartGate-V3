package fabric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"

	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

type ConnectionTracker interface {
	Count(context.Context, state.PoolDispatchGraph) (int, error)
	Close(context.Context, state.PoolDispatchGraph) (int, error)
}

type ConnectionLedger interface {
	List(context.Context, state.PoolDispatchGraph) ([]TrackedConnection, error)
	CloseOne(context.Context, state.PoolDispatchGraph, TrackedConnection) (int, error)
}

type TrackedConnection struct {
	ID              string `json:"id"`
	Protocol        string `json:"protocol"`
	State           string `json:"state,omitempty"`
	SourceIP        string `json:"source_ip"`
	DestinationIP   string `json:"destination_ip"`
	SourcePort      string `json:"source_port,omitempty"`
	DestinationPort string `json:"destination_port,omitempty"`
	ReplySourceIP   string `json:"reply_source_ip,omitempty"`
	ReplyDestIP     string `json:"reply_destination_ip,omitempty"`
	ReplySourcePort string `json:"reply_source_port,omitempty"`
	ReplyDestPort   string `json:"reply_destination_port,omitempty"`
	Upload          uint64 `json:"upload"`
	Download        uint64 `json:"download"`
}

type ShellConnectionTracker struct {
	Runner netgate.Runner
}

func (tracker ShellConnectionTracker) Count(ctx context.Context, dispatch state.PoolDispatchGraph) (int, error) {
	connections, err := tracker.List(ctx, dispatch)
	if err != nil {
		return 0, err
	}
	return len(connections), nil
}

func (tracker ShellConnectionTracker) List(ctx context.Context, dispatch state.PoolDispatchGraph) ([]TrackedConnection, error) {
	if err := validateTrackedDispatch(dispatch); err != nil {
		return nil, err
	}
	output, err := tracker.runner().Run(ctx, []string{
		"conntrack", "-L", "-m", "mark", "--mark", formatMark(dispatch.Mark) + "/" + formatMark(dispatch.MarkMask), "-o", "id,extended",
	})
	if err != nil {
		return nil, err
	}
	connections, err := ParseConntrackList(output)
	if err != nil {
		return nil, fmt.Errorf("list conntrack entries for pool %s generation %d: %w", dispatch.Pool, dispatch.Generation, err)
	}
	return connections, nil
}

func (tracker ShellConnectionTracker) Close(ctx context.Context, dispatch state.PoolDispatchGraph) (int, error) {
	before, err := tracker.Count(ctx, dispatch)
	if err != nil || before == 0 {
		return before, err
	}
	owner := fmt.Sprintf("smartgate:fabric/conntrl:%s:%d", dispatch.Pool, dispatch.Generation)
	if _, err := tracker.runner().Run(ctx, []string{
		"sh", "-c", "set -eu; printf '%s\\n' \"$3\" >/dev/null; if ! conntrack -D -m mark --mark \"$1/$2\" >/dev/null 2>&1; then remaining=$(conntrack -L -m mark --mark \"$1/$2\" 2>/dev/null | wc -l); test \"$remaining\" -eq 0; fi",
		"sh", formatMark(dispatch.Mark), formatMark(dispatch.MarkMask), owner,
	}); err != nil {
		return 0, err
	}
	remaining, err := tracker.Count(ctx, dispatch)
	if err != nil {
		return 0, err
	}
	if remaining != 0 {
		return before - remaining, fmt.Errorf("pool %s generation %d still has %d conntrack entries", dispatch.Pool, dispatch.Generation, remaining)
	}
	return before, nil
}

func (tracker ShellConnectionTracker) CloseOne(ctx context.Context, dispatch state.PoolDispatchGraph, connection TrackedConnection) (int, error) {
	if err := validateTrackedDispatch(dispatch); err != nil {
		return 0, err
	}
	if err := validateTrackedConnection(connection); err != nil {
		return 0, err
	}
	argv := []string{
		"conntrack", "-D", "-p", connection.Protocol,
		"-s", connection.SourceIP, "-d", connection.DestinationIP,
		"-m", "mark", "--mark", formatMark(dispatch.Mark) + "/" + formatMark(dispatch.MarkMask),
	}
	if connection.SourcePort != "" {
		argv = append(argv, "--sport", connection.SourcePort)
	}
	if connection.DestinationPort != "" {
		argv = append(argv, "--dport", connection.DestinationPort)
	}
	if _, err := tracker.runner().Run(ctx, argv); err != nil {
		remaining, listErr := tracker.List(ctx, dispatch)
		if listErr == nil && !containsTrackedConnection(remaining, connection.ID) {
			return 1, nil
		}
		return 0, err
	}
	return 1, nil
}

func (tracker ShellConnectionTracker) runner() netgate.Runner {
	if tracker.Runner != nil {
		return tracker.Runner
	}
	return netgate.ShellRunner{}
}

type NoopConnectionTracker struct{}

func (NoopConnectionTracker) Count(context.Context, state.PoolDispatchGraph) (int, error) {
	return 0, nil
}

func (NoopConnectionTracker) Close(context.Context, state.PoolDispatchGraph) (int, error) {
	return 0, nil
}

func (NoopConnectionTracker) List(context.Context, state.PoolDispatchGraph) ([]TrackedConnection, error) {
	return []TrackedConnection{}, nil
}

func (NoopConnectionTracker) CloseOne(context.Context, state.PoolDispatchGraph, TrackedConnection) (int, error) {
	return 0, nil
}

func validateTrackedDispatch(dispatch state.PoolDispatchGraph) error {
	if dispatch.Pool == "" || dispatch.Binding == "" || dispatch.Generation == 0 || dispatch.Mark == 0 || dispatch.MarkMask == 0 {
		return fmt.Errorf("connection dispatch metadata is incomplete")
	}
	return nil
}

func parseConnectionCount(output string) (int, error) {
	lines := strings.Fields(output)
	if len(lines) == 0 {
		return 0, fmt.Errorf("empty conntrack count")
	}
	count, err := strconv.Atoi(lines[len(lines)-1])
	if err != nil || count < 0 {
		return 0, fmt.Errorf("invalid conntrack count %q", lines[len(lines)-1])
	}
	return count, nil
}

func ParseConntrackList(output string) ([]TrackedConnection, error) {
	lines := strings.Split(output, "\n")
	out := make([]TrackedConnection, 0, len(lines))
	seen := map[string]struct{}{}
	for lineNumber, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		hasSource := strings.Contains(line, "src=")
		hasDestination := strings.Contains(line, "dst=")
		if !hasSource && !hasDestination {
			continue
		}
		connection, err := parseConntrackLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		if _, exists := seen[connection.ID]; exists {
			continue
		}
		seen[connection.ID] = struct{}{}
		out = append(out, connection)
	}
	return out, nil
}

func parseConntrackLine(line string) (TrackedConnection, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return TrackedConnection{}, fmt.Errorf("invalid conntrack entry")
	}
	protocolIndex := 0
	if fields[0] == "ipv4" || fields[0] == "ipv6" {
		protocolIndex = 2
	}
	if protocolIndex >= len(fields) {
		return TrackedConnection{}, fmt.Errorf("conntrack entry is missing a protocol")
	}
	connection := TrackedConnection{Protocol: strings.ToLower(fields[protocolIndex])}
	if connection.Protocol != "tcp" && connection.Protocol != "udp" {
		return TrackedConnection{}, fmt.Errorf("unsupported protocol %q", connection.Protocol)
	}
	firstTupleField := len(fields)
	for index := protocolIndex + 1; index < len(fields); index++ {
		if strings.Contains(fields[index], "=") {
			firstTupleField = index
			break
		}
	}
	if connection.Protocol == "tcp" && firstTupleField > protocolIndex+1 {
		candidate := fields[firstTupleField-1]
		if _, err := strconv.ParseUint(candidate, 10, 64); err != nil && !strings.HasPrefix(candidate, "[") {
			connection.State = candidate
		}
	}
	original := map[string]string{}
	reply := map[string]string{}
	current := original
	sourceCount := 0
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if key == "src" {
			sourceCount++
			if sourceCount == 2 {
				current = reply
			}
		}
		if sourceCount == 0 {
			continue
		}
		if _, exists := current[key]; !exists {
			current[key] = strings.TrimSpace(value)
		}
	}
	connection.SourceIP = original["src"]
	connection.DestinationIP = original["dst"]
	connection.SourcePort = original["sport"]
	connection.DestinationPort = original["dport"]
	connection.ReplySourceIP = reply["src"]
	connection.ReplyDestIP = reply["dst"]
	connection.ReplySourcePort = reply["sport"]
	connection.ReplyDestPort = reply["dport"]
	upload, err := parseConnectionUint(original["bytes"])
	if err != nil {
		return TrackedConnection{}, fmt.Errorf("invalid original bytes: %w", err)
	}
	download, err := parseConnectionUint(reply["bytes"])
	if err != nil {
		return TrackedConnection{}, fmt.Errorf("invalid reply bytes: %w", err)
	}
	connection.Upload = upload
	connection.Download = download
	if err := validateTrackedConnection(connection); err != nil {
		return TrackedConnection{}, err
	}
	connection.ID = trackedConnectionID(connection)
	return connection, nil
}

func validateTrackedConnection(connection TrackedConnection) error {
	if connection.Protocol != "tcp" && connection.Protocol != "udp" {
		return fmt.Errorf("connection protocol must be tcp or udp")
	}
	if connection.SourceIP == "" || connection.DestinationIP == "" {
		return fmt.Errorf("connection tuple is incomplete")
	}
	if net.ParseIP(connection.SourceIP) == nil {
		return fmt.Errorf("invalid source IP %q", connection.SourceIP)
	}
	if net.ParseIP(connection.DestinationIP) == nil {
		return fmt.Errorf("invalid destination IP %q", connection.DestinationIP)
	}
	if connection.SourcePort == "" || connection.DestinationPort == "" {
		return fmt.Errorf("connection ports are incomplete")
	}
	if _, err := parseConnectionPort(connection.SourcePort); err != nil {
		return fmt.Errorf("invalid source port: %w", err)
	}
	if _, err := parseConnectionPort(connection.DestinationPort); err != nil {
		return fmt.Errorf("invalid destination port: %w", err)
	}
	for field, value := range map[string]string{
		"reply source IP": connection.ReplySourceIP,
		"reply dest IP":   connection.ReplyDestIP,
	} {
		if value != "" && net.ParseIP(value) == nil {
			return fmt.Errorf("invalid %s %q", field, value)
		}
	}
	for field, value := range map[string]string{
		"reply source port": connection.ReplySourcePort,
		"reply dest port":   connection.ReplyDestPort,
	} {
		if value == "" {
			continue
		}
		if _, err := parseConnectionPort(value); err != nil {
			return fmt.Errorf("invalid %s: %w", field, err)
		}
	}
	return nil
}

func parseConnectionPort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid port %q", raw)
	}
	return port, nil
}

func parseConnectionUint(raw string) (uint64, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid unsigned integer %q", raw)
	}
	return value, nil
}

func trackedConnectionID(connection TrackedConnection) string {
	value := strings.Join([]string{
		connection.Protocol,
		connection.SourceIP,
		connection.DestinationIP,
		connection.SourcePort,
		connection.DestinationPort,
	}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:24]
}

func containsTrackedConnection(connections []TrackedConnection, id string) bool {
	for _, connection := range connections {
		if connection.ID == id {
			return true
		}
	}
	return false
}
