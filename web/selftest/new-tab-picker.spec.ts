import { test, expect } from "@playwright/test";

// This runs against the embedded app only inside the sanctioned web testbox.
for (const width of [1280, 390]) {
  test(`4017: t opens the button's New tab picker without creating a tab at ${width}px`, async ({ page, request }, info) => {
    await page.setViewportSize({ width, height: 844 });
    const response = await request.post("/v1/Snapshot", { data: {} });
    const snapshot = await response.json();
    const session = snapshot.data.instances.find((s: { title: string }) =>
      s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
    expect(session?.id).toBeTruthy();
    let creates = 0;
    await page.route("**/v1/CreateTab", route => {
      creates++;
      return route.fulfill({ json: { data: null, error: { message: "picker test refusal", daemon_rejected: true } } });
    });
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    await page.keyboard.press("Control+]");
    const trigger = page.getByRole("button", { name: "New tab · Terminal or VS Code", exact: true, includeHidden: true });
    const menu = page.getByRole("menu", { name: "Tab type", exact: true });
    const sessionActions = page.getByRole("button", { name: "Session actions", exact: true, includeHidden: true });
    const tabs = await page.locator(".af-tabbar .af-tab").count();
    await page.keyboard.press("t");
    await expect(menu).toBeVisible();
    await expect(trigger).toHaveAttribute("aria-expanded", "true");
    await expect(menu.getByRole("menuitem", { name: "Terminal", exact: true })).toBeFocused();
    expect(creates).toBe(0);
    await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(tabs);
    await page.screenshot({ path: info.outputPath("new-tab-picker.png") });
    await page.keyboard.press("Escape");
    await expect(menu).toBeHidden();
    if (width <= 768) {
      const controls = page.getByRole("button", { name: "More app controls", exact: true });
      await expect(controls).toHaveAttribute("aria-expanded", "false");
      // A shortcut-opened picker restores navigation, even when it had to open
      // both enclosing phone disclosures to make the picker reachable.
      await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
      await page.keyboard.press("t");
      await expect(menu).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(controls).toHaveAttribute("aria-expanded", "false");
      await controls.click();
      // If the disclosure was already open, canceling the nested picker preserves it.
      await page.evaluate(() => (document.activeElement as HTMLElement)?.blur());
      await page.keyboard.press("t");
      await expect(menu).toBeVisible();
      await page.keyboard.press("Escape");
      // Phone tab choices are permanently inline while the enclosing disclosure
      // is open. Escape ends the keyboard picker without hiding that prior view.
      await expect(trigger).toHaveAttribute("aria-expanded", "false");
      await expect(controls).toHaveAttribute("aria-expanded", "true");
      await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
      await page.keyboard.press("t");
    }
    if (width > 768) {
      await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
      await page.keyboard.press("t");
    }
    expect(creates).toBe(0);
    await expect(menu).toBeVisible();
    await menu.getByRole("menuitem", { name: "Terminal", exact: true }).click();
    await expect.poll(() => creates).toBe(1);
    await expect(page.locator(".af-toast")).toContainText("picker test refusal");
  });
}

for (const width of [1280, 390]) {
  test(`picker owns navigation and activation keys at ${width}px`, async ({ page, request }) => {
    await page.setViewportSize({ width, height: 844 });
    const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
    const session = snapshot.data.instances.find((s: { title: string }) =>
      s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
    const creates: { id: string; kind?: string; shell?: boolean }[] = [];
    await page.route("**/v1/CreateTab", route => {
      creates.push(route.request().postDataJSON());
      return route.fulfill({ json: { data: null, error: { message: "picker test refusal", daemon_rejected: true } } });
    });
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    await page.keyboard.press("Control+]");
    const menu = page.getByRole("menu", { name: "Tab type", exact: true });
    const terminal = menu.getByRole("menuitem", { name: "Terminal", exact: true });
    const code = menu.getByRole("menuitem", { name: "VS Code", exact: true });
    await page.keyboard.press("t");
    await expect(terminal).toBeFocused();
    await page.keyboard.press("ArrowDown");
    await expect(code).toBeFocused();
    await page.keyboard.press("Enter");
    await expect.poll(() => creates.map(c => c.kind)).toEqual(["vscode"]);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    expect(page.url()).toContain(encodeURIComponent(session.id));
    await page.keyboard.press("Control+]");
    await page.keyboard.press("t");
    for (const [key, target] of [
      ["ArrowUp", code], ["ArrowDown", terminal], ["End", code],
      ["Home", terminal], ["ArrowDown", code], ["ArrowUp", terminal],
    ] as const) {
      await page.keyboard.press(key);
      await expect(target).toBeFocused();
      await expect(page.locator(".af-term-title")).toHaveText(session.title);
    }
    await page.keyboard.press("Space");
    // The existing shell API uses shell: true and omits kind.
    await expect.poll(() => creates.map(c => c.kind)).toEqual(["vscode", undefined]);
    expect(creates.map(c => c.id)).toEqual([session.id, session.id]);
    expect(creates[1].shell).toBe(true);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
  });
}

test("phone shortcut cancel restores navigation after desktop recomposition", async ({ page, request }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  await page.keyboard.press("t");
  const menu = page.getByRole("menu", { name: "Tab type", exact: true });
  const trigger = page.getByRole("button", { name: "New tab · Terminal or VS Code", exact: true, includeHidden: true });
  const sessionActions = page.getByRole("button", { name: "Session actions", exact: true, includeHidden: true });
  await expect(menu).toBeVisible();
  await page.setViewportSize({ width: 1280, height: 844 });
  await expect(trigger).toHaveAttribute("aria-expanded", "true");
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
});

test("desktop shortcut cancel restores navigation while click cancel returns to the trigger", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  const menu = page.getByRole("menu", { name: "Tab type", exact: true });
  const trigger = page.getByRole("button", { name: "New tab · Terminal or VS Code", exact: true });
  const sessionActions = page.getByRole("button", { name: "Session actions", exact: true });

  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(page.locator(".af-rail")).toBeFocused();
  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(sessionActions).toHaveAttribute("aria-expanded", "false");

  await sessionActions.click();
  await trigger.click();
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(trigger).toBeFocused();
  await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
});

test("shortcut cancel preserves a Session actions disclosure the user opened", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  const sessionActions = page.getByRole("button", { name: "Session actions", exact: true });
  const menu = page.getByRole("menu", { name: "Tab type", exact: true });

  await sessionActions.click();
  await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
  await page.evaluate(() => (document.activeElement as HTMLElement)?.blur());
  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
});

