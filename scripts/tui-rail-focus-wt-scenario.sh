#!/usr/bin/env bash
# Real-TUI regression for the #1417 leak class: `w` (close tab) / `t` (new tab)
# must stay inert while the Automations rail owns focus, and must keep working
# once focus returns to the tree. Run via:
#
#   scripts/testbox.sh scenario scripts/tui-rail-focus-wt-scenario.sh
#
# Before the fix, `w` under Automations focus opened a delete-tab confirmation
# against the store's sticky last tree selection, and `t` opened the new-tab
# picker against the sidebar cursor's row — neither of which the rail displays.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

cleanup() {
    af_quit >/dev/null 2>&1 || true
}
trap cleanup EXIT

af_reset_sandbox

# The Automations rail only renders when a task exists. Seed one directly into
# the sandbox tasks.json (schema_version 1) instead of driving the create form,
# so the rail renders deterministically without depending on the form's
# field-tab sequence. ProjectPath points at the mock-repo the TUI launches in.
mkdir -p "$AGENT_FACTORY_HOME"
cat > "$AGENT_FACTORY_HOME/tasks.json" <<JSON
{"schema_version":1,"tasks":[{"id":"rail-task","name":"rail-task","prompt":"selftest prompt","cron_expr":"0 3 * * *","project_path":"$AF_DRIVER_REPO","program":"claude","enabled":true,"created_at":"2026-01-01T00:00:00Z"}]}
JSON

af_boot
af_ensure_nav

# A multi-tab instance: agent + two shell tabs (so a leaked `w` has a deletable
# target beyond the agent tab, and the leak is observable).
af_new_instance leak-target
af_select leak-target
af_new_tab   # shell
af_new_tab   # shell-2

af_focus_tree
baseline_tabs=$(_af_tab_count)
_af_log "baseline tab count: $baseline_tabs"
[ "$baseline_tabs" -ge 3 ] || { _af_fail "precondition: need >=3 tabs (agent + 2 shell), got $baseline_tabs"; exit 1; }

# --- Focus the Automations rail: Tab until the status-bar footer NO LONGER
# advertises the tab verbs (t new tab / w del tab) and DOES advertise `manage`.
# The rail footer swaps the tab verbs out for `manage` (↵/enter manage), so the
# absence of `t new tab`/`w del tab` in the footer row is the focus proof.
# `_af_menu_row` returns the footer line ending in `? help · q quit`.
focus_automations() {
    local _ i=0
    for _ in $(seq 1 12); do
        local footer
        footer="$(af_capture | _af_menu_row || true)"
        if printf '%s' "$footer" | grep -qE 'manage' \
           && ! printf '%s' "$footer" | grep -qE 't new tab|w del tab|n new'; then
            return 0
        fi
        af_send Tab
        sleep "$AF_DRIVER_POLL"
        i=$((i+1))
    done
    _af_fail "could not focus the Automations rail (footer never dropped t new tab/w del tab)"
    return 1
}
focus_automations
_af_log "assert: automations rail holds focus (footer: $(af_capture | _af_menu_row | tr -s ' '))"

# --- `w` under rail focus must NOT open the delete-tab confirmation.
af_send w
sleep 1
if af_capture | grep -qE 'Delete tab "'; then
    _af_fail "#1417: w under Automations focus opened the delete-tab confirmation (the leak)"
    exit 1
fi
_af_log "assert OK: w under Automations focus did not open the delete confirmation"

# --- `t` under rail focus must NOT open the new-tab picker.
af_send t
sleep 1
if af_capture | grep -qE 'New tab'; then
    _af_fail "#1417: t under Automations focus opened the new-tab picker (the leak)"
    exit 1
fi
_af_log "assert OK: t under Automations focus did not open the new-tab picker"

# Roster must be untouched by the rail-focused presses.
post_rail_tabs=$(_af_tab_count)
[ "$post_rail_tabs" -eq "$baseline_tabs" ] || {
    _af_fail "#1417: rail-focused w/t changed the tab count ($baseline_tabs -> $post_rail_tabs)"
    exit 1
}
_af_log "assert OK: roster untouched under rail focus ($post_rail_tabs tabs)"

# --- No regression: focus back to the tree; `w` opens the confirmation, `t` the
# picker. Cancel the confirmation so no tab is destroyed.
af_focus_tree

af_send w
if ! af_wait_for 'Delete tab "' "$AF_DRIVER_TIMEOUT" 'tree-focused w opens delete confirmation' >/dev/null 2>&1; then
    _af_fail "no regression: w with tree focus must still open the delete confirmation"
    exit 1
fi
af_send Esc
sleep 1
_af_log "assert OK: w with tree focus still opens the delete confirmation (no regression)"

af_send t
if ! af_wait_for 'New tab' "$AF_DRIVER_TIMEOUT" 'tree-focused t opens new-tab picker' >/dev/null 2>&1; then
    _af_fail "no regression: t with tree focus must still open the new-tab picker"
    exit 1
fi
af_send Esc
sleep 1
_af_log "assert OK: t with tree focus still opens the new-tab picker (no regression)"

# Roster unchanged after the cancelled tree-focused dialogs.
final_tabs=$(_af_tab_count)
[ "$final_tabs" -eq "$baseline_tabs" ] || {
    _af_fail "tree-focused w/t (cancelled) changed the tab count ($baseline_tabs -> $final_tabs)"
    exit 1
}

echo "PASS: #1417 — w/t inert under Automations rail focus, still working under tree focus"
