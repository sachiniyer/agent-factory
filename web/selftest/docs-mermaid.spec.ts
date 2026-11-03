// Mermaid in the Docs view (#6642): a ```mermaid fence renders as an SVG in the
// live preview, the mermaid library arrives in its own lazy chunk that only a
// document containing a block fetches, an invalid diagram reports inline next
// to its source, and a hostile diagram — click callbacks, javascript: links,
// HTML in labels, CSS and metadata that name URLs — renders inert inside the
// daemon's control origin and can never make the browser ask for what it names
// (the render happens inside a srcdoc iframe whose CSP denies every fetch).
import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { expect, test, type APIRequestContext, type Locator, type Page, type TestInfo } from "@playwright/test";

const DIAGRAM = `flowchart LR
  User["user"] --> AF["af daemon"]
  AF --> Docs["docs pane"]
  Docs --> User`;

const fence = (body: string): string => "```mermaid\n" + body + "\n```";

interface Started { path: string; tab?: string; tab_error?: string }

// A proposal whose docs carry the given markdown, opened to its first doc.
async function openDocs(page: Page, request: APIRequestContext,
  title: string, slug: string, docs: Record<string, string>): Promise<Locator> {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  const started = JSON.parse(af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title,
    "--docs", Object.keys(docs).join(","))) as Started;
  expect(started.tab_error ?? "", "start must create the Docs tab").toBe("");
  for (const [name, body] of Object.entries(docs)) {
    writeFileSync(join(started.path, `${name}.md`), body);
  }
  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();
  await page.goto(`/#/session/${session.id}`);
  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();
  return pane;
}

async function still(pane: Locator, info: TestInfo, name: string): Promise<void> {
  await pane.page().evaluate(() => document.fonts.ready);
  const pixels = await pane.screenshot({ animations: "disabled", caret: "hide" });
  await writeFile(info.outputPath(`golden-${name}`), pixels);
  expect(pixels).toMatchSnapshot(name);
}

test("docs mermaid: a mermaid fence renders an SVG, only when a doc holds one, in both themes", async ({ page, request }, info) => {
  const mermaidLoads: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/lazy/mermaid.js")) mermaidLoads.push(r.url());
  });
  // The strip orders non-template docs alphabetically, so a-plain opens first
  // and the mermaid chunk must stay unfetched until the b-diagram tab is chosen.
  const pane = await openDocs(page, request, "mermaid-probe", "mermaid-plan", {
    "a-plain": "# Plain\n\nNo diagrams here — the mermaid chunk must stay unloaded.\n",
    "b-diagram": `# Diagram\n\nThe shape of the change:\n\n${fence(DIAGRAM)}\n\nShip it.\n`,
  });
  await expect(pane.locator(".af-docs-tab")).toHaveText(["a-plain", "b-diagram"]);
  await expect(pane.locator(".cm-content")).toContainText("No diagrams here");
  expect(mermaidLoads, "a doc without a mermaid block must not fetch the chunk").toHaveLength(0);

  await pane.locator(".af-docs-tab", { hasText: "diagram" }).click();
  const mermaid = pane.locator(".cm-md-mermaid");
  await expect(mermaid.locator("svg")).toBeVisible({ timeout: 30_000 });
  expect(mermaidLoads, "the mermaid chunk loads once, on demand").toHaveLength(1);
  await expect(mermaid.locator("svg text").filter({ hasText: "af daemon" })).toHaveCount(1);
  await expect(pane.locator(".cm-md-mermaid-errtext:visible")).toHaveCount(0);

  // Cursor inside the fence shows its source, the way tables already behave.
  await mermaid.click();
  await expect(mermaid).toHaveCount(0);
  await expect(pane.locator(".cm-line", { hasText: "flowchart LR" })).toBeVisible();
  // Cursor back out re-renders the diagram.
  await pane.locator(".cm-line", { hasText: "Ship it." }).click();
  await expect(mermaid.locator("svg")).toBeVisible({ timeout: 30_000 });

  await still(pane, info, "docs-mermaid.png");
  await page.emulateMedia({ colorScheme: "dark" });
  // The diagram is re-rendered against the dark tokens before the still.
  await expect(mermaid).toHaveAttribute("data-mmd-theme", "dark");
  await still(pane, info, "docs-mermaid-dark.png");
});

