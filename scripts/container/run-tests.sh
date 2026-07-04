#!/usr/bin/env bash
# Runs INSIDE the testbox container (see scripts/testbox.sh): copy the
# read-only source mount to a writable tree, then run the full suite.
# Bare `go test ./...` is safe here — the container has its own tmux
# server and a throwaway home, so nothing can leak onto the host.
set -euo pipefail

# Copy without .git: dev checkouts are often linked worktrees whose .git is
# a pointer to a host path that does not exist in the container.
mkdir -p /work
(cd /src && tar -c --exclude=.git .) | tar -x -C /work
cd /work

if [ $# -eq 0 ]; then
    set -- ./...
fi
exec go test -count=1 "$@"
