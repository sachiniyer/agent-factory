#!/usr/bin/env bash
# demo-agent.sh — a FAKE coding-agent transcript, used only when recording the
# README demo GIF (#1032). record-demo.sh points the sandbox's
# program_overrides.claude at this script, so every af instance's pane streams
# a realistic "agent working" transcript instead of a bare bash prompt — then
# drops to a live shell so the instance stays ready. It is NOT part of `af`.
#
# The transcript is chosen from the worktree/branch name, so each demo session
# ("fix-auth-timeout", "add-dark-mode", …) tells its own little story in the
# Agent tab. Nothing here talks to the network or a real model.
set -u

c_cyan=$'\033[36m'; c_yellow=$'\033[33m'; c_blue=$'\033[34m'
c_green=$'\033[32m'; c_dim=$'\033[2m'; c_bold=$'\033[1m'; c_reset=$'\033[0m'

# Identify the task from the branch name (falls back to the worktree dir).
task="$(git branch --show-current 2>/dev/null || true)"
[ -n "$task" ] || task="$(basename "$PWD")"

line() {  # line <glyph> <color> <text> [delay]
    printf '%s%s%s %s\n' "$2" "$1" "$c_reset" "$3"
    sleep "${4:-0.5}"
}

printf '%s%s Claude Code%s %s· %s%s\n\n' \
    "$c_bold" "$c_cyan" "$c_reset" "$c_dim" "$task" "$c_reset"
sleep 0.4

case "$task" in
    *auth*)
        line "●" "$c_cyan"   "Analyzing auth middleware in src/session.go"
        line "●" "$c_cyan"   "Found: token refresh races on expiry (5s window)"
        line "✎" "$c_yellow" "Editing src/session.go   ${c_green}+18 ${c_dim}-4${c_reset}"
        line "✎" "$c_yellow" "Editing src/token.go     ${c_green}+6  ${c_dim}-0${c_reset}"
        line "▶" "$c_blue"   "Running ./test.sh …" 0.8
        line "✓" "$c_green"  "14 tests passed"
        line " " "$c_dim"    "${c_dim}fixed refresh race, added regression test${c_reset}" 0.2
        ;;
    *dark*)
        line "●" "$c_cyan"   "Scanning UI theme tokens (styles.go)"
        line "✎" "$c_yellow" "Adding theme.Dark palette   ${c_green}+42 ${c_dim}-2${c_reset}"
        line "✎" "$c_yellow" "Wiring prefers-color-scheme toggle"
        line "▶" "$c_blue"   "go build ./… …" 0.8
        line "✓" "$c_green"  "build ok"
        line " " "$c_dim"    "${c_dim}dark theme + runtime toggle wired${c_reset}" 0.2
        ;;
    *refactor*|*api*)
        line "●" "$c_cyan"   "Mapping api/client.go call sites (12)"
        line "✎" "$c_yellow" "Extracting doRequest() helper   ${c_green}+61 ${c_dim}-88${c_reset}"
        line "✎" "$c_yellow" "Collapsing duplicated retry logic"
        line "▶" "$c_blue"   "go vet ./… …" 0.8
        line "✓" "$c_green"  "clean"
        line " " "$c_dim"    "${c_dim}-27 net lines, retry paths unified${c_reset}" 0.2
        ;;
    *test*|*integration*)
        # Fuller transcript: this is the session the demo attaches to
        # full-screen, so it streams a substantive, believable run that fills
        # the pane instead of leaving it mostly empty.
        line "●" "$c_cyan"   "Scanning API surface for untested handlers" 0.25
        line "●" "$c_cyan"   "Found 7 endpoints without integration coverage" 0.25
        line "✎" "$c_yellow" "Writing integration_test.go   ${c_green}+134 ${c_dim}-0${c_reset}" 0.2
        line " " "$c_dim"    "  ${c_dim}+ TestListTodos        ${c_green}+22${c_reset}" 0.12
        line " " "$c_dim"    "  ${c_dim}+ TestAddTodo          ${c_green}+28${c_reset}" 0.12
        line " " "$c_dim"    "  ${c_dim}+ TestDoneTodo         ${c_green}+24${c_reset}" 0.12
        line " " "$c_dim"    "  ${c_dim}+ TestAddTodo_Empty    ${c_green}+18${c_reset}" 0.12
        line " " "$c_dim"    "  ${c_dim}+ TestConcurrentWrites ${c_green}+31${c_reset}" 0.12
        line "▶" "$c_blue"   "Running ./test.sh …" 0.35
        line " " "$c_green"  "  ${c_green}PASS${c_reset}  TestListTodos          ${c_dim}0.01s${c_reset}" 0.12
        line " " "$c_green"  "  ${c_green}PASS${c_reset}  TestAddTodo            ${c_dim}0.01s${c_reset}" 0.12
        line " " "$c_green"  "  ${c_green}PASS${c_reset}  TestDoneTodo           ${c_dim}0.02s${c_reset}" 0.12
        line " " "$c_green"  "  ${c_green}PASS${c_reset}  TestAddTodo_Empty      ${c_dim}0.00s${c_reset}" 0.12
        line " " "$c_green"  "  ${c_green}PASS${c_reset}  TestConcurrentWrites   ${c_dim}0.03s${c_reset}" 0.12
        line "✓" "$c_green"  "7 passed, 0 failed" 0.15
        line " " "$c_dim"    "${c_dim}coverage 61% → 88%${c_reset}" 0.2
        ;;
    *)
        line "●" "$c_cyan"   "Reading project files"
        line "✎" "$c_yellow" "Applying changes   ${c_green}+24 ${c_dim}-6${c_reset}"
        line "▶" "$c_blue"   "Running ./test.sh …" 0.8
        line "✓" "$c_green"  "ok"
        ;;
esac

printf '\n%s✓ Done — ready for review%s\n\n' "$c_green$c_bold" "$c_reset"

# Drop to a live shell so the instance stays ready (a program that exits marks
# the instance dead). The pane keeps the transcript above the prompt.
exec bash
