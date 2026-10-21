import type { SmartNode } from "./types";

// Older V3 builds serialize empty Go slices as null.
export function normalizeSmartNode(node: SmartNode): SmartNode {
  return {
    ...node,
    aliases: node.aliases ?? [],
    referenced_by_pools: node.referenced_by_pools ?? [],
    capability: Object.fromEntries(
      Object.entries(node.capability ?? {}).map(([core, capability]) => [
        core,
        capability ? { ...capability, reasons: capability.reasons ?? [] } : capability,
      ]),
    ),
  };
}

export function normalizeSmartNodes(data: { nodes: SmartNode[] }): { nodes: SmartNode[] } {
  return { ...data, nodes: (data.nodes ?? []).map(normalizeSmartNode) };
}
