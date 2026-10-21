import type { ReactNode } from "react";
import { sans } from "./tokens";

type Tone = "ok" | "warn" | "err" | "info" | "muted" | "accent";

const palette: Record<Tone, { fg: string; bg: string; bd: string }> = {
  ok: { fg: "var(--success)", bg: "color-mix(in srgb, var(--success) 12%, transparent)", bd: "color-mix(in srgb, var(--success) 30%, transparent)" },
  warn: { fg: "var(--warning)", bg: "color-mix(in srgb, var(--warning) 14%, transparent)", bd: "color-mix(in srgb, var(--warning) 30%, transparent)" },
  err: { fg: "var(--danger)", bg: "color-mix(in srgb, var(--danger) 12%, transparent)", bd: "color-mix(in srgb, var(--danger) 30%, transparent)" },
  info: { fg: "var(--info)", bg: "color-mix(in srgb, var(--info) 12%, transparent)", bd: "color-mix(in srgb, var(--info) 30%, transparent)" },
  muted: { fg: "var(--muted)", bg: "var(--surface-2)", bd: "var(--border)" },
  accent: { fg: "var(--accent)", bg: "color-mix(in srgb, var(--accent) 10%, transparent)", bd: "color-mix(in srgb, var(--accent) 30%, transparent)" },
};

export function Badge({ tone = "muted", children }: { tone?: Tone; children: ReactNode }) {
  const p = palette[tone];
  return (
    <span
      className="inline-flex items-center gap-1.5 px-2.5 py-0.5 text-[11px] tracking-wide whitespace-nowrap shrink-0 rounded-[6px]"
      style={{ background: p.bg, color: p.fg, border: `1px solid ${p.bd}`, ...sans }}
    >
      {children}
    </span>
  );
}

export function Dot({ tone = "ok" }: { tone?: Tone }) {
  return (
    <span
      className="size-1.5 rounded-full inline-block"
      style={{ background: palette[tone].fg, boxShadow: `0 0 8px ${palette[tone].fg}` }}
    />
  );
}
