import { execFileSync } from "node:child_process";
import { test, expect, type APIRequestContext, type Page } from "@playwright/test";

// A session whose agent can never produce pane output (#6465, #4217). The shared
// probe-a/probe-b sessions run `cat`, which echoes whatever the browser terminal
// sends back while a pane attaches; that output is a Running edge, and a Running
// edge after the agent went idle is exactly what clears an attention request
// (session/attention.go). Under load it cleared the request between the spec
// raising it and the spec interacting with it, so the Dismiss click found nothing
// to click. Every spec that raises an attention request and then interacts with it
// uses one of these, so nothing but the spec's own action can clear it.
//
// The name must contain "probe-quiet": the fake agent in web-selftest-entry.sh keys
// off the session's directory and turns echo off and sleeps for those. Give each
// spec file its own name -- sessions are created in beforeAll and killed in afterAll.
//
// perTest: a fresh session for every test (beforeEach/afterEach). Needed when tests
// attach at different widths: the first attach sizes the pane, and the next test's
// attach at another width reflows the pane's content in tmux, which the daemon reads
// as pane activity and clears the request (measured: the 390px test after the 1280px
// one lost its request within a second, 7 of 8 repeats). A session no test has
// attached to has never been resized.

export const READY_MARKER = process.env.AF_WEB_READY_MARKER ?? "AF_SELFTEST_READY";

export function quietSession(name: string, opts: { perTest?: boolean } = {}) {
  if (!name.includes("probe-quiet")) throw new Error(`quiet session ${name} must contain "probe-quiet"`);

  const create = () => {
    const afBin = process.env.AF_BIN;
    const mockRepo = process.env.AF_MOCK_REPO;
    test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
    execFileSync(afBin as string, ["sessions", "create", "--repo", mockRepo as string, "--name", name, "--program", "claude"], { stdio: "ignore" });
  };
  const kill = () => {
    if (!process.env.AF_BIN) return;
    try {
      execFileSync(process.env.AF_BIN, ["sessions", "kill", name], { stdio: "ignore" });
    } catch {
      // already gone
    }
  };
  if (opts.perTest) {
    test.beforeEach(create);
    test.afterEach(kill);
  } else {
    test.beforeAll(create);
    test.afterAll(kill);
  }

  return {
    name,
    async id(request: APIRequestContext) {
      const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
      const session = snapshot.data.instances.find((s: { title: string }) => s.title === name);
      expect(session?.id, `seeded session ${name}`).toBeTruthy();
      return session.id as string;
    },
    // Attached and showing the agent's one and only output. The quiet agent emits
    // nothing further, so this is a real condition, not a settle wait.
    async open(page: Page, id: string) {
      await page.goto(`/#/session/${encodeURIComponent(id)}`);
      await expect(page.locator(".af-term-title")).toHaveText(name);
      await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);
    },
  };
}