test("docs mermaid: an invalid diagram reports inline and keeps the document", async ({ page, request }) => {
  const pane = await openDocs(page, request, "mermaid-bad-probe", "mermaid-bad-plan", {
    goals: `# Goals\n\nA broken fence:\n\n${fence("this is not a diagram at all")}\n\nAfterwards.\n`,
  });
  const mermaid = pane.locator(".cm-md-mermaid");
  await expect(mermaid.locator(".cm-md-mermaid-errtext"), "a parse failure names itself").toBeVisible({ timeout: 30_000 });
  await expect(mermaid.locator(".cm-md-mermaid-errtext")).toContainText("render");
  await expect(mermaid.locator(".cm-md-mermaid-src")).toContainText("this is not a diagram at all");
  await expect(pane.locator(".cm-content"), "the document after the fence still renders").toContainText("Afterwards.");
});

// A URL a diagram could have chosen: a probe marker (the fixtures name
// /af-mmd-probe/* paths that exist nowhere), the web-tab proxy path (which
// forwards to a session's dev server), or anything off this origin at all.
// Same-origin counts — that is the point.
function diagramChosen(url: string, pageUrl: string): boolean {
  const pageOrigin = new URL(pageUrl).origin;
  const u = new URL(url);
  return u.pathname.includes("/af-mmd-probe/") || u.pathname.startsWith("/v1/webtab/") ||
    ((u.protocol === "http:" || u.protocol === "https:") && u.origin !== pageOrigin);
}

// Chromium fires "request" for a load the render frame's CSP then denies —
// requestWillBeSent precedes the block — so a request event alone is not a
// leak. But neither is finished-only: a failed or aborted fetch can still
// have reached the wire (DNS, connect). A request counts as denied only when
// its failure errorText says blocked, or a CSP-violation console message
// names its URL.
function watchRequests(page: Page): { finished: Set<string>; failed: Map<string, string>; cspDenied: Set<string> } {
  const finished = new Set<string>();
  const failed = new Map<string, string>();
  const cspDenied = new Set<string>();
  page.on("requestfinished", (r) => finished.add(r.url()));
  page.on("requestfailed", (r) => failed.set(r.url(), r.failure()?.errorText ?? ""));
  page.on("console", (m) => {
    const text = m.text();
    if (text.includes("Content Security Policy")) {
      const url = text.match(/'(https?:[^']+)'/);
      if (url) cspDenied.add(url[1]);
    }
  });
  return { finished, failed, cspDenied };
}

/** True when a diagram-chosen request actually reached the wire — completed,
 * or failed/aborted without evidence the frame's CSP refused it. */
function leaked(u: string, pageUrl: string, w: ReturnType<typeof watchRequests>): boolean {
  if (!diagramChosen(u, pageUrl)) return false;
  if (w.finished.has(u)) return true;
  const err = w.failed.get(u);
  if (err === undefined) return true; // attempted, still in flight — not denied
  return !/blocked/i.test(err) && !w.cspDenied.has(u);
}

test("docs mermaid: click callbacks, javascript: links and HTML labels render inert", async ({ page, request }) => {
  const dialogs: string[] = [];
  page.on("dialog", (d) => {
    dialogs.push(d.message());
    void d.dismiss();
  });
  // Requests are collected raw: during openDocs' own goto, page.url() is
  // still about:blank, so the page origin is only known at assertion time.
  const requests: string[] = [];
  page.on("request", (r) => requests.push(r.url()));
  const { finished } = watchRequests(page);
  const hostile = `%%{init: {"themeVariables": {"fontFamily": "x; background: url(https://mmd.invalid/pixel.png);"}}}%%
flowchart LR
  A["<img src=x onerror=globalThis.__pwned=1>"] --> B["<b>normal</b>"]
  click A call alert(1)
  click B href "javascript:globalThis.__pwned=2"`;
  const pane = await openDocs(page, request, "mermaid-hostile-probe", "mermaid-hostile-plan", {
    goals: `# Hostile\n\n${fence(hostile)}\n\nEnd.\n`,
  });
  const mermaid = pane.locator(".cm-md-mermaid");
  await expect(mermaid.locator("svg")).toBeVisible({ timeout: 30_000 });
  const svg = mermaid.locator("svg");
  await expect(svg.locator("a"), "click statements must not produce links").toHaveCount(0);
  await expect(svg.locator("img"), "label HTML must not become elements").toHaveCount(0);
  await expect(svg.locator("foreignObject")).toHaveCount(0);
  await expect(svg.locator("script")).toHaveCount(0);
  const onAttrs = await svg.evaluate((root) => {
    const out: string[] = [];
    for (const el of [root, ...root.querySelectorAll("*")]) {
      for (const a of el.attributes) if (a.name.toLowerCase().startsWith("on")) out.push(`${el.tagName}.${a.name}`);
    }
    return out;
  });
  expect(onAttrs, "no event-handler attributes may survive rendering").toEqual([]);
  // Even if a handler somehow existed, poking every node must not fire it.
  for (const node of await svg.locator("g").all()) {
    await node.dispatchEvent("click").catch(() => {});
  }
  expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
  expect(dialogs, "no dialog may come out of a diagram").toEqual([]);
  const chosen = requests.filter((u) => leaked(u, page.url(), { finished, failed: new Map(), cspDenied: new Set() }));
  expect(chosen, "a diagram may not make the browser fetch anything it names").toEqual([]);
  await expect(pane.locator(".cm-content")).toContainText("End.");
});

