Visual polish audit — #4015 / Group A #4017

Baseline: e4e67cb294b70e215067e7410c4dbf3035f08b02, fetched origin/master before branching.

[inventory.csv](inventory.csv) records every source literal and Unicode character length, including technical strings so no short label is filtered away. Template lengths count placeholders as authored; daemon-provided names, errors and user text are unbounded and are not rewritten. config/manifest.go is included as the source of server-provided settings descriptions; its data is unchanged.

[buttons.md](buttons.md) inventories controls before editing. [copy.md](copy.md) records final changed presentation strings; other inventory entries are retained. Empty-state/hint copy lives in recovery/workspace/overlay files, confirmation copy in lifecycle/modal handlers, headers in component/tab/pane renderers, and help/settings in help/config/account/task files. These categories overlap within files; the exhaustive inventory preserves source locations rather than guessing which runtime state a literal reaches.

Button comparison: pre-#3972 dark borders were #a1aaba against #434c5e; master changed them to #3d3d3d against #2b2b2b. The shared action recipe now uses the existing muted-ink role for edges, balanced horizontal padding, medium label weight, accent hover and a visible keyboard outline. The 4px radius and 44px targets remain. Keybar controls reserve their intrinsic label width so More keys fits without a taller row.

One-time account explanation: accounts are agent identities, not configuration keys. af runs the agent’s own login in that account’s directory and does not read, store or forward its credential. Follow the URL/device-code instructions in the login pane. The empty account choice inherits configured defaults; it is not an explicit ambient override. Existing option values and request omission rules remain unchanged.

Preservation: local archive retains owned worktrees and refs. Sandbox archive publishes work before removal. Ordinary sandbox restore preserves/pushes work before replacement and refuses uncertain preservation. Session deletion permanently removes its record and af-owned resources; external checkouts and user-owned branches follow existing ownership rules. CLI/wire names and force behavior are unchanged.

All app/daemon execution and captures use containers. No dev-install, host daemon, reset, or AF-home mutation is part of this work.

[goldens.md](goldens.md) records the review of every regenerated golden. [before/](before/) contains fresh master captures; [after images](../../selftest/goldens) are the committed expectations. [after-phone/](after-phone/) covers the extra 360/390/430 pixel widths in both themes.

Blank cells in copy.md mean a removed or newly displayed string. Lengths count authored Unicode text and placeholders. Use configured default appends the resolved account when known; the collapsed summary uses the existing “name (default)” form. promptModal had no entry point and is removed.

Capture commands (both before and after, using the corresponding checkout):

```sh
AF_UPDATE_GOLDENS=1 AF_TESTBOX_CACHE_MAX=off scripts/testbox.sh perf
AF_PLAYWRIGHT_ARGS='recovery.spec.ts --update-snapshots' make web-selftest-container
```

The harness writes artifacts under web/test-results/<run-id>. Demo goldens and additional phone captures came from the perf run; recovery element crops came from the recovery run. After reading the images, the resulting PNGs were copied into web/selftest/goldens and checked by the strict container selftest.
