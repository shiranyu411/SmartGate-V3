import { sans } from "./tokens";

export function Segmented<T extends string>({
  value,
  onChange,
  options,
  disabled,
}: {
  value: T;
  onChange: (v: T) => void;
  options: { key: T; label: string; title?: string; disabled?: boolean }[] | T[];
  disabled?: boolean;
}) {
  const items = (options as Array<unknown>).map((o) =>
    typeof o === "string"
      ? { key: o as T, label: o as string, title: undefined as string | undefined }
      : (o as { key: T; label: string; title?: string; disabled?: boolean })
  );

  return (
    <div
      className="inline-flex p-1 rounded-[8px]"
      style={{
        background: "var(--surface-2)",
        border: "1px solid var(--border)",
        boxShadow: "inset 0 1px 2px rgba(0,0,0,0.03)",
        ...sans,
      }}
    >
      {items.map((o) => {
        const on = o.key === value;
        return (
          <button
            key={o.key}
            title={o.title}
            disabled={disabled || o.disabled}
            aria-pressed={on}
            onClick={() => !o.disabled && onChange(o.key)}
            className={`h-8 px-3 sm:px-4 rounded-[4px] text-xs sm:text-sm transition-all duration-200 disabled:opacity-40 disabled:cursor-not-allowed ${on ? "font-medium" : "hover:bg-[var(--surface)]"}`}
            style={
              on
                ? { background: "var(--surface)", color: "var(--accent)", boxShadow: "var(--shadow-sm)", border: "1px solid var(--border)" }
                : { color: "var(--muted)", border: "1px solid transparent" }
            }
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

export function Toggle({ on, onChange, disabled }: { on: boolean; onChange: (v: boolean) => void; disabled?: boolean }) {
  return (
    <button
      onClick={() => !disabled && onChange(!on)}
      disabled={disabled}
      className="relative h-6 w-11 rounded-full transition-colors duration-300 disabled:opacity-50 hover:brightness-105"
      style={{ background: on ? "var(--accent)" : "var(--border-strong)" }}
    >
      <span
        className="absolute top-0.5 size-5 rounded-full transition-all duration-300 ease-out"
        style={{
          left: on ? 22 : 2,
          background: "var(--surface)",
          boxShadow: on ? "0 2px 4px rgba(122, 42, 28, 0.3)" : "0 1px 3px rgba(0,0,0,0.15)",
        }}
      />
    </button>
  );
}
