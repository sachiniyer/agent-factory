// Called only after the demo daemon exits. No API create loop, no new agents.
import { readdirSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import assert from "node:assert/strict";
assert(existsSync("/.dockerenv") || existsSync("/run/.containerenv"), "container only");
const home = process.env.AGENT_FACTORY_HOME;
assert.equal(home, "/work/demo-home/.agent-factory");
const dir = join(home, "instances");
let seeded = false;
for (const repo of readdirSync(dir)) {
  const file = join(dir, repo, "instances.json");
  if (!existsSync(file)) continue;
  const data = JSON.parse(readFileSync(file, "utf8"));
  const rows = Array.isArray(data) ? data : data.instances;
  const source = rows.find(r => r.title === "add-json-export");
  if (!source) continue;
  assert(!seeded, "exactly one demo repo");
  const liveCount = rows.length;
  assert.equal(liveCount, 4, "the demo must seed exactly four live sessions");
  for (let i = liveCount; i < 1000; i++) {
    const title = `perf-${String(i).padStart(4, "0")}`;
    rows.push({
      id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`,
      title, path: source.path, branch: `perf/${title}`, program: "claude",
      status: 5, liveness: 3, startup_state_unknown: true,
      lost_restore_failure: { attempts: 6, error: "Synthetic baseline: no runtime provisioned" },
      created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
      worktree: { repo_path: source.worktree.repo_path, base_commit_sha: source.worktree.base_commit_sha,
        session_name: title, branch_name: `perf/${title}`,
        worktree_path: `/work/absent/${title}`, branch_created_by_us: false },
    });
  }
  assert.equal(rows.length, 1000);
  writeFileSync(file, JSON.stringify(data));
  seeded = true;
}
assert(seeded, "demo storage record exists");
