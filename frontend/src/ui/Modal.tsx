import type { ReactNode } from "react";
import { useEffect } from "react";
import { serif, sans, eyebrowStyle } from "./tokens";

export function Modal({
  open,
  onClose,
  title,
  eyebrow,
  width = 640,
  children,
  footer,
}: {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  eyebrow?: ReactNode;
  width?: number;
  children: ReactNode;
  footer?: ReactNode;
}) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      window.removeEventListener("keydown", onKey);
      document.body.style.overflow = prevOverflow;
    };
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 grid place-items-center px-4 backdrop-blur-[2px] transition-all"
      style={{ background: "color-mix(in srgb, var(--bg) 80%, rgba(0,0,0,0.4))" }}
      onClick={onClose}
    >
      <div
        className="relative w-full max-h-[88vh] overflow-auto"
        style={{
          maxWidth: width,
          background: "var(--surface)",
          border: "1px solid var(--border)",
          borderRadius: "var(--radius-lg)",
          boxShadow: "var(--shadow-float)",
        }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-5 sm:px-8 pt-8 pb-5">
          {eyebrow ? <p style={eyebrowStyle}>{eyebrow}</p> : null}
          <h2
            className="text-[26px] mt-2 font-medium"
            style={{ ...serif, color: "var(--fg-strong)", letterSpacing: "-0.01em" }}
          >
            {title}
          </h2>
        </div>
        <div className="px-5 sm:px-8 pb-8" style={{ ...sans, color: "var(--fg)" }}>
          {children}
        </div>
        {footer ? (
          <div
            className="px-5 sm:px-8 py-5 flex items-center justify-end gap-3"
            style={{
              borderTop: "1px solid var(--border)",
              background: "var(--surface-2)",
              borderBottomLeftRadius: "var(--radius-lg)",
              borderBottomRightRadius: "var(--radius-lg)",
            }}
          >
            {footer}
          </div>
        ) : null}
      </div>
    </div>
  );
}
