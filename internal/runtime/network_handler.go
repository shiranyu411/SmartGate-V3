package runtime

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"smartgate/internal/netgate"
	"strings"
	"time"

	"smartgate/internal/state"
)

type networkCheck struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
type networkReport struct {
	Status    string         `json:"status"`
	Checks    []networkCheck `json:"checks"`
	CheckedAt time.Time      `json:"checked_at"`
}

func (api systemAPI) networkDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeAPIData(w, http.StatusOK, api.checkNetwork(ctx))
}

func (api systemAPI) checkNetwork(ctx context.Context) networkReport {
	report := networkReport{Status: "ok", Checks: []networkCheck{}, CheckedAt: time.Now().UTC()}
	add := func(id, label string, check func(context.Context) error) {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ok := check(checkCtx) == nil
		detail := "检查通过"
		if !ok {
			detail = "检查失败，请尝试恢复网络服务；若持续失败请查看系统日志"
			report.Status = "degraded"
		}
		report.Checks = append(report.Checks, networkCheck{id, label, ok, detail})
	}
	command := func(argv []string) func(context.Context) error {
		return func(ctx context.Context) error {
			result := api.run(ctx, argv)
			if !result.OK || systemCommandIsDevNoop(result) {
				return fmt.Errorf("network check failed")
			}
			return nil
		}
	}
	if api.store == nil {
		report.Status = "unavailable"
		return report
	}
	// Rescue is persisted separately from the runtime cache and can change
	// without a daemon restart. Use the live status view, as the system page does.
	if api.statusProvider != nil && api.statusProvider().RescueActive {
		report.Status = "disabled"
		return report
	}
	topology, err := api.store.LoadAppliedTopology()
	runtimeState, runtimeErr := api.store.LoadRuntime()
	if err != nil || runtimeErr != nil {
		report.Status = "unavailable"
		return report
	}
	if runtimeState.RescueActive || (topology.GatewayMode != state.GatewayModeOn && topology.GatewayMode != state.GatewayModeGlobal) {
		report.Status = "disabled"
		return report
	}
	graph, err := rebuildLiveExecutionGraph(topology, time.Now().UTC())
	if err != nil || api.fabric == nil || api.workers == nil {
		report.Status = "unavailable"
		return report
	}
	add("core", "代理服务与 SmartGate DNS", func(ctx context.Context) error {
		for _, binding := range graph.PoolBindings {
			if binding.Target.Kind == "node" && !api.workers.IsRunning(binding.Target.Worker) {
				return fmt.Errorf("bound worker is not running")
			}
		}
		return probeFabricTCP(ctx, graph.Fabric.DNS.Listen)
	})
	add("dnsmasq", "局域网 DNS 服务", command([]string{"/etc/init.d/dnsmasq", "status"}))
	add("dns_rules", "客户端 DNS 接管与策略路由", api.fabric.Verify)
	add("persistence", "防火墙与 DNS 自动恢复", command([]string{"sh", "-c", netgate.NetworkPersistenceVerifyScript, "sh", "/etc/dnsmasq.d/smartgate.conf", "owner=smartgate:dnsmasq:smartgate-upstream"}))
	add("internet", "SmartGate DNS 与当前代理出口", func(ctx context.Context) error {
		host, port, err := net.SplitHostPort(graph.Fabric.DNS.Listen)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			if ip.To4() == nil {
				host = "::1"
			} else {
				host = "127.0.0.1"
			}
		}
		resolver := net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(host, port))
		}}
		addresses, err := resolver.LookupIPAddr(ctx, "www.gstatic.com")
		if err != nil {
			return err
		}
		if len(addresses) == 0 {
			return fmt.Errorf("no DNS answer")
		}
		for _, binding := range graph.PoolBindings {
			if binding.Pool != graph.Fabric.FallbackPool {
				continue
			}
			if binding.Target.Kind == "node" {
				path, err := (nodesAPI{workers: api.workers}).appliedNodeBindingPath(topology, binding.Target.SmartNode)
				if err != nil {
					return err
				}
				_, err = api.workers.DelayPath(ctx, path, defaultNodeDelayURL, 5*time.Second)
				return err
			}
			if binding.Target.Kind == state.NoCandidateBlock {
				return fmt.Errorf("fallback pool is blocked")
			}
			transport := &http.Transport{Proxy: nil}
			defer transport.CloseIdleConnections()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, defaultNodeDelayURL, nil)
			if err != nil {
				return err
			}
			response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			if response.StatusCode != 204 {
				return fmt.Errorf("unexpected HTTP status")
			}
			return nil
		}
		return fmt.Errorf("fallback pool has no binding")
	})
	return report
}

func (api systemAPI) repairNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "method not allowed")
		return
	}
	if !api.confirmed(w, r) {
		return
	}
	if api.networkRepair == nil {
		writeAPIError(w, 503, "network restoration is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := api.networkRepair(ctx); err != nil {
		log.Printf("network service restoration failed: %v", err)
		// Do not return command output or provider configuration to the browser.
		message := "网络恢复失败，请查看系统日志"
		if strings.Contains(err.Error(), "active proxy mode") || strings.Contains(err.Error(), "captured applied") {
			message = "当前为直连/救援模式或尚无已应用配置，未开启代理"
		}
		writeAPIError(w, http.StatusConflict, message)
		return
	}
	report := api.checkNetwork(ctx)
	writeAPIData(w, http.StatusOK, report)
}
