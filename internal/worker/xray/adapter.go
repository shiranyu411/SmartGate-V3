package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"smartgate/internal/netgate"
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type Config struct {
	Log       map[string]any   `json:"log"`
	Inbounds  []map[string]any `json:"inbounds"`
	Outbounds []map[string]any `json:"outbounds"`
	Routing   map[string]any   `json:"routing"`
	Policy    map[string]any   `json:"policy"`
	Stats     map[string]any   `json:"stats"`
	API       map[string]any   `json:"api"`
}

type Adapter struct {
	Runtime *worker.FileRuntime
	Binary  string
	Version string
}

func (adapter *Adapter) Core() smartnode.CoreID { return smartnode.CoreID("xray") }

func (adapter *Adapter) ProbeVersion(ctx context.Context) (string, error) {
	output, err := adapter.runner().Run(ctx, []string{adapter.binary(), "version"})
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) < 2 || fields[0] != "Xray" {
		return "", fmt.Errorf("unexpected Xray version output %q", strings.TrimSpace(output))
	}
	return fields[1], nil
}

func (adapter *Adapter) Prepare(ctx context.Context, opID string, request worker.CompileRequest) (worker.Candidate, error) {
	if adapter.Runtime == nil {
		return worker.Candidate{}, fmt.Errorf("Xray worker runtime is required")
	}
	config, err := adapter.Compile(request)
	if err != nil {
		return worker.Candidate{}, err
	}
	adapter.Runtime.Core = smartnode.CoreID("xray")
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
		return fmt.Errorf("Xray worker runtime is required")
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
	address, err := xrayControlAddress(path.Worker)
	if err != nil {
		return err
	}
	_, err = adapter.runner().Run(ctx, []string{
		adapter.binary(), "api", "bo", "--server=" + address, "-b", path.Route.Selector, path.Executor.ID,
	})
	return err
}

func (adapter *Adapter) VerifyRoute(ctx context.Context, path worker.BindingPath) error {
	if adapter.Runtime != nil && adapter.Runtime.SkipReadiness {
		return nil
	}
	return worker.VerifySOCKSRouteConfirmed(ctx, path.DNS, 5*time.Second)
}

// VerifyRouteSelection has nothing local to check for Xray: the balancer
// override applied by SelectRoute cannot be read back through the API, and the
// upstream probe is deliberately left to VerifyRoute.
func (adapter *Adapter) VerifyRouteSelection(_ context.Context, path worker.BindingPath) error {
	if path.Route.Selector == "" || path.Executor.ID == "" {
		return fmt.Errorf("xray route %s has no selector or executor", path.Route.ID)
	}
	return nil
}

func (adapter *Adapter) CountRouteConnections(context.Context, worker.BindingPath) (int, error) {
	if adapter.Runtime != nil && adapter.Runtime.SkipReadiness {
		return 0, nil
	}
	return 0, worker.ErrConnectionCountUnsupported
}

func (adapter *Adapter) ListRouteConnections(context.Context, worker.BindingPath) ([]worker.Connection, error) {
	return nil, worker.ErrConnectionObservationUnsupported
}

func (adapter *Adapter) CloseRouteConnection(context.Context, worker.BindingPath, string) error {
	return worker.ErrConnectionObservationUnsupported
}

func (adapter *Adapter) CloseRouteConnections(ctx context.Context, path worker.BindingPath) (int, error) {
	if adapter.Runtime != nil && adapter.Runtime.SkipReadiness {
		return 0, nil
	}
	address, err := xrayControlAddress(path.Worker)
	if err != nil {
		return 0, err
	}
	inbounds := make([]map[string]any, 0, 2)
	ids := make([]string, 0, 2)
	for _, inbound := range []state.WorkerInbound{path.TProxy, path.DNS} {
		compiled, err := compileXrayInbound(inbound)
		if err != nil {
			return 0, err
		}
		inbounds = append(inbounds, compiled)
		ids = append(ids, inbound.ID)
	}
	body, err := json.Marshal(map[string]any{"inbounds": inbounds})
	if err != nil {
		return 0, err
	}
	runtimeDir := os.TempDir()
	if adapter.Runtime != nil && strings.TrimSpace(adapter.Runtime.RuntimeDir) != "" {
		runtimeDir = adapter.Runtime.RuntimeDir
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return 0, err
	}
	directory, err := os.MkdirTemp(runtimeDir, "xray-route-reset-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(directory)
	configPath := filepath.Join(directory, "inbounds.json")
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		return 0, err
	}
	remove := []string{adapter.binary(), "api", "rmi", "--server=" + address}
	remove = append(remove, ids...)
	if _, err := adapter.runner().Run(ctx, remove); err != nil {
		return 0, err
	}
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := adapter.runner().Run(restoreCtx, []string{adapter.binary(), "api", "adi", "--server=" + address, configPath}); err != nil {
		return 0, fmt.Errorf("restore Xray route inbounds after close: %w", err)
	}
	return 0, nil
}

