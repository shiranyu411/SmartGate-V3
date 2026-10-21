package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"smartgate/internal/netgate"
	"smartgate/internal/selection"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type Config struct {
	Log          map[string]any   `json:"log,omitempty"`
	DNS          map[string]any   `json:"dns,omitempty"`
	Inbounds     []map[string]any `json:"inbounds"`
	Outbounds    []map[string]any `json:"outbounds"`
	Route        map[string]any   `json:"route"`
	Experimental map[string]any   `json:"experimental,omitempty"`
}

type Adapter struct {
	Runtime *worker.FileRuntime
	Version string
}

func (adapter *Adapter) Core() smartnode.CoreID { return smartnode.CoreID("sing-box") }

func (adapter *Adapter) ProbeVersion(ctx context.Context) (string, error) {
	output, err := adapter.runner().Run(ctx, []string{adapter.Descriptor().Process.Binary, "version"})
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) < 3 || fields[0] != "sing-box" || fields[1] != "version" {
		return "", fmt.Errorf("unexpected sing-box version output %q", strings.TrimSpace(output))
	}
	return fields[2], nil
}

func (adapter *Adapter) runner() netgate.Runner {
	if adapter.Runtime != nil && adapter.Runtime.Runner != nil {
		return adapter.Runtime.Runner
	}
	return netgate.ShellRunner{}
}

func (adapter *Adapter) Prepare(ctx context.Context, opID string, request worker.CompileRequest) (worker.Candidate, error) {
	if adapter.Runtime == nil {
		return worker.Candidate{}, fmt.Errorf("sing-box worker runtime is required")
	}
	config, err := adapter.Compile(request)
	if err != nil {
		return worker.Candidate{}, err
	}
	adapter.Runtime.Core = smartnode.CoreID("sing-box")
	return adapter.Runtime.Prepare(ctx, opID, request.Worker.ID, config)
}

func (adapter *Adapter) Compile(request worker.CompileRequest) (worker.CandidateConfig, error) {
	config, sensitive, err := BuildConfig(request)
	return worker.CandidateConfig{
		Value: config, SensitiveValues: sensitive, ReadyPorts: readyPorts(request.Worker),
	}, err
}

func (adapter *Adapter) Verify(candidate worker.Candidate) error {
	if adapter.Runtime == nil {
		return fmt.Errorf("sing-box worker runtime is required")
	}
	return adapter.Runtime.Verify(candidate)
}

func (adapter *Adapter) Install(ctx context.Context, candidate worker.Candidate) (worker.Activation, error) {
	return adapter.Runtime.Install(ctx, candidate)
}

func (adapter *Adapter) Activate(ctx context.Context, candidate worker.Candidate) (worker.Activation, error) {
	return adapter.Runtime.Activate(ctx, candidate)
}

func (adapter *Adapter) Start(ctx context.Context, target state.WorkerTarget) (worker.Activation, error) {
	return adapter.Runtime.StartInstalled(ctx, target.ID, readyPorts(target))
}

func (adapter *Adapter) Backup(opID, workerID string) (worker.Backup, error) {
	return adapter.Runtime.Backup(opID, workerID)
}

func (adapter *Adapter) Restore(ctx context.Context, backup worker.Backup) (worker.Activation, error) {
	return adapter.Runtime.Restore(ctx, backup)
}

func (adapter *Adapter) Stop(ctx context.Context, workerID string) error {
	return adapter.Runtime.Stop(ctx, workerID)
}

func (adapter *Adapter) Probe(ctx context.Context, workerID string) error {
	return adapter.Runtime.Probe(ctx, workerID)
}

func (adapter *Adapter) RemoveCandidate(candidate worker.Candidate) error {
	return adapter.Runtime.RemoveCandidate(candidate)
}

