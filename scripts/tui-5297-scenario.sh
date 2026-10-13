#!/usr/bin/env bash
# Real-TUI regression for #5297 — a modal must never share a row with
# background content: no chopped pane-preview strip to the right of the box,
# no rail glyphs clipped mid-cell at its left.
#
#   scripts/testbox.sh scenario scripts/tui-5297-scenario.sh
#
# or, on a parked sandbox (`scripts/testbox.sh playtest -d`), inject the
# provenance inputs yourself — the container cannot resolve them from the
# linked-worktree /src:
#
#   git archive --format=tar 78d031456fa890b943a89524a8d3d0adff92883d >/tmp/m.tar
#   chmod 644 /tmp/m.tar
#   docker cp /tmp/m.tar <name>:/tmp/af-master-src.tar
#   docker exec -e AF_BRANCH_SHA="$(git rev-parse HEAD)" \
#               -e AF_MASTER_SHA=78d031456fa890b943a89524a8d3d0adff92883d \
#               -e AF_MASTER_TAR=/tmp/af-master-src.tar \
#               <name> bash /src/scripts/tui-5297-scenario.sh
#
# The bug (80x24): every modal is centered on the whole frame, and the
# compositor resumed the faded background text in the margins beside the box —
# a ~14-column strip of chopped pane-preview cells past the right border, and
# rail glyphs clipped mid-cell on the left (▾ beta╭, ⎇-de│). The fix makes the
# compositor emit blank backdrop in those margins, once for all overlays.
#
# Both legs run in THIS sandbox at exactly 80x24, against binaries built inside
# it:
#   af.master — git archive of the pinned pre-fix control revision, which MUST
#               show the strip (proves the scenario can detect it)
#   af        — the playtest entry's branch build, which must NOT
# Per leg the scenario drives every overlay the issue names — session-created,
# help (?), search (/), the attach gate (o), and the delete confirmation (D) —
# captures the 80x24 frame of each, and asserts no non-blank cell appears
# outside the modal's rectangle on any row it occupies. Provenance (git SHAs +
# binary sha256 + the running TUI's /proc/<pid>/exe sha256) is recorded so
# every capture can be tied to exact code.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
export AF_DRIVER_COLS=80 AF_DRIVER_ROWS=24

# The control must stay a KNOWN-BUGGY revision: once this fix merges, archiving
# the moving origin/master would build a fixed control that can never show the
# strip, and the master leg would fail forever after. `testbox.sh scenario`
# archives this pinned SHA in place of origin/master; a manual run passes the
# same SHA as AF_MASTER_SHA with a tar of it. 78d03145 is the master the fix
# branches from (post-v1.0.312, which the issue was filed against).
AF_CONTROL_SHA=78d031456fa890b943a89524a8d3d0adff92883d

