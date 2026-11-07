// Mermaid in the Docs view (#6642): a ```mermaid fence renders as an SVG in the
// live preview, the mermaid library arrives inside its own lazy document that
// only a document containing a block fetches, an invalid diagram reports
// inline next to its source, and a hostile diagram — click callbacks,
// javascript: links, HTML in labels, CSS and metadata that name URLs —
// renders inert inside the daemon's control origin and can never make the
// browser ask for what it names (the render happens inside a sandboxed frame
// document whose CSP denies every fetch).
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { writeFileSync } from "node:fs";
import { writeFile } from "node:fs/promises";
import { createServer, type Server } from "node:http";
import { join } from "node:path";
import { expect, test, type APIRequestContext, type Locator, type Page, type TestInfo } from "@playwright/test";

const DIAGRAM = `flowchart LR
  User["user"] --> AF["af daemon"]
  AF --> Docs["docs pane"]
  Docs --> User`;

const fence = (body: string): string => "```mermaid\n" + body + "\n```";

interface Started { path: string; tab?: string; tab_error?: string }

// A proposal whose docs carry the given markdown, opened to its first doc.
// Names are PREFIXED with the project so the same spec can run under both the
// chromium and webkit projects against one daemon's persistent session and
// proposal stores without colliding on titles/slugs. A suffix would graft the
// project name onto the tail ("mermaid-probe-chromium" contains "probe-c") —
// sidebar absence assertions that substring-match session titles would count
// these sessions.
async function openDocs(page: Page, request: APIRequestContext,
  title: string, slug: string, docs: Record<string, string>): Promise<Locator> {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });
  const run = test.info().project.name;
  title = `${run}-${title}`;
  slug = `${run}-${slug}`;
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
    if (r.url().includes("/lazy/mermaid-frame.html")) mermaidLoads.push(r.url());
  });
  // The strip orders non-template docs alphabetically, so a-plain opens first
  // and the mermaid frame must stay unfetched until the b-diagram tab is chosen.
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
  expect(mermaidLoads, "the mermaid frame document loads once, on demand").toHaveLength(1);
  await expect(mermaid.locator("svg text").filter({ hasText: "af daemon" })).toHaveCount(1);
  await expect(pane.locator(".cm-md-mermaid-errtext:visible")).toHaveCount(0);

  // Cursor inside the fence shows its source, the way tables already behave.
  await mermaid.click();
  await expect(mermaid).toHaveCount(0);
  await expect(pane.locator(".cm-line", { hasText: "flowchart LR" })).toBeVisible();
  // Cursor back out re-renders the diagram.
  await pane.locator(".cm-line", { hasText: "Ship it." }).click();
  await expect(mermaid.locator("svg")).toBeVisible({ timeout: 30_000 });

  // Goldens are pixel-calibrated to Chromium's rasterizer — the WebKit
  // project exercises the same render and theme path but skips the bitmap
  // compare (its pixels legitimately differ).
  if (info.project.name === "chromium") {
    await still(pane, info, "docs-mermaid.png");
    await page.emulateMedia({ colorScheme: "dark" });
    // The diagram is re-rendered against the dark tokens before the still.
    await expect(mermaid).toHaveAttribute("data-mmd-theme", "dark");
    await still(pane, info, "docs-mermaid-dark.png");
  } else {
    await page.emulateMedia({ colorScheme: "dark" });
    await expect(mermaid).toHaveAttribute("data-mmd-theme", "dark");
    await expect(mermaid.locator("svg"), "the dark-theme re-render must still paint").toBeVisible({ timeout: 30_000 });
  }
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
  // ERR_BLOCKED_BY_CSP exactly — the looser /blocked/i would excuse
  // ERR_BLOCKED_BY_ORB, _RESPONSE and _CLIENT, which are real network events.
  return !/ERR_BLOCKED_BY_CSP/.test(err) && !w.cspDenied.has(u);
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
  const watcher = watchRequests(page);
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
  const chosen = requests.filter((u) => leaked(u, page.url(), watcher));
  expect(chosen, "a diagram may not make the browser fetch anything it names").toEqual([]);
  await expect(pane.locator(".cm-content")).toContainText("End.");
});

