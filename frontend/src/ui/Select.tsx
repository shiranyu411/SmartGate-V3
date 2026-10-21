import { Children, Fragment, isValidElement, useContext, useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode, type SelectHTMLAttributes, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { FieldLabelContext } from "./fieldLabel";
import { sans } from "./tokens";

type Option = { value: string; label: string; disabled: boolean };
function textOf(node: ReactNode): string {
  return Children.toArray(node).map((child) => isValidElement<{ children?: ReactNode }>(child) ? textOf(child.props.children) : String(child)).join("");
}
function optionsOf(children: ReactNode, disabled = false): Option[] {
  return Children.toArray(children).flatMap((child) => {
    if (!isValidElement<{ children?: ReactNode; value?: string | number; label?: string; disabled?: boolean }>(child)) return [];
    if (child.type === Fragment || child.type === "optgroup") return optionsOf(child.props.children, disabled || !!child.props.disabled);
    if (child.type !== "option") return [];
    const label = child.props.label ?? textOf(child.props.children);
    return [{ value: String(child.props.value ?? label), label, disabled: disabled || !!child.props.disabled }];
  });
}

// A native form control preserves existing onChange/form semantics; the visible
// trigger and popup use the same theme in every browser and both color modes.
export function Select(props: SelectHTMLAttributes<HTMLSelectElement>) {
  const { children, value, defaultValue, style, className, disabled, id, ...rest } = props;
  const labelID = useContext(FieldLabelContext);
  const generatedID = useId();
  const listID = `${generatedID}-options`;
  const native = useRef<HTMLSelectElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [localValue, setLocalValue] = useState(String(defaultValue ?? ""));
  const [active, setActive] = useState(0);
  const [position, setPosition] = useState({ top: 0, left: 0, width: 0, maxHeight: 280 });
  const typeahead = useRef({ value: "", at: 0 });
  const options = optionsOf(children);
  const selectedValue = value === undefined ? localValue : String(value);
  const selected = options.find((option) => option.value === selectedValue) ?? options[0];
  useEffect(() => { if (disabled) setOpen(false); }, [disabled]);

  function show(index?: number) {
    if (disabled) return;
    const selectedIndex = options.findIndex((option) => option.value === selected?.value && !option.disabled);
    const firstEnabled = options.findIndex((option) => !option.disabled);
    setActive(index ?? (selectedIndex >= 0 ? selectedIndex : Math.max(0, firstEnabled)));
    setOpen(true);
  }
  function choose(index: number) {
    const option = options[index];
    if (!option || option.disabled || !native.current) return;
    native.current.value = option.value;
    setLocalValue(option.value);
    native.current.dispatchEvent(new Event("change", { bubbles: true }));
    setOpen(false);
    trigger.current?.focus();
  }
  function move(direction: number) {
    for (let step = 1; step <= options.length; step++) {
      const next = (active + direction * step + options.length) % options.length;
      if (!options[next].disabled) { setActive(next); break; }
    }
  }
  function onKey(event: KeyboardEvent<HTMLButtonElement>) {
    if (event.key === "Escape" && open) { event.preventDefault(); event.stopPropagation(); setOpen(false); return; }
    if (event.key === "Tab") { setOpen(false); return; }
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (!open) show(); else move(event.key === "ArrowDown" ? 1 : -1);
    } else if (event.key === "Home" || event.key === "End") {
      event.preventDefault();
      const indices = options.map((_, index) => index).filter((index) => !options[index].disabled);
      show(event.key === "Home" ? indices[0] : indices.at(-1));
    } else if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      if (open) choose(active); else show();
    } else if (event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
      event.preventDefault();
      const now = Date.now();
      const query = (now - typeahead.current.at < 700 ? typeahead.current.value : "") + event.key.toLowerCase();
      typeahead.current = { value: query, at: now };
      const index = options.findIndex((option) => !option.disabled && option.label.toLowerCase().startsWith(query));
      if (index >= 0) show(index);
    }
  }

  useLayoutEffect(() => {
    if (!open) return;
    const update = () => {
      const rect = trigger.current?.getBoundingClientRect();
      if (!rect) return;
      const below = window.innerHeight - rect.bottom - 12;
      const above = rect.top - 12;
      const upwards = below < Math.min(240, above);
      const height = Math.max(48, Math.min(280, (upwards ? above : below), options.length * 40 + 12));
      const width = Math.min(rect.width, window.innerWidth - 24);
      setPosition({ top: upwards ? rect.top - height - 6 : rect.bottom + 6, left: Math.max(12, Math.min(rect.left, window.innerWidth - width - 12)), width, maxHeight: height });
    };
    update();
    window.addEventListener("resize", update);
    window.addEventListener("scroll", update, true);
    return () => { window.removeEventListener("resize", update); window.removeEventListener("scroll", update, true); };
  }, [open, options.length]);
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent) => {
      if (!trigger.current?.contains(event.target as Node) && !menu.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [open]);
  useEffect(() => { if (open) menu.current?.querySelector(`[id="${listID}-${active}"]`)?.scrollIntoView?.({ block: "nearest" }); }, [active, open, listID]);
  const resetValue = String(defaultValue ?? options[0]?.value ?? "");
  useEffect(() => {
    const form = native.current?.form;
    const reset = () => { setLocalValue(resetValue); setOpen(false); };
    form?.addEventListener("reset", reset);
    return () => form?.removeEventListener("reset", reset);
  }, [resetValue]);

  return (
    <span className={`relative block min-w-0 ${className || ""}`} style={style}>
      <select {...rest} ref={native} value={value} defaultValue={defaultValue} disabled={disabled} tabIndex={-1} aria-hidden="true"
        onChange={(event) => { setLocalValue(event.target.value); rest.onChange?.(event); }}
        onInvalid={(event) => { event.preventDefault(); trigger.current?.focus(); rest.onInvalid?.(event); }}
        style={{ position: "absolute", width: 1, height: 1, opacity: 0, pointerEvents: "none" }}>
        {children}
      </select>
      <button ref={trigger} id={id} type="button" role="combobox" disabled={disabled}
        aria-label={props["aria-label"]} aria-labelledby={props["aria-labelledby"] ?? (!props["aria-label"] ? labelID : undefined)}
        aria-describedby={props["aria-describedby"]} aria-invalid={props["aria-invalid"]} aria-required={props.required}
        aria-expanded={open && !disabled} aria-controls={listID} aria-haspopup="listbox" aria-activedescendant={open ? `${listID}-${active}` : undefined}
        title={props.title} onKeyDown={onKey} onClick={() => open ? setOpen(false) : show()}
        onBlur={() => setOpen(false)}
        className="sg-select-trigger w-full h-10 px-3.5 flex items-center justify-between gap-3 text-sm text-left transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
        style={{ ...sans, background: "var(--surface-2)", border: "1px solid var(--border)", borderRadius: "var(--radius-sm)", color: "var(--fg-strong)" }}>
        <span className="truncate">{selected?.label || "请选择"}</span>
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true" className="shrink-0" style={{ color: "var(--muted)", transform: open ? "rotate(180deg)" : undefined }}><path d="m4 6 4 4 4-4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" /></svg>
      </button>
      {open && !disabled && createPortal(
        <div ref={menu} id={listID} role="listbox" aria-label={props["aria-label"]} aria-labelledby={!props["aria-label"] ? labelID : undefined}
          className="sg-select-menu" style={{ ...sans, position: "fixed", ...position, zIndex: 100, overflowY: "auto", padding: 5, background: "var(--surface)", color: "var(--fg-strong)", border: "1px solid var(--border-strong)", borderRadius: "var(--radius-md)", boxShadow: "var(--shadow-float)" }}>
          {options.map((option, index) => <div key={`${option.value}-${index}`} id={`${listID}-${index}`} role="option" aria-selected={option.value === selected?.value} aria-disabled={option.disabled || undefined}
            onPointerMove={(event) => event.pointerType === "mouse" && !option.disabled && setActive(index)} onMouseDown={(event) => event.preventDefault()}
            onClick={() => choose(index)} className="flex items-center justify-between gap-3 text-sm px-3 py-2.5 rounded-[var(--radius-sm)]"
            style={{ cursor: option.disabled ? "not-allowed" : "pointer", opacity: option.disabled ? 0.4 : 1, color: option.value === selected?.value ? "var(--accent)" : "var(--fg-strong)", background: index === active ? "var(--surface-2)" : "transparent", overflowWrap: "anywhere" }}>
            <span>{option.label}</span><span aria-hidden="true">{option.value === selected?.value ? "✓" : ""}</span>
          </div>)}
        </div>, document.body,
      )}
    </span>
  );
}