EVID="$HOME/sandbox/overlay-strip-5297"
# Fresh evidence per run: a retained sandbox's rerun must not mix this run's
# provenance with a prior run's captures.
case "$EVID" in */sandbox/*) rm -rf "$EVID" ;; esac
mkdir -p "$EVID"

# --- provenance --------------------------------------------------------------
# /src is a linked worktree: its .git is a FILE pointing at the host's gitdir,
# unreachable inside the container, so git cannot answer rev-parse/archive
# here. `testbox.sh scenario` sees this script consume them and supplies the
# two SHAs and the master source as a tar:
#   AF_BRANCH_SHA  — `git rev-parse HEAD` of the worktree mounted at /src
#   AF_MASTER_SHA  — the control revision: the script's pinned AF_CONTROL_SHA
#                    when declared, else `git rev-parse origin/master`
#   AF_MASTER_TAR  — container path of `git archive <control-sha>` output
# (falling back to in-container git only if a non-worktree /src ever has one).
git config --global --add safe.directory /src 2>/dev/null || true
BRANCH_SHA="${AF_BRANCH_SHA:-$(git -C /src rev-parse HEAD 2>/dev/null || true)}"
MASTER_SHA="${AF_MASTER_SHA:-$(git -C /src rev-parse origin/master 2>/dev/null || true)}"
[ -n "$BRANCH_SHA" ] || { _af_fail "branch SHA unknown — pass AF_BRANCH_SHA"; exit 1; }
[ -n "$MASTER_SHA" ] || { _af_fail "master SHA unknown — pass AF_MASTER_SHA"; exit 1; }
if [ "$MASTER_SHA" != "$AF_CONTROL_SHA" ]; then
    _af_log "note: control SHA is $MASTER_SHA, not the pinned $AF_CONTROL_SHA — running anyway"
fi
# Rebuild the branch binary from the mounted tree before it is measured:
# under AF_SELFTEST_NAME sandbox reuse, ~/bin/af can still be the boot build
# of an older HEAD while provenance.txt names the current one. In-place with
# -buildvcs=false, exactly as the entry script does.
(cd /src && go build -buildvcs=false -o "$HOME/bin/af" .)
MASTER_SRC="$(mktemp -d /tmp/af-master.XXXXXX)"
# Clean the extracted tree on ANY exit, not just the happy path — in a retained
# sandbox a failed tar/build (set -e exits before a trailing rm) would strand a
# full repo copy per attempt.
trap 'rm -rf "$MASTER_SRC"' EXIT
if [ -n "${AF_MASTER_TAR:-}" ]; then
    tar -xf "$AF_MASTER_TAR" -C "$MASTER_SRC"
elif ! git -C /src archive "$MASTER_SHA" | tar -x -C "$MASTER_SRC"; then
    _af_fail "no master source: pass AF_MASTER_TAR (or give /src a reachable gitdir)"
    exit 1
fi
(cd "$MASTER_SRC" && go build -buildvcs=false -o "$HOME/bin/af.master" .)
{
    echo "branch HEAD (bin/af):      $BRANCH_SHA"
    echo "control (af.master):       $MASTER_SHA (pinned: $AF_CONTROL_SHA)"
    sha256sum "$HOME/bin/af" "$HOME/bin/af.master"
} | tee "$EVID/provenance.txt"

# --- helpers -----------------------------------------------------------------

# tui_provenance <label> <bin> — prove WHICH binary the running TUI is (a leg
# that silently ran the wrong binary voids the control). The TUI is launched as
# `cd <repo> && <abs bin>` — a direct child of the drive pane's shell — so
# resolve that child's /proc/<pid>/exe and sha256 it against the binary this
# leg intended. Fails loud on any mismatch.
tui_provenance() {
    local label="$1" bin="$2" pane_pid tui_pid exe sha want comm
    pane_pid="$(tmux display-message -p -t "$AF_DRIVER_SESSION" '#{pane_pid}')"
    comm="$(basename "$bin")"
    tui_pid="$(pgrep -P "$pane_pid" -x "$comm" 2>/dev/null | head -1 || true)"
    if [ -z "$tui_pid" ]; then
        tui_pid="$(ps -eo pid=,ppid=,args= 2>/dev/null | awk -v pp="$pane_pid" -v b="$bin" '$2 == pp && $3 == b { print $1; exit }' || true)"
    fi
    [ -n "$tui_pid" ] || { _af_fail "$label: no TUI process found (child of pane shell $pane_pid named '$comm')"; return 1; }
    exe="$(readlink "/proc/$tui_pid/exe")"
    sha="$(sha256sum "$exe" | cut -d' ' -f1)"
    want="$(sha256sum "$bin" | cut -d' ' -f1)"
    {
        echo "$label tui pid:    $tui_pid"
        echo "$label tui exe:    $exe"
        echo "$label tui sha256: $sha"
        echo "$label bin sha256:  $want ($bin)"
    } | tee "$EVID/$label-tui-provenance.txt"
    [ "$exe" = "$bin" ] || { _af_fail "$label: running TUI exe '$exe' != intended '$bin'"; return 1; }
    [ "$sha" = "$want" ] || { _af_fail "$label: running TUI sha256 != $bin"; return 1; }
}

# modal_margins <capture-file> <anchor> — the assertion this scenario exists
# for: on every row the modal occupies, the cells OUTSIDE its box must be blank.
#
# The box is located from the anchor row itself: the modal's interior pads with
# blanks, so the last border glyph before the anchor is its own left edge —
# never a chopped background border, which cannot appear inside the modal's
# (opaque) rectangle on any build. The top border row is found by walking up
# that column through '│' until it isn't one (the '╭' row), and the box width
# is measured from that row's contiguous '╭─…─╮' — immune to any stray '│'/'╮'
# the background left past the modal's right edge. The bottom is the mirror
# walk down to '╰'.
#
# Prints the band geometry and every offending fragment; exit 1 when a margin
# is non-blank (the strip), 0 when clean, 2 when the box cannot be located.
modal_margins() {
    python3 - "$1" "$2" <<'PY'
import re, sys

path, anchor = sys.argv[1], sys.argv[2]
lines = open(path, encoding='utf-8').read().split('\n')

ai = next((i for i, l in enumerate(lines) if anchor in l), None)
if ai is None:
    print(f"anchor {anchor!r} not on screen", file=sys.stderr)
    sys.exit(2)
row = lines[ai]
start = row.index(anchor)
left = max(row.rfind('│', 0, start), row.rfind('╭', 0, start), row.rfind('╰', 0, start))
if left < 0:
    print(f"no modal border left of {anchor!r} on row {ai}: {row!r}", file=sys.stderr)
    sys.exit(2)

top = ai
while top > 0 and left < len(lines[top]) and lines[top][left] == '│':
    top -= 1
m = re.match(r'╭─*╮', lines[top][left:]) if left < len(lines[top]) else None
if not m:
    print(f"no '╭─…─╮' top border at col {left} on row {top}: {lines[top]!r}",
          file=sys.stderr)
    sys.exit(2)
right = left + m.end() - 1

bot = ai
while bot + 1 < len(lines) and left < len(lines[bot]) and lines[bot][left] == '│':
    bot += 1

print(f"anchor '{anchor}' on row {ai}; modal band rows {top}..{bot}, box cols {left}..{right}")
bad = 0
for r in range(top, bot + 1):
    for side, frag in (('left', lines[r][:left]), ('right', lines[r][right + 1:])):
        if frag.strip():
            n = sum(1 for c in frag if c != ' ')
            bad += n
            print(f"  row {r:2d} {side:5s} margin ({n} non-blank cells): {frag.rstrip()!r}")
print(f"OFFENDING-CELLS {bad}")
sys.exit(1 if bad else 0)
PY
}

# purge_archived — af_reset_sandbox wipes instances/ + sibling worktrees and
# deletes every non-master branch, but archived worktrees live under
# $AGENT_FACTORY_HOME/archived/ — NOT in its wipe list — and git refuses
# `branch -D` on a branch still checked out there. A re-run then collides:
# "dev/<name> is already checked out at .../archived/...". Drop the archived
# dir, prune the registrations, and delete the now-freed branches. Same
# fail-closed sandbox-path guard af_reset_sandbox uses.
purge_archived() {
    case "$AGENT_FACTORY_HOME" in
        */sandbox/* | */sandbox | /tmp/* | */af-driver*) ;;
        *)
            _af_fail "purge_archived: refusing — '$AGENT_FACTORY_HOME' is not a sandbox path"
            return 1
            ;;
    esac
    rm -rf "$AGENT_FACTORY_HOME/archived"
    if [ -d "$AF_DRIVER_REPO/.git" ]; then
        git -C "$AF_DRIVER_REPO" worktree prune 2>/dev/null || true
        local b
        for b in $(git -C "$AF_DRIVER_REPO" for-each-ref \
            --format='%(refname:short)' refs/heads/ 2>/dev/null \
            | grep -vE '^(master|main)$' || true); do
            git -C "$AF_DRIVER_REPO" branch -D "$b" 2>/dev/null || true
        done
    fi
}

