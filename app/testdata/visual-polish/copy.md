| File | Before | Length | After | Length |
| --- | --- | ---: | --- | ---: |
| ui/config_pane.go | Select a register row and press enter to add one. | 49 | Select register · enter to add an account. | 42 |
| ui/config_pane_accounts.go | agent identities, not config keys · af runs the agent's own login and never reads the credential | 96 | Sign in with the agent’s login flow · follow the URL in the pane | 64 |
| ui/overlay/promptOverlay.go | Sent to the agent as soon as it is ready… | 41 | Prompt for the agent… | 21 |
| ui/overlay/selectionOverlay.go | ↑/↓ navigate · enter select · esc cancel | 40 | ↑/↓ select · enter confirm · esc cancel | 39 |
| ui/overlay/searchOverlay.go | ↑/↓ navigate · enter select · esc close | 39 | ↑/↓ select · enter open · esc close | 35 |
| ui/overlay/projectPickerOverlay.go | registry unreadable · list may be incomplete | 44 | Cannot read registry · list may be incomplete | 45 |
| ui/overlay/projectPickerOverlay.go | Add project — enter a repo path: | 32 | Enter a repo path: | 18 |
| ui/overlay/projectPickerOverlay.go | j/k navigate · enter add · esc cancel | 37 | j/k select · enter add · esc cancel | 35 |
| ui/overlay/projectPickerOverlay.go | j/k navigate · enter switch · D delete · esc cancel | 51 | j/k select · enter switch · D delete · esc cancel | 49 |
| ui/tab_pane.go | No sessions yet — press n to create one. | 40 | No sessions · n to create one. | 30 |
| ui/tab_pane.go | VS Code tab — view in the web UI ⏎  ⏎ The editor opens this session's worktree. A terminal can't render it. | 103 | Open this VS Code tab in the web UI. | 36 |
| ui/tab_pane.go | Press Enter to open a terminal on the remote machine. | 53 | Enter to open a remote terminal. | 32 |
| ui/tab_pane.go | Terminal tab not available for remote sessions. ⏎ Configure remote_hooks.terminal_cmd to enable it. ⏎ Use the Agent tab to see session output. | 138 | Set remote_hooks.terminal_cmd for a remote terminal. ⏎ Use the Agent tab for output. | 82 |
| ui/tabbed_window.go |  %s · %s · Preview  | 19 |  %s · %s · preview  | 19 |
| ui/tabbed_window.go |  · Keyboard  | 12 |  · keyboard  | 12 |
| ui/tabbed_window.go |  %s · %s — selected: %s  | 24 |  %s · %s · selected: %s  | 24 |
| ui/tabbed_window.go |  No session selected  | 21 |  Select a session  | 18 |
| ui/hooks_pane.go | enter to focus and edit hooks | 29 | enter edit hooks | 16 |
| ui/task_pane.go | watch tasks run on their watch command's output, not on manual trigger | 70 | Watch tasks run on output, not manually. | 40 |
| ui/task_pane.go | long-running cmd; 1 stdout line = 1 event | 41 | One output line triggers one run | 32 |
| ui/task_pane.go | enter to focus and edit tasks | 29 | enter edit tasks | 16 |
| ui/task_pane_edit.go | n/a — a target session is not this task's to reap | 49 | Target session is kept. | 23 |
| ui/task_pane_edit.go | (optional) {{line}} expands to the event line | 45 | Optional · {{line}} inserts the event | 37 |
| app/handle_overlay.go | Sessions already created by this task remain available. | 55 | Keep existing sessions. | 23 |
| app/home_view.go | The last loaded sessions are retained. af retries automatically. | 64 | Showing saved sessions · retrying automatically. | 48 |
| app/help.go | A terminal UI that manages multiple Claude Code (and other local agents) in separate workspaces. | 96 | Manage agents in separate workspaces. | 37 |
| app/help.go | Create a new session | 20 | Create a session | 16 |
| app/help.go | Create a new remote session (requires remote_hooks config) | 58 | Create a remote session (needs remote_hooks) | 44 |
| app/help.go | While naming a new session: pick its agent / initial prompt / backend / account | 79 | New session: agent · prompt · backend · account | 47 |
| app/help.go | Switch to another project (repo) in place | 41 | Switch projects | 15 |
| app/help.go | Manage tasks (n inside the manager creates one, r runs one) | 59 | Manage tasks · n create · r run | 31 |
| app/help.go | Kill (delete) the selected session | 34 | Delete session · remove only af-owned resources | 47 |
| app/help.go | Archive the selected live session | 33 | Archive locally · sandboxes publish work first | 46 |
| app/help.go | Restore the selected archived / lost / dead session | 51 | Restore · sandboxes push before replacement or refuse | 53 |
| app/help.go | Retry a session blocked at a usage limit (re-spawn + resume) | 60 | Resume after a usage limit | 26 |
| app/help.go | Navigate between sessions | 25 | Select a session | 16 |
| app/help.go | Interact with the session in its pane (all keys go to it) | 57 | Type in the pane · all keys go to the agent | 43 |
| app/help.go | Leave interactive mode (back to navigation) | 43 | Return to navigation | 20 |
| app/help.go | Attach to the selected session full-screen | 42 | Attach full-screen | 18 |
| app/help.go | Detach from a full-screen session | 33 | Leave full-screen | 17 |
| app/help.go | Cycle focus: tree → open panes → automations | 44 | Focus tree → panes → tasks | 26 |
| app/help.go | Cycle focus backwards | 21 | Focus previous area | 19 |
| app/help.go | Open the selected tab as a pane (or focus its pane) | 51 | Open or focus the tab’s pane | 28 |
| app/help.go | Commit the current preview as another pane | 42 | Keep the preview as a pane | 26 |
| app/help.go | Hide the focused pane (the tab keeps running) | 45 | Hide pane · tab keeps running | 29 |
| app/help.go | Move focus between open panes | 29 | Focus another pane | 18 |
| app/help.go | Navigate the tree (sessions and their tabs) | 43 | Select a session or tab | 23 |
| app/help.go | Collapse the selected session's tabs | 36 | Collapse tabs | 13 |
| app/help.go | Expand the selected session's tabs | 34 | Expand tabs | 11 |
| app/help.go | Open the config agent to change your settings | 45 | Configure with assistant | 24 |
| app/help.go | Open the worktree hooks editor | 30 | Edit worktree hooks | 19 |
| app/help.go | Open the global config editor | 29 | Edit settings | 13 |
| app/help.go | Copy PR URL to clipboard | 24 | Copy PR link | 12 |
| app/help.go | Select one of the first nine tabs by number (s opens it, enter attaches) | 72 | Select tab 1–9 · s open · enter attach | 38 |
| app/help.go | Jump to ANY tab by number or name — there is no tab limit | 57 | Jump to any tab by number or name | 33 |
| app/help.go | Close the current tab (the agent tab can't be closed) | 53 | Close tab · agent tab stays | 27 |
| app/help.go | Scroll the current tab preview (navigation mode only) | 53 | Scroll preview in navigation mode | 33 |
| app/help.go | Quit the application | 20 | Quit | 4 |
| app/help.go | You are typing into this pane's terminal: every key — including tab — | 69 | All keys, including tab, go to the agent. | 41 |
| app/help.go | goes to the agent/shell. The pane's frame turns green while it has the | 70 | The pane’s keyboard label shows where you type. | 47 |
| app/help.go | keyboard, and the sessions rail stays visible. | 46 | The sessions rail stays visible. | 32 |
| app/help.go |  to return to navigation. | 25 |  to navigate. | 13 |
| app/help.go | Full-screen attach is still available on  | 41 | Attach full-screen with  | 24 |
| app/help.go |  (from nav mode). | 17 |  in navigation mode. | 20 |
| ui/config_pane_accounts.go | Holds a %s credential · ↵ runs %s's own login again in a tmux session scoped to this account, replacing it. af never reads the credential. | 138 | %s credential saved · enter to log in again and replace it. | 59 |
| ui/config_pane_accounts.go | No %s credential yet · ↵ runs %s's own login in a tmux session scoped to this account and hands you the terminal. It is a device code — the pane prints a URL, you finish it in your own browser. af never reads the credential. | 224 | Log in to %s · follow the URL and device code in the pane. | 58 |
| ui/overlay/confirmationOverlay.go | Press y/enter to confirm, n or esc to cancel | 44 | y/enter confirm · n/esc cancel | 30 |
| app/design_stills_test.go | Kill Apply design roles? Its running process will stop. | 55 | Delete session Apply design roles? Permanently remove its af-owned resources. | 77 |
| app/design_stills_test.go | The worktree and conversation remain available. | 47 | User-owned work stays. Archive instead to keep af-owned work. | 61 |
| api/sessions_watch.go | idle (ready for review) | 23 | idle · awaiting input | 21 |
| api/sessions_watch.go | session %q is idle (ready for review) | 37 | session %q is idle · awaiting input | 35 |
| api/sessions_watch.go | Block until a session goes idle, or until any session in the fleet changes state | 80 | Wait for idle, or a fleet stop-state change or disappearance | 60 |
| api/sessions_watch.go | Watch a session and return when its agent finishes working: exit 0 the moment ⏎ the session goes IDLE (the agent stopped working and is awaiting input), so an ⏎ operator or root agent can dispatch a session and be notified on completion ⏎ instead of polling 'af sessions preview'. | 274 | Watch a session and exit 0 when its agent is idle and awaiting input. ⏎ Idle does not mean the work is complete. | 110 |
| api/sessions_watch.go | With NO title (or --all) it watches every session in scope and returns when the ⏎ first one CHANGES STATE, printing which and why. That form is edge-triggered: the | 161 | With NO title (or --all) it watches every session in scope and returns on the ⏎ first stop-state change or disappearance, printing which session and why. It is edge-triggered: the | 177 |
| api/sessions_watch_fleet.go | waiting for any session to change state | 39 | waiting for a session stop-state change or disappearance | 56 |
| api/sessions_lifecycle.go | Not available for remote or in-place (--here) sessions: archive relocates the ⏎ worktree, which those don't own. The relocated worktree path is printed on ⏎ success. | 161 | Sandboxes publish work before removal; restore recreates them from the published ⏎ branch. In-place (--here) sessions cannot be archived because af does not own ⏎ their worktree. Local archives print the relocated worktree path on success. | 235 |
| api/sessions_lifecycle.go | Archive is the default way to finish with a session: tear down its tmux ⏎ and move its git worktree out to the global archive directory | 133 | Archive keeps a session restorable. Locally, stop its terminals ⏎ and move its owned git worktree to the global archive directory | 127 |
| app/handle_tabs.go | VS Code | 7 | VS Code (web UI) | 16 |
| app/account_picker.go | Ambient identity (the agent's own login) | 40 | Use configured default | 22 |
| app/help.go |  | 0 | Hand off to another agent | 25 |
| app/help.go |  | 0 | Search sessions | 15 |
| app/handle_actions.go | [!] Archive session '%s'? ⏎  ⏎ Its tmux is torn down and its worktree is moved out to the archive directory (branch + uncommitted changes preserved). Restore later with %s. | 168 | Archive session '%s'? ⏎  ⏎ Local: stop terminals and move the worktree to the archive. ⏎ Sandboxes: publish work, then remove the sandbox. ⏎ Restore with %s. | 149 |
| app/handle_actions.go | [!] Restore remote session '%s'? ⏎  ⏎ If its sandbox can't be reached, restore refuses to replace it because unreachability is not proof that it is gone. If the sandbox answers that its agent is gone, restore provisions a fresh one from the last pushed commit and discards any changes on the old sandbox that were never pushed. A reachable live sandbox just reconnects, losing nothing. | 381 | Restore sandbox session '%s'? ⏎  ⏎ Reconnect if live. Otherwise, push work before replacement. ⏎ Restore refuses if reachability or preservation is uncertain. | 152 |
| app/kill_confirm.go | [!] Kill session '%s'? | 22 | Delete session '%s'? ⏎ Permanently remove the session and resources owned by af. | 78 |
| app/kill_confirm.go | Kill the root agent anyway? | 27 | Delete the root session and its af-owned resources? | 51 |
| keys/keys.go | kill | 4 | delete session | 14 |
| api/sessions_lifecycle.go | Permanently destroy a session and prune its worktree branch | 59 | Permanently delete a session and af-owned resources | 51 |
| api/sessions_lifecycle.go | Permanently destroy a session: tear down tmux, remove the worktree, ⏎ delete the stored session record, and prune the session branch when Agent ⏎ Factory owns it. | 158 | Permanently delete the session record and stop its terminals. Remove only ⏎ worktrees and branches owned by af; user-owned resources stay. | 136 |
| api/sessions_lifecycle.go | Kill always destroys the session, including any uncommitted or unmerged work on ⏎ its branch — there is no undo. To keep a session restorable instead, archive it. | 160 | Deletion is permanent. Uncommitted or unmerged work in af-owned resources may ⏎ be lost. Archive instead to keep the session restorable. | 134 |
| ui/workspace.go | No panes open — s opens the selected tab | 40 | No panes · s open tab | 21 |
| app/account_picker.go |  | 0 | Use configured default (%s) | 27 |
| app/help.go |      - Kill (delete) the selected session | 41 |      - Permanently delete the session | 37 |
| app/help.go |      - Detach from a full-screen session | 40 |      - Leave full-screen | 24 |
| ui/overlay/confirmationOverlay.go | Press %s to confirm, %s or esc to cancel | 40 | %s confirm · %s/esc cancel | 26 |
