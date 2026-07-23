#!/usr/bin/env bash
# Shared source-tree copy for the container entrypoints (run-tests.sh,
# web-selftest-entry.sh). Sourced, not executed.
#
# The /src bind mount is read-only and carries the HOST user's ownership, so
# every entrypoint mirrors it into a writable tree before building.

# copy_src_tree SRC DEST [extra tar args...] — mirror SRC into DEST, minus .git
# and minus anything this user cannot read.
#
# .git is dropped because dev checkouts are often linked worktrees whose .git is
# a pointer to a host path that does not exist in the container.
#
# Unreadable paths are dropped because a single one used to take the entire run
# down before a test compiled (#2432). The image runs as `dev` (uid 1000, see
# Dockerfile.test) while the mount carries the host user's ownership, so a
# mode-0600 file belonging to a developer whose uid is not 1000 is unreadable
# here, and `tar -c` exits non-zero on it under `set -e`. Those files are
# working-directory debris — .env, .netrc, editor state, agent tool configs —
# never repo content under test, and an external contributor hitting this on
# their own .env learns only that the harness is broken.
#
# Skipped, not tolerated with --ignore-failed-read: that flag drops files
# SILENTLY, which turns a missing source file into a mystery compile error far
# from its cause. Every skip is named on stderr, so if one ever does matter, the
# reason is already on screen above the failure.
#
# The filter is permission-based rather than gitignore-based on purpose. This
# tree is frequently a linked worktree whose .git does not resolve in the
# container (the same fact that makes .git unusable above), so git cannot be
# asked which paths are repo content.
copy_src_tree() {
    local src="$1" dest="$2"
    shift 2

    mkdir -p "$dest"

    # GNU find/-readable: this runs inside the Linux testbox image only.
    local excludes
    excludes="$(mktemp)"
    (cd "$src" && find . -path ./.git -prune -o ! -readable -print) >"$excludes" 2>/dev/null || true

    if [ -s "$excludes" ]; then
        echo "testbox: skipping $(wc -l <"$excludes" | tr -d ' ') path(s) under $src that uid $(id -u) cannot read:" >&2
        sed 's/^/  /' "$excludes" >&2
        echo "testbox: the harness mirrors your whole working tree, so private files land here." >&2
        echo "testbox: if the build needs one of them, make it readable and re-run." >&2
    fi

    (cd "$src" && tar -c --exclude=.git --exclude-from="$excludes" "$@" .) | tar -x -C "$dest"
    rm -f "$excludes"
}