func (adapter *Adapter) SelectRoute(ctx context.Context, path worker.BindingPath) error {
	if adapter.Runtime != nil && adapter.Runtime.SkipReadiness {
		return nil
	}
	client, err := routeClient(path)
	if err != nil {
		return err
	}
	if err := client.Select(ctx, path.Route.Selector, path.Executor.ID); err != nil {
		return err
	}
	selected, err := client.Selected(ctx, path.Route.Selector)
	if err != nil {
		return err
	}
	if selected != path.Executor.ID {
		return fmt.Errorf("sing-box route %s selected %s, expected %s", path.Route.ID, selected, path.Executor.ID)
	}
	return nil
}

func (adapter *Adapter) VerifyRoute(ctx context.Context, path worker.BindingPath) error {
	if adapter.Runtime != nil && adapter.Runtime.SkipReadiness {
		return nil
	}
	if err := adapter.VerifyRouteSelection(ctx, path); err != nil {
		return err
	}
	return worker.VerifySOCKSRouteConfirmed(ctx, path.DNS, 5*time.Second)
}

func (adapter *Adapter) VerifyRouteSelection(ctx context.Context, path worker.BindingPath) error {
	if adapter.Runtime != nil && adapter.Runtime.SkipReadiness {
		return nil
	}
	client, err := routeClient(path)
	if err != nil {
		return err
	}
	selected, err := client.Selected(ctx, path.Route.Selector)
	if err != nil {
		return err
	}
	if selected != path.Executor.ID {
		return fmt.Errorf("sing-box route %s selected %s, expected %s", path.Route.ID, selected, path.Executor.ID)
	}
	return nil
}

func (adapter *Adapter) DelayRoute(ctx context.Context, path worker.BindingPath, testURL string, timeout time.Duration) (worker.DelayResult, error) {
	client, err := routeClient(path)
	if err != nil {
		return worker.DelayResult{}, err
	}
	result, err := client.Delay(ctx, path.Executor.ID, testURL, timeout)
	if err != nil {
		return worker.DelayResult{}, err
	}
	return worker.DelayResult{DelayMS: result.DelayMS, URL: result.URL}, nil
}

func (adapter *Adapter) CountRouteConnections(ctx context.Context, path worker.BindingPath) (int, error) {
	if adapter.Runtime != nil && adapter.Runtime.SkipReadiness {
		return 0, nil
	}
	client, err := routeClient(path)
	if err != nil {
		return 0, err
	}
	return selection.CountConnectionsMatchingSelector(ctx, client, path.Route.Selector)
}

func (adapter *Adapter) ListRouteConnections(ctx context.Context, path worker.BindingPath) ([]worker.Connection, error) {
	client, err := routeClient(path)
	if err != nil {
		return nil, err
	}
	payload, err := client.Connections(ctx)
	if err != nil {
		return nil, err
	}
	return clashRouteConnections(payload, path)
}

func (adapter *Adapter) CloseRouteConnection(ctx context.Context, path worker.BindingPath, connectionID string) error {
	client, err := routeClient(path)
	if err != nil {
		return err
	}
	return client.CloseConnection(ctx, connectionID)
}

func (adapter *Adapter) CloseRouteConnections(ctx context.Context, path worker.BindingPath) (int, error) {
	if adapter.Runtime != nil && adapter.Runtime.SkipReadiness {
		return 0, nil
	}
	client, err := routeClient(path)
	if err != nil {
		return 0, err
	}
	return selection.CloseConnectionsMatchingSelector(ctx, client, path.Route.Selector)
}

func routeClient(path worker.BindingPath) (clashClient, error) {
	control := path.Worker.Control
	if control.Kind != "clash" || control.Port < 1 || control.Port > 65535 {
		return clashClient{}, fmt.Errorf("sing-box route has invalid control endpoint")
	}
	if control.Listen != "127.0.0.1" && control.Listen != "::1" {
		return clashClient{}, fmt.Errorf("sing-box route control must be loopback")
	}
	return clashClient{baseURL: "http://" + net.JoinHostPort(control.Listen, strconv.Itoa(control.Port))}, nil
}

