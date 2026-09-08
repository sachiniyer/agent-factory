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


Ownership and button-contract review: deletion now reads the session's `external_worktree` flag. External sessions delete their record/runtime while keeping the checkout and branch; they are not offered Archive. Af-owned sessions keep the loss warning. The design template and generated guide now say “Hide pane”. Shared resting action outlines use `--af-border` and labels use weight 600, with the accent hover retained; the token contract is unchanged.

| Before | After |
| --- | --- |
| Permanently deletes the session, its af-owned worktree and af-created branch. Uncommitted changes and unpushed commits are lost. Archive to keep them. (external sessions too) | Permanently deletes the session record and runtime. Your checkout and branch stay. (external sessions only; af-owned copy unchanged) |
| Close pane (style-guide template, generated guide, original phone evidence README) | Hide pane |

The constructor/string inventories above record the original base, not the current implementation. The shared button correction is in CSS, not individual call sites. The browser regression checks the prescribed resting border token and weight in both themes and across desktop/phone widths; the earlier blanket 3:1 dark-outline assertion was stricter than the normative contract's 1.5:1 dark-outline requirement and is replaced by the token check.

Read every regenerated golden listed below (90 design stills and 18 recovery stills). The lighter dark-mode borders are the specified border token; heavier labels retain action emphasis. The six existing 360/390/430px after-phone captures were also read and refreshed with the same styling; before captures are retained.

| Golden | Reviewed change |
| --- | --- |
| account-error-dark.png | Settings/account actions use border-token edges and 600 labels; notices and fields remain intact; scrolled views keep actions reachable. |
| account-error.png | Settings/account actions use border-token edges and 600 labels; notices and fields remain intact; scrolled views keep actions reachable. |
| add-account-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| add-account.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| add-project-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| add-project.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| agent-tab-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| agent-tab.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| assistant-error-dark.png | Settings/account actions use border-token edges and 600 labels; notices and fields remain intact; scrolled views keep actions reachable. |
| assistant-error.png | Settings/account actions use border-token edges and 600 labels; notices and fields remain intact; scrolled views keep actions reachable. |
| comparison-review-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| comparison-review.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| config-accounts-dark.png | Settings/account actions use border-token edges and 600 labels; notices and fields remain intact; scrolled views keep actions reachable. |
| config-accounts.png | Settings/account actions use border-token edges and 600 labels; notices and fields remain intact; scrolled views keep actions reachable. |
| config-dirty-dark.png | Settings/account actions use border-token edges and 600 labels; notices and fields remain intact; scrolled views keep actions reachable. |
| config-dirty.png | Settings/account actions use border-token edges and 600 labels; notices and fields remain intact; scrolled views keep actions reachable. |
| create-compact-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| create-compact.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| create-defaults-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| create-defaults.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| dashboard-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| dashboard.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| edit-task-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| edit-task.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| event-intake-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| event-intake.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| kill-confirmation-dark.png | Cancel/Delete session buttons use border-token edges and 600 labels; this af-owned fixture retains the work-loss/Archive warning. |
| kill-confirmation.png | Cancel/Delete session buttons use border-token edges and 600 labels; this af-owned fixture retains the work-loss/Archive warning. |
| login-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| login-expired-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| login-expired-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| login.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| new-session-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| new-session.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| no-accounts-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-accounts-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-daemon-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-daemon-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-project-registered-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-project-registered-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-sessions-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-sessions-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-tasks-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| no-tasks-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| notice-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| notice-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| parallel-work-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| parallel-work.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| phone-add-account-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-add-account.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-config-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-config.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-controls-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-controls.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-create-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-create.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-drawer-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-drawer.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-filter-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-filter.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-project-menu-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-project-menu.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-session-320-dark.png | Header/keybar buttons use border-token edges and 600 labels; all six controls fit at 320px. |
| phone-session-320.png | Header/keybar buttons use border-token edges and 600 labels; all six controls fit at 320px. |
| phone-session-actions-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-session-actions.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-session-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-session-first-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-session-first.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-session.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-tab-types-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-tab-types.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-tasks-dark.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-tasks.png | Phone navigation/actions use border-token edges and 600 labels; controls and menus remain inside the viewport. |
| phone-terminal-keyboard-dark.png | Header/keybar buttons use border-token edges and 600 labels; locked Ctrl retains its accent marker and outline where shown. |
| phone-terminal-keyboard.png | Header/keybar buttons use border-token edges and 600 labels; locked Ctrl retains its accent marker and outline where shown. |
| phone-terminal-modifier-locked-dark.png | Header/keybar buttons use border-token edges and 600 labels; locked Ctrl retains its accent marker and outline where shown. |
| phone-terminal-modifier-locked.png | Header/keybar buttons use border-token edges and 600 labels; locked Ctrl retains its accent marker and outline where shown. |
| project-menu-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| project-menu.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| remove-task-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| remove-task.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| review-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| review.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| scheduled-triage-dark.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| scheduled-triage.png | Form/dialog action buttons use border-token edges and 600 labels; authored copy, values and scrollable-field affordances remain intact. |
| session-actions-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| session-actions.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| session-filter-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| session-filter.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| session-lifecycle-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| session-lifecycle.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| sign-in-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| sign-in-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| split-panes-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| split-panes.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| tab-types-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| tab-types.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| task-actions-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| task-actions.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| tasks-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| tasks.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| terminal-keyboard-dark.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| terminal-keyboard.png | Appbar, pane, rail or task actions use border-token edges and 600 labels; content and selected-state affordances remain intact. |
| tokenless-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| tokenless-light.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| unavailable-dark.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |
| unavailable.png | Recovery/Connect action uses the border token and 600 label; condition and next-action copy remain readable. |

