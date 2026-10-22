import { test, expect, type APIRequestContext, type Page } from "@playwright/test";

async function carrySessionActionsToPhone(
  page: Page,
  request: APIRequestContext,
  filterSelected = false,
  sessionTitle = process.env.AF_WEB_SESSION_A ?? "probe-a",
) {
  await page.setViewportSize({ width: 1280, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === sessionTitle);
  expect(session?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  if (filterSelected) {
    for (const kind of ["attention", "needs-you", "working", "waiting-limit", "broken"]) {
      await page.locator(".af-rail-filter").click();
      const item = page.locator(`.af-filter-item[data-kind="${kind}"]`);
      await expect(item).toBeVisible();
      if (await item.getAttribute("aria-checked") === "true") await item.click();
      await expect(item).toHaveAttribute("aria-checked", "false");
      await page.locator(".af-rail-title").click();
      await expect(page.locator(".af-filter-menu")).toBeHidden();
    }
    await expect(page.locator(".af-row-selected")).toHaveCount(0);
  }
  const sessionActions = page.getByRole("button", { name: "More session actions", exact: true, includeHidden: true });
  await sessionActions.click();
  await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
  await page.setViewportSize({ width: 390, height: 844 });
  const controls = page.getByRole("button", { name: "More app controls", exact: true, includeHidden: true });
  await expect(controls).toBeVisible();
  await controls.focus();
  await page.keyboard.press("Enter");
  await expect(controls).toHaveAttribute("aria-expanded", "true");
  await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
  return { controls, session, sessionActions };
}

async function splitTabToRight(page: Page, index: number): Promise<void> {
  await page.evaluate((tabIndex) => {
    const tab = document.querySelector<HTMLElement>(`.af-tab[data-tab-index="${tabIndex}"]`);
    const pane = document.querySelector<HTMLElement>(".af-term-host .af-pane");
    if (!tab || !pane) throw new Error("drag source or target pane not found");
    const transfer = new DataTransfer();
    tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: transfer }));
    const rect = pane.getBoundingClientRect();
    const init = {
      bubbles: true, cancelable: true, dataTransfer: transfer,
      clientX: rect.right - 6, clientY: rect.top + rect.height / 2,
    };
    pane.dispatchEvent(new DragEvent("dragenter", init));
    pane.dispatchEvent(new DragEvent("dragover", init));
    pane.dispatchEvent(new DragEvent("drop", init));
    tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: transfer }));
  }, index);
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
}
// This runs against the embedded app only inside the sanctioned web testbox.
for (const width of [1280, 390]) {
  test(`t and T create a tab of their kind in one press, and so do the header buttons, at ${width}px`, async ({ page, request }) => {
    await page.setViewportSize({ width, height: 844 });
    const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
    const session = snapshot.data.instances.find((s: { title: string }) =>
      s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
    expect(session?.id).toBeTruthy();
    const kinds: string[] = [];
    await page.route("**/v1/CreateTab", route => {
      kinds.push(route.request().postDataJSON()?.kind ?? "shell");
      return route.fulfill({ json: { data: null, error: { message: "header test refusal", daemon_rejected: true } } });
    });
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    const terminal = page.getByRole("button", { name: "New terminal tab", exact: true });
    const vscode = page.getByRole("button", { name: "New VS Code tab", exact: true });
    await expect(terminal).toBeVisible();
    await expect(page.getByRole("menu", { name: "Tab type" })).toHaveCount(0);
    await page.keyboard.press("Control+]");
    await page.keyboard.press("t");
    await expect.poll(() => kinds.length).toBe(1);
    await terminal.click();
    await expect.poll(() => kinds.length).toBe(2);
    expect(kinds).toEqual(["shell", "shell"]);
    if (await vscode.isVisible()) {
      await page.evaluate(() => (document.activeElement as HTMLElement)?.blur());
      await page.keyboard.press("Shift+T");
      await expect.poll(() => kinds.length).toBe(3);
      await vscode.click();
      await expect.poll(() => kinds.length).toBe(4);
      expect(kinds).toEqual(["shell", "shell", "vscode", "vscode"]);
    }
    await expect(page.locator(".af-toast")).toContainText("header test refusal");
  });
}

test("holding t creates one terminal tab, not one per key repeat", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  expect(session?.id).toBeTruthy();
  let creates = 0;
  await page.route("**/v1/CreateTab", route => {
    creates += 1;
    return route.fulfill({ json: { data: null, error: { message: "header test refusal", daemon_rejected: true } } });
  });
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  await page.keyboard.down("t");
  await page.keyboard.down("t");
  await page.keyboard.down("t");
  await page.keyboard.up("t");
  await expect(page.locator(".af-toast")).toContainText("header test refusal");
  expect(creates).toBe(1);
});

test("double-clicking a header tab button creates one tab", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  expect(session?.id).toBeTruthy();
  let creates = 0;
  let release!: () => void;
  const held = new Promise<void>(resolve => { release = resolve; });
  await page.route("**/v1/CreateTab", async route => {
    creates += 1;
    await held;
    await route.fulfill({ json: { data: null, error: { message: "header test refusal", daemon_rejected: true } } });
  });
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  const terminal = page.getByRole("button", { name: "New terminal tab", exact: true });
  await terminal.dblclick();
  await expect.poll(() => creates).toBe(1);
  release();
  await expect(page.locator(".af-toast")).toContainText("header test refusal");
  expect(creates).toBe(1);
});

