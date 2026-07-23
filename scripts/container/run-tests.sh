#!/usr/bin/env bash
# Runs INSIDE the testbox container (see scripts/testbox.sh): copy the
# read-only source mount to a writable tree, then run the full suite.
# Bare `go test ./...` is safe here — the container has its own tmux
# server and a throwaway home, so nothing can leak onto the host.
set -euo pipefail

# /src is a read-only mount owned by the host user; mark it safe so any git
# invocation the toolchain makes does not refuse it as "dubious ownership"
# (#1167). Run from a non-repo cwd (WORKDIR is /src). Kept in step with
# playtest-entry.sh.
(cd / && git config --global --add safe.directory /src) 2>/dev/null || true

# Copy without .git (linked worktrees) or anything this user cannot read
# (#2432); copy-src.sh carries the reasoning for both.
# shellcheck source=scripts/container/copy-src.sh
. /src/scripts/container/copy-src.sh
copy_src_tree /src /work
cd /work

if [ $# -eq 0 ]; then
    set -- ./...
fi
# -buildvcs=false: /work has no .git, but disabling the VCS stamp keeps the
# build off git entirely and consistent with the play-test build (#1167).
exec go test -count=1 -buildvcs=false "$@"
