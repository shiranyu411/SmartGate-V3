import type { CoreID, SmartNode } from "../lib/types.js";

export type SmartNodeFilters = {
  keyword: string;
  provider: string;
  protocol: string;
  core: string;
  capability: string;
  health: string;
};

export function filterSmartNodes(nodes: SmartNode[], filters: SmartNodeFilters): SmartNode[] {
  const keyword = filters.keyword.trim().toLowerCase();
  return nodes.filter((node) => {
    if (filters.provider && !node.aliases.some((alias) => alias.provider_id === filters.provider)) return false;
    if (filters.protocol && node.identity.protocol !== filters.protocol) return false;
    if (filters.core === "unset" && node.core_choice !== null) return false;
    if (filters.core && filters.core !== "unset" && node.core_choice !== filters.core) return false;
    if (filters.capability && !Object.values(node.capability).some((capability) => capability?.status === filters.capability)) return false;
    if (filters.health === "untested" && node.health) return false;
    if (filters.health && filters.health !== "untested" && node.health?.status !== filters.health) return false;
    if (!keyword) return true;
    const searchText = [
      node.id,
      node.display_name,
      node.identity.protocol,
      node.identity.server,
      ...node.aliases.flatMap((alias) => [alias.provider_id, alias.raw_tag]),
    ].join(" ").toLowerCase();
    return searchText.includes(keyword);
  });
}

export function groupSmartNodesByProvider(nodes: SmartNode[], providerIDs: string[]) {
  providerIDs = [...new Set(providerIDs)];
  const order = new Map(providerIDs.map((id, index) => [id, index]));
  const groups = new Map<string, SmartNode[]>();
  for (const node of nodes) {
    const providerID = node.source_missing ? "" : (
      providerIDs.find((id) => node.aliases.some((alias) => alias.provider_id === id))
      ?? node.aliases[0]?.provider_id
      ?? ""
    );
    const group = groups.get(providerID) ?? [];
    group.push(node);
    groups.set(providerID, group);
  }
  return [...groups].sort(([left], [right]) => {
    if (!left) return right ? 1 : 0;
    if (!right) return -1;
    return (order.get(left) ?? providerIDs.length) - (order.get(right) ?? providerIDs.length)
      || left.localeCompare(right, undefined, { numeric: true });
  }).map(([providerID, members]) => ({ providerID, nodes: members }));
}

export function selectableCores(node: SmartNode): CoreID[] {
  return Object.entries(node.capability)
    .filter(([, capability]) => capability && capability.status !== "unsupported_by_core")
    .map(([core]) => core);
}

export function requiresDegradedConfirmation(node: SmartNode, core: CoreID | ""): boolean {
  return core !== "" && node.capability[core]?.status === "degraded";
}

export function coreIDsForNode(node: SmartNode): CoreID[] {
  return Object.keys(node.capability);
}
