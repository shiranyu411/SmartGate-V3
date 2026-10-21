import type { GatewayMode } from "../lib/types.js";

export type GatewayModeState = {
  saved: GatewayMode;
  applied?: GatewayMode;
  running?: GatewayMode;
  dirty: boolean;
};

export function gatewayModeLabel(mode?: GatewayMode): string {
  switch (mode) {
    case "on":
      return "开启";
    case "global":
      return "全局代理";
    case "safe":
      return "安全直连";
    case "off":
      return "关闭";
    default:
      return "未知";
  }
}

export function gatewayModeHeadline(mode?: GatewayMode): string {
  switch (mode) {
    case "on":
      return "全屋透明代理运行中，书房正常。";
    case "global":
      return "公网流量全部走代理，LAN 保持直连。";
    case "safe":
      return "当前运行于安全直连模式。";
    case "off":
      return "网关运行功能当前已关闭。";
    default:
      return "网关运行状态等待确认…";
  }
}

export function gatewayModeSummary(state: GatewayModeState): string {
  const running = gatewayModeLabel(state.running);
  const applied = gatewayModeLabel(state.applied);
  const saved = gatewayModeLabel(state.saved);
  if (state.dirty || state.saved !== state.applied) {
    return `正在运行：${running}；已应用：${applied}；已保存目标：${saved}（待应用）。`;
  }
  return `正在运行：${running}；已应用与已保存配置均为：${applied}。`;
}
