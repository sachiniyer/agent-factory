#!/usr/bin/env bash
# demo-tab.sh — run the mock project's real tests in an AF Terminal tab.
#
# This is intentionally a command, not a rendered transcript. The demo starts
# it inside one agent's worktree after that agent finishes, so the result shown
# beside the Agent pane is produced by the files that agent actually changed.
set -euo pipefail

run_tests() {
    printf 'Running ./test.sh …\n'
    if ./test.sh; then
        printf '✓ Tests pass · watching this worktree\n'
    else
        printf '✕ Tests failed · watching this worktree\n'
    fi
}

run_tests
while sleep 8; do
    printf '\n'
    run_tests
done
