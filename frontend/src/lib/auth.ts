// Token 存取 + 订阅，供 api.ts 与 React 端共用
// 后端契约：localStorage.setItem("smartgate_token", token)

const KEY = "smartgate_token";

type Listener = (token: string | null) => void;
const listeners = new Set<Listener>();

export function getToken(): string {
  try {
    return localStorage.getItem(KEY) || "";
  } catch {
    return "";
  }
}

export function setToken(token: string): void {
  try {
    localStorage.setItem(KEY, token);
  } catch {
    /* 忽略 */
  }
  for (const fn of listeners) fn(token);
}

export function clearToken(): void {
  try {
    localStorage.removeItem(KEY);
  } catch {
    /* 忽略 */
  }
  for (const fn of listeners) fn(null);
}

export function subscribeToken(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