func clashRouteConnections(payload map[string]any, path worker.BindingPath) ([]worker.Connection, error) {
	raw, ok := payload["connections"]
	if !ok {
		return nil, fmt.Errorf("clash connections payload missing connections")
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("clash connections payload has invalid connections field")
	}
	out := make([]worker.Connection, 0, len(items))
	for index, item := range items {
		connection, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("clash connection %d is not an object", index)
		}
		chains, err := clashStrings(connection["chains"], fmt.Sprintf("connection %d chains", index))
		if err != nil {
			return nil, err
		}
		if !clashChainsMatchPath(chains, path) {
			continue
		}
		id, err := clashString(connection["id"], fmt.Sprintf("connection %d id", index))
		if err != nil {
			return nil, err
		}
		metadata, err := clashMap(connection["metadata"], fmt.Sprintf("connection %d metadata", index))
		if err != nil {
			return nil, err
		}
		upload, err := clashNumber(connection["upload"], fmt.Sprintf("connection %d upload", index))
		if err != nil {
			return nil, err
		}
		download, err := clashNumber(connection["download"], fmt.Sprintf("connection %d download", index))
		if err != nil {
			return nil, err
		}
		start, err := clashOptionalString(connection["start"], fmt.Sprintf("connection %d start", index))
		if err != nil {
			return nil, err
		}
		parsed := worker.Connection{
			ID: id, Metadata: metadata, Upload: upload, Download: download, Chains: chains, Start: start,
		}
		out = append(out, parsed)
	}
	return out, nil
}

func clashChainsMatchPath(chains []string, path worker.BindingPath) bool {
	for _, chain := range chains {
		if chain == path.Route.ID || chain == path.Route.Selector || chain == path.TProxy.ID || chain == path.DNS.ID {
			return true
		}
	}
	return false
}

func clashMap(value any, field string) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	typed, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("clash %s is not an object", field)
	}
	return typed, nil
}

func clashString(value any, field string) (string, error) {
	if value == nil {
		return "", fmt.Errorf("clash %s is required", field)
	}
	if typed, ok := value.(string); ok {
		if strings.TrimSpace(typed) == "" {
			return "", fmt.Errorf("clash %s is required", field)
		}
		return typed, nil
	}
	return "", fmt.Errorf("clash %s is not a string", field)
}

func clashOptionalString(value any, field string) (string, error) {
	if value == nil {
		return "", nil
	}
	typed, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("clash %s is not a string", field)
	}
	return typed, nil
}

func clashNumber(value any, field string) (float64, error) {
	if value == nil {
		return 0, nil
	}
	switch typed := value.(type) {
	case int:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case float64:
		return typed, nil
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, fmt.Errorf("clash %s is not a number: %w", field, err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("clash %s is not a number", field)
	}
}

func clashStrings(value any, field string) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("clash %s is not an array", field)
	}
	out := make([]string, 0, len(items))
	for index, item := range items {
		typed, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("clash %s[%d] is not a string", field, index)
		}
		out = append(out, typed)
	}
	return out, nil
}

