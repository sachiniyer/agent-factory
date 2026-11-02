// Mermaid in the Docs view (#6642): a ```mermaid fence renders as an SVG in the
// live preview, the mermaid library arrives in its own lazy chunk that only a
// document containing a block fetches, an invalid diagram reports inline next
// to its source, and a hostile diagram — click callbacks, javascript: links,
// HTML in labels — renders inert inside the daemon's control origin.
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
  // The strip order follows --docs order, so the no-diagram doc opens first.
  const pane = await openDocs(page, request, "mermaid-probe", "mermaid-plan", {
    plain: "# Plain\n\nNo diagrams here — the mermaid chunk must stay unloaded.\n",
    diagram: `# Diagram\n\nThe shape of the change:\n\n${fence(DIAGRAM)}\n\nShip it.\n`,
  });
  await expect(pane.locator(".af-docs-tab")).toHaveText(["plain", "diagram"]);
  await expect(pane.locator(".cm-content")).toContainText("No diagrams here");
  expect(mermaidLoads, "a doc without a mermaid block must not fetch the chunk").toHaveLength(0);

  await pane.locator(".af-docs-tab", { hasText: "diagram" }).click();
  const mermaid = pane.locator(".cm-md-mermaid");
  await expect(mermaid.locator("svg")).toBeVisible({ timeout: 30_000 });
  expect(mermaidLoads, "the mermaid chunk loads once, on demand").toHaveLength(1);
  await expect(mermaid.locator("svg text").filter({ hasText: "af daemon" })).toHaveCount(1);
  await expect(mermaid.locator(".cm-md-mermaid-errtext")).toHaveCount(0);

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

test("docs mermaid: click callbacks, javascript: links and HTML labels render inert", async ({ page, request }) => {
  const dialogs: string[] = [];
  page.on("dialog", (d) => {
    dialogs.push(d.message());
    void d.dismiss();
  });
  const external: string[] = [];
  page.on("request", (r) => {
    const u = new URL(r.url());
    const origin = u.protocol === "http:" || u.protocol === "https:" ? u.origin : null;
    if (origin !== null && origin !== new URL(page.url()).origin) external.push(r.url());
  });
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
  expect(external, "a diagram may not reach outside this origin").toEqual([]);
  await expect(pane.locator(".cm-content")).toContainText("End.");
});