test("shortcut cancel preserves user-opened Session actions across desktop to phone", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  const sessionActions = page.getByRole("button", { name: "Session actions", exact: true, includeHidden: true });
  const menu = page.getByRole("menu", { name: "Tab type", exact: true });

  await sessionActions.click();
  await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
  await page.evaluate(() => (document.activeElement as HTMLElement)?.blur());
  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(sessionActions).toHaveAttribute("aria-expanded", "true");
});

test("desktop shortcut cancel closes app controls opened by phone recomposition", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  const controls = page.getByRole("button", { name: "More app controls", exact: true, includeHidden: true });
  const menu = page.getByRole("menu", { name: "Tab type", exact: true });

  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(menu).toBeVisible();
  await expect(controls).toHaveAttribute("aria-expanded", "true");
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(controls).toHaveAttribute("aria-expanded", "false");
  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
});

test("desktop picker stays visible after phone recomposition", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  await page.keyboard.press("t");
  const menu = page.getByRole("menu", { name: "Tab type", exact: true });
  const trigger = page.getByRole("button", { name: "New tab · Terminal or VS Code", exact: true, includeHidden: true });
  const sessionActions = page.getByRole("button", { name: "Session actions", exact: true, includeHidden: true });
  await expect(menu).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(menu).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Terminal", exact: true })).toBeFocused();
  await page.setViewportSize({ width: 1280, height: 844 });
  await expect(menu).toBeVisible();
  await expect.poll(async () => page.evaluate(() => {
    const menu = document.querySelector<HTMLElement>('[role="menu"][aria-label="Tab type"]');
    const trigger = document.querySelector<HTMLElement>(".af-tab-new");
    if (!menu || !trigger) return false;
    const menuBox = menu.getBoundingClientRect();
    const triggerBox = trigger.getBoundingClientRect();
    return menuBox.top >= triggerBox.bottom && menuBox.left >= 0 && menuBox.right <= window.innerWidth;
  })).toBe(true);
  await expect(menu.getByRole("menuitem", { name: "Terminal", exact: true })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(sessionActions).toHaveAttribute("aria-expanded", "false");
  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();

  // Closing clears the active callback. Reopening the same control must restore
  // it so the next responsive recomposition anchors the menu again.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: "More app controls", exact: true }).click();
  await page.evaluate(() => (document.activeElement as HTMLElement)?.blur());
  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
  await page.setViewportSize({ width: 1280, height: 844 });
  await expect.poll(async () => page.evaluate(() => {
    const menu = document.querySelector<HTMLElement>('[role="menu"][aria-label="Tab type"]');
    const trigger = document.querySelector<HTMLElement>(".af-tab-new");
    if (!menu || !trigger) return false;
    const menuBox = menu.getBoundingClientRect();
    const triggerBox = trigger.getBoundingClientRect();
    return menuBox.top >= triggerBox.bottom && menuBox.left >= 0 && menuBox.right <= window.innerWidth;
  })).toBe(true);
  await expect(menu.getByRole("menuitem", { name: "Terminal", exact: true })).toBeFocused();
});

