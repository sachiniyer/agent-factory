import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

function topLevelFunction(source: string, name: string): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const node = ast.statements.find(statement =>
    ts.isFunctionDeclaration(statement) && statement.name?.text === name);
  assert.ok(node, `index.ts must declare ${name}`);
  return node.getText(ast);
}

async function runTrigger(trigger: () => Promise<void>, committed: boolean): Promise<string[]> {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handler = topLevelFunction(source, "doTriggerTask");
  const events: string[] = [];
  const context = {
    triggerTask: trigger,
    isMutationCommittedError: () => committed,
    refreshTasks: () => events.push("refresh"),
    surfaceTabError: () => events.push("surface"),
  };
  const code = ts.transpileModule(`let token = "token";\n${handler}`, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  }).outputText;
  runInNewContext(code, context);
  (context as typeof context & { doTriggerTask(task: unknown): void }).doTriggerTask({ id: "t" });
  await new Promise(resolve => setImmediate(resolve));
  return events;
}

test("a committed Run now refreshes and surfaces the warning (#5762)", async () => {
  const events = await runTrigger(() => Promise.reject(new Error("committed")), true);
  assert.deepEqual(events, ["refresh", "surface"]);
});

test("a refused Run now only surfaces the error", async () => {
  const events = await runTrigger(() => Promise.reject(new Error("refused")), false);
  assert.deepEqual(events, ["surface"]);
});

test("a clean Run now refreshes without a warning", async () => {
  const events = await runTrigger(() => Promise.resolve(), false);
  assert.deepEqual(events, ["refresh"]);
});
