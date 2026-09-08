| Source | Action/footer string | Length |
| --- | --- | ---: |
| ui/hooks_pane.go:203 | enter save · esc cancel | 23 |
| ui/hooks_pane.go:205 | n add · enter edit · D delete · esc back | 40 |
| ui/hooks_pane.go:207 | n add · enter · esc back | 24 |
| ui/task_pane_edit.go:421 | tab/shift+tab fields · enter create · esc cancel ·  | 51 |
| ui/task_pane_edit.go:423 | tab fields · enter · esc cancel ·  | 34 |
| ui/task_pane_edit.go:427 | tab/shift+tab fields · enter save | 33 |
| ui/task_pane_edit.go:429 | tab fields · enter save | 23 |
| ui/task_pane_edit.go:442 | r run now · x toggle · D delete · esc list ·  | 45 |
| ui/task_pane_edit.go:443 | r run · x toggle · D del · esc list ·  | 38 |
| ui/task_pane_edit.go:444 | r run · x toggle · D del · esc ·  | 33 |
| ui/task_pane_edit.go:450 | x toggle · D delete · esc list ·  | 33 |
| ui/task_pane_edit.go:451 | x toggle · D del · esc list ·  | 30 |
| ui/task_pane_edit.go:452 | x toggle · D del · esc ·  | 25 |
| ui/task_pane.go:778 | ↑/↓ select · n new · enter edit · r run now · x toggle · D delete · esc back | 76 |
| ui/task_pane.go:779 | r run now · x toggle · D delete · ? back · esc | 46 |
| ui/task_pane.go:783 | ↑/↓ select · n new · enter edit · x toggle · D delete · esc back | 64 |
| ui/task_pane.go:784 | x toggle · D delete · ? back · esc | 34 |
| ui/task_pane.go:787 | enter edit · n new · ? actions · esc back | 41 |
| ui/task_pane.go:788 | enter edit · ? actions · esc | 28 |
| ui/overlay/projectPickerOverlay.go:252 | enter add · esc back | 20 |
| ui/overlay/projectPickerOverlay.go:279 | j/k navigate · enter add · esc cancel | 37 |
| ui/overlay/projectPickerOverlay.go:281 | j/k navigate · enter switch · D delete · esc cancel | 51 |
| ui/overlay/projectPickerOverlay.go:284 | j/k · enter · esc | 17 |
| ui/overlay/promptOverlay.go:153 | enter newline · tab done · ctrl+c cancel | 40 |
| ui/overlay/promptOverlay.go:155 | tab done · ctrl+c cancel | 24 |
| ui/overlay/selectionOverlay.go:155 | ↑/↓ navigate · enter select · esc cancel | 40 |
| ui/overlay/selectionOverlay.go:156 | ↑/↓ nav · enter · esc cancel | 28 |
| ui/overlay/selectionOverlay.go:157 | enter · esc cancel | 18 |
| ui/overlay/confirmationOverlay.go:351 | Too small to confirm safely · resize | 36 |
| ui/overlay/confirmationOverlay.go:361 |  confirm ·  | 11 |
| ui/overlay/searchOverlay.go:350 | ↑/↓ navigate · enter select · esc close | 39 |
| ui/overlay/searchOverlay.go:352 | ↑/↓ nav · enter · esc close | 27 |
| app/help.go:321 | Close: esc ·  | 13 |
| app/help.go:425 | enter continue · esc close | 26 |
| app/help.go:442 | enter attach full-screen · esc cancel | 37 |
| config/manifest.go:246 | How often the background service checks sessions for new output · use a duration such as 1500ms or 30m; legacy integer milliseconds remain accepted. | 148 |
| config/manifest.go:282 | How many rotated log files to keep before the oldest is deleted · 0 keeps none. | 79 |
| config/manifest.go:306 | Operator command run in a session worktree after its panes exit and before it moves into the archive · empty disables it; failures warn but do not cancel the archive. | 166 |
| config/manifest.go:359 | Which of an agent's logged-in accounts a new session runs as · one entry per agent, most useful set per project so one project runs as your work identity and another as your personal one. | 187 |
| config/manifest.go:452 | How the ssh backend verifies a remote host key · strict (default) refuses an unknown or changed key; accept-new trusts an unknown key on first connect (still refuses a changed one); insecure skips verification · global-only: a repo selects ssh.host but only the operator relaxes verification. | 292 |
| config/manifest.go:501 | Legacy path map: repositories that always keep a session named root running · accepted forever for compatibility; use the current root_agent project profile for new configuration. | 179 |

Also audited: all keys/keys.go help descriptors (in inventory.csv), confirmation custom keys and refusal-only cancel, menu action groups, search/selection/project-picker rows, prompt footer, task form save/select controls, config/account/hook footer verbs, first-run help affordances. Values and key bindings remain intact.
