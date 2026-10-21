import type { ButtonHTMLAttributes, ReactNode } from "react";
import { sans } from "./tokens";

type Variant = "primary" | "ghost" | "danger" | "link";
type Size = "sm" | "md";

type Props = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: Variant;
  size?: Size;
  loading?: boolean;
};

export function Button({
  variant = "primary",
  size = "md",
  loading = false,
  disabled,
  style,
  className,
  children,
  ...rest
}: Props) {
  const base: React.CSSProperties = {
    ...sans,
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    gap: 6,
    whiteSpace: "nowrap",
    flexShrink: 0,
    lineHeight: 1.2,
    transition: "all 200ms cubic-bezier(0.4, 0, 0.2, 1)",
    cursor: loading || disabled ? "not-allowed" : "pointer",
    opacity: loading || disabled ? 0.6 : 1,
    // 中文标签下 0.04em 会额外拉宽并易触发断行，这里只用轻微字距
    letterSpacing: "0.01em",
    fontWeight: 500,
    borderRadius: "var(--radius-sm)",
  };

  const sizing =
    size === "sm"
      ? { minHeight: 32, height: 32, padding: "0 12px", fontSize: 13 }
      : { minHeight: 40, height: 40, padding: "0 18px", fontSize: 14 };

  const variants: Record<Variant, React.CSSProperties> = {
    primary: {
      background: "var(--accent)",
      color: "var(--accent-fg)",
      border: "1px solid var(--accent)",
      boxShadow: "var(--shadow-sm)",
    },
    ghost: {
      background: "transparent",
      color: "var(--fg-strong)",
      border: "1px solid var(--border-strong)",
    },
    danger: {
      background: "var(--danger)",
      color: "#fff",
      border: "1px solid var(--danger)",
      boxShadow: "var(--shadow-sm)",
    },
    link: {
      background: "transparent",
      color: "var(--accent)",
      border: "none",
      padding: 0,
      height: "auto",
      minHeight: 0,
    },
  };

  const variantHoverClasses: Record<Variant, string> = {
    primary: "hover:brightness-110 active:scale-[0.98]",
    ghost: "hover:bg-[var(--surface-2)] active:scale-[0.98]",
    danger: "hover:brightness-110 active:scale-[0.98]",
    link: "hover:opacity-80 active:opacity-60",
  };

  return (
    <button
      {...rest}
      disabled={loading || disabled}
      className={`${variantHoverClasses[variant]} ${className || ""}`}
      style={{ ...base, ...sizing, ...variants[variant], ...(style || {}) }}
    >
      {loading ? "…" : (children as ReactNode)}
    </button>
  );
}