# --- legs --------------------------------------------------------------------

run_leg() { # <master|branch> <binary>
    local label="$1"
    LEG="$label"
    export AF_DRIVER_BIN="$2"

    af_reset_sandbox
    purge_archived
    af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'
    # Clear seen-bits 2 (instance-start) and 4 (instance-attach) so `n` surfaces
    # the "Session created" screen and `o` the attach gate. General (1) and
    # interactive (8) stay marked — the former is driven with '?' on demand and
    # the latter must not gate the pane-content step below.
    AF_DRIVER_HELP_SEEN=9 af_boot
    # Prove the running TUI is THIS leg's binary before trusting any capture
    # (the control is void if it silently ran the branch build).
    tui_provenance "$label" "$2" || return 1

    # --- overlay 1: session-created (first-run screen) -----------------------
    # af_new_instance's ready-marker wait could hide UNDER the 22-row modal, so
    # drive the naming form by hand and sync on the overlay itself.
    af_ensure_nav
    af_focus_tree || return 1
    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'name prompt' || return 1
    af_wait_gone '[Ss]ession [Nn]ame:' 1 'old name prompt label' || return 1
    af_send_literal "bleed-$label"
    af_send Enter
    af_wait_for 'Session created' "$AF_DRIVER_TIMEOUT" 'session-created overlay' || return 1
    modal_margins_capture 'Session created' 'session-created'
    af_send Escape
    af_wait_gone 'Session created' "$AF_DRIVER_TIMEOUT" 'session-created dismissed' || return 1
    af_wait_for "bleed-$label.*●" "$AF_DRIVER_TIMEOUT" 'instance ready' || return 1

    # Give the workspace real content behind the next overlays: open the
    # session's pane, type a marker line into its shell, then hand focus back
    # to the tree so nav keys drive the TUI again. Under the bug this is
    # exactly the text that bleeds past the modal's right border.
    af_select "bleed-$label" || return 1
    af_open_pane || return 1
    af_enter_interactive || return 1
    af_send_to_pane "echo STRIP_PROBE_${label}_0123456789abcdef" || return 1
    af_exit_interactive || return 1
    af_focus_tree || return 1
    af_select "bleed-$label" || return 1

    # --- overlay 2: general help (?) -----------------------------------------
    af_send '?'
    af_wait_for 'Manage agents in separate workspaces' "$AF_DRIVER_TIMEOUT" 'help overlay' || return 1
    modal_margins_capture 'Manage agents in separate workspaces' 'help'
    af_send Escape
    af_wait_gone 'Manage agents in separate workspaces' "$AF_DRIVER_TIMEOUT" 'help dismissed' || return 1

    # --- overlay 3: session search (/) ---------------------------------------
    af_send '/'
    af_wait_for 'Search sessions' "$AF_DRIVER_TIMEOUT" 'search overlay' || return 1
    modal_margins_capture 'Search sessions' 'search'
    af_send Escape
    af_wait_gone 'Search sessions' "$AF_DRIVER_TIMEOUT" 'search dismissed' || return 1

    # --- overlay 4: attach gate (o) ------------------------------------------
    af_send 'o'
    af_wait_for 'Attaching to session' "$AF_DRIVER_TIMEOUT" 'attach gate' || return 1
    modal_margins_capture 'Attaching to session' 'attach-gate'
    af_send Escape
    af_wait_gone 'Attaching to session' "$AF_DRIVER_TIMEOUT" 'attach gate cancelled' || return 1

    # --- overlay 5: delete confirmation (D) ----------------------------------
    af_send 'D'
    af_wait_for "Delete session" "$AF_DRIVER_TIMEOUT" 'delete confirmation' || return 1
    modal_margins_capture 'Delete session' 'delete-confirm'
    af_send Escape
    af_wait_gone 'Delete session' "$AF_DRIVER_TIMEOUT" 'delete confirmation cancelled' || return 1

    af_quit || return 1
}