func BuildConfig(request worker.CompileRequest) (Config, []string, error) {
	if request.Worker.Core != smartnode.CoreID("sing-box") {
		return Config{}, nil, fmt.Errorf("worker %s is not a sing-box target", request.Worker.ID)
	}
	executors := make(map[string]state.NodeExecutor, len(request.Executors))
	outbounds := make([]map[string]any, 0, len(request.Executors)+1)
	sensitive := make([]string, 0)
	for _, executor := range request.Executors {
		if executor.Worker != request.Worker.ID || executor.Core != smartnode.CoreID("sing-box") {
			return Config{}, nil, fmt.Errorf("executor %s is assigned to a different worker", executor.ID)
		}
		source, exists := request.Identities[executor.SmartNode]
		if !exists {
			return Config{}, nil, fmt.Errorf("executor %s has no protected identity input", executor.ID)
		}
		outbound, secrets, err := compileIdentityOutbound(executor, source)
		if err != nil {
			return Config{}, nil, err
		}
		outbounds = append(outbounds, outbound)
		executors[executor.ID] = executor
		sensitive = append(sensitive, secrets...)
	}
	sort.Slice(outbounds, func(i, j int) bool { return fmt.Sprint(outbounds[i]["tag"]) < fmt.Sprint(outbounds[j]["tag"]) })
	routes := make(map[string]state.WorkerRoute, len(request.Worker.Routes))
	for _, route := range request.Worker.Routes {
		if route.ID == "" || route.Selector == "" || route.Pool == "" || len(route.Candidates) == 0 {
			return Config{}, nil, fmt.Errorf("worker %s has an incomplete route", request.Worker.ID)
		}
		if _, exists := routes[route.ID]; exists {
			return Config{}, nil, fmt.Errorf("worker %s has duplicate route %s", request.Worker.ID, route.ID)
		}
		for _, candidate := range route.Candidates {
			if _, ok := executors[candidate]; !ok {
				return Config{}, nil, fmt.Errorf("worker route %s references missing executor %s", route.ID, candidate)
			}
		}
		if _, ok := executors[route.Selected]; !ok || !containsString(route.Candidates, route.Selected) {
			return Config{}, nil, fmt.Errorf("worker route %s has invalid selected executor %s", route.ID, route.Selected)
		}
		routes[route.ID] = route
		outbounds = append(outbounds, map[string]any{
			"type": "selector", "tag": route.Selector,
			"outbounds": append([]string(nil), route.Candidates...), "default": route.Selected,
			"interrupt_exist_connections": true,
		})
	}

	bindings := make(map[string]state.PoolBinding, len(request.Bindings))
	for _, binding := range request.Bindings {
		if binding.Target.Worker != request.Worker.ID || binding.Target.Kind != "node" {
			return Config{}, nil, fmt.Errorf("binding %s is assigned to a different worker", binding.ID)
		}
		if _, ok := executors[binding.Target.Executor]; !ok {
			return Config{}, nil, fmt.Errorf("binding %s references missing executor %s", binding.ID, binding.Target.Executor)
		}
		bindings[binding.ID] = binding
	}

	inbounds := make([]map[string]any, 0, len(request.Worker.Inbounds))
	routeRules := make([]map[string]any, 0, len(request.Worker.Inbounds)*2)
	for _, inbound := range request.Worker.Inbounds {
		route, ok := routes[inbound.Route]
		if !ok {
			return Config{}, nil, fmt.Errorf("worker inbound %s references missing route %s", inbound.ID, inbound.Route)
		}
		if route.Pool != inbound.Pool || route.Lane != inbound.Lane {
			return Config{}, nil, fmt.Errorf("worker inbound %s route metadata does not match", inbound.ID)
		}
		if inbound.Binding != "" {
			binding, ok := bindings[inbound.Binding]
			if !ok || binding.Pool != inbound.Pool || binding.Generation != inbound.Generation {
				return Config{}, nil, fmt.Errorf("worker inbound %s has invalid active binding metadata", inbound.ID)
			}
		}
		switch inbound.Kind {
		case "tproxy":
			networks, err := singBoxNetworks(inbound.Network)
			if err != nil {
				return Config{}, nil, fmt.Errorf("worker inbound %s: %w", inbound.ID, err)
			}
			inbounds = append(inbounds, map[string]any{
				"type": "tproxy", "tag": inbound.ID, "listen": inbound.Listen,
				"listen_port": inbound.Port, "network": networks,
			})
			routeRules = append(routeRules, map[string]any{"inbound": []string{inbound.ID}, "action": "sniff"})
		case "socks":
			if inbound.Listen != "127.0.0.1" && inbound.Listen != "::1" {
				return Config{}, nil, fmt.Errorf("worker DNS SOCKS inbound %s must be loopback", inbound.ID)
			}
			inbounds = append(inbounds, map[string]any{
				"type": "socks", "tag": inbound.ID, "listen": inbound.Listen, "listen_port": inbound.Port,
			})
		default:
			return Config{}, nil, fmt.Errorf("worker inbound %s has unsupported kind %q", inbound.ID, inbound.Kind)
		}
		routeRules = append(routeRules, map[string]any{
			"inbound": []string{inbound.ID}, "action": "route", "outbound": route.Selector,
		})
	}
	// Keep unmatched traffic fail-closed using the current rule-action form.
	// This must remain after every inbound route.
	routeRules = append(routeRules, map[string]any{"action": "reject"})
	if len(inbounds) == 0 {
		return Config{}, nil, fmt.Errorf("sing-box worker %s has no active inbounds", request.Worker.ID)
	}
	control := request.Worker.Control
	if control.Listen != "127.0.0.1" && control.Listen != "::1" {
		return Config{}, nil, fmt.Errorf("sing-box control API must bind loopback")
	}
	config := Config{
		Log: map[string]any{"level": "info"},
		// Only resolves proxy server names. Never use the system loopback DNS:
		// that enters Fabric and can depend on this same worker being ready.
		DNS: map[string]any{"servers": []map[string]any{{
			"type": "udp", "tag": "node-bootstrap", "server": "223.5.5.5", "server_port": 53,
		}}},
		Inbounds:  inbounds,
		Outbounds: outbounds,
		Route: map[string]any{
			"auto_detect_interface": true, "rules": routeRules,
			"default_domain_resolver": map[string]any{"server": "node-bootstrap", "strategy": "prefer_ipv4"},
		},
		Experimental: map[string]any{"clash_api": map[string]any{
			"external_controller": fmt.Sprintf("%s:%d", control.Listen, control.Port), "default_mode": "Rule",
		}},
	}
	return config, uniqueSensitive(sensitive), nil
}