### Merge of #4026: destructive confirmations

Merged master `1281215a604e1845e8544b2a848fa6a56eaf1933` and rebuilt dist with
`npm ci --prefer-offline && npm run build`. The deletion action now keeps
master's `af-danger` treatment. Shared neutral border, hover and disabled-color
rules exclude danger buttons; shared spacing and the accent focus ring remain.
The ownership-dependent deletion disclosure and “Delete session” label are kept.

Read the container captures individually:

| Capture | Review |
| --- | --- |
| `kill-confirmation.png` | Delete session changes from a filled primary action to a danger outline/text; Cancel remains neutral; the full loss warning fits. |
| `kill-confirmation-dark.png` | Same change in the dark palette; the warning and distinct action outline remain readable. |
| `kill-failed-light.png` | Recaptured recovery crop matches the existing golden; the refused-operation message and retry instruction are unchanged. |
| `kill-failed-dark.png` | Recaptured recovery crop matches the existing golden in the dark palette. |
| `docs/assets/4017/item18/after-light.png` | Refreshed full confirmation evidence retains the ownership warning, danger action, and visible accent keyboard-focus ring. |
| `docs/assets/4017/item18/after-dark.png` | Refreshed dark confirmation evidence retains the same warning and distinct focus ring. |
| `docs/assets/recovery/web-kill-failed-light.png` | Refreshed full recovery evidence shows the loss warning, retained failure details, and danger retry action together. |
| `docs/assets/recovery/web-kill-failed-dark.png` | Refreshed dark recovery evidence shows the same complete disclosure and retry action. |

The 320px light/dark keybars and phone keyboard/create captures were also read;
the Arrows control fits within the viewport. Unrelated fixture capture drift was
not copied into the committed goldens.
### Round four: ownership and unknown account policy

Deletion copy now follows both daemon ownership flags. A missing legacy
`branch_created_by_us` flag preserves the branch, matching `session/instance_data.go`.
The external-checkout and af-created-branch variants remain unchanged.

The form now disables Create while ListAccounts is pending, including after a
project switch. If the request fails, Create remains available with an explicit
notice that the daemon default, if any, applies. The form claims no default only when a loaded registry
reports none for the resolved agent. These are form-state changes;
no persisted data or request shape changes.

| Surface/state | Before | After |
| --- | --- | --- |
| Delete session · af-owned worktree, preserved branch | Permanently deletes the session, its af-owned worktree and af-created branch. Uncommitted changes and unpushed commits are lost. Archive to keep them. | Permanently deletes the session and its worktree. Your branch and its commits stay. Uncommitted changes are lost. Archive to keep them. |
| Account · pending | Use agent login (no default) | Loading accounts… |
| Account · pending hint | — | Wait for the account policy to load. |
| Account · failed | Use agent login (no default) | Accounts unavailable |
| Account · failed hint | — | Accounts could not be loaded. The daemon default, if any, applies. |
| Account · program unresolved | Use agent login (no default) | Use daemon default |
| Account · program unresolved hint | — | The daemon default, if any, applies. |

Regression evidence: the account unit tests went red with actual
`Use agent login (no default)` versus expected `Loading accounts…` and
`Accounts unavailable`; the container ownership tests went red for both false
and missing branch-ownership flags, expecting `branch and its commits stay`
but receiving the branch-loss warning. The initial account browser reproduction
also had a wrong field selector (`Title` versus `Session title`), corrected before
the full run; those selector timeouts are not product-failure evidence.

