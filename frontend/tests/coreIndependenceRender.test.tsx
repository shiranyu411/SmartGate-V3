// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../src/lib/api";
import { keys } from "../src/lib/queries";
import type { CoreInfo, SmartNode, WorkerStatus } from "../src/lib/types";
import { NodesPage } from "../src/pages/Nodes";
import { WorkersPage } from "../src/pages/Workers";

const thirdCore = "fakecore";
const nodeID = "node-third-core";

const cores: CoreInfo[] = [
  { id: "core-a", display_name: "Core A" },
  { id: "core-b", display_name: "Core B" },
  { id: thirdCore, display_name: "" },
];

const node: SmartNode = {
  id: nodeID,
  revision: 1,
  display_name: "Third-core fixture",
  identity: {
    protocol: "fooprotocol",
    server: "127.0.0.1",
    port: 18080,
    transport: { type: "tcp" },
    security: { type: "none" },
    secret_ref_masked: "secret://fixture",
  },
  aliases: [],
  capability: {
    "core-a": { status: "unsupported_by_core", reasons: ["unsupported_protocol"] },
    "core-b": { status: "unsupported_by_core", reasons: ["unsupported_protocol"] },
    [thirdCore]: { status: "supported", reasons: [] },
  },
  core_choice: thirdCore,
  referenced_by_pools: [],
};

const worker: WorkerStatus = {
  id: "worker-third-core",
  core: thirdCore,
  version: "dev",
  state: "running",
  executors: [],
  pools: [],
};

let mountedRoot: Root | undefined;
let container: HTMLDivElement | undefined;

afterEach(async () => {
  if (mountedRoot) {
    await act(async () => mountedRoot?.unmount());
  }
  container?.remove();
  mountedRoot = undefined;
  container = undefined;
  vi.restoreAllMocks();
});

describe("FakeCore frontend extension contract", () => {
  it("keeps the core-choice result visible when an older API returns null affected pools", async () => {
    const queryClient = createQueryClient();
    const editable: SmartNode = {
      ...node,
      capability: {
        [thirdCore]: { status: "supported", reasons: [] },
        "core-a": { status: "supported", reasons: [] },
      },
    };
    queryClient.setQueryData(keys.nodes, { nodes: [editable] });
    queryClient.setQueryData(keys.smartNode(nodeID), editable);
    queryClient.setQueryData(keys.providers, { providers: [] });
    queryClient.setQueryData(keys.cores, cores);
    const updated = { ...editable, core_choice: "core-a" };
    vi.spyOn(api, "put").mockResolvedValue({ smart_node: updated, affected_pools: null, dirty: false });
    vi.spyOn(api, "get").mockImplementation(async (path) =>
      path === "/nodes" ? { nodes: [updated] } : path.startsWith("/nodes/") ? updated : {},
    );
    await renderPage(<NodesPage />, queryClient);
    await act(async () => {
      container?.querySelector("tbody tr[tabindex='0']")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    const coreButton = [...sectionNamed("执行核心选择").querySelectorAll("button")]
      .find((button) => button.textContent === "Core A");
    await act(async () => {
      coreButton?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    const save = [...(container?.querySelectorAll("button") ?? [])]
      .find((button) => button.textContent === "保存核心选择");
    expect(save?.disabled).toBe(false);
    await act(async () => {
      save?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 30));
    });
    expect(container?.textContent).toContain("无 Pool 引用此节点");
  });

  it("keeps unreferenced legacy nodes visible during interactions and cache updates", async () => {
    const queryClient = createQueryClient();
    const legacy = {
      ...node,
      aliases: null,
      referenced_by_pools: null,
      capability: { [thirdCore]: { status: "supported", reasons: null } },
    } as unknown as SmartNode;
    queryClient.setQueryData(keys.nodes, { nodes: [legacy] });
    queryClient.setQueryData(keys.smartNode(nodeID), legacy);
    queryClient.setQueryData(keys.providers, { providers: [] });
    queryClient.setQueryData(keys.cores, cores);

    await renderPage(<NodesPage />, queryClient);
    const nodeRow = requireElement(container?.querySelector("tbody tr[tabindex='0']"), "unreferenced node row");
    await act(async () => {
      nodeRow.dispatchEvent(new MouseEvent("mouseover", { bubbles: true }));
      container?.dispatchEvent(new Event("scroll", { bubbles: true }));
      nodeRow.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    expect(sectionNamed("执行核心选择").textContent).toContain("引用 0 个 Pool");
    await act(async () => {
      queryClient.setQueryData(keys.nodes, { nodes: [{ ...legacy, revision: 2 }] });
      queryClient.setQueryData(keys.smartNode(nodeID), { ...legacy, revision: 2 });
      await new Promise((resolve) => setTimeout(resolve, 20));
    });
    expect(container?.textContent).toContain("Third-core fixture");
    expect(container?.textContent).toContain("引用 0 个 Pool");
  });

  it("renders the third capability row and core choice with the core ID fallback", async () => {
    const queryClient = createQueryClient();
    queryClient.setQueryData(keys.nodes, { nodes: [node] });
    queryClient.setQueryData(keys.smartNode(nodeID), node);
    queryClient.setQueryData(keys.providers, { providers: [] });
    queryClient.setQueryData(keys.cores, cores);

    await renderPage(<NodesPage />, queryClient);

    const nodeRow = requireElement(container?.querySelector("tbody tr[tabindex='0']"), "node row");
    await act(async () => {
      nodeRow.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });

    const capabilitySection = sectionNamed("Capability");
    const capabilityRows = capabilitySection.querySelectorAll(":scope > div.divide-y > div");
    expect(capabilityRows).toHaveLength(3);
    expect(capabilityRows[2]?.textContent).toContain(thirdCore);
    expect(capabilityRows[2]?.textContent).toContain("支持");

    const choiceSection = sectionNamed("执行核心选择");
    const choices = [...choiceSection.querySelectorAll("button")];
    expect(choices).toHaveLength(3);
    expect(choices.map((choice) => choice.textContent)).toContain(thirdCore);
  });

  it("renders a third-core worker with the core ID fallback", async () => {
    const queryClient = createQueryClient();
    queryClient.setQueryData(keys.workers, { workers: [worker] });
    queryClient.setQueryData(keys.cores, cores);

    await renderPage(<WorkersPage />, queryClient);

    const heading = [...(container?.querySelectorAll("h2") ?? [])]
      .find((element) => element.textContent === thirdCore);
    expect(heading).toBeDefined();
    expect(container?.textContent).toContain("worker-third-core");
  });
});

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
}

async function renderPage(page: React.ReactNode, queryClient: QueryClient) {
  container = document.createElement("div");
  document.body.append(container);
  mountedRoot = createRoot(container);
  await act(async () => {
    mountedRoot?.render(<QueryClientProvider client={queryClient}>{page}</QueryClientProvider>);
  });
}

function sectionNamed(title: string): HTMLElement {
  const heading = [...(container?.querySelectorAll("h3") ?? [])]
    .find((element) => element.textContent === title);
  return requireElement(heading?.closest("section"), `${title} section`);
}

function requireElement<T extends Element>(element: T | null | undefined, label: string): T {
  if (!element) throw new Error(`missing ${label}`);
  return element;
}