test("docs mermaid: a render cannot make the browser ask for anything the diagram names", async ({ page, request, browserName }) => {
  const requests: string[] = [];
  page.on("request", (r) => requests.push(r.url()));
  const { finished, failed, cspDenied } = watchRequests(page);
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
  // A state diagram's `style` statement is raw CSS to end of line and lands
  // verbatim in the shape's style attribute — the one place a url() can hide
  // behind characters a character-class scan skips (U+2028 below). Mermaid's
  // own style handling keeps only the first declaration, so each statement
  // carries exactly one: S1's is the hostile url() the sanitizer must strip,
  // S2's benign fill is the control proving single declarations do reach
  // the page (otherwise the probe would exercise nothing).
  const stateStyle = `stateDiagram-v2
  S1 --> S2
  style S1 background:url(\u{2028}/af-mmd-probe/state-style.png)
  style S2 fill:#8ff`;
  // Journey labels come out as <switch><foreignObject/><text/></switch> —
  // the switch must survive so the SVG <text> fallback does.
  const journey = `journey
  title A shift
  section Morning
    Wake up: 5: Me
    Coffee: 4: Me
  section Evening
    Sleep: 3: Me`;
  // The img-node fence goes FIRST: whatever it does — error or a hung render
  // — the plain and probe diagrams after it must still render, proving a
  // wedged queue can't take the renderer down until reload (N2).
  const pane = await openDocs(page, request, "mermaid-fetch-probe", "mermaid-fetch-plan", {
    goals: `# Fetch probes\n\n${fence(imgNode)}\n\n${fence(DIAGRAM)}\n\n${fence(directive)}\n\n${fence(frontmatter)}\n\n${fence(stateStyle)}\n\n${fence(journey)}\n\nEnd.\n`,
  });
  const widgets = pane.locator(".cm-md-mermaid");
  // Only the img-node widget may report an inline error (its fetch is denied
  // inside the frame); every other widget MUST render a real svg — an error
  // is not success.
  await expect(widgets.nth(0).locator("svg, .cm-md-mermaid-errtext").first()).toBeVisible({ timeout: 40_000 });
  for (const i of [1, 2, 3, 4, 5]) {
    await expect(widgets.nth(i).locator("svg"), `widget ${i} must render`).toBeVisible({ timeout: 40_000 });
  }
  // The state diagram rendered — but its style statement's probe URL must
  // have been scrubbed from whatever attribute or style block carried it,
  // while S2's single benign declaration must have made it through (the
  // control proving the probe exercised the sanitizer at all).
  const stateSvg = widgets.nth(4).locator("svg");
  const [probeSurvivors, controlHits] = await stateSvg.evaluate((root) => {
    const hits: string[] = [];
    let control = 0;
    for (const el of [root, ...root.querySelectorAll("*")]) {
      for (const a of el.attributes) {
        if (a.value.includes("af-mmd-probe")) hits.push(`${el.tagName}@${a.name}`);
        if (a.value.includes("#8ff")) control++;
      }
      if ((el.textContent ?? "").includes("af-mmd-probe")) hits.push(`${el.tagName}#text`);
    }
    return [hits, control] as const;
  });
  expect(probeSurvivors, "no element may carry the state style's probe URL").toEqual([]);
  expect(controlHits, "a benign single-declaration style must reach the page").toBeGreaterThan(0);
  // Journey: section/task names live in <switch>'s SVG <text> fallback —
  // dropped wholesale before <switch> joined the keep list.
  const journeySvg = widgets.nth(5).locator("svg");
  await expect(journeySvg.locator("text").filter({ hasText: "Coffee" }).first(),
    "journey task names must survive via the switch's text fallback").toBeVisible();
  expect(await journeySvg.locator("foreignObject").count(),
    "the foreignObject branch of every switch must still be dropped").toBe(0);
  // N1: the render frame's document must put the DNS-prefetch kill first —
  // anchor hosts inside label HTML are prefetched before CSP applies. The
  // frame is a served document now (#6669), so this reads it off the wire.
  const frameDoc = await (await request.get("/lazy/mermaid-frame.html")).text();
  expect(
    frameDoc.startsWith('<!doctype html><meta http-equiv="x-dns-prefetch-control" content="off">'),
    "the x-dns-prefetch-control meta must be the frame's first element",
  ).toBe(true);
  // No diagram-chosen URL may reach the wire.
  const leakedUrls = requests.filter((u) => leaked(u, page.url(), { finished, failed, cspDenied }));
  expect(leakedUrls, "zero diagram-chosen requests reached the wire, same-origin counted").toEqual([]);
  // And the img-node probe must show its fetch attempted and denied — proof
  // the frame's CSP engaged, not that the code path never ran. The attempt is
  // observable only in Chromium: WebKit refuses a CSP-denied load before its
  // network stack reports it, so nothing surfaces to watch there — a stronger
  // denial, and the engine-level invariant (never completed) is what matters.
  const imgProbe = new URL("/af-mmd-probe/img-node.png", page.url()).href;
  if (browserName === "chromium") {
    expect(
      requests.includes(imgProbe) && (failed.has(imgProbe) || cspDenied.has(imgProbe)) && !finished.has(imgProbe),
      "the img-node load must be attempted and denied by the render frame's CSP",
    ).toBe(true);
  }
  expect(finished.has(imgProbe), "the img-node load must never complete").toBe(false);
  // Same for the state-style probe: the frame may attempt it while the svg
  // sits in the frame's scratch, but it must be denied there and never
  // complete — the page copy carries no trace of it at all (checked above).
  const styleProbe = new URL("/af-mmd-probe/state-style.png", page.url()).href;
  if (requests.includes(styleProbe)) {
    expect(
      (failed.has(styleProbe) || cspDenied.has(styleProbe)) && !finished.has(styleProbe),
      "the state-style load, if attempted, must be denied by the render frame's CSP",
    ).toBe(true);
  }
  await expect(pane.locator(".cm-content")).toContainText("End.");
});