test("phone header tab button dismisses an open More app controls menu", async ({ page, request }) => {
  const { controls } = await carrySessionActionsToPhone(page, request);
  await page.route("**/v1/CreateTab", route =>
    route.fulfill({ json: { data: null, error: { message: "header test refusal", daemon_rejected: true } } }));
  const terminal = page.getByRole("button", { name: "New terminal tab", exact: true });
  await expect(terminal).toBeVisible();
  await terminal.focus();
  await page.keyboard.press("Enter");
  await expect(controls).toHaveAttribute("aria-expanded", "false");
  await expect(page.locator(".af-appbar-tools")).toBeHidden();
});

for (const key of ["Enter", "Space"]) {
  test(`phone ${key} and Escape dismiss carried desktop session actions`, async ({ page, request }) => {
    await page.setViewportSize({ width: 1280, height: 844 });
    const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
    const session = snapshot.data.instances.find((s: { title: string }) =>
      s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    const sessionActions = page.getByRole("button", { name: "More session actions", exact: true, includeHidden: true });
    await sessionActions.click();
    await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
    await page.setViewportSize({ width: 390, height: 844 });
    const controls = page.getByRole("button", { name: "More app controls", exact: true, includeHidden: true });
    await expect(controls).toBeVisible();
    await controls.focus();
    await page.keyboard.press(key);
    await expect(controls).toHaveAttribute("aria-expanded", "true");
    await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
    await page.keyboard.press("Escape");
    await expect(controls).toHaveAttribute("aria-expanded", "false");
    await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
    await page.setViewportSize({ width: 1280, height: 844 });
    await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
  });
}

for (const activation of ["view tab", "view shortcut"] as const) {
  test(`phone ${activation} dismisses carried desktop Session actions`, async ({ page, request }) => {
    const { controls, sessionActions } = await carrySessionActionsToPhone(page, request);
    if (activation === "view tab") {
      const tasks = page.getByRole("tab", { name: "Tasks", exact: true });
      await tasks.focus();
      await page.keyboard.press("Enter");
    } else {
      await page.evaluate(() => (document.activeElement as HTMLElement)?.blur());
      await page.keyboard.press("]");
    }
    await expect(controls).toHaveAttribute("aria-expanded", "false");
    await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
  });
}

for (const action of ["Archive session", "Delete session"] as const) {
  for (const key of ["Enter", "Space"] as const) {
    test(`phone fallback ${action} via ${key} dismisses carried Session actions`, async ({ page, request }) => {
      const { controls, session, sessionActions } = await carrySessionActionsToPhone(page, request, true);
      const target = page.locator(".af-term-actions").getByRole("button", {
        name: `${action} “${session.title}”`, exact: true,
      });
      await expect(target).toBeVisible();
      await target.focus();
      await page.keyboard.press(key);
      await expect(page.getByRole("dialog")).toBeVisible();
      await expect(controls).toHaveAttribute("aria-expanded", "false");
      await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
      await page.getByRole("dialog").getByRole("button", { name: "Cancel", exact: true }).click();
    });
  }
}

for (const activation of ["tab Enter", "tab Space", "tab shortcut"] as const) {
  test(`phone ${activation} dismisses carried actions before tab-kind recomposition`, async ({ page, request }) => {
    const title = process.env.AF_WEB_SESSION_WEB ?? "probe-web";
    const { controls, session, sessionActions } = await carrySessionActionsToPhone(page, request, false, title);
    const webIndex = session.tabs.findIndex((tab: { kind: number }) => tab.kind === 3);
    expect(webIndex).toBeGreaterThan(0);
    if (activation === "tab shortcut") {
      await page.evaluate(() => (document.activeElement as HTMLElement)?.blur());
      await page.keyboard.press(String(webIndex + 1));
    } else {
      await page.locator(`.af-tab[data-tab-index="${webIndex}"]`).focus();
      await page.keyboard.press(activation.slice(4));
    }
    await expect(controls).toHaveAttribute("aria-expanded", "false");
    await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
  });
}

for (const key of ["Enter", "Space"] as const) {
  test(`phone ${key} dismisses Session actions when entering session-first`, async ({ page, request }) => {
    const title = process.env.AF_WEB_SESSION_WEB ?? "probe-web";
    await page.setViewportSize({ width: 1280, height: 844 });
    const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
    const session = snapshot.data.instances.find((s: { title: string }) => s.title === title);
    expect(session?.id).toBeTruthy();
    const webIndex = session.tabs.findIndex((tab: { kind: number }) => tab.kind === 3);
    expect(webIndex).toBeGreaterThan(0);
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    await page.locator(`.af-tab[data-tab-index="${webIndex}"]`).click();
    await expect(page.locator(`.af-tab[data-tab-index="${webIndex}"]`)).toHaveAttribute("aria-selected", "true");
    await page.setViewportSize({ width: 390, height: 844 });
    const sessionActions = page.getByRole("button", { name: "More session actions", exact: true, includeHidden: true });
    await sessionActions.click();
    await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
    await page.locator('.af-tab[data-tab-index="0"]').focus();
    await page.keyboard.press(key);
    await expect(page.locator(".af-app")).toHaveClass(/af-session-first/);
    await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
  });
}

test("phone touch pane drop dismisses carried Session actions before recomposition", async ({ page, request }) => {
  const title = process.env.AF_WEB_SESSION_WEB ?? "probe-web";
  const { session, sessionActions } = await carrySessionActionsToPhone(page, request, false, title);
  const webIndex = session.tabs.findIndex((tab: { kind: number }) => tab.kind === 3);
  expect(webIndex).toBeGreaterThan(0);
  const webTab = page.locator(`.af-tab[data-tab-index="${webIndex}"]`);
  await webTab.scrollIntoViewIfNeeded();
  const point = await webTab.evaluate((tab) => {
    const r = tab.getBoundingClientRect();
    const event = new PointerEvent("pointerdown", {
      bubbles: true, cancelable: true, pointerId: 42, pointerType: "touch",
      clientX: r.left + r.width / 2, clientY: r.top + r.height / 2,
    });
    tab.dispatchEvent(event);
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  });
  await expect(page.locator("body")).toHaveClass(/af-dragging-tab/, { timeout: 2_000 });
  await page.locator(".af-tabbar").evaluate((bar, { pointerId, from }) => {
    const pane = document.querySelector<HTMLElement>(".af-term-host .af-pane");
    if (!pane) throw new Error("focused pane not found");
    const r = pane.getBoundingClientRect();
    const x = r.left + r.width / 2;
    const y = r.top + r.height / 2;
    bar.dispatchEvent(new PointerEvent("pointermove", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch",
      clientX: (from.x + x) / 2, clientY: (from.y + y) / 2,
    }));
    bar.dispatchEvent(new PointerEvent("pointermove", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch", clientX: x, clientY: y,
    }));
    bar.dispatchEvent(new PointerEvent("pointerup", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch", clientX: x, clientY: y,
    }));
  }, { pointerId: 42, from: point });
  await expect(page.locator(`.af-tab[data-tab-index="${webIndex}"]`)).toHaveAttribute("aria-selected", "true");
  await expect(page.locator(".af-app")).not.toHaveClass(/af-session-first/);
  await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
});