func singBoxNetworks(raw string) ([]string, error) {
	parts := strings.FieldsFunc(strings.ToLower(raw), func(character rune) bool {
		return character == ',' || character == ' ' || character == '\t' || character == '\n'
	})
	seen := map[string]struct{}{}
	networks := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "tcp" && part != "udp" {
			return nil, fmt.Errorf("unsupported network %q", part)
		}
		if _, exists := seen[part]; exists {
			continue
		}
		seen[part] = struct{}{}
		networks = append(networks, part)
	}
	if len(networks) == 0 {
		return nil, fmt.Errorf("at least one network is required")
	}
	return networks, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func readyPorts(target state.WorkerTarget) []string {
	ports := []string{fmt.Sprintf("%s:%d", target.Control.Listen, target.Control.Port)}
	for _, inbound := range target.Inbounds {
		if inbound.Kind == "socks" {
			ports = append(ports, fmt.Sprintf("%s:%d", inbound.Listen, inbound.Port))
		}
	}
	return ports
}

func sensitiveValues(value any) []string {
	keys := map[string]struct{}{
		"uuid": {}, "password": {}, "username": {}, "auth": {}, "auth_str": {}, "token": {},
		"private_key": {}, "pre_shared_key": {}, "psk": {}, "public_key": {}, "short_id": {},
	}
	var values []string
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, item := range typed {
				if _, sensitive := keys[strings.ToLower(key)]; sensitive {
					if text := strings.TrimSpace(fmt.Sprint(item)); text != "" && text != "<nil>" {
						values = append(values, text)
					}
				}
				visit(item)
			}
		case []any:
			for _, item := range typed {
				visit(item)
			}
		}
	}
	visit(value)
	return values
}

func uniqueSensitive(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) < 4 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