// The render frame's document is a separate security principal (#6669): the
// frame CSP isolates what the frame may LOAD, but only sandboxing isolates its
// PRIVILEGE. sandbox without allow-same-origin gives it an opaque origin — no
// parent DOM, no storage or cookies, no credentials on a /v1 call — and the
// only channel back is a bound MessagePort carrying the SVG string, which the
// parent validates and sanitizes before it touches the DOM. This test is the one that
// must run in WebKit too: a srcdoc frame inherits the page's CSP, and in
// WebKit 'self' matches nothing in an opaque origin, so the old shape rendered
// nothing at all — the frame is a real served document now, and the daemon's
// REAL headers (the CSP response header, not a meta) are what authorize it.
test("docs mermaid: the render frame is an opaque origin — it cannot reach the parent or the API", async ({ page, request }) => {
  // Every request attributed to a non-main frame, and every request carrying
  // the opaque origin's serialized Origin — the render frame must issue none
  // of either: its whole library is inline, and a request from it is exactly
  // what an authenticating proxy would have to answer without credentials.
  const frameSubresource: string[] = [];
  const nullOrigin: string[] = [];
  page.on("request", (r) => {
    const f = r.frame();
    if (f && f !== page.mainFrame() && !r.isNavigationRequest()) {
      frameSubresource.push(`${r.method()} ${r.url()}`);
    }
    if (r.headers()["origin"] === "null") nullOrigin.push(r.url());
  });
  // Every wire fetch of the frame document, measured in BODY BYTES: the first
  // open serves the library, and the FRESH frame after the forced navigation
  // below must revalidate — a 304 answers with no body, so the doc's ~5 MB
  // stays cached (#6669 re-review: without validators every open paid it).
  // Bytes, not statuses: a revalidation resolves to a page-level 200 in
  // Chromium (the cached body answers) while WebKit reports the wire 304 —
  // responseBodySize is the honest wire cost under either reporting.
  const frameDocBytes: Promise<number>[] = [];
  page.on("requestfinished", (r) => {
    if (r.url().endsWith("/lazy/mermaid-frame.html"))
      frameDocBytes.push(r.sizes().then((s) => s.responseBodySize));
  });
  const pane = await openDocs(page, request, "mermaid-sandbox-probe", "mermaid-sandbox-plan", {
    goals: `# Sandbox\n\n${fence(DIAGRAM)}\n\nEnd.\n`,
  });
  // The diagram still renders — the channel becoming a port-bound postMessage
  // changes nothing the document shows. This line is the WebKit regression itself:
  // under srcdoc+inherited-CSP it timed out 30s and died.
  await expect(pane.locator(".cm-md-mermaid svg")).toBeVisible({ timeout: 30_000 });

  // The render frame is the iframe navigated to the served frame document.
  const frame = page.frames().find((f) => f.url().endsWith("/lazy/mermaid-frame.html"));
  expect(frame, "the render frame must exist once a diagram rendered").toBeTruthy();
  if (!frame) return;

  // The REAL response headers are the policy: the served document must carry
  // its own CSP — default-src 'none' with exactly a hash'd script-src, the two
  // directives that sandbox the DOCUMENT however it is loaded, and validators
  // so a re-open revalidates instead of refetching. A static-server stand-in
  // could never check this; the daemon's header is what WebKit enforces.
  const resp = await request.get("/lazy/mermaid-frame.html");
  const csp = resp.headers()["content-security-policy"] ?? "";
  expect(csp, "the frame document must arrive with its own CSP").toContain("default-src 'none'");
  expect(csp, "the frame document must not fetch").toContain("connect-src 'none'");
  expect(csp, "script-src is a hash, not a blanket inline grant").not.toMatch(/script-src 'unsafe-inline'/);
  expect(csp, "the document runs sandboxed however it is loaded").toContain("sandbox allow-scripts");
  expect(csp, "only this origin may frame it").toContain("frame-ancestors 'self'");
  const hdrHash = /script-src 'sha256-([A-Za-z0-9+/=]+)'/.exec(csp)?.[1];
  expect(hdrHash, "script-src must be exactly one sha256 source").toBeTruthy();
  const body = await resp.text();
  expect(body, "the script element carries no nonce — the hash names it").not.toContain("nonce=");
  // The CSP's hash must be the sha256 of the served script ELEMENT's text —
  // recomputed here from the body, not read from the sidecar the server does.
  const openTag = `<script type="module">`;
  const scriptText = body.slice(body.indexOf(openTag) + openTag.length, body.lastIndexOf("</script>"));
  expect(
    createHash("sha256").update(scriptText, "utf8").digest("base64"),
    "the CSP hash must name the served script text",
  ).toBe(hdrHash);
  expect(resp.headers()["access-control-allow-origin"], "no CORS grant is needed — the frame fetches nothing").toBeFalsy();
  expect(resp.headers()["etag"], "the document must carry a validator").toBeTruthy();
  expect(resp.headers()["cache-control"], "a revalidation must re-check, not expire").toBe("no-cache");

  // Structural: sandboxed to scripts alone — never allow-same-origin — and the
  // parent's own handle on the element sees no document through it.
  const attrs = await page.evaluate(() => {
    const el = [...document.querySelectorAll("iframe")].find((f) =>
      f.src.endsWith("/lazy/mermaid-frame.html"));
    if (!el) return null;
    return { sandbox: el.getAttribute("sandbox"), contentDoc: el.contentDocument !== null };
  });
  expect(attrs?.sandbox, "the frame must be sandboxed to scripts only").toBe("allow-scripts");
  expect(attrs?.contentDoc, "the parent must not reach the frame's document").toBe(false);

  // Inside the frame: an opaque origin, no reachable parent DOM, no storage.
  // The origin probe is self.origin — the WINDOW's principal — not
  // location.origin: on a sandboxed src document, location keeps reporting
  // the URL's origin (both engines) while the document is genuinely opaque.
  const inside = await frame.evaluate(() => {
    const out = { origin: self.origin } as Record<string, string>;
    try {
      out.parentDocument = parent.document === undefined ? "undefined" : typeof parent.document;
    } catch (e) {
      out.parentDocument = `threw ${(e as Error).name}`;
    }
    try {
      void localStorage.getItem("af-token");
      out.localStorage = "reachable";
    } catch {
      out.localStorage = "threw";
    }
    return out;
  });
  expect(inside.origin, "the render frame must run in an opaque origin").toBe("null");
  expect(inside.parentDocument, "the frame must not read the parent's DOM").not.toBe("object");
  expect(inside.localStorage, "sandboxed storage must refuse access").toBe("threw");

  // A /v1 call from inside the frame can only fail: connect-src 'none' denies
  // it before the wire, and at the wire it would be a cross-site request with
  // Origin: null and no credentials that the daemon refuses anyway.
  const call = await frame.evaluate(async (api) => {
    try {
      const r = await fetch(`${api}/v1/health`);
      return `status ${r.status}`;
    } catch (e) {
      return `refused (${(e as Error).name})`;
    }
  }, new URL(page.url()).origin);
  expect(call, "a /v1 fetch from inside the frame must never succeed").toContain("refused");

  // The frame fetches NOTHING on its own — no lazy chunk, no font, nothing.
  // That is the proxy-compatibility contract: nothing ever arrives at an
  // authenticating reverse proxy from the opaque origin asking a question it
  // has no credentials to answer, so a deployment that renders behind SSO
  // keeps rendering.
  expect(frameSubresource, "the render frame must issue zero subresource fetches").toEqual([]);
  expect(nullOrigin, "no request may carry the opaque origin's Origin: null").toEqual([]);

  // If the frame's document navigates itself, the parent must drop the whole
  // frame — whatever lives in it next is not the renderer, and it must never
  // see a diagram source or have a reply painted. Force the navigation from
  // the parent (the element handle can: src is not a privileged write) and
  // the next render must come up through a FRESH frame instead.
  await page.evaluate(() => {
    const el = [...document.querySelectorAll("iframe")].find((f) =>
      f.src.endsWith("/lazy/mermaid-frame.html")) as HTMLIFrameElement | undefined;
    if (el) el.src = "about:blank";
  });
  await expect
    .poll(async () => page.evaluate(() =>
      [...document.querySelectorAll("iframe")].some((f) =>
        f.src.endsWith("/lazy/mermaid-frame.html") || f.src === "about:blank")),
      { timeout: 5_000 })
    .toBe(false);
  await expect(pane.locator(".cm-md-mermaid svg"), "the last render stays painted — its svg is already sanitized DOM").toBeVisible();

  // And the NEXT render must come up through a FRESH frame: a theme flip
  // re-renders every mounted diagram, so this doubles as the rebuild check —
  // the replacement window the drop refused is not the one that answers.
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(pane.locator(".cm-md-mermaid")).toHaveAttribute("data-mmd-theme", "dark", { timeout: 30_000 });
  const fresh = page.frames().find((f) => f.url().endsWith("/lazy/mermaid-frame.html"));
  expect(fresh, "a fresh render frame must open for the next render").toBeTruthy();
  expect(await fresh?.evaluate(() => self.origin), "the fresh frame is opaque too").toBe("null");

  // The fresh frame's navigation revalidated: the doc is ~3.4 MB inline, so
  // a re-open's wire cost is a 304's body (none), not the library again. A
  // browser that answered from cache without even revalidating would emit no
  // second request — that is the same property satisfied harder, not a miss.
  const docBytes = await Promise.all(frameDocBytes);
  expect(docBytes.length, "at most the first open and the re-open fetch the document").toBeLessThanOrEqual(2);
  // ~0.95 MB on the wire after #6943 — the whole document, not a sliver of
  // it. Whatever the size drifts to, an open fetches it all while a
  // revalidation fetches nothing.
  expect(docBytes[0], "the first open fetches the whole library once").toBeGreaterThan(500_000);
  if (docBytes.length === 2) {
    expect(docBytes[1], "the re-open revalidates — a 304 body, not the library again").toBeLessThan(100_000);
  }
});