Validation: full unfiltered container web selftest **217 passed (6.6m)**,
unit tests **750 passed**, typecheck/build passed, design suite **5 passed**, and
the three-run 1,000-session performance budget passed. Container runs were serial
and observed load1 < 110 and fewer than four running containers before starting.

Read the recaptured `kill-confirmation.png` and `kill-confirmation-dark.png`
individually: both exactly match the committed goldens because their fixture has
an af-created branch; the danger action, neutral Cancel, full loss disclosure,
and wrapping remain intact. Read `create-compact.png` and
`create-compact-dark.png`: loaded account choices and the primary action remain
visible without a stale loading/failure message. No golden was re-baselined for
unrelated edge-pixel capture differences. The four ownership browser cases cover
external, preserved, created, and missing legacy branch flags.

### Round five: sandbox deletion and #4047 merge

Deletion selects workspace kind before local ownership. `isOffBoxWorkspace`
shares the existing `OFF_BOX_BACKENDS` set with the tab-capability fallback; no
backend list is duplicated and no daemon field is added. The local external,
af-created-branch and preserved-branch variants stay unchanged.

| Surface | Before | Length | After | Length |
| --- | --- | ---: | --- | ---: |
| Delete session · off-box workspace | Permanently deletes the session and its worktree. Your branch and its commits stay. Uncommitted changes are lost. Archive to keep them. | 135 | Permanently removes the sandbox. Unpushed commits and uncommitted changes are lost. Archive publishes the branch first. | 119 |

Merged master `bb6d6179c0e9d38d187ffbd4f9143e5be6ad4a52` and kept its
#4047 rail prefix ordering and tests. The only conflict was `web/dist/sw.js`: took
master temporarily, then rebuilt all dist assets with `npm ci --prefer-offline`
and `npm run build`. Source changes reapplied cleanly.

Red evidence: the pure Docker copy-selection test expected `removes the sandbox`
but received `Your branch and its commits stay`. All four off-box backend tests
failed before the fix; the local-ownership test passed. The fixed unit coverage
also checks that off-box semantics win even if local ownership flags are present.

Read each new sandbox-confirmation golden individually:

| Golden | Review |
| --- | --- |
| `sandbox-delete-360-light.png` | Full loss warning wraps to three readable lines; Archive guidance fits; Cancel and Delete session remain visible within the 360px viewport. |
| `sandbox-delete-360-dark.png` | Same three-line disclosure in the dark palette; danger action remains distinct from neutral Cancel. |
| `sandbox-delete-1440-light.png` | Full warning and Archive guidance fit on two lines in the desktop modal; both actions are visible. |
| `sandbox-delete-1440-dark.png` | Same two-line desktop layout, with readable warning and distinct action outlines. |

The browser fixture retains only `worktree.repo_path` for the existing rail's
project routing and omits both ownership flags; the initial attempt that removed
that routing key entirely exposed the rail's existing scoping limitation and was
corrected without changing production routing. The unit cases select copy for all
four off-box backend types, including Docker, independently of local flags.

Final validation: **776 unit tests passed**; typecheck/build passed after
`npm ci --prefer-offline`; the full unfiltered container web selftest passed
**219 tests (6.5m)** with snapshot updates off. The first full capture run had
217 passes and two new-baseline creation failures; all four newly captured
images were read before the strict retry. The strict design suite passed all
five tests without regenerating existing goldens, and the three-run 1,000-session
performance budget passed. Gofmt/build/vet/fast lint/file-length checks and strict
MkDocs also passed. All container starts observed the shared-box load gate and
suites ran one at a time.

## Round six · account reloads, archived deletion and copy audit

Merged `origin/master` at `3181a936b20c34e58849ff8c5d41478acbbc3cb4`, including #4030. Resolved `web/src/ui.ts` with master’s named Delete tab tooltip and confirmation flow, keeping the polish. Took master’s `web/dist/af-web.js` and `web/dist/sw.js` provisionally, then rebuilt both from source. This revision preserves a deliberate account choice independently of temporary loading rows. Both account and program catalogs must settle before checking whether that identity is still offered. A changed agent or removed identity clears the deliberate choice; an unavailable policy keeps a named choice blocked rather than silently creating under a different identity. A unit test captures the real `createSession` request body, and a browser test switches projects and verifies `CreateSession.account`.

