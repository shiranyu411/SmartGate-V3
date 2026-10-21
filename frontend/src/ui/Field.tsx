import { useId, type InputHTMLAttributes, type TextareaHTMLAttributes, type ReactNode } from "react";
import { serif, sans } from "./tokens";
import { FieldLabelContext } from "./fieldLabel";
export { Select } from "./Select";

export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  children: ReactNode;
}) {
  const labelID = useId();
  return (
    <label className="block group">
      <div className="flex items-baseline justify-between mb-2 gap-2">
        <span id={labelID} className="text-[15px] transition-colors group-focus-within:text-[var(--accent)]" style={{ ...serif, color: "var(--fg-strong)" }}>
          {label}
        </span>
        {hint ? (
          <span className="text-xs italic opacity-80" style={{ ...serif, color: "var(--muted)" }}>
            {hint}
          </span>
        ) : null}
      </div>
      <FieldLabelContext.Provider value={labelID}>{children}</FieldLabelContext.Provider>
      {error ? (
        <p className="break-words mt-2 text-xs font-medium" style={{ ...sans, color: "var(--danger)" }}>
          {error}
        </p>
      ) : null}
    </label>
  );
}

const inputBase: React.CSSProperties = {
  background: "var(--surface-2)",
  border: "1px solid var(--border)",
  color: "var(--fg-strong)",
  borderRadius: "var(--radius-sm)",
  boxShadow: "inset 0 1px 2px rgba(0,0,0,0.02)",
  ...sans,
};

const inputClasses = "w-full text-sm transition-all duration-200 focus:outline-none focus:border-[var(--accent)] focus:ring-1 focus:ring-[var(--accent)] hover:bg-[var(--surface)]";

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  const { style, className, ...rest } = props;
  return (
    <input
      {...rest}
      className={`h-10 px-3.5 ${inputClasses} ${className || ""}`}
      style={{ ...inputBase, ...(style || {}) }}
    />
  );
}

export function Textarea(props: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  const { style, className, ...rest } = props;
  return (
    <textarea
      {...rest}
      className={`min-h-24 p-3.5 ${inputClasses} ${className || ""}`}
      style={{ ...inputBase, ...(style || {}) }}
    />
  );
}
