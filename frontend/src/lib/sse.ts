// SSE 客户端：浏览器 EventSource 不能带 header，所以 token 以 query 参数传
// 后端契约：new EventSource(`/api/events?token=${token}`)

import { API_BASE } from "./api";
import { getToken } from "./auth";
import type { RuntimeStatus } from "./types";

export type StatusEvent = {
  time?: string;
  runtime?: Partial<RuntimeStatus>;
};

type Handlers = {
  onStatus?: (ev: StatusEvent) => void;
  onError?: (err: Event) => void;
  onOpen?: () => void;
};

export function openEvents(handlers: Handlers): () => void {
  const token = getToken();
  const url = `${API_BASE}/events${token ? `?token=${encodeURIComponent(token)}` : ""}`;
  const es = new EventSource(url);

  if (handlers.onOpen) {
    es.onopen = () => handlers.onOpen?.();
  }

  es.addEventListener("status", (e) => {
    const me = e as MessageEvent<string>;
    try {
      const parsed = JSON.parse(me.data) as StatusEvent;
      handlers.onStatus?.(parsed);
    } catch {
      /* 忽略坏帧 */
    }
  });

  es.onerror = (err) => {
    handlers.onError?.(err);
  };

  return () => es.close();
}
