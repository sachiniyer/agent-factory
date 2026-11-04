// Fenced code blocks in the Docs view (#6731): a ``` fence off the cursor
// renders as a finished block — markers gone, language badge on top, syntax
// highlighting painted by the codeblocks lazy chunk (dist/lazy/codeblocks.js),
// which only a document holding a language-named fence ever fetches. A fence
// the cursor touches shows its source. An unknown language is plain code,
// never an error. A long line scrolls inside the block instead of widening
// the page — asserted at phone width — and the code is only ever text: no
// element comes out of it, and nothing it names is fetched.
import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { expect, test, type APIRequestContext, type Locator, type Page, type TestInfo } from "@playwright/test";

const GO = `// greet the world
package main

import "fmt"

func main() {
	fmt.Println("hello", 42)
}`;

const UNKNOWN = `z z z z — this language does not exist, so the block is plain code`;

// "constructor" is an Object.prototype member name: an object-literal lookup
// would hand back something truthy that is not a grammar. It must render as
// plain code, never an error.
const PROTOTYPE_NAMED = `this fence's info string is an inherited member name — plain code, never an error`;

const PLAIN = `no language here — just <plain> text & symbols`;

const MERMAID = `flowchart LR
  Code["code"] --> Diagram["diagram"]`;

const fence = (info: string, body: string): string => "```" + info + "\n" + body + "\n```";

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

test("docs codeblocks: fences render as highlighted blocks, only when a doc names a language, in both themes", async ({ page, request }, info) => {
  const chunkLoads: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/lazy/codeblocks.js")) chunkLoads.push(r.url());
  });
  // The strip orders non-template docs alphabetically, so a-plain opens first
  // and the grammar chunk must stay unfetched until the b-code tab is chosen.
  const pane = await openDocs(page, request, "codeblocks-probe", "codeblocks-plan", {
    "a-plain": `# Plain\n\nA fence with no language needs no grammars:\n\n${fence("", PLAIN)}\n`,
    "b-code": `# Code\n\nThe shape of the change:\n\n${fence("go", GO)}\n\n${fence("brainfuck", UNKNOWN)}\n\n${fence("constructor", PROTOTYPE_NAMED)}\n\n${fence("mermaid", MERMAID)}\n\nShip it.\n`,
  });
  await expect(pane.locator(".af-docs-tab")).toHaveText(["a-plain", "b-code"]);
  await expect(pane.locator(".cm-content")).toContainText("no language here");
  // A plain fence still renders as a finished block — markers hidden — without
  // ever fetching the grammar chunk.
  await expect(pane.locator(".cm-md-fence-code code")).toContainText("just <plain> text");
  await expect(pane.locator('.cm-line', { hasText: "```" })).toHaveCount(0);
  expect(chunkLoads, "a doc without a named language must not fetch the chunk").toHaveLength(0);

  await pane.locator(".af-docs-tab", { hasText: "b-code" }).click();
  const blocks = pane.locator(".cm-md-fence");
  await expect(blocks).toHaveCount(3); // go, brainfuck, constructor — mermaid is its own widget
  expect(chunkLoads, "the grammar chunk loads once, on demand").toHaveLength(1);

  // The go block: badge, and highlighted spans once the chunk lands.
  const go = blocks.nth(0);
  await expect(go.locator(".cm-md-fence-lang")).toHaveText("go");
  await expect(go.locator(".cm-md-fence-code .tok-keyword", { hasText: "package" })).toHaveCount(1, { timeout: 15_000 });
  await expect(go.locator(".cm-md-fence-code .tok-comment", { hasText: "greet the world" })).toHaveCount(1);
  await expect(go.locator(".cm-md-fence-code code")).toContainText("fmt.Println");

  // The unknown languages: same finished block, badge, but plain — the chunk
  // resolving to no grammar is not an error, and neither is an info string
  // that happens to be an Object.prototype member name.
  for (const [nth, lang, text] of [
    [1, "brainfuck", "this language does not exist"],
    [2, "constructor", "inherited member name"],
  ] as const) {
    const unknown = blocks.nth(nth);
    await expect(unknown.locator(".cm-md-fence-lang")).toHaveText(lang);
    await expect(unknown.locator(".cm-md-fence-code code")).toContainText(text);
    await expect(unknown.locator(".cm-md-fence-code span[class*=tok-]")).toHaveCount(0);
  }

  // The mermaid fence beside them still renders its diagram.
  await expect(pane.locator(".cm-md-mermaid svg")).toBeVisible({ timeout: 30_000 });

  // Cursor inside a fence shows its source, the way tables already behave.
  await go.click();
  await expect(pane.locator(".cm-md-fence")).toHaveCount(2, "the unknown-language blocks still render; the go fence is source");
  await expect(pane.locator(".cm-line", { hasText: "func main()" })).toBeVisible();
  // Cursor back out re-renders the finished block.
  await pane.locator(".cm-line", { hasText: "Ship it." }).click();
  await expect(go.locator(".cm-md-fence-code .tok-keyword", { hasText: "package" })).toHaveCount(1, { timeout: 15_000 });

  await still(pane, info, "docs-codeblocks.png");
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(page.locator("html")).toHaveAttribute("data-af-theme", "dark");
  await still(pane, info, "docs-codeblocks-dark.png");
});

