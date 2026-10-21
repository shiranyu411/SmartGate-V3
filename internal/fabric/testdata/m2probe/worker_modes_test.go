package main

import (
	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	workersingbox "smartgate/internal/worker/singbox"
	workerxray "smartgate/internal/worker/xray"
	"testing"
)

func TestRealCoreSmokeFixturesHaveRoutableInbounds(t *testing.T) {
	for _, core := range []smartnode.CoreID{"sing-box", "xray"} {
		t.Run(string(core), func(t *testing.T) {
			target := probeWorkerTarget("fixture-worker", core, "general", "binding-general", 16001, 16002, 19090)
			executor := state.NodeExecutor{ID: "fixture-executor", SmartNode: "fixture-node", Revision: 1, Core: core, Worker: target.ID}
			binding := probeBinding("binding-general", "general", executor.SmartNode, executor.ID, core, target.ID)
			request := probeWorkerRequest(target, executor, binding, map[string]any{"type": "socks", "server": "192.0.2.1", "server_port": 1080})
			if len(request.Worker.Routes) != 1 {
				t.Fatal("fixture has no worker route")
			}
			for _, inbound := range request.Worker.Inbounds {
				if inbound.Route != request.Worker.Routes[0].ID {
					t.Fatal("fixture inbound has no route reference")
				}
			}
			var err error
			if core == "sing-box" {
				_, _, err = workersingbox.BuildConfig(request)
			} else {
				_, _, err = workerxray.BuildConfig(request)
			}
			if err != nil {
				t.Fatalf("real-core smoke fixture is invalid: %v", err)
			}
		})
	}
}
