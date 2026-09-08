Visual polish audit — #4015 / Group A #4017

Baseline: e4e67cb294b70e215067e7410c4dbf3035f08b02, fetched origin/master before branching.

[inventory.csv](inventory.csv) records every source literal and Unicode character length, including technical strings so no short label is filtered away. Template lengths count placeholders as authored; daemon-provided names, errors and user text are unbounded and are not rewritten. config/manifest.go is included as the source of server-provided settings descriptions; its data is unchanged.

[buttons.md](buttons.md) inventories controls before editing. The PR body records final changed presentation strings; other inventory entries are retained. Empty-state/hint copy lives in recovery/workspace/overlay files, confirmation copy in lifecycle/modal handlers, headers in component/tab/pane renderers, and help/settings in help/config/account/task files. These categories overlap within files; the exhaustive inventory preserves source locations rather than guessing which runtime state a literal reaches.

Button comparison: pre-#3972 dark borders were #a1aaba against #434c5e; master changed them to #3d3d3d against #2b2b2b. The shared action recipe now uses the existing muted-ink role for edges, balanced horizontal padding, medium label weight, accent hover and a visible keyboard outline. The 4px radius and 44px targets remain. Keybar controls reserve their intrinsic label width so More keys fits without a taller row.

One-time account explanation: accounts are agent identities, not configuration keys. af runs the agent’s own login in that account’s directory and does not read, store or forward its credential. Follow the URL/device-code instructions in the login pane. The empty account choice inherits configured defaults; it is not an explicit ambient override. Existing option values and request omission rules remain unchanged.

Preservation: local archive retains owned worktrees and refs. Sandbox archive publishes work before removal. Ordinary sandbox restore preserves/pushes work before replacement and refuses uncertain preservation. Session deletion permanently removes its record, af-owned worktree and af-created branch. Uncommitted changes and unpushed commits in them are lost; archive instead to keep them. External checkouts and pre-existing branches follow existing ownership rules. CLI/wire names and force behavior are unchanged.

All app/daemon execution and captures use containers. No dev-install, host daemon, reset, or AF-home mutation is part of this work.

The PR body records the review of every regenerated golden. Before images for existing goldens are linked directly to the base commit; after images are the committed expectations. Only the extra 360/390/430 pixel phone captures are retained in [before/](before/) and [after-phone/](after-phone/), in both themes.

Blank cells in the PR copy table mean a removed or newly displayed string. Lengths count authored Unicode text and placeholders. Use configured default appends the resolved account when known; the collapsed summary uses the existing “name (default)” form. promptModal had no entry point and is removed.

Capture commands (both before and after, using the corresponding checkout):

```sh
AF_UPDATE_GOLDENS=1 AF_TESTBOX_CACHE_MAX=off scripts/testbox.sh perf
AF_PLAYWRIGHT_ARGS='recovery.spec.ts --update-snapshots' make web-selftest-container
```

The harness writes artifacts under web/test-results/<run-id>. Demo goldens and additional phone captures came from the perf run; recovery element crops came from the recovery run. After reading the images, the resulting PNGs were copied into web/selftest/goldens and checked by the strict container selftest.

Before/after examples (before links use the base-commit goldens):

| Surface | Before | After |
| --- | --- | --- |
| Deletion | [Before](https://raw.githubusercontent.com/sachiniyer/agent-factory/e4e67cb294b70e215067e7410c4dbf3035f08b02/web/selftest/goldens/kill-confirmation-dark.png) | [After](../../selftest/goldens/kill-confirmation-dark.png) |
| Phone creation | [Before](https://raw.githubusercontent.com/sachiniyer/agent-factory/e4e67cb294b70e215067e7410c4dbf3035f08b02/web/selftest/goldens/phone-create.png) | [After](../../selftest/goldens/phone-create.png) |
| Accounts | [Before](https://raw.githubusercontent.com/sachiniyer/agent-factory/e4e67cb294b70e215067e7410c4dbf3035f08b02/web/selftest/goldens/config-accounts-dark.png) | [After](../../selftest/goldens/config-accounts-dark.png) |

Review correction: deletion explicitly warns about losing uncommitted changes and unpushed commits and offers Archive. Zero-live project removal names retained archives/tasks and when the project remains in the switcher. Read the four updated goldens: both kill-confirmation dialogs grow to fit the warning; both kill-failed error crops grow from 126 to 127 pixels due to centering, with error text unchanged. Unrelated demo capture drift was not rebaselined.


Accuracy-round visual review: recaptured the container demo and read the account restriction states. The inherited registration-only choice keeps Create disabled with the same explanation; the logged-out choice keeps its credential notice; the logged-in choice allows Create. The no-default label is “Use agent login (no default)” so it fits the phone field. Add project names the absolute-path requirement. Updated goldens are limited to new-session, create-compact, create-defaults, phone-create and add-project in both themes; unrelated capture drift is retained locally, not rebaselined.


Project/keybar review round: project removal now derives its regular/in-place breakdown from the selected root's non-archived store sessions and the daemon-projected `external_worktree` flag. Regular sessions retain the archive sentence; in-place sessions are explicitly ended permanently and cannot be restored. The project-delete tooltip no longer promises universal archival. Keybar buttons can shrink to their existing 44px minimum, and the design harness now covers a focused session at 320px in both themes. The user guide, style guide and original keybar evidence README say “More keys”.

| Before | After |
| --- | --- |
| Archive N sessions and remove the project. Keep the repo; restore sessions anytime. (including in-place sessions) | N in-place sessions are ended permanently and cannot be restored. Their checkouts and branches are kept. Archive M regular sessions; restore those sessions anytime. Remove the project; the repo stays. (regular-session sentence only when M > 0; existing archive-only copy when N = 0) |
| Delete project NAME (archives its sessions, restorable) | Delete project NAME (review session consequences) |
| Delete project NAME (removes the empty project) | Delete project NAME (review session consequences) |
| Back (keybar documentation, three locations) | More keys |

Golden review: added only `phone-session-320.png` and `phone-session-320-dark.png`. Both show the focused session with all six keybar labels fully visible, 44px targets, a 48px header and a 44px keybar. Read all 24 differing captures. The four existing keyboard/modifier stills differ only by 1–3 pixels at the top-left header corner, not in the keybar; those and other unrelated capture drift are not rebaselined.