test("docs mermaid: a render cannot make the browser ask for anything the diagram names", async ({ page, request }) => {
  const requests: string[] = [];
  page.on("request", (r) => requests.push(r.url()));
  const { finished, failed } = watchRequests(page);
  // Every fetch vector a diagram can name, each pointed at a marker path that
  // exists nowhere: a plain diagram first (must render), then flowchart img
  // node metadata, a top-level fontFamily via an init directive (lands in
  // the host div's style attribute and the generated <style>), the same via
  // yaml frontmatter, and an image-set() value with a bare-string URL — plus
  // an external URL for good measure.
  const imgNode = `flowchart LR
  P@{ img: "/af-mmd-probe/img-node.png", label: "img", w: 40, h: 40, constraint: "off" }
  P --> Q["node"]`;
  const directive = `%%{init: {"fontFamily": "x; background: url(/af-mmd-probe/font-directive.png);"}}%%
flowchart LR
  D1 --> D2`;
  const frontmatter = `---
config:
  fontFamily: "x; background: image-set('/af-mmd-probe/imgset.png' 1x), url(https://mmd.invalid/f.png)"
---
flowchart LR
  F1 --> F2`;
  // The img-node fence goes FIRST: whatever it does — error or a hung render
  // — the plain and probe diagrams after it must still render, proving a
  // wedged queue can't take the renderer down until reload (N2).
  const pane = await openDocs(page, request, "mermaid-fetch-probe", "mermaid-fetch-plan", {
    goals: `# Fetch probes\n\n${fence(imgNode)}\n\n${fence(DIAGRAM)}\n\n${fence(directive)}\n\n${fence(frontmatter)}\n\nEnd.\n`,
  });
  const widgets = pane.locator(".cm-md-mermaid");
  // Only the img-node widget may report an inline error (its fetch is denied
  // inside the frame); every other widget MUST render a real svg — an error
  // is not success.
  await expect(widgets.nth(0).locator("svg, .cm-md-mermaid-errtext").first()).toBeVisible({ timeout: 40_000 });
  for (const i of [1, 2, 3]) {
    await expect(widgets.nth(i).locator("svg"), `widget ${i} must render`).toBeVisible({ timeout: 40_000 });
  }
  // N1: the render frame's document must put the DNS-prefetch kill first —
  // anchor hosts inside label HTML are prefetched before CSP applies.
  const firstMeta = await page.evaluate(() => {
    const f = [...document.querySelectorAll("iframe")].find((el) => el.srcdoc.includes("x-dns-prefetch-control"));
    return f?.contentDocument?.head?.firstElementChild?.outerHTML ?? null;
  });
  expect(firstMeta, "the x-dns-prefetch-control meta must be the frame's first element")
    .toContain('http-equiv="x-dns-prefetch-control"');
  // No diagram-chosen URL may reach the wire.
  const leakedUrls = requests.filter((u) => leaked(u, page.url(), { finished, failed, cspDenied }));
  expect(leakedUrls, "zero diagram-chosen requests reached the wire, same-origin counted").toEqual([]);
  // And the img-node probe must show its fetch attempted and denied — proof
  // the frame's CSP engaged, not that the code path never ran.
  const imgProbe = new URL("/af-mmd-probe/img-node.png", page.url()).href;
  expect(
    requests.includes(imgProbe) && (failed.has(imgProbe) || cspDenied.has(imgProbe)) && !finished.has(imgProbe),
    "the img-node load must be attempted and denied by the render frame's CSP",
  ).toBe(true);
  await expect(pane.locator(".cm-content")).toContainText("End.");
});
