import type { CoreID, CoreInfo } from "./types.js";

export type CoreLabelMap = Record<CoreID, string>;

/** Subscription User-Agent presets. Kept here so pages stay core-token-free. */
export const SUBSCRIPTION_UA_PRESETS: { value: string; label: string }[] = [
  { value: "", label: "默认（SmartGate/2.0）" },
  { value: "clash-verge/v2.5.2", label: "Clash Verge" },
  { value: "sing-box/1.12.0", label: "sing-box" },
  { value: "mihomo/v1.19.2", label: "mihomo (Clash Meta)" },
  { value: "Shadowrocket/2.2.65", label: "Shadowrocket" },
];

export function coreLabelMap(cores: CoreInfo[] = []): CoreLabelMap {
  return Object.fromEntries(cores.map((core) => [core.id, core.display_name || core.id]));
}

export function coreLabel(core: CoreID | "" | null | undefined, labels: CoreLabelMap = {}): string {
  if (!core) return "未设置";
  return labels[core] || core;
}
