#!/usr/bin/env bash
# demo-agent.sh — launch the real Codex CLI for the README demo.
#
# The recorder copies this wrapper into the throwaway play-test container and
# points program_overrides.codex at it. There is deliberately no transcript,
# canned output, or agent stand-in here: every line in the recorded Agent pane
# comes from Codex working in the session's real AF-owned worktree.
set -euo pipefail

codex_bin="${AF_DEMO_CODEX_BIN:-$(command -v codex-real || true)}"
if [ -z "$codex_bin" ]; then
    echo "demo-agent: codex-real is not installed in the play-test sandbox" >&2
    exit 1
fi

# Codex's inner bubblewrap sandbox cannot create namespaces inside the already
# fenced play-test container. This mode is intended for an external sandbox:
# the container exposes only the repository read-only, a disposable mock repo,
# a throwaway AF home, and its private tmux server.
exec "$codex_bin" \
    --dangerously-bypass-approvals-and-sandbox \
    --no-alt-screen \
    "$@"
