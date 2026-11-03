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

// A request a diagram could have chosen: anything carrying a probe marker
// (the fixtures name /af-mmd-probe/* paths that exist nowhere), anything on
// the web-tab proxy path (which forwards to a session's dev server), or
// anything off this origin at all. Same-origin counts — that is the point.
function diagramChosenRequests(requests: string[], pageUrl: string): string[] {
  const pageOrigin = new URL(pageUrl).origin;
  return requests.filter((u) => {
    const url = new URL(u);
    return url.pathname.includes("/af-mmd-probe/") || url.pathname.startsWith("/v1/webtab/") ||
      ((url.protocol === "http:" || url.protocol === "https:") && url.origin !== pageOrigin);
  });
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
  const chosen = diagramChosenRequests(requests, page.url());
  expect(chosen, "a diagram may not make the browser ask for anything it names").toEqual([]);
  await expect(pane.locator(".cm-content")).toContainText("End.");
});

test("docs mermaid: a render cannot make the browser ask for anything the diagram names", async ({ page, request }) => {
  const requests: string[] = [];
  page.on("request", (r) => requests.push(r.url()));
  // Every fetch vector a diagram can name, each pointed at a marker path that
  // exists nowhere: a plain diagram first (must render), then flowchart img
  // node metadata, a top-level fontFamily via an init directive (lands in
  // the host div's style attribute and the generated <style>), the same via
  // yaml frontmatter, and an image-set() value with a bare-string URL — plus
  // an external URL for good measure.
  const imgNode = `flowchart LR
  P@{ img: "/af-mmd-probe/img-node.png", label: "img", w: 40, h: 40, constraint: "off" }
  P --> Q["node"]`;
  const directive = `%%{init: {"fontFamily": "x; background: url(/af-mmd-probe/font-directive.png);"}}}%%
flowchart LR
  D1 --> D2`;
  const frontmatter = `---
config:
  fontFamily: "x; background: image-set('/af-mmd-probe/imgset.png' 1x), url(https://mmd.invalid/f.png)"
---
flowchart LR
  F1 --> F2`;
  const pane = await openDocs(page, request, "mermaid-fetch-probe", "mermaid-fetch-plan", {
    goals: `# Fetch probes\n\n${fence(DIAGRAM)}\n\n${fence(imgNode)}\n\n${fence(directive)}\n\n${fence(frontmatter)}\n\nEnd.\n`,
  });
  const widgets = pane.locator(".cm-md-mermaid");
  await expect(widgets.first().locator("svg")).toBeVisible({ timeout: 30_000 });
  // Each widget either rendered an svg or reported inline — the img node
  // errors, its fetch denied by the frame's CSP. Either way nothing may
  // have fetched, same-origin included.
  for (const w of await widgets.all()) {
    await expect(w.locator("svg, .cm-md-mermaid-errtext").first()).toBeVisible({ timeout: 30_000 });
  }
  const chosen = diagramChosenRequests(requests, page.url());
  expect(chosen, "zero diagram-chosen requests, same-origin counted").toEqual([]);
  await expect(pane.locator(".cm-content")).toContainText("End.");
});
