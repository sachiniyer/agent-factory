| Source | Button constructor |
| --- | --- |
| web/src/account_login_overlay.ts:84 | h("button", { type: "button", class: "af-ghost af-assistant-close" }, "×") |
| web/src/accounts.ts:194 | h( ⏎     "button", ⏎     { type: "button", class: "af-ghost af-accounts-login" }, ⏎     entry.logged_in ? "Log in again" : "Log in", ⏎   ) |
| web/src/accounts.ts:253 | h("button", { type: "button", class: "af-ghost af-accounts-add" }, "Register") |
| web/src/components.ts:12 | h("button", { type: "button", class: "af-term-more" }, h("span", { class: "af-term-more-label" }, "Actions"), h("span", { class: "af-term-more-compact", ariaHidden: "true" }, "…")) |
| web/src/components.ts:97 | h("button", { type: "button", class: \`af-ghost af-term-action ${className}\` }, label) |
| web/src/components.ts:136 | h("button", { type: "button", class: "af-pane-close", title: "Close pane" }, icon("x")) |
| web/src/components.ts:163 | h("button", { type: "button", class: "af-viewtab", role: "tab" }, labels[view]) |
| web/src/components.ts:201 | h("button", { type: "button", class: "af-ghost" }, "Cancel") |
| web/src/components.ts:202 | h("button", { type: "submit", class: opts.confirmClass }, opts.confirmLabel) |
| web/src/config.ts:389 | h( ⏎       "button", ⏎       { type: "button", class: "af-ghost af-config-assistant-btn" }, ⏎       "Configure with assistant", ⏎     ) |
| web/src/config.ts:407 | h( ⏎           "button", ⏎           { type: "button", class: "af-ghost af-config-toggle" }, ⏎           folded ? \`Show ${inTier.length} advanced settings\` : "Hide advanced settings", ⏎         ) |
| web/src/config.ts:521 | h("button", { type: "button", class: "af-primary af-config-save" }, "Save") |
| web/src/config_assistant.ts:90 | h("button", { type: "button", class: "af-ghost af-assistant-close" }, "×") |
| web/src/dirpicker.ts:144 | h("button", { type: "button", class: "af-ghost af-dirpicker-up" }, "Up") |
| web/src/dirpicker.ts:153 | h("button", { type: "button", class: "af-ghost af-dirpicker-home" }, "Home") |
| web/src/dirpicker.ts:157 | h("button", { type: "button", class: "af-ghost af-dirpicker-use-here" }, "Use this") |
| web/src/dirpicker.ts:273 | h("button", { type: "button", class: "af-dirpicker-item" }, label) |
| web/src/dirpicker.ts:292 | h("button", { type: "button", class: "af-ghost af-dirpicker-use" }, "Use") |
| web/src/recovery.ts:14 | h("button", { type: "button", class: "af-recovery-action" }, state.action) |
| web/src/tasks.ts:313 | h( ⏎       "button", ⏎       { type: "button", class: "af-tasks-add", title: "Add task" }, ⏎       icon("plus"), ⏎       "Add", ⏎     ) |
| web/src/tasks.ts:387 | h( ⏎       "button", ⏎       { type: "button", class: "af-ghost af-task-action" }, ⏎       t.enabled ? "Disable" : "Enable", ⏎     ) |
| web/src/tasks.ts:394 | h("button", { type: "button", class: "af-ghost af-task-action" }, "Edit") |
| web/src/tasks.ts:401 | h("button", { type: "button", class: "af-ghost af-task-action" }, "Trigger") |
| web/src/tasks.ts:405 | h("button", { type: "button", class: "af-ghost af-task-action" }, "Remove") |
| web/src/tasks.ts:533 | h("button", { type: "button", class: "af-weekday" }, day.letter) |
| web/src/ui.ts:642 | h( ⏎     "button", ⏎     { type: "submit", class: "af-primary", disabled: state.connecting }, ⏎     state.connecting ? "Connecting…" : "Connect", ⏎   ) |
| web/src/ui.ts:698 | h( ⏎     "button", ⏎     { type: "submit", class: "af-primary", disabled: state.connecting }, ⏎     state.connecting ? "Connecting…" : "Connect", ⏎   ) |
| web/src/ui.ts:912 | h("button", { type: "button", class: "af-ghost" }, "Disconnect") |
| web/src/ui.ts:921 | h("button", { type: "button", class: "af-theme-opt" }, themeLabel(choice)) |
| web/src/ui.ts:942 | h( ⏎       "button", ⏎       { type: "button", class: "af-project-switch" }, ⏎       switchGlyph, ⏎       this.projectSwitchName, ⏎       switchCaret, ⏎     ) |
| web/src/ui.ts:971 | h("button", { type: "button", class: "af-nav-toggle" }, icon("menu")) |
| web/src/ui.ts:1005 | h( ⏎       "button", ⏎       { type: "button", class: "af-rail-new", title: "New session" }, ⏎       icon("plus"), ⏎       "New", ⏎     ) |
| web/src/ui.ts:1021 | h("button", { type: "button", class: "af-rail-filter" }, filterGlyph, this.filterDot) |
| web/src/ui.ts:1173 | h("button", { type: "button", class: "af-recovery-action" }, "Dismiss") |
| web/src/ui.ts:1493 | h("button", { type: "button", class: lifecycleClass }) |
| web/src/ui.ts:1518 | h( ⏎         "button", ⏎         { type: "button", class: killClass }, ⏎         "Kill", ⏎       ) |
| web/src/ui.ts:1578 | h( ⏎         "button", ⏎         { type: "button", class: "af-rail-empty-new", title: "New session" }, ⏎         icon("plus"), ⏎         "New", ⏎       ) |
| web/src/ui.ts:1588 | h("button", { type: "button", class: "af-rail-empty-new af-rail-show-archived" }, "Show archived") |
| web/src/ui.ts:1595 | h("button", { type: "button", class: "af-rail-empty-new af-rail-reset-filter" }, "Reset filter") |
| web/src/ui.ts:1621 | h("button", { type: "button", class: "af-ghost af-filter-reset" }, "Reset to default") |
| web/src/ui.ts:1637 | h( ⏎       "button", ⏎       { type: "button", class: \`af-filter-item${on ? " af-filter-item-on" : ""}\` }, ⏎       check, ⏎       h("span", { class: "af-filter-item-label" }, filterLabel(kind)), ⏎       h("span", { class: "af-filter-item-count" }, String(count)), ⏎     ) |
| web/src/ui.ts:1682 | h("button", { type: "button", class: "af-ghost af-project-add" }, "+ Add project") |
| web/src/ui.ts:1694 | h("button", { type: "button", class: "af-ghost af-project-delete" }, "Delete project") |
| web/src/ui.ts:1744 | h("button", { type: "button", class: cls }, check, label, meta) |
| web/src/ui.ts:1768 | h( ⏎       "button", ⏎       { type: "button", class: "af-tab-new", title: "Create a terminal or VS Code tab" }, ⏎       icon("plus", "af-tab-new-plus"), ⏎       h("span", {}, "New tab"), ⏎       icon("chevron-down", "af-tab-new-caret"), ⏎     ) |
| web/src/ui.ts:1846 | h("button", { type: "button", class: "af-tab-menu-item" }, label) |
| web/src/ui.ts:2788 | h("button", { type: "button", class: cls, draggable: true }) |

Also audited: dynamic terminal-keybar rows, tab-close spans, defaults/account disclosure summaries, actionable project/filter/status chips, tab/appearance/view navigation, task actions, account login/register, recovery links, and pane hides. The constructor inventory includes icon-only controls and their accessible names.
