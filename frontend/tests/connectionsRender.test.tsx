// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, it, vi } from "vitest";
import { keys } from "../src/lib/queries";
import { ConnectionsPage } from "../src/pages/Connections";

let root: Root | undefined;
let container: HTMLDivElement | undefined;
let client: QueryClient | undefined;
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });

afterEach(async () => {
  await act(async () => root?.unmount());
  container?.remove();
  client?.clear();
  vi.unstubAllGlobals();
});

it("shows DNS candidates with their source, binding paths without raw chains and current/old filters", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  const base = { pool: "general", worker: "worker-a", smart_node: "node-a", core: "core-a", upload: 10, download: 20, chains: [], metadata: { network: "tcp", sourceIP: "192.0.2.10", destinationIP: "198.51.100.20", destinationPort: 443, dnsHosts: ["site.example", "shared.example"] } };
  client.setQueryData(keys.connections, { connections: [{ ...base, id: "current", generation: 2, stale_generation: false }, { ...base, id: "old", generation: 1, stale_generation: true, metadata: { ...base.metadata, host: "reported.example" } }], download_total: 40, upload_total: 20 });
  client.setQueryData(keys.runtimeStatus, { bindings: [], drains: [] });
  client.setQueryData(keys.pools, { business_pools: [{ id: "general", name: "日常上网" }] });
  client.setQueryData(keys.nodes, { nodes: [{ id: "node-a", display_name: "测试节点" }] });
  client.setQueryData(keys.cores, [{ id: "core-a", display_name: "测试核心" }]);
  container = document.createElement("div"); document.body.append(container); root = createRoot(container);
  await act(async () => root?.render(<QueryClientProvider client={client!}><ConnectionsPage /></QueryClientProvider>));
  const table = container.querySelector("table")!;
  expect(table.textContent).toContain("site.example");
  expect(table.textContent).toContain("DNS 关联 · 2 个候选域名");
  expect(table.textContent).toContain("198.51.100.20:443");
  expect(table.textContent).toContain("reported.example");
  expect(table.textContent).toContain("日常上网 → 测试核心 → 测试节点");
  expect(table.textContent).toContain("按业务池线路关联");
  expect(table.textContent).not.toContain("代数");
  const filter = container.querySelector<HTMLButtonElement>('[aria-label="按线路状态筛选"][role="combobox"]')!;
  await act(async () => filter.click());
  await act(async () => (Array.from(document.querySelectorAll('[role="option"]')).find((option) => option.textContent?.includes("切换前的旧线路")) as HTMLElement).click());
  expect(table.querySelectorAll("tbody tr")).toHaveLength(1);
  expect(table.textContent).toContain("reported.example");
  expect(table.textContent).not.toContain("site.example");
});

it("renders and filters kernel-only connections with legacy null chains after a poll", async () => {
  vi.stubGlobal("ResizeObserver", class {
    observe() {}
    disconnect() {}
  });
  client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  const connection = {
    id: "kernel-only", pool: "general", generation: 1, stale_generation: false,
    worker: "", smart_node: "", core: "", upload: 0, download: 0, chains: null,
    metadata: { host: "fixture.example", network: "udp", sourceIP: "192.0.2.10", destinationIP: "198.51.100.20" },
  };
  const data = { connections: [connection], download_total: 0, upload_total: 0 };
  client.setQueryData(keys.connections, data);
  client.setQueryData(keys.runtimeStatus, { bindings: [], drains: [] });
  client.setQueryData(keys.pools, { business_pools: [] });
  client.setQueryData(keys.nodes, { nodes: [] });
  client.setQueryData(keys.cores, []);
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
  await act(async () => {
    root?.render(<QueryClientProvider client={client!}><ConnectionsPage /></QueryClientProvider>);
  });
  expect(container.textContent).toContain("fixture.example");
  await act(async () => {
    client?.setQueryData(keys.connections, { ...data, download_total: 50 });
    await new Promise((resolve) => setTimeout(resolve, 20));
  });
  const input = container.querySelector("input")!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, "fixture");
    input.dispatchEvent(new Event("input", { bubbles: true }));
    container?.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
  expect(container.textContent).toContain("fixture.example");
  expect(container.querySelector("tbody tr")).not.toBeNull();
});