for (const shortcut of ["Alt+j", "Alt+k", "Alt+w"] as const) {
  test(`phone ${shortcut} dismisses carried actions when focused pane kind changes`, async ({ page, request }) => {
    const title = process.env.AF_WEB_SESSION_WEB ?? "probe-web";
    await page.setViewportSize({ width: 1280, height: 844 });
    const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
    const session = snapshot.data.instances.find((s: { title: string }) => s.title === title);
    expect(session?.id).toBeTruthy();
    const webIndex = session.tabs.findIndex((tab: { kind: number }) => tab.kind === 3);
    expect(webIndex).toBeGreaterThan(0);
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    await splitTabToRight(page, webIndex);
    await page.locator(".af-term-host .xterm").click();
    await expect(page.locator('.af-tab[data-tab-index="0"]')).toHaveAttribute("aria-selected", "true");

    const sessionActions = page.getByRole("button", {
      name: "More session actions", exact: true, includeHidden: true,
    });
    await sessionActions.click();
    await page.setViewportSize({ width: 390, height: 844 });
    const controls = page.getByRole("button", { name: "More app controls", exact: true, includeHidden: true });
    // Pointer activation is an outside mousedown and correctly dismisses the inner
    // disclosure. Use the finding's no-mousedown keyboard path so the Alt chord is
    // the first user action that can invalidate the carried state.
    await expect(controls).toBeVisible();
    await controls.focus();
    await page.keyboard.press("Enter");
    await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
    await page.evaluate(() => (document.activeElement as HTMLElement)?.blur());

    await page.keyboard.press(shortcut);
    await expect(page.locator(".af-app")).not.toHaveClass(/af-session-first/);
    await expect(controls).toHaveAttribute("aria-expanded", "false");
    await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
  });
}

test("phone Switch project activation dismisses carried session actions", async ({ page, request }) => {
  const { controls, sessionActions } = await carrySessionActionsToPhone(page, request);
  // The switcher button is display:none in the session-first composition — its
  // listbox inlines into the drawer instead — so no pointer or keypress can
  // reach it there. Dispatch the activation its click handler is wired for;
  // the contract under test is that running the handler retires carried state.
  await page.evaluate(() => {
    const button = document.querySelector<HTMLElement>(".af-project-switch");
    if (!button) throw new Error("project switcher not mounted");
    button.click();
  });
  await expect(controls).toHaveAttribute("aria-expanded", "false");
  await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
  // The carried state is retired, not merely hidden: returning to desktop must
  // not restore a disclosure the user's nested action already replaced.
  await page.setViewportSize({ width: 1280, height: 844 });
  await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
});