// The frame document is served UNAUTHENTICATED like the rest of the shell,
// and af's own iframe sandbox attribute only protects the loads af itself
// initiates: a foreign origin could frame it bare (no attribute) and a user
// could open it top-level — either way the document would execute at the
// daemon's real origin, where af.token lives. The CSP answers both: its
// `sandbox` directive gives the document an opaque origin however it loads,
// and `frame-ancestors 'self'` refuses foreign framing outright, so the doc
// never runs outside the origin that owns it (#6669 re-review, item 1).
test("docs mermaid: the frame document is inert outside af's own origin — foreign framing and top-level", async ({ page }) => {
  const afOrigin = new URL(process.env.AF_WEB_BASE_URL as string).origin;
  const docURL = `${afOrigin}/lazy/mermaid-frame.html`;

  // A foreign origin — exactly what an agent's dev server on another port
  // is — frames the document UNSANDBOXED (no attribute at all) and listens
  // for its ready announce, which posts to "*": if the document executed,
  // the announce cannot help arriving.
  const foreign = await new Promise<Server>((resolve) => {
    const s = createServer((_req, res) => {
      res.setHeader("content-type", "text/html");
      res.end(
        `<!doctype html><title>foreign</title>` +
          `<script>window.__heard = []; addEventListener("message", (e) => __heard.push(String(e.data)));</script>` +
          `<iframe src="${docURL}"></iframe>`,
      );
    });
    s.listen(0, "127.0.0.1", () => resolve(s));
  });
  try {
    const addr = foreign.address();
    if (typeof addr !== "object" || !addr) throw new Error("no port");
    await page.goto(`http://127.0.0.1:${addr.port}/`);
    // frame-ancestors 'self' refuses the load outright: no document means no
    // announce — and the sandbox directive is the backstop even if a load
    // ever slipped through. Five seconds is several times any real load.
    await page.waitForTimeout(5_000);
    const heard = await page.evaluate(() => (window as unknown as { __heard: string[] }).__heard);
    expect(heard.filter((m) => m.includes("af-mmd")),
      "a foreign frame must never see the render document execute").toEqual([]);
  } finally {
    foreign.close();
  }

  // Top-level: the document loads and its module runs — but its own CSP
  // sandbox makes it an opaque origin anyway, so af.token is unreachable,
  // and with no real parent the ready announce goes nowhere and nothing
  // renders.
  await page.goto(docURL);
  const top = await page.evaluate(() => {
    const out = { origin: self.origin, topLevel: self === window.parent } as Record<string, unknown>;
    try {
      void localStorage.getItem("af-token");
      out.localStorage = "reachable";
    } catch {
      out.localStorage = "threw";
    }
    return out;
  });
  expect(top.topLevel, "sanity: this really is a top-level load").toBe(true);
  expect(top.origin, "a top-level load must still be an opaque origin — the CSP sandbox covers it").toBe("null");
  expect(top.localStorage, "an opaque origin must not reach af's storage").toBe("threw");
  await expect(page.locator("svg"), "no parent handshake means nothing renders").toHaveCount(0);
});

