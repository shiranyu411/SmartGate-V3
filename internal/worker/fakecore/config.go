package fakecore

import (
	"fmt"
	"sort"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
	"smartgate/internal/worker"
)

type Config struct {
	Core     smartnode.CoreID `json:"core"`
	Worker   WorkerConfig     `json:"worker"`
	Nodes    []NodeConfig     `json:"nodes"`
	Routes   []RouteConfig    `json:"routes"`
	Inbounds []InboundConfig  `json:"inbounds"`
}

type WorkerConfig struct {
	ID      string              `json:"id"`
	Control state.WorkerControl `json:"control"`
}

type NodeConfig struct {
	Executor  string                 `json:"executor"`
	SmartNode string                 `json:"smart_node"`
	Revision  int                    `json:"revision"`
	Identity  smartnode.NodeIdentity `json:"identity"`
	RawTag    string                 `json:"raw_tag"`
}

type RouteConfig struct {
	ID         string   `json:"id"`
	Pool       string   `json:"pool"`
	Lane       int      `json:"lane"`
	Selector   string   `json:"selector"`
	Candidates []string `json:"candidates"`
	Selected   string   `json:"selected"`
}

type InboundConfig struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Pool       string `json:"pool"`
	Route      string `json:"route"`
	Lane       int    `json:"lane"`
	Binding    string `json:"binding,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	Listen     string `json:"listen"`
	Port       int    `json:"port"`
	Network    string `json:"network"`
}

func BuildConfig(request worker.CompileRequest) (Config, error) {
	if request.Worker.Core != CoreID {
		return Config{}, fmt.Errorf("worker %s is not a FakeCore target", request.Worker.ID)
	}
	if request.Worker.ID == "" {
		return Config{}, fmt.Errorf("FakeCore worker id is required")
	}
	if request.Worker.Control.Listen != "127.0.0.1" && request.Worker.Control.Listen != "::1" {
		return Config{}, fmt.Errorf("FakeCore control API must bind loopback")
	}
	if request.Worker.Control.Port < 1 || request.Worker.Control.Port > 65535 {
		return Config{}, fmt.Errorf("FakeCore control API port is invalid")
	}

	executors := make(map[string]state.NodeExecutor, len(request.Executors))
	nodes := make([]NodeConfig, 0, len(request.Executors))
	for _, executor := range sortedExecutors(request.Executors) {
		if executor.Worker != request.Worker.ID || executor.Core != CoreID {
			return Config{}, fmt.Errorf("executor %s is assigned to a different worker", executor.ID)
		}
		source, exists := request.Identities[executor.SmartNode]
		if !exists {
			return Config{}, fmt.Errorf("executor %s has no protected identity input", executor.ID)
		}
		if source.Identity.Protocol != "fooprotocol" && source.Identity.Protocol != "tuic" && source.Identity.Protocol != "hysteria2" && source.Identity.Protocol != "vless" {
			return Config{}, fmt.Errorf("executor %s protocol %s is not supported by FakeCore", executor.ID, source.Identity.Protocol)
		}
		nodes = append(nodes, NodeConfig{
			Executor: executor.ID, SmartNode: executor.SmartNode, Revision: executor.Revision,
			Identity: source.Identity, RawTag: source.RawTag,
		})
		executors[executor.ID] = executor
	}
	if len(nodes) == 0 {
		return Config{}, fmt.Errorf("FakeCore worker %s has no executors", request.Worker.ID)
	}

	routes := make(map[string]state.WorkerRoute, len(request.Worker.Routes))
	compiledRoutes := make([]RouteConfig, 0, len(request.Worker.Routes))
	for _, route := range sortedRoutes(request.Worker.Routes) {
		if route.ID == "" || route.Selector == "" || route.Pool == "" || len(route.Candidates) == 0 {
			return Config{}, fmt.Errorf("worker %s has an incomplete route", request.Worker.ID)
		}
		if _, exists := routes[route.ID]; exists {
			return Config{}, fmt.Errorf("worker %s has duplicate route %s", request.Worker.ID, route.ID)
		}
		for _, candidate := range route.Candidates {
			if _, ok := executors[candidate]; !ok {
				return Config{}, fmt.Errorf("worker route %s references missing executor %s", route.ID, candidate)
			}
		}
		if _, ok := executors[route.Selected]; !ok || !containsString(route.Candidates, route.Selected) {
			return Config{}, fmt.Errorf("worker route %s has invalid selected executor %s", route.ID, route.Selected)
		}
		candidates := append([]string(nil), route.Candidates...)
		sort.Strings(candidates)
		compiledRoutes = append(compiledRoutes, RouteConfig{
			ID: route.ID, Pool: route.Pool, Lane: route.Lane, Selector: route.Selector,
			Candidates: candidates, Selected: route.Selected,
		})
		routes[route.ID] = route
	}
	if len(compiledRoutes) == 0 {
		return Config{}, fmt.Errorf("FakeCore worker %s has no routes", request.Worker.ID)
	}

	bindings := make(map[string]state.PoolBinding, len(request.Bindings))
	for _, binding := range request.Bindings {
		if binding.Target.Worker != request.Worker.ID || binding.Target.Kind != "node" {
			return Config{}, fmt.Errorf("binding %s is assigned to a different worker", binding.ID)
		}
		if _, ok := executors[binding.Target.Executor]; !ok {
			return Config{}, fmt.Errorf("binding %s references missing executor %s", binding.ID, binding.Target.Executor)
		}
		bindings[binding.ID] = binding
	}

	compiledInbounds := make([]InboundConfig, 0, len(request.Worker.Inbounds))
	for _, inbound := range sortedInbounds(request.Worker.Inbounds) {
		route, ok := routes[inbound.Route]
		if !ok {
			return Config{}, fmt.Errorf("worker inbound %s references missing route %s", inbound.ID, inbound.Route)
		}
		if route.Pool != inbound.Pool || route.Lane != inbound.Lane {
			return Config{}, fmt.Errorf("worker inbound %s route metadata does not match", inbound.ID)
		}
		if inbound.Binding != "" {
			binding, ok := bindings[inbound.Binding]
			if !ok || binding.Pool != inbound.Pool || binding.Generation != inbound.Generation {
				return Config{}, fmt.Errorf("worker inbound %s has invalid active binding metadata", inbound.ID)
			}
		}
		compiledInbounds = append(compiledInbounds, InboundConfig{
			ID: inbound.ID, Kind: inbound.Kind, Pool: inbound.Pool, Route: inbound.Route, Lane: inbound.Lane,
			Binding: inbound.Binding, Generation: inbound.Generation, Listen: inbound.Listen, Port: inbound.Port, Network: inbound.Network,
		})
	}
	if len(compiledInbounds) == 0 {
		return Config{}, fmt.Errorf("FakeCore worker %s has no inbounds", request.Worker.ID)
	}

	return Config{
		Core:   CoreID,
		Worker: WorkerConfig{ID: request.Worker.ID, Control: request.Worker.Control},
		Nodes:  nodes, Routes: compiledRoutes, Inbounds: compiledInbounds,
	}, nil
}
