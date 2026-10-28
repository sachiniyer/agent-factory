Visual polish audit — #4015 / Group A #4017

Baseline: e4e67cb294b70e215067e7410c4dbf3035f08b02, fetched origin/master before branching.

[inventory.csv](inventory.csv) records every source literal and Unicode character length, including technical strings so no short label is filtered away. Template lengths count placeholders as authored; daemon-provided names, errors and user text are unbounded and are not rewritten. config/manifest.go is included as the source of server-provided settings descriptions; its data is unchanged.

[buttons.md](buttons.md) inventories controls before editing. [copy.md](copy.md) records final changed presentation strings; other inventory entries are retained. Empty-state/hint copy lives in recovery/workspace/overlay files, confirmation copy in lifecycle/modal handlers, headers in component/tab/pane renderers, and help/settings in help/config/account/task files. These categories overlap within files; the exhaustive inventory preserves source locations rather than guessing which runtime state a literal reaches.

Button comparison: pre-#3954 footer actions used accent; master flattened them into ink and highlighted isolated confirmation key characters. Shared action styles restore accent/bold footer keys and paint complete confirm/cancel labels, with no added cells. Frames, rounded corners and hit-target geometry retain their shared recipes.

One-time account explanation: accounts are agent identities, not configuration keys. af runs the agent’s own login in that account’s directory and does not read, store or forward its credential. Follow the URL/device-code instructions in the login pane. The empty account choice inherits configured defaults; it is not an explicit ambient override. Existing option values and request omission rules remain unchanged.

Preservation: local archive retains owned worktrees and refs. Sandbox archive publishes work before removal. Ordinary sandbox restore preserves/pushes work before replacement and refuses uncertain preservation. Session deletion permanently removes its record, af-owned worktree and af-created branch. Uncommitted changes and unpushed commits in them are lost; archive instead to keep them. External checkouts and pre-existing branches follow existing ownership rules. CLI/wire names and force behavior are unchanged.

All app/daemon execution and captures use containers. No dev-install, host daemon, reset, or AF-home mutation is part of this work.

[goldens.md](goldens.md) records the review of every regenerated still. The longer Delete session footer adds ten cells: the full tab/pane hint set fits at 110 columns, and both limit-recovery hints fit at 71. Below 42 columns the delete hint sheds after the optional actions so Help and Quit remain visible. The binding remains unchanged and general help names it.

Existing sandbox restore confirmations (Enter/open and full-screen entry) now describe push-before-replacement and refusal on uncertainty. The dedicated r binding retains its existing immediate dispatch; adding consent there belongs to the separate consent audit, not this presentation pass.

Blank cells in copy.md mean a removed or newly advertised string. Lengths count authored Unicode text, including template placeholders; configured-default labels append the resolved account when known.
