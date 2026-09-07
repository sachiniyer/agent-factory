import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

// Count authored Unicode characters, not UTF-8 bytes or the dynamic title.
test("lifecycle confirmation copy fits a 160-character reading budget", () => {
  const source = readFileSync(new URL("./modals.ts", import.meta.url), "utf8");
  const lifecycle = source.slice(source.indexOf("const copy = {"), source.indexOf("}[opts.action]"));
  const bodies = [...lifecycle.matchAll(/body: "([^"]+)"/g)].map(match => match[1]);
  assert.equal(bodies.length, 3);
  for (const body of bodies) assert.ok([...body].length <= 160, `${[...body].length} characters: ${body}`);
  assert.match(bodies[0], /permanent|undo/i);
  assert.match(bodies[0], /Uncommitted changes and unpushed commits.*lost/);
  assert.match(bodies[0], /Archive.*keep/);
  assert.doesNotMatch(bodies[0], /User-owned work stays/);
  assert.doesNotMatch(bodies[0], /prune/);
  assert.match(bodies[1], /publish/);
  assert.match(bodies[2], /push.*replacement.*refuses/);
  assert.match(bodies[1], /restor/i);
});

test("zero live sessions does not imply an empty project", () => {
  const source = readFileSync(new URL("./modals.ts", import.meta.url), "utf8");
  assert.match(source, /No live sessions to archive/);
  assert.doesNotMatch(source, /Remove the empty project/);
});
