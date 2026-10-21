// 书房 设计语言共享 token / inline style 片段
// 用 inline style 而不是 Tailwind class，因为颜色全靠 CSS 变量
// 由 index.css 的 [data-style="reading"][data-theme="..."] 提供

export const serif = { fontFamily: "var(--font-serif)" } as const;
export const sans = { fontFamily: "var(--font-sans)" } as const;
export const mono = { fontFamily: "var(--font-mono)" } as const;

export const eyebrowStyle = {
  color: "var(--muted)",
  fontSize: 11,
  letterSpacing: "0.22em",
  textTransform: "uppercase" as const,
  fontWeight: 500,
  fontFamily: "var(--font-sans)",
} as const;

// 状态色映射
export const tone = {
  ok: "var(--success)",
  warn: "var(--warning)",
  err: "var(--danger)",
  info: "var(--info)",
  muted: "var(--muted)",
} as const;