Deletion now receives `isArchived(session)` as well as workspace ownership. Archived sandboxes have already published their branch and lost their runtime; their copy offers Restore. Archived local worktrees still use the ownership flag: archive itself keeps the branch, but subsequent `GitWorktree.cleanup` still calls `branch -D` when `branchCreatedByUs` is true (`session/git/worktree_ops.go:769`). It would be incorrect to promise every archived local branch survives deletion. The two local archived variants retain that distinction. No daemon fields or request shapes changed.

### Copy table additions

| Surface | Before | After |
| --- | --- | --- |
| Archived sandbox deletion | Permanently removes the sandbox. Unpushed commits and uncommitted changes are lost. Archive publishes the branch first. | Permanently deletes the session record. Its branch stays published from the archive. Restore instead to use the session again. |
| Archived local, af-created branch | Permanently deletes the session, its af-owned worktree and af-created branch. Uncommitted changes and unpushed commits are lost. Archive to keep them. | Permanently deletes the session, archived worktree and af-created branch. Uncommitted changes and unpushed commits are lost. Restore instead to keep the session. |
| Archived local, pre-existing branch | Permanently deletes the session and its worktree. Your branch and its commits stay. Uncommitted changes are lost. Archive to keep them. | Permanently deletes the session and archived worktree. Your branch and its commits stay. Uncommitted changes are lost. Restore instead to keep the session. |
| Unverifiable explicit account | Accounts could not be loaded. The daemon default, if any, applies. | Cannot verify the selected account. Reopen this form to try again. |
| Session-action guide labels | Kill | Delete session |
| Web selftest guide | The kill confirm removes the session's row. | The Delete session confirmation removes the session's row. |
| Usage-limit guide labels | Retry | Retry limit |
| Web account guide | Ambient identity | Use configured default (…), Use agent login (no default), or Use daemon default; these rows send no override |
| Recovery caption and alt text | Kill failed | Delete session failed |
| TUI manual-test wait | Ambient identity | Use the agent's own login |
| Generated skill: deletion guidance | Delete a session and only af-owned worktrees and branches; user-owned resources stay | Delete a session; work in af-owned workspaces can be lost |
| Generated skill: agent tab guidance | kill the session instead | delete the session instead |
| Generated skill: cleanup consequence | deletes only af-owned worktrees and branches; user-owned resources stay | permanently removes af-owned workspaces; uncommitted changes and unpushed commits there can be lost |

### COPY RULE audit hits

Searched `docs/`, `session/systemprompt.go` (`afUsageBody`), generated `plugins/**/SKILL.md`, and `scripts/tui-driver*.sh` for the renamed labels, including Kill/Kill session, Close pane, Ambient identity, usage-limit Retry, keybar Back, empty project, destroys the session, VS Code and the idle/watch wording.

- Fixed `docs/web.md:121,154,161,171`, `docs/usage-limits.md:310`, `docs/concepts.md:23`, `docs/sessions.md:49`, `docs/dev/web-selftest.md:103`, `docs/design/recovery-stills.md:19`, and `docs/dev/tui-manual-testing.md:402`.
- Corrected `session/systemprompt.go:95,106,128`; regenerated Claude, Codex, Amp and Gemini skills. The generator requires an append-only content digest: added release 3.13 and committed generated plugin/marketplace versions. Reviewed the generated diffs: only these three guidance replacements and version metadata changed. `cli.md`, `api.md` and the generated design guide did not drift.
- Retained `docs/web.md:236` Keep/Archive/Kill: these are the separate task On done catalog choices. Retained `docs/web.md:391` Retry: this is the dev-server connection retry. CLI `kill`, `KillSession` routes, process-killing prose and CLI deprecation text remain command/mechanism names. Historical `docs/assets/design/tui-{a,b,c}` captures and recorded source excerpts remain historical evidence, not current instructions.
- `scripts/tui-driver.sh:560` “Kill ONLY our own named session” and `scripts/tui-driver-selftest.sh:1427` “Back in the TUI” are narrative safety/navigation comments, not stale label assertions. No renamed-label assertion remained in these drivers. VS Code in the skill describes the CLI-created editor, not the renamed TUI picker row. The current generated design source/guide already say Hide pane and More keys.

Visual review: no existing golden was regenerated. Re-read all four live-sandbox goldens (360/1440, light/dark): their loss warning and button bounds remain unchanged. Read all four new archived screenshots from the container report: sandbox copy uses three lines at 360 and two at 1440; local preserved-branch copy uses four and three lines respectively. Cancel and Delete session remain fully visible with clear spacing. These captures are test-report artifacts; no duplicate PNG evidence was added to git.