func (adapter *Adapter) binary() string {
	if strings.TrimSpace(adapter.Binary) != "" {
		return adapter.Binary
	}
	return "/usr/bin/xray"
}

func (adapter *Adapter) runner() netgate.Runner {
	if adapter.Runtime != nil && adapter.Runtime.Runner != nil {
		return adapter.Runtime.Runner
	}
	return netgate.ShellRunner{}
}

func xrayControlAddress(target state.WorkerTarget) (string, error) {
	control := target.Control
	if control.Kind != "grpc" || control.Port < 1 || control.Port > 65535 {
		return "", fmt.Errorf("Xray route has invalid control endpoint")
	}
	if control.Listen != "127.0.0.1" && control.Listen != "::1" {
		return "", fmt.Errorf("Xray route control must be loopback")
	}
	return net.JoinHostPort(control.Listen, strconv.Itoa(control.Port)), nil
}

func BuildConfig(request worker.CompileRequest) (Config, []string, error) {
	if request.Worker.Core != smartnode.CoreID("xray") {
		return Config{}, nil, fmt.Errorf("worker %s is not an Xray target", request.Worker.ID)
	}
	executors := make(map[string]state.NodeExecutor, len(request.Executors))
	outbounds := make([]map[string]any, 0, len(request.Executors)+2)
	var sensitive []string
	for _, executor := range request.Executors {
		if executor.Worker != request.Worker.ID || executor.Core != smartnode.CoreID("xray") {
			return Config{}, nil, fmt.Errorf("executor %s is assigned to a different worker", executor.ID)
		}
		source, exists := request.Identities[executor.SmartNode]
		if !exists {
			return Config{}, nil, fmt.Errorf("executor %s has no protected identity input", executor.ID)
		}
		outbound, secrets, err := compileOutbound(executor, source)
		if err != nil {
			return Config{}, nil, err
		}
		outbounds = append(outbounds, outbound)
		sensitive = append(sensitive, secrets...)
		executors[executor.ID] = executor
	}
	sort.Slice(outbounds, func(i, j int) bool { return fmt.Sprint(outbounds[i]["tag"]) < fmt.Sprint(outbounds[j]["tag"]) })
	routes := make(map[string]state.WorkerRoute, len(request.Worker.Routes))
	balancers := make([]map[string]any, 0, len(request.Worker.Routes))
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
		balancers = append(balancers, map[string]any{
			"tag": route.Selector, "selector": append([]string(nil), route.Candidates...),
			"strategy": map[string]any{"type": "random"},
		})
	}
	outbounds = append(outbounds,
		map[string]any{"tag": "Block", "protocol": "blackhole", "settings": map[string]any{}},
		map[string]any{"tag": "api", "protocol": "freedom", "settings": map[string]any{}},
	)

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

	inbounds := make([]map[string]any, 0, len(request.Worker.Inbounds)+1)
	rules := make([]map[string]any, 0, len(request.Worker.Inbounds)+1)
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
		compiled, err := compileXrayInbound(inbound)
		if err != nil {
			return Config{}, nil, err
		}
		inbounds = append(inbounds, compiled)
		rules = append(rules, map[string]any{
			"type": "field", "inboundTag": []string{inbound.ID}, "balancerTag": route.Selector,
		})
	}
	if len(inbounds) == 0 {
		return Config{}, nil, fmt.Errorf("Xray worker %s has no active inbounds", request.Worker.ID)
	}
	control := request.Worker.Control
	if control.Listen != "127.0.0.1" && control.Listen != "::1" {
		return Config{}, nil, fmt.Errorf("Xray control API must bind loopback")
	}
	inbounds = append(inbounds, map[string]any{
		"tag": "api-in", "listen": control.Listen, "port": control.Port, "protocol": "dokodemo-door",
		"settings": map[string]any{"address": control.Listen},
	})
	rules = append([]map[string]any{{"type": "field", "inboundTag": []string{"api-in"}, "outboundTag": "api"}}, rules...)
	return Config{
		Log:       map[string]any{"loglevel": "warning"},
		Inbounds:  inbounds,
		Outbounds: outbounds,
		Routing:   map[string]any{"domainStrategy": "AsIs", "rules": rules, "balancers": balancers},
		Policy: map[string]any{
			"levels": map[string]any{"0": map[string]any{"statsUserUplink": true, "statsUserDownlink": true}},
			"system": map[string]any{"statsInboundUplink": true, "statsInboundDownlink": true, "statsOutboundUplink": true, "statsOutboundDownlink": true},
		},
		Stats: map[string]any{},
		API:   map[string]any{"tag": "api", "services": []string{"HandlerService", "LoggerService", "RoutingService", "StatsService"}},
	}, uniqueSensitive(sensitive), nil
}