test("docs codeblocks: a long line scrolls inside the block at phone width and never widens the page", async ({ page, request }, info) => {
  const longLine = "echo " + "af ".repeat(80).trimEnd() + " — a command too long for a phone";
  const pane = await openDocs(page, request, "codeblocks-phone-probe", "codeblocks-phone-plan", {
    goals: `# Phone\n\n${fence("sh", `#!/bin/sh\n${longLine}\n`)}\n\nAfter.\n`,
  });
  // The docs pane mounts at the desktop size, then the phone arrives: the
  // split view hides it ("1 pane hidden — too narrow") because two panes do not
  // fit at 390px. The phone header's one-tap docs shortcut (#docsShortcuts)
  // re-selects the docs tab, which focuses — and so un-hides — its pane.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: "Open Proposal" }).click();
  const pre = pane.locator(".cm-md-fence-code");
  await expect(pre).toBeVisible();
  const [scrollW, clientW, pageW] = await pre.evaluate((el) => [
    el.scrollWidth, el.clientWidth, document.documentElement.scrollWidth,
  ]);
  expect(scrollW, "the long line must overflow the block").toBeGreaterThan(clientW);
  expect(pageW, "the block must scroll internally, not widen the page").toBeLessThanOrEqual(390);
  await still(pane, info, "docs-codeblocks-phone.png");
});

test("docs codeblocks: code is only ever text — no markup, no fetch, no handlers", async ({ page, request }) => {
  const requests: string[] = [];
  page.on("request", (r) => requests.push(r.url()));
  page.on("dialog", (d) => void d.dismiss());
  const hostile = `<img src="/af-cb-probe/pixel.png" onerror="globalThis.__pwned=1">
<script>globalThis.__pwned=2</script>
<a href="https://cb.invalid">x</a>
fetch("https://cb.invalid/api")`;
  const pane = await openDocs(page, request, "codeblocks-hostile-probe", "codeblocks-hostile-plan", {
    goals: `# Hostile\n\n${fence("html", hostile)}\n\nEnd.\n`,
  });
  const block = pane.locator(".cm-md-fence");
  await expect(block).toHaveCount(1);
  const code = block.locator(".cm-md-fence-code code");
  // The whole source is text — the chunk's spans may style it, but nothing
  // else may ever exist inside. Wait for the highlight so the span walk
  // covers the painted state, not just the plain first paint.
  await expect(code.locator("span[class*=tok-]").first()).toBeVisible({ timeout: 15_000 });
  await expect(code).toContainText("onerror");
  const tags = await code.evaluate((el) => [...el.querySelectorAll("*")].map((n) => n.tagName));
  expect(new Set(tags), "only styling spans may exist inside the code").toEqual(new Set(["SPAN"]));
  expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
  expect(
    requests.filter((u) => u.includes("/af-cb-probe/") || u.includes("cb.invalid")),
    "code may never make the browser fetch anything it names",
  ).toEqual([]);
  await expect(pane.locator(".cm-content")).toContainText("End.");
});
