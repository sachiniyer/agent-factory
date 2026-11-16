// A Docs tab click whose press straddles a doc-list refresh (#7409). The pane
// refreshes its doc list after every proposal.changed event — an agent's edit,
// a disk write the daemon's scan picks up, a doc added elsewhere — and that
// refresh used to rebuild every tab button. A press that went down on the old
// button and came up on its replacement was not a click on either, so the doc
// never opened. The docs-mermaid spec lost its "b-diagram" click to exactly this
// on WebKit, whose synthesized press is wide enough to straddle the refresh
// that the spec's own doc writes provoke.
//
// Runs under both the chromium and webkit projects (see playwright.config.ts),
// so names carry the project as a prefix, like docs-mermaid.spec.ts.
import { execFileSync } from "node:child_process";
import { expect, test } from "@playwright/test";

test("docs tab: a click whose press straddles a doc-list refresh still opens the doc", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const run = test.info().project.name;
  const title = `${run}-docs-straddle-probe`;
  const slug = `${run}-docs-straddle-plan`;
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  const started = JSON.parse(af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title,
    "--docs", "a-first,b-second")) as { tab_error?: string };
  expect(started.tab_error ?? "", "start must create the Docs tab").toBe("");

  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();
  await page.goto(`/#/session/${session.id}`);
  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();
  const tabs = pane.locator(".af-docs-tab");
  await expect(tabs).toHaveText(["a-first", "b-second"]);
  const second = pane.locator(".af-docs-tab", { hasText: "b-second" });
  await expect(pane.locator('.af-docs-tab[aria-selected="true"]')).toHaveText("a-first");

  // Press on b-second and hold it while another doc is added: the daemon
  // publishes proposal.changed, and the pane's list refresh re-renders the
  // strip under the held button. The new tab appearing is the observable proof
  // that the refresh landed before the release.
  const box = await second.boundingBox();
  expect(box, "the tab must have a box to press").toBeTruthy();
  await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
  await page.mouse.down();
  const added = await request.post("/v1/AddProposalDoc", {
    data: { session_id: session.id, slug, name: "c-third", author: "human" },
  });
  expect(added.ok(), `AddProposalDoc: ${added.status()} ${await added.text()}`).toBe(true);
  await expect(tabs, "the list refresh lands while the button is held").toHaveText(["a-first", "b-second", "c-third"]);
  await page.mouse.up();

  await expect(second, "the press and the release are one click on b-second").toHaveAttribute("aria-selected", "true");
  await expect(pane.locator('.af-docs-tab[aria-selected="true"]')).toHaveText("b-second");
});
