import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

// Count authored Unicode characters, not UTF-8 bytes or the dynamic title.
test("lifecycle confirmation copy fits a 160-character reading budget", () => {
  const source = readFileSync(new URL("./modals.ts", import.meta.url), "utf8");
  const lifecycle = source.slice(source.indexOf("const copy = {"), source.indexOf("}[opts.action]"));
  const bodies = [...lifecycle.matchAll(/body:([\s\S]*?)(?=\n    },)/g)]
    .flatMap(match => [...match[1].matchAll(/"([^"]+)"/g)].map(literal => literal[1]));
  assert.equal(bodies.length, 4);
  for (const body of bodies) assert.ok([...body].length <= 160, `${[...body].length} characters: ${body}`);
  const [external, owned, archive, restore] = bodies;
  assert.match(external, /session record and runtime/);
  assert.match(external, /checkout and branch stay/);
  assert.doesNotMatch(external, /Archive|are lost/);
  assert.match(owned, /permanent|undo/i);
  assert.match(owned, /Uncommitted changes and unpushed commits.*lost/);
  assert.match(owned, /Archive.*keep/);
  assert.doesNotMatch(owned, /User-owned work stays|prune/);
  assert.match(archive, /publish/);
  assert.match(restore, /push.*replacement.*refuses/);
  assert.match(archive, /restor/i);
});

test("zero live sessions does not imply an empty project", () => {
  const source = readFileSync(new URL("./modals.ts", import.meta.url), "utf8");
  assert.match(source, /No live sessions to archive/);
  assert.doesNotMatch(source, /Remove the empty project/);
});
