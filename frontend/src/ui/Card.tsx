import type { CSSProperties, ReactNode } from "react";
import { serif, sans, eyebrowStyle } from "./tokens";

export function Card({
  children,
  className,
  style,
  inset = false,
}: {
  children: ReactNode;
  className?: string;
  style?: CSSProperties;
  inset?: boolean;
}) {
  return (
    <div
      className={`transition-all duration-300 ${className || ""}`}
      style={{
        background: "var(--surface)",
        border: "1px solid var(--border)",
        borderRadius: "var(--radius-md)",
        boxShadow: "var(--shadow-sm)",
        padding: inset ? "28px" : 0,
        ...style,
      }}
    >
      {children}
    </div>
  );
}

export function SectionHeader({
  eyebrow,
  title,
  desc,
  right,
}: {
  eyebrow?: ReactNode;
  title: ReactNode;
  desc?: ReactNode;
  right?: ReactNode;
}) {
  return (
    <div className="flex items-end justify-between gap-4 flex-wrap">
      <div>
        {eyebrow ? <p style={eyebrowStyle}>{eyebrow}</p> : null}
        <h2
          className="text-[26px] mt-2 font-medium"
          style={{ ...serif, color: "var(--fg-strong)", letterSpacing: "-0.01em" }}
        >
          {title}
        </h2>
        {desc ? (
          <p className="text-sm mt-1.5" style={{ ...sans, color: "var(--muted)" }}>
            {desc}
          </p>
        ) : null}
      </div>
      {right}
    </div>
  );
}

export function Rule({ glyph }: { glyph?: string }) {
  if (!glyph) return <hr style={{ borderColor: "var(--border)", margin: 0, opacity: 0.6 }} />;
  return (
    <div className="flex items-center gap-4 py-2" style={{ color: "var(--muted)" }}>
      <span className="flex-1 h-px" style={{ background: "var(--border)", opacity: 0.8 }} />
      <span className="text-xs tracking-[0.4em] uppercase" style={serif}>
        {glyph}
      </span>
      <span className="flex-1 h-px" style={{ background: "var(--border)", opacity: 0.8 }} />
    </div>
  );
}

export function Eyebrow({ children }: { children: ReactNode }) {
  return <p style={eyebrowStyle}>{children}</p>;
}
