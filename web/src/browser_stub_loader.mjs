// A node module-resolution hook that stubs the BROWSER-only dependencies a source
// file drags in, so a unit test can import modules that plain node otherwise cannot
// load. Two things break under `node --test` that esbuild resolves happily at bundle
// time, both via terminal.ts:
//
//   - `import "@xterm/xterm/css/xterm.css"` — node throws ERR_UNKNOWN_FILE_EXTENSION
//     on a stylesheet.
//   - `import { Terminal } from "@xterm/xterm"` — the package ships UMD/CJS with no
//     named ESM export, so node throws "does not provide an export named 'Terminal'".
//
// That chain (split.ts → terminal.ts → xterm) is why split.ts had no unit tests at
// all before split_focus.test.ts. The stubs are inert class shells: a test that only
// pins pure logic never constructs them, and one that needs real terminal behavior
// belongs in the Playwright selftest instead, against a real browser.
//
// TEST-ONLY. Not an entry point (build.mjs bundles from index.ts, so esbuild never
// sees this file), and registered per test file — node --test gives each file its own
// process — so it cannot leak into the rest of the suite.

const PACKAGE_STUBS = {
  "@xterm/xterm": "export class Terminal {}",
  "@xterm/addon-fit": "export class FitAddon {}",
};

const STUB_SCHEME = "af-browser-stub:";

export async function resolve(specifier, context, nextResolve) {
  if (Object.hasOwn(PACKAGE_STUBS, specifier)) {
    return { url: STUB_SCHEME + specifier, shortCircuit: true };
  }
  return nextResolve(specifier, context);
}

export async function load(url, context, nextLoad) {
  if (url.startsWith(STUB_SCHEME)) {
    return {
      format: "module",
      shortCircuit: true,
      source: PACKAGE_STUBS[url.slice(STUB_SCHEME.length)],
    };
  }
  if (url.endsWith(".css")) {
    return { format: "module", shortCircuit: true, source: "export default {};" };
  }
  return nextLoad(url, context);
}