test("picker closes before keyboard activation of a sibling action", async ({ page, request }) => {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  await page.keyboard.press("t");
  const menu = page.getByRole("menu", { name: "Tab type", exact: true });
  await expect(menu).toBeVisible();
  await menu.evaluate(el => {
    const sibling = document.createElement("button");
    sibling.textContent = "Sibling action";
    sibling.addEventListener("click", () => {
      const dialog = document.createElement("div");
      dialog.setAttribute("role", "dialog");
      dialog.textContent = "Sibling modal";
      dialog.addEventListener("keydown", event => {
        if (event.key === "Escape") dialog.remove();
      });
      dialog.tabIndex = -1;
      document.body.append(dialog);
      dialog.focus();
    });
    // The real Handoff action is a sibling of the picker container, not a child
    // of the picker wrap. Keep the fixture's focus geometry the same so focusout
    // exercises the product's outside-focus path.
    el.parentElement!.parentElement!.append(sibling);
  });
  await page.keyboard.press("End");
  await page.keyboard.press("Tab");
  const sibling = page.getByRole("button", { name: "Sibling action", exact: true });
  await expect(sibling).toBeFocused();
  await expect(menu).toBeHidden();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toBeHidden();
});

for (const key of ["Enter", "Space"]) {
  test(`picker does not consume ${key} after Tab leaves its items`, async ({ page, request }) => {
    const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
    const session = snapshot.data.instances.find((s: { title: string }) =>
      s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    await page.keyboard.press("Control+]");
    await page.keyboard.press("t");
    const menu = page.getByRole("menu", { name: "Tab type", exact: true });
    await expect(menu).toBeVisible();
    // A native button immediately after the picker models the next header action
    // without depending on whether this fixture offers Handoff or clipboard access.
    await menu.evaluate(el => {
      const button = document.createElement("button");
      button.textContent = "Next header action";
      button.addEventListener("click", () => { button.dataset.activated = "true"; });
      el.parentElement!.after(button);
    });
    await page.keyboard.press("End");
    await page.keyboard.press("Tab");
    const next = page.getByRole("button", { name: "Next header action", exact: true });
    await expect(next).toBeFocused();
    await page.keyboard.press(key);
    await expect(next).toHaveAttribute("data-activated", "true");
  });
}