# modal_margins_capture <anchor> <name> — capture + check, bookkeeping the
# offending cell count into the leg's verdict.
modal_margins_capture() {
    local anchor="$1" name="$2" rc cells
    sleep 1 # settle past the capture tick so the frame is the steady state
    af_capture >"$EVID/$LEG-$name.txt"
    set +e
    modal_margins "$EVID/$LEG-$name.txt" "$anchor" | tee "$EVID/$LEG-$name-margins.txt"
    rc=${PIPESTATUS[0]}
    set -e
    if [ "$rc" -eq 2 ]; then
        _af_fail "$LEG/$name: could not locate the modal box — the capture is unusable"
        af_capture >&2
        return 1
    fi
    if [ "$rc" -eq 0 ]; then
        _af_log "$LEG/$name: clean — no background cells beside the modal"
    else
        cells="$(sed -nE 's/^OFFENDING-CELLS ([0-9]+).*/\1/p' "$EVID/$LEG-$name-margins.txt")"
        STRIP_CELLS=$((STRIP_CELLS + ${cells:-0}))
        _af_log "$LEG/$name: $cells non-blank background cells beside the modal (see $EVID/$LEG-$name-margins.txt)"
    fi
    return 0
}

STRIP_CELLS=0
run_leg master "$HOME/bin/af.master"
master_cells="$STRIP_CELLS"
STRIP_CELLS=0
run_leg branch "$HOME/bin/af"
branch_cells="$STRIP_CELLS"

{
    echo "master leg: $master_cells non-blank background cells beside modal boxes"
    echo "branch leg: $branch_cells non-blank background cells beside modal boxes"
} | tee "$EVID/verdict.txt"

# The control MUST reproduce the defect — else the scenario cannot see it and
# the branch leg proves nothing.
if [ "$master_cells" -eq 0 ]; then
    _af_fail "control leg: expected chopped background beside at least one modal on master — got none"
    exit 1
fi
if [ "$branch_cells" -ne 0 ]; then
    _af_fail "branch leg: $branch_cells background cells still render beside modal boxes (#5297 not fixed)"
    af_capture >&2 || true
    exit 1
fi

printf 'PASS #5297: master control shows the chopped-background strip (%d cells); branch renders blank margins beside every modal\n' "$master_cells"
