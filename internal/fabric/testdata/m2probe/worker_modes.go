package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
	workersingbox "smartgate/internal/worker/singbox"
	workerxray "smartgate/internal/worker/xray"
)

func runWorkerExport(args []string) {
	flags := flag.NewFlagSet("worker-export", flag.ExitOnError)
	outputDir := flags.String("output-dir", "", "worker config output directory")
	generalProxy := flags.String("general-proxy", "", "General SOCKS5 proxy address")
	aiProxy := flags.String("ai-proxy", "", "AI SOCKS5 proxy address")
	_ = flags.Parse(args)
	if strings.TrimSpace(*outputDir) == "" {
		fatalf("worker-export output directory is required")
	}
	generalHost, generalPort := parseProxyEndpoint(*generalProxy)
	aiHost, aiPort := parseProxyEndpoint(*aiProxy)
	if err := os.MkdirAll(*outputDir, 0o700); err != nil {
		fatalf("create worker output directory: %v", err)
	}

	generalRequest := probeWorkerRequest(
		probeWorkerTarget("w_sb_1", smartnode.CoreID("sing-box"), "general", "pb_general", singBoxPort, 16002, 9090),
		state.NodeExecutor{ID: "ne_general", SmartNode: "sn_general", Revision: 1, Core: smartnode.CoreID("sing-box"), Worker: "w_sb_1"},
		probeBinding("pb_general", "general", "sn_general", "ne_general", smartnode.CoreID("sing-box"), "w_sb_1"),
		map[string]any{"type": "socks", "server": generalHost, "server_port": generalPort},
	)
	generalConfig, _, err := workersingbox.BuildConfig(generalRequest)
	if err != nil {
		fatalf("build sing-box worker config: %v", err)
	}
	generalPath := filepath.Join(*outputDir, "w_sb_1.json")
	writeJSONConfig(generalPath, generalConfig)

	aiRequest := probeWorkerRequest(
		probeWorkerTarget("w_xr_1", smartnode.CoreID("xray"), "ai", "pb_ai", xrayPort, 17002, 10085),
		state.NodeExecutor{ID: "ne_ai", SmartNode: "sn_ai", Revision: 1, Core: smartnode.CoreID("xray"), Worker: "w_xr_1"},
		probeBinding("pb_ai", "ai", "sn_ai", "ne_ai", smartnode.CoreID("xray"), "w_xr_1"),
		map[string]any{"type": "socks", "server": aiHost, "server_port": aiPort},
	)
	aiConfig, _, err := workerxray.BuildConfig(aiRequest)
	if err != nil {
		fatalf("build Xray worker config: %v", err)
	}
	aiPath := filepath.Join(*outputDir, "w_xr_1.json")
	writeJSONConfig(aiPath, aiConfig)

	fmt.Printf("WORKER_CONFIG w_sb_1 %s\n", generalPath)
	fmt.Printf("WORKER_CONFIG w_xr_1 %s\n", aiPath)
}

func parseProxyEndpoint(raw string) (string, int) {
	host, rawPort, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		fatalf("parse SOCKS5 proxy address %q: %v", raw, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		fatalf("invalid SOCKS5 proxy port in %q", raw)
	}
	if strings.TrimSpace(host) == "" {
		fatalf("SOCKS5 proxy host is required")
	}
	return host, port
}

func probeWorkerTarget(id string, core smartnode.CoreID, pool, binding string, tproxyPort, dnsPort, controlPort int) state.WorkerTarget {
	controlKind := "clash"
	if core == smartnode.CoreID("xray") {
		controlKind = "grpc"
	}
	return state.WorkerTarget{
		ID: id, Core: core,
		Control: state.WorkerControl{Listen: "127.0.0.1", Port: controlPort, Kind: controlKind},
		Inbounds: []state.WorkerInbound{
			{ID: "wi_" + pool + "_tproxy", Kind: "tproxy", Pool: pool, Binding: binding, Generation: 1, Listen: "::", Port: tproxyPort, Network: "tcp,udp"},
			{ID: "wi_" + pool + "_dns", Kind: "socks", Pool: pool, Binding: binding, Generation: 1, Listen: "127.0.0.1", Port: dnsPort, Network: "tcp"},
		},
	}
}

func probeBinding(id, pool, smartNode, executor string, core smartnode.CoreID, workerID string) state.PoolBinding {
	return state.PoolBinding{
		ID: id, Pool: pool, Generation: 1, Since: time.Unix(0, 0).UTC(),
		Target: state.BindingTarget{Kind: "node", SmartNode: smartNode, Executor: executor, Core: core, Worker: workerID},
	}
}

func probeWorkerRequest(target state.WorkerTarget, executor state.NodeExecutor, binding state.PoolBinding, outbound map[string]any) worker.CompileRequest {
	source, err := smartnode.IdentitySourceFromRaw(smartnode.RawNode{
		ProviderID: "m2probe", RawTag: executor.ID, Outbound: outbound,
	})
	if err != nil {
		fatalf("build worker identity: %v", err)
	}
	routeID := "wr_" + binding.Pool
	target.Routes = []state.WorkerRoute{{ID: routeID, Pool: binding.Pool, Selector: "Pool_" + binding.Pool, Candidates: []string{executor.ID}, Selected: executor.ID}}
	for i := range target.Inbounds {
		target.Inbounds[i].Route = routeID
	}
	return worker.CompileRequest{
		Worker: target, Executors: []state.NodeExecutor{executor}, Bindings: []state.PoolBinding{binding},
		Identities: map[string]smartnode.IdentitySource{executor.SmartNode: source}, Generation: time.Unix(0, 0).UTC(),
	}
}

func writeJSONConfig(path string, value any) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatalf("encode %s: %v", path, err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		fatalf("write %s: %v", path, err)
	}
}