func compileXrayInbound(inbound state.WorkerInbound) (map[string]any, error) {
	switch inbound.Kind {
	case "tproxy":
		return map[string]any{
			"tag": inbound.ID, "listen": inbound.Listen, "port": inbound.Port, "protocol": "dokodemo-door",
			"settings":       map[string]any{"network": "tcp,udp", "followRedirect": true},
			"streamSettings": map[string]any{"sockopt": map[string]any{"tproxy": "tproxy"}},
			"sniffing":       map[string]any{"enabled": true, "destOverride": []string{"http", "tls", "quic"}, "routeOnly": true},
		}, nil
	case "socks":
		if inbound.Listen != "127.0.0.1" && inbound.Listen != "::1" {
			return nil, fmt.Errorf("worker DNS SOCKS inbound %s must be loopback", inbound.ID)
		}
		return map[string]any{
			"tag": inbound.ID, "listen": inbound.Listen, "port": inbound.Port, "protocol": "socks",
			"settings": map[string]any{"auth": "noauth", "udp": false},
		}, nil
	default:
		return nil, fmt.Errorf("worker inbound %s has unsupported kind %q", inbound.ID, inbound.Kind)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func compileOutbound(executor state.NodeExecutor, source smartnode.IdentitySource) (map[string]any, []string, error) {
	identity := source.Identity
	if identity.Server == "" || identity.Port < 1 || identity.Port > 65535 {
		return nil, nil, fmt.Errorf("executor %s has invalid server endpoint", executor.ID)
	}
	secrets, err := source.ResolveSecrets()
	if err != nil {
		return nil, nil, fmt.Errorf("executor %s: %w", executor.ID, err)
	}
	settings := map[string]any{}
	var sensitive []string
	switch identity.Protocol {
	case "vless":
		id := secretString(secrets, "uuid")
		if id == "" {
			return nil, nil, fmt.Errorf("executor %s VLESS uuid is required", executor.ID)
		}
		encryption, err := identityExtraString(source, "encryption")
		if err != nil {
			return nil, nil, fmt.Errorf("executor %s: %w", executor.ID, err)
		}
		user := map[string]any{"id": id, "encryption": firstNonEmpty(encryption, "none")}
		if identity.Security.Flow != "" {
			user["flow"] = identity.Security.Flow
		}
		settings["vnext"] = []map[string]any{{"address": identity.Server, "port": identity.Port, "users": []map[string]any{user}}}
		sensitive = append(sensitive, id)
	case "vmess":
		id := secretString(secrets, "uuid")
		if id == "" {
			return nil, nil, fmt.Errorf("executor %s VMess uuid is required", executor.ID)
		}
		alterID, err := identityExtraInt(source, "alter_id")
		if err != nil {
			return nil, nil, fmt.Errorf("executor %s: %w", executor.ID, err)
		}
		security, err := identityExtraString(source, "security")
		if err != nil {
			return nil, nil, fmt.Errorf("executor %s: %w", executor.ID, err)
		}
		user := map[string]any{"id": id, "alterId": alterID, "security": firstNonEmpty(security, "auto")}
		settings["vnext"] = []map[string]any{{"address": identity.Server, "port": identity.Port, "users": []map[string]any{user}}}
		sensitive = append(sensitive, id)
	case "trojan":
		password := secretString(secrets, "password")
		if password == "" {
			return nil, nil, fmt.Errorf("executor %s Trojan password is required", executor.ID)
		}
		settings["servers"] = []map[string]any{{"address": identity.Server, "port": identity.Port, "password": password}}
		sensitive = append(sensitive, password)
	case "shadowsocks":
		password := secretString(secrets, "password")
		method, err := identityExtraString(source, "method")
		if err != nil {
			return nil, nil, fmt.Errorf("executor %s: %w", executor.ID, err)
		}
		if password == "" || method == "" {
			return nil, nil, fmt.Errorf("executor %s Shadowsocks method and password are required", executor.ID)
		}
		settings["servers"] = []map[string]any{{"address": identity.Server, "port": identity.Port, "method": method, "password": password}}
		sensitive = append(sensitive, password)
	case "socks", "http":
		serverConfig := map[string]any{"address": identity.Server, "port": identity.Port}
		username := secretString(secrets, "username")
		password := secretString(secrets, "password")
		if username != "" || password != "" {
			serverConfig["users"] = []map[string]any{{"user": username, "pass": password}}
			sensitive = append(sensitive, username, password)
		}
		settings["servers"] = []map[string]any{serverConfig}
	default:
		return nil, nil, fmt.Errorf("executor %s protocol %s is unsupported by Xray adapter", executor.ID, identity.Protocol)
	}
	stream, streamSecrets, err := compileStreamSettings(executor.ID, source)
	if err != nil {
		return nil, nil, err
	}
	sensitive = append(sensitive, streamSecrets...)
	return map[string]any{
		"tag": executor.ID, "protocol": identity.Protocol, "settings": settings, "streamSettings": stream,
		"mux": map[string]any{"enabled": false},
	}, sensitive, nil
}

func compileStreamSettings(executorID string, source smartnode.IdentitySource) (map[string]any, []string, error) {
	transport := source.Identity.Transport
	network := strings.ToLower(firstNonEmpty(transport.Type, "tcp"))
	stream := map[string]any{"network": network, "sockopt": map[string]any{"tcpFastOpen": false}}
	switch network {
	case "tcp":
	case "ws":
		settings := map[string]any{"path": firstNonEmpty(transport.Path, "/")}
		if transport.Host != "" {
			settings["headers"] = map[string]any{"Host": transport.Host}
		}
		stream["wsSettings"] = settings
	case "grpc":
		if transport.ServiceName == "" {
			return nil, nil, fmt.Errorf("executor %s gRPC service name is required", executorID)
		}
		stream["grpcSettings"] = map[string]any{"serviceName": transport.ServiceName}
	case "httpupgrade":
		settings := map[string]any{"path": firstNonEmpty(transport.Path, "/")}
		if transport.Host != "" {
			settings["host"] = transport.Host
		}
		stream["httpupgradeSettings"] = settings
	case "xhttp":
		settings := map[string]any{"path": firstNonEmpty(transport.Path, "/")}
		if transport.Host != "" {
			settings["host"] = transport.Host
		}
		mode, err := identityExtraString(source, "transport.mode")
		if err != nil {
			return nil, nil, fmt.Errorf("executor %s: %w", executorID, err)
		}
		if mode != "" {
			settings["mode"] = mode
		}
		stream["xhttpSettings"] = settings
	case "quic":
		stream["quicSettings"] = map[string]any{}
	default:
		return nil, nil, fmt.Errorf("executor %s transport %s is unsupported by Xray adapter", executorID, network)
	}

	security := source.Identity.Security
	if security.Type == "reality" {
		if security.PublicKeyRef == "" {
			return nil, nil, fmt.Errorf("executor %s REALITY public key is required", executorID)
		}
		var publicKey string
		if err := source.ResolveReference(security.PublicKeyRef, &publicKey); err != nil {
			return nil, nil, fmt.Errorf("executor %s: %w", executorID, err)
		}
		if security.SNI == "" {
			return nil, nil, fmt.Errorf("executor %s REALITY server name is required", executorID)
		}
		settings := map[string]any{
			"serverName": security.SNI, "publicKey": publicKey,
			"fingerprint": firstNonEmpty(security.Fingerprint, "chrome"),
		}
		if security.ShortID != "" {
			settings["shortId"] = security.ShortID
		}
		stream["security"] = "reality"
		stream["realitySettings"] = settings
		return stream, []string{publicKey, security.ShortID}, nil
	}
	if security.Type == "tls" {
		settings := map[string]any{
			"serverName":    security.SNI,
			"allowInsecure": security.SkipCertVerify,
		}
		if len(security.ALPN) > 0 {
			settings["alpn"] = append([]string(nil), security.ALPN...)
		}
		if security.Fingerprint != "" {
			settings["fingerprint"] = security.Fingerprint
		}
		stream["security"] = "tls"
		stream["tlsSettings"] = settings
	} else if security.Type != "none" {
		return nil, nil, fmt.Errorf("executor %s security %s is unsupported by Xray adapter", executorID, security.Type)
	}
	return stream, nil, nil
}

func secretString(secrets map[string]any, key string) string {
	value, exists := secrets[key]
	if !exists || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func identityExtraString(source smartnode.IdentitySource, key string) (string, error) {
	if _, exists := source.Identity.Extra[key]; !exists {
		return "", nil
	}
	var value string
	if _, err := source.ResolveExtra(key, &value); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func identityExtraInt(source smartnode.IdentitySource, key string) (int, error) {
	if _, exists := source.Identity.Extra[key]; !exists {
		return 0, nil
	}
	var value int
	if _, err := source.ResolveExtra(key, &value); err != nil {
		return 0, err
	}
	return value, nil
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

func uniqueSensitive(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
