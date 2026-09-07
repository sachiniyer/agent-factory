| File | Before | Length | After | Length |
| --- | --- | ---: | --- | ---: |
| web/src/modals.ts | This permanently destroys the session and prunes its branch. This can't be undone. | 82 | Permanently delete the session and resources owned by af. User-owned work stays. | 80 |
| web/src/modals.ts | This tears down the session's terminal and moves its worktree to the archive. You can restore it later. | 103 | Local: move the worktree to the archive. Sandboxes: publish work, then remove the sandbox. Restore anytime. | 107 |
| web/src/modals.ts | This moves the session's worktree back next to its repo and re-spawns the agent, returning it to the live rail. | 111 | Restore the worktree and agent. Sandboxes push work before replacement; restore refuses if preservation is uncertain. | 117 |
| web/src/modals.ts | The new agent starts fresh with a summary of the work so far. Same worktree and branch — nothing is discarded. | 110 | Start a new agent with a summary. Keep the worktree and branch. | 63 |
| web/src/modals.ts | Remove this project from the list. It has no sessions to archive, and your real git repo is untouched — you can add it again anytime. | 133 | Remove the empty project. Keep the repo; add it again anytime. | 62 |
| web/src/modals.ts | Archive ${opts.sessionCount} ${word} and remove this project. Archived sessions stay restorable and your real git repo is untouched — restore any of them to bring the project back. | 180 | Archive ${opts.sessionCount} ${word} and remove the project. Keep the repo; restore sessions anytime. | 101 |
| web/src/modals.ts | An absolute path to a git checkout on the daemon host (~ is expanded there). It becomes an empty project you can create sessions into. | 134 | Enter a repo path on the daemon host (~ works). | 47 |
| web/src/modals.ts | Enter a repository path, or pick one above. | 43 | Enter or choose a repo path. | 28 |
| web/src/modals.ts | This deletes the task and stops future runs. Existing sessions are kept. | 72 | Delete the task and stop future runs. Keep existing sessions. | 61 |
| web/src/modals.ts | Browse the daemon host | 22 | Browse host | 11 |
| web/src/components.ts | Resume this session from its usage-limit wall | 45 | Retry after the usage limit | 27 |
| web/src/components.ts | Continue this session under a different agent | 45 | Continue with another agent | 27 |
| web/src/components.ts | Review the details, then ${opts.confirmLabel.toLowerCase()} again. | 66 | Check the error, then ${opts.confirmLabel.toLowerCase()} again. | 63 |
| web/src/components.ts | Edit defaults · | 15 | Defaults · | 10 |
| web/src/config.ts | No settings are available — use Configure with assistant or check the daemon connection. | 88 | No settings available. Try Configure with assistant. | 52 |
| web/src/config_assistant.ts | The daemon reported the config assistant unavailable. Close and try again. | 74 | Assistant unavailable. Close and retry. | 39 |
| web/src/config_assistant.ts | Could not reach the daemon. Close and try again. | 48 | Daemon unreachable. Close and retry. | 36 |
| web/src/config_assistant.ts | Starting the assistant… | 23 | Starting… | 9 |
| web/src/accounts.ts | This daemon reports no agents that support accounts. | 52 | No agents support accounts. | 27 |
| web/src/accounts.ts | A session cannot be scoped to a ${entry.agent} account yet — registering and logging in work. | 93 | ${entry.agent} accounts support login only; sessions cannot use them yet. | 73 |
| web/src/dirpicker.ts | Showing the first ${listing.entries.length} directories — type the path below to reach one that is not listed. | 110 | First ${listing.entries.length} directories · enter a path for more. | 68 |
| web/src/ui.ts | The agent tab stays first · drag it onto a pane to split instead | 64 | Agent tab stays first · drag to a pane to split | 47 |
| web/src/ui.ts | Check the daemon and its listener address, then retry. | 54 | Check the daemon address, then retry. | 37 |
| web/src/ui.ts | Paste the daemon bearer token to connect. Get it from  | 54 | Paste the daemon token from  | 28 |
| web/src/ui.ts | It stays saved in this browser until you disconnect. | 52 | Saved here until you disconnect. | 32 |
| web/src/ui.ts | This daemon does not require a token for your connection. | 57 | No token needed. | 16 |
| web/src/ui.ts | No sessions match the filter — ${hiddenCount(scoped, state.statusFilter)} hidden  | 81 | No matches · ${hiddenCount(scoped, state.statusFilter)} hidden  | 63 |
| web/src/ui.ts | Create a terminal or VS Code tab | 32 | New terminal or VS Code tab | 27 |
| web/src/recovery.ts | Check the session before taking further action. | 47 | Check the session before acting. | 32 |
| web/src/recovery.ts | Review the details before taking further action. | 48 | Review the result before acting. | 32 |
| web/src/recovery.ts | Review the details, then try again. | 35 | Check the error, then retry. | 28 |
| web/src/account_login_overlay.ts | The login flow ended — close this to see the account's state | 60 | Login ended · close to check the account | 40 |
| web/src/tasks.ts | Not applicable — the target session is meant to be reused. | 58 | Target session will be reused. | 30 |
| web/src/tasks.ts | A cron expression is required for a cron task. | 46 | Enter a cron expression. | 24 |
| web/src/tasks.ts | Select at least one day of the week. | 36 | Select at least one day. | 24 |
| web/src/tasks.ts | No projects yet — add one from the project switcher first | 57 | Add a project first. | 20 |
| web/src/tasks.ts | Could not load choices; the current value is kept. | 50 | Choices unavailable · current value kept. | 41 |
| web/src/tasks.ts | A name and a project are required. | 34 | Enter a name and choose a project. | 34 |
| web/src/tasks.ts | A prompt is required for a cron task. | 37 | Enter a prompt. | 15 |
| web/src/tasks.ts | A watch command is required for a watch task. | 45 | Enter a watch command. | 22 |
| web/src/accounts.ts | Agent identities, not config keys. af runs the agent's own login flow against a directory and never reads, stores or forwards the credential. Signing in is a device code · the pane prints a URL, you finish it in your own browser. | 229 | Sign in with the agent’s login flow. Follow the URL in the pane. | 64 |
| web/src/components.ts | Retry | 5 | Retry limit | 11 |
| web/src/components.ts | Close pane | 10 | Hide pane | 9 |
| web/src/ui.ts | Close tab | 9 | Delete tab | 10 |
| web/src/ui.ts | Kill | 4 | Delete session | 14 |
| web/src/ui.ts | Kill session “${killSession.title}” | 35 | Delete session “${killSession.title}” | 37 |
| web/src/modals.ts | Kill ${opts.sessionTitle}? | 26 | Delete session ${opts.sessionTitle}? | 36 |
| web/src/modals.ts | Kill | 4 | Delete session | 14 |
| web/src/terminal-keybar.ts | Back | 4 | More keys | 9 |
| web/src/account_scope.ts | Ambient identity (the agent's own login) | 40 | Use configured default | 22 |
| web/src/modals.ts | ambient | 7 | Use configured default | 22 |
| web/src/modals.ts | Send prompt to ${sessionTitle} | 30 |  | 0 |
| web/src/modals.ts | Send | 4 |  | 0 |
| web/src/modals.ts | Prompt | 6 |  | 0 |
| web/src/modals.ts | Enter a prompt to send. | 23 |  | 0 |
| web/src/account_scope.ts |  | 0 | Use configured default (${accountDefaultFor(accounts, agent)}) | 62 |
