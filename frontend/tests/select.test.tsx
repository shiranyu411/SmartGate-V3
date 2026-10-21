// @vitest-environment jsdom
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, it } from "vitest";
import { Field, Select } from "../src/ui/Field";

Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
let root: Root;
let container: HTMLDivElement;
afterEach(async () => { await act(async () => root?.unmount()); container?.remove(); });

async function mount(content: React.ReactNode) {
  container=document.createElement("div");document.body.append(container);root=createRoot(container);
  await act(async () => root.render(content));
}
it("uses themed options, skips disabled choices and updates the existing controlled form event", async () => {
  function Form() {
    const [value,setValue]=useState("a");
    return <form><Field label="选择线路"><Select name="route" value={value} onChange={(event)=>setValue(event.target.value)}>
      <option value="a">线路 A</option><option value="disabled" disabled>不可用线路</option><option value="b">线路 B</option>
    </Select></Field><output>{value}</output></form>;
  }
  await mount(<Form/>);
  const trigger=container.querySelector<HTMLButtonElement>('[role="combobox"]')!;
  await act(async()=>{trigger.focus();trigger.click();});
  await act(async()=>trigger.dispatchEvent(new KeyboardEvent("keydown",{key:"ArrowDown",bubbles:true})));
  expect(document.querySelector('[role="listbox"]')).not.toBeNull();
  await act(async()=>trigger.dispatchEvent(new KeyboardEvent("keydown",{key:"Enter",bubbles:true})));
  expect(container.querySelector("output")!.textContent).toBe("b");
  expect(new FormData(container.querySelector("form")!).get("route")).toBe("b");
  expect(trigger.textContent).toContain("线路 B");
  expect(document.querySelector('[role="listbox"]')).toBeNull();
  expect(document.activeElement).toBe(trigger);
});
it("supports pointer selection, outside dismissal and Escape without dismissing the containing dialog", async () => {
  await mount(<Select aria-label="筛选业务池" defaultValue="a"><option value="a">业务池 A</option><option value="b">业务池 B</option></Select>);
  const trigger=container.querySelector<HTMLButtonElement>('[role="combobox"]')!;
  await act(async()=>trigger.click());
  await act(async()=>(document.querySelectorAll('[role="option"]')[1] as HTMLElement).click());
  expect(trigger.textContent).toContain("业务池 B");
  await act(async()=>trigger.click());
  let escaped=false; const listener=()=>{escaped=true};window.addEventListener("keydown",listener);
  await act(async()=>trigger.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true})));
  window.removeEventListener("keydown",listener);
  expect(escaped).toBe(false);expect(document.querySelector('[role="listbox"]')).toBeNull();
  await act(async()=>trigger.click());
  await act(async()=>document.body.dispatchEvent(new Event("pointerdown",{bubbles:true})));
  expect(document.querySelector('[role="listbox"]')).toBeNull();
});

it("preserves required validation and form reset for an uncontrolled field", async () => {
  await mount(<form><Field label="任务类型"><Select name="task" required defaultValue=""><option value="">请选择</option><option value="refresh">刷新订阅</option></Select></Field></form>);
  const form=container.querySelector("form")!;
  const trigger=container.querySelector<HTMLButtonElement>('[role="combobox"]')!;
  await act(async()=>{expect(form.reportValidity()).toBe(false);});
  expect(document.activeElement).toBe(trigger);
  await act(async()=>trigger.click());
  await act(async()=>(document.querySelectorAll('[role="option"]')[1] as HTMLElement).click());
  expect(new FormData(form).get("task")).toBe("refresh");
  expect(form.reportValidity()).toBe(true);
  await act(async()=>form.reset());
  expect(new FormData(form).get("task")).toBe("");
  expect(trigger.textContent).toContain("请选择");
});
