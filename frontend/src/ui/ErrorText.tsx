import { errorDiagnostic, errorText } from "../lib/errorText";

export function ErrorText({
  error,
  context,
  fallback,
  showDiagnostic = false,
}: {
  error: unknown;
  context?: "default" | "login";
  fallback?: string;
  showDiagnostic?: boolean;
}) {
  const text = errorText(error, { context, fallback });
  const diagnostic = errorDiagnostic(error);
  const visibleDiagnostic = showDiagnostic && diagnostic && !diagnostic.endsWith(text);
  return (
    <span className="min-w-0" title={diagnostic}>
      <span>{text}</span>
      {visibleDiagnostic ? (
        <span
          className="mt-1 block break-words text-[11px] leading-5 font-normal"
          style={{ color: "var(--muted)" }}
        >
          诊断：{diagnostic}
        </span>
      ) : null}
    </span>
  );
}
