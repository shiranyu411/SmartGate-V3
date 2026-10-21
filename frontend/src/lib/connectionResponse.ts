import type { ConnectionsData } from "./types";

export function normalizeConnections(data: ConnectionsData): ConnectionsData {
  return {
    ...data,
    connections: (data.connections ?? []).map((connection) => ({
      ...connection,
      chains: connection.chains ?? [],
    })),
  };
}
