#!/usr/bin/env bash
# Runs INSIDE the testbox container (see scripts/testbox.sh lifecycle):
# build af from the mounted source, then hand over to scripts/lifecycle.sh,
# which builds its own throwaway machines under a scratch workspace and drives
# the clean-install / install->upgrade scenarios against them.
#
# Everything here — the tmux server, every daemon, every AF home, every binary
# this installs — dies with the container. Teardown is container exit.
#
# NOTE ON COVERAGE: this container has no systemd (PID 1 is docker-init), so
# `af daemon install` cannot work here and the supervision assertion (#4) is
# SKIPped. That assertion only really runs on the CI runner, which has a real
# systemd user manager. The skip is printed loudly in the summary — see
# docs/lifecycle-testing.md.
set -euo pipefail

export AF_LIFECYCLE_WORKSPACE="${AF_LIFECYCLE_WORKSPACE:-$HOME/af-lifecycle}"
# The harness is destructive by design and refuses to run without this; the
# container is exactly the throwaway environment it wants.
export AF_LIFECYCLE_DISPOSABLE=1

mkdir -p "$HOME/bin"

echo ">>> building af from /src ..."
# Same two ownership guards as playtest-entry.sh (#1167): /src is a read-only
# bind mount owned by the host user, so git would refuse the "dubious
# ownership" repo and the toolchain would fail reading .git for a VCS stamp.
# Keep the entry scripts consistent on this.
(cd / && git config --global --add safe.directory /src) 2>/dev/null || true
(cd /src && go build -buildvcs=false -o "$HOME/bin/af" .)

# Scenario A installs "the af a new user gets today" — this tree's build.
# Scenario B ignores it and installs REAL published releases instead.
export AF_LIFECYCLE_AF_BIN="$HOME/bin/af"

echo ">>> af built: $("$AF_LIFECYCLE_AF_BIN" version 2>/dev/null | head -1)"

exec bash /src/scripts/lifecycle.sh "$@"
