// @vitest-environment jsdom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, expect, it } from "vitest";
import { PageBoundary } from "../src/shell/PageBoundary";

let root: Root | undefined;
let container: HTMLDivElement | undefined;

afterEach(async () => {
  await act(async () => root?.unmount());
  container?.remove();
});

it("contains a rendering error and recovers when navigating to another page", async () => {
  function BrokenPage(): never {
    throw new Error("fixture render failure");
  }
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container, { onCaughtError: () => {} });
  await act(async () => {
    root?.render(
      <MemoryRouter initialEntries={["/broken"]}>
        <nav><Link to="/overview">Overview navigation</Link></nav>
        <PageBoundary>
          <Routes>
            <Route path="/broken" element={<BrokenPage />} />
            <Route path="/overview" element={<h1>Healthy overview</h1>} />
          </Routes>
        </PageBoundary>
      </MemoryRouter>,
    );
  });
  expect(container.querySelector("[role=alert]")?.textContent).toContain("页面暂时无法显示");
  expect(container.textContent).not.toContain("fixture render failure");
  await act(async () => {
    container?.querySelector("nav a")?.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
  expect(container.querySelector("[role=alert]")).toBeNull();
  expect(container.textContent).toContain("Healthy overview");
});