// #6943 shrank the frame document by about a third — ELK and KaTeX are out.
// Both were reachable before: a flowchart-elk fence or an %%{init}%%
// directive could select the elk layout engine, and a $$…$$ sequence label
// went through KaTeX. The build now resolves the elk loader's chunk import
// to dagre's, so every diagram that could ask for elk still renders — with
// dagre — and KaTeX is a stub that returns the TeX source, so a $$ label
// shows its source instead of typeset math. Every diagram still renders.
test("docs mermaid: a fence that asks for elk renders with dagre; $$ labels still render", async ({ page, request }) => {
  const pane = await openDocs(page, request, "mermaid-shrunk-probe", "mermaid-shrunk-plan", {
    "a-elk-type": `# ELK by type\n\n${fence("flowchart-elk LR\n  A --> B --> C")}\n`,
    "b-elk-init": `# ELK by directive\n\n${fence('%%{init: {"layout": "elk"}}%%\nflowchart LR\n  A --> B --> C')}\n`,
    "c-math": `# Math\n\n${fence("flowchart LR\n  A[\"$$x^2$$\"] --> B")}\n\n${fence("sequenceDiagram\n  A->>B: $$x^2$$")}\n`,
  });
  // elk-by-type: the fence's own words select the elk layout engine. The
  // engine is not in the bundle — the loader resolves to dagre and the
  // diagram renders anyway.
  await expect(pane.locator(".cm-md-mermaid svg"), "a flowchart-elk fence must still render").toBeVisible({ timeout: 30_000 });
  // elk-by-directive: the %%{init}%% key is not secure-listed, so it also
  // selects elk — same dagre substitution.
  await pane.locator(".af-docs-tab", { hasText: "b-elk-init" }).click();
  await expect(pane.locator(".cm-md-mermaid svg"), "an init directive asking for elk must still render").toBeVisible({ timeout: 30_000 });
  // $$: the stub answers renderToString with the TeX source, so the label
  // pipeline resolves and the diagram renders — a sequence $$ message's
  // foreignObject is stripped by the parent sanitizer as it always was
  // (KaTeX's markup lived in one too), and a flowchart $$ label is SVG
  // <text> that shows the source literally, exactly as before.
  await pane.locator(".af-docs-tab", { hasText: "c-math" }).click();
  const diagrams = pane.locator(".cm-md-mermaid svg");
  await expect(diagrams, "both math-carrying diagrams must render").toHaveCount(2, { timeout: 30_000 });
  await expect(diagrams.first(), "the flowchart $$ label renders its source as SVG text").toContainText("x^2");
  for (const i of [0, 1]) {
    expect(await diagrams.nth(i).innerHTML(),
      "typeset-KaTeX markup must not appear — the package is stubbed out").not.toContain("katex-html");
  }
});
