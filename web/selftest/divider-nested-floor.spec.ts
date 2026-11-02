import { expect, test, type Page } from "@playwright/test";

// #6560: dragging an ancestor divider can sliver nested leaves, because the
// drag's subtree floor sums leaf minima but ignores the descendant split's own
// ratio. With an inner split skewed to ~0.34, its true floor along the row
// axis is max(min/0.34, min/0.66) + divider ≈ 945px, not min+div+min = 644px —
// so the ancestor clamp lets the inner subtree shrink to 644 and the short
// child renders at ~218px.
const PANE_FLOOR_PX = 300;

const sessionA = process.env.AF_WEB_SESSION_A ?? "probe-a";

async function dragTabToPane(page: Page, tabIndex: number, paneIndex: number, edge: "left" | "right"): Promise<void> {
  await page.evaluate(
    ({ tabIndex, paneIndex, edge }) => {
      const tab = document.querySelectorAll<HTMLElement>(".af-tabbar .af-tab")[tabIndex];
      const pane = document.querySelectorAll<HTMLElement>(".af-term-host .af-pane")[paneIndex];
      if (!tab || !pane) {
        throw new Error(`drag source ${tabIndex} or target pane ${paneIndex} not found`);
      }
      const dt = new DataTransfer();
      tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));
      const r = pane.getBoundingClientRect();
      const m = 6;
      const init = {
        bubbles: true,
        cancelable: true,
        dataTransfer: dt,
        clientX: edge === "left" ? r.left + m : r.right - m,
        clientY: r.top + r.height / 2,
      };
      pane.dispatchEvent(new DragEvent("dragenter", init));
      pane.dispatchEvent(new DragEvent("dragover", init));
      pane.dispatchEvent(new DragEvent("drop", init));
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
    },
    { tabIndex, paneIndex, edge },
  );
}

async function paneWidths(page: Page): Promise<number[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>(".af-term-host .af-pane")]
      .filter((p) => p.getClientRects().length > 0)
      .map((p) => Math.round(p.getBoundingClientRect().width)),
  );
}

test("#6560: an ancestor drag cannot sliver a skewed nested leaf", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  // Wide enough that all three panes clear the floor: host ~1900px.
  await page.setViewportSize({ width: 2220, height: 800 });

  // probe-a's tab roster is shared with the suite — count relative to it.
  // Three shells: a new shell is SELECTED on create, so the last one stays
  // shown in the left pane while the first two are dragged — dragging a
  // shown tab onto another pane trips the one-tab-one-pane dedupe (it MOVES,
  // closing the old leaf) and onto its own pane routes through
  // companionTab's substitution, neither of which lands the named tab.
  const tabs = page.locator(".af-tabbar .af-tab");
  const base = await tabs.count();
  for (let i = 0; i < 3; i++) {
    await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  }
  await expect(tabs).toHaveCount(base + 3);
  await dragTabToPane(page, base, 0, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);
  // Drop my second new tab on the RIGHT pane's right edge: [a | (b|c)].
  await dragTabToPane(page, base + 1, 1, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(3);
  await expect(page.locator(".af-split-narrow")).toHaveCount(0);

  // Skew the INNER split: pull its divider toward b until the drag's own floor
  // stops it (b ends at its usable minimum, inner ratio ≈0.34 — perfectly
  // legal while the inner subtree is wide).
  const inner = page.locator(".af-term-host .af-divider:visible").nth(1);
  const innerBox = await inner.boundingBox();
  const hostBox = await page.locator(".af-term-host").boundingBox();
  expect(innerBox && hostBox).toBeTruthy();
  await page.mouse.move(innerBox!.x + innerBox!.width / 2, innerBox!.y + innerBox!.height / 2);
  await page.mouse.down();
  await page.mouse.move(hostBox!.x + hostBox!.width / 2 + 5, innerBox!.y + innerBox!.height / 2, { steps: 10 });
  await page.mouse.up();

  // Now drag the ANCESTOR divider as far toward the nested pair as it goes.
  // The naive subtree floor (leaf floors summed) under-clamps: the skewed
  // inner ratio means the pair needs far more room than the sum implies.
  const root = page.locator(".af-term-host .af-divider:visible").first();
  const rootBox = await root.boundingBox();
  await page.mouse.move(rootBox!.x + rootBox!.width / 2, rootBox!.y + rootBox!.height / 2);
  await page.mouse.down();
  await page.mouse.move(hostBox!.x + hostBox!.width - 5, rootBox!.y + rootBox!.height / 2, { steps: 10 });
  await page.mouse.up();

  const widths = await paneWidths(page);
  expect(
    widths.every((w) => w >= PANE_FLOOR_PX),
    `after the ancestor squeeze the panes render at ${widths.join(", ")}px; no nested leaf may be a sliver`,
  ).toBe(true);
});

test("#6573: a divider beside fit-hidden leaves still drags — its floor prices only visible leaves", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  // Wide enough that the balanced four-pane layout below actually fits.
  await page.setViewportSize({ width: 2220, height: 800 });

  // [(a|b)|(c|d)]: five shells — the last stays the pane-shown tab, so the
  // four drags all move genuinely unshown tabs (a shown tab's drop on another
  // pane dedupes into a move, never a split).
  const tabs = page.locator(".af-tabbar .af-tab");
  const base = await tabs.count();
  for (let i = 0; i < 5; i++) {
    await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  }
  await expect(tabs).toHaveCount(base + 5);
  await dragTabToPane(page, base, 0, "right"); // [d | a]
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);
  await dragTabToPane(page, base + 1, 0, "left"); // [(b|d) | a]
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(3);
  await dragTabToPane(page, base + 2, 2, "right"); // [(b|d) | (a|c)]
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(4);

  // Focus the leaf in the RIGHT pair so the fit hides one leaf per side:
  // leaf order is [b,d,a,c], the protected leaf is a, and fitToSize drops
  // violators right-to-left — c, then d — leaving {b, a} visible while each
  // root side still carries a hidden sibling.
  await page.locator(".af-term-host .af-pane").nth(2).click();
  // Aim the host at ~700px: two panes fit (2*320+4), three cannot.
  const host = page.locator(".af-term-host");
  const hostW0 = (await host.boundingBox())!.width;
  await page.setViewportSize({ width: Math.round(2220 - (hostW0 - 700)) });
  await expect(page.locator(".af-split-narrow")).toContainText("2 panes hidden");
  await expect(page.locator(".af-term-host .af-pane:visible")).toHaveCount(2);

  // The only rendered divider is the ROOT one — each side holds one visible
  // leaf plus a hidden sibling. Its retained subtree floor sums BOTH leaves
  // per side (644px each) against the ~696px span, so an unfixed clamp
  // deadlocks (lo > hi → the ratio can never move).
  const root = page.locator(".af-term-host .af-divider:visible");
  await expect(root).toHaveCount(1);
  const before = await paneWidths(page);
  const rootBox = await root.boundingBox();
  await page.mouse.move(rootBox!.x + rootBox!.width / 2, rootBox!.y + rootBox!.height / 2);
  await page.mouse.down();
  await page.mouse.move(rootBox!.x + 100, rootBox!.y + rootBox!.height / 2, { steps: 10 });
  await page.mouse.up();

  const after = await paneWidths(page);
  expect(
    after.some((w, i) => Math.abs(w - before[i]) > 5),
    `a 100px drag on the root divider must move the panes (before ${before.join(", ")}px, after ${after.join(", ")}px); a frozen ratio is the #6573 deadlock`,
  ).toBe(true);
});
