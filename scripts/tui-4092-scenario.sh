#!/usr/bin/env bash
# Real-TUI play-test for #4092. It recreates the sibling-moved-HEAD shape in
# the disposable container repository, then reads the daemon's session status
# and the 120x30 tree frame a user actually sees.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=30
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
af_boot
af_new_instance idle
af_select idle
echo 'fixture: live idle session selected in the 120x30 tree'

bin="$(_af_resolve_bin)"
holder="$(cd "$AF_DRIVER_REPO" && "$bin" sessions get idle | jq -er '.worktree.worktree_path')"
branch="$(git -C "$holder" symbolic-ref --short HEAD)"
sibling="${AF_DRIVER_REPO}-takeover"

# Populate the holder's index with enough tracked paths for the mass-revert
# symptom to become visible after a sibling advances the shared ref.
for i in $(seq 1 25); do
    printf 'old\n' >"$holder/mass-$i.txt"
done
git -C "$holder" add -A
git -C "$holder" commit -qm 'play-test baseline'

git -C "$AF_DRIVER_REPO" worktree add -q -b takeover "$sibling" master
ordinary="$(git -C "$sibling" checkout "$branch" 2>&1 || true)"
if ! printf '%s\n' "$ordinary" | grep -qE 'already (used by worktree|checked out at)'; then
    echo "FIXTURE INVALID: ordinary checkout did not report a held-worktree refusal: $ordinary" >&2
    exit 1
fi
echo "fixture: ordinary checkout refused the held branch: $ordinary"
git -C "$sibling" checkout --ignore-other-worktrees -q -B "$branch" "$branch"
for i in $(seq 1 25); do
    printf 'new\n' >"$sibling/mass-$i.txt"
done
git -C "$sibling" add -A
git -C "$sibling" commit -qm 'advance shared ref from sibling'

staged="$(git -C "$holder" diff --cached --name-only | wc -l | tr -d ' ')"
unstaged="$(git -C "$holder" diff --name-only | wc -l | tr -d ' ')"
head_oid="$(git -C "$holder" rev-parse HEAD)"
reflog_oid="$(git -C "$holder" log -g -1 --format=%H HEAD)"
if [ "$staged" -ne 25 ] || [ "$unstaged" -ne 0 ] || [ "$head_oid" = "$reflog_oid" ]; then
    echo "FIXTURE INVALID: staged=$staged unstaged=$unstaged HEAD=$head_oid reflog=$reflog_oid" >&2
    exit 1
fi

deadline=$(( $(_af_now) + 45 ))
warning=''
while [ "$(_af_now)" -lt "$deadline" ]; do
    warning="$(cd "$AF_DRIVER_REPO" && "$bin" sessions get idle | jq -r '.worktree_warning // empty')"
    [ -n "$warning" ] && break
    sleep 1
done
if [ -z "$warning" ]; then
    echo 'TIMEOUT: session status never received a worktree warning' >&2
    exit 1
fi
for expected in \
    '25 staged paths and zero unstaged paths' \
    "HEAD is ${head_oid:0:12}" \
    "latest HEAD reflog entry is ${reflog_oid:0:12}"; do
    if ! printf '%s\n' "$warning" | grep -qF "$expected"; then
        echo "STATUS INVALID: warning omitted '$expected': $warning" >&2
        exit 1
    fi
done

af_wait_for '\[worktree unsafe\]' 20 'worktree warning prefix in the session tree'
af_wait_for 'DANGER:' 20 'worktree warning detail in the selected tree row'

echo '----- session status: worktree_warning -----'
printf '%s\n' "$warning"
echo '----- 120x30 TUI: selected session tree row -----'
af_capture
echo '------------------------------------------------'
echo 'PASS: #4092 session status and the real 120x30 tree surface the corroborated takeover warning'
