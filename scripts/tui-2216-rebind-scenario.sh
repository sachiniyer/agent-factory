#!/usr/bin/env bash
# Real-TUI play-test for the project.rebind parity gap (#2216 leftover, the
# daemon-backed rebind PR): `af projects rebind` used to be CLI-only, so a TUI
# user whose registered checkout moved had no repair that stayed in the UI.
#
#   scripts/testbox.sh scenario scripts/tui-2216-rebind-scenario.sh
#
# Drives the whole repair through the picker: the moved registration renders
# `· missing`, `b` opens the rebind path field on that registry-backed row, a
# daemon rejection stays inline for correction, and the good path rebinds —
# same stable prj_ id, new root, picker closes.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
A="$HOME/sandbox/proj-a"
MOVED="$HOME/sandbox/proj-a-moved"
NOGIT="$HOME/sandbox/not-a-repo"

af="$(_af_resolve_bin)"
deadline=$(( $(_af_now) + 180 ))
while [ ! -x "$af" ]; do
    [ "$(_af_now)" -ge "$deadline" ] && { echo "af binary never appeared" >&2; exit 1; }
    sleep 1
done

# Fixture: register a repo, then MOVE it — the record's root is now absent
# (path_exists=false) and its replacement exists, exactly the state rebind
# repairs. A whole-checkout move carries the marker, so the daemon keeps the
# checkout id and only the root changes.
git init -q -b master "$A"
git -C "$A" config user.email t@t
git -C "$A" config user.name t
git -C "$A" commit -q --allow-empty -m initial
mkdir -p "$NOGIT"

# Give the sandbox daemon an HTTP listener BEFORE it first starts (the first
# af command below auto-ensures it), so the remote-target leg can point the
# TUI at a LIVE daemon — a dead URL never gets the TUI past its connect
# spinner, and the refusal has to be observed on a daemon that answers.
printf '\n[network]\nlisten_addr = "127.0.0.1:9477"\nrequire_token = false\n' >> "$AGENT_FACTORY_HOME/config.toml"

echo "=== registering proj-a ==="
"$af" projects add "$A"
pid="$("$af" projects list | grep -o '"id": "[^"]*"' | head -1 | cut -d'"' -f4)"
[ -n "$pid" ] || { echo "FIXTURE INVALID: no project id in list output" >&2; exit 1; }
echo "=== registered id: $pid ==="
mv "$A" "$MOVED"

af_boot || exit 1
af_ensure_nav
af_send C-p
af_wait_for 'Switch project' "$AF_DRIVER_TIMEOUT" 'project picker overlay' || exit 1

# 1. The moved registration renders as the repairable state.
af_assert_screen 'proj-a.*missing' || { af_capture; exit 1; }
echo "assert OK: the moved registration is marked missing"

# 2. Put the cursor on proj-a's row: anchor at the top (k is idempotent),
#    then step down until the ▸ marker sits on the row that needs repair.
af_send k k k
i=0
until af_capture | grep -qE '▸.*proj-a'; do
    i=$((i + 1))
    [ "$i" -gt 10 ] && { echo "FAIL: cursor never reached the proj-a row" >&2; af_capture; exit 1; }
    af_send j
    sleep "$AF_DRIVER_POLL"
done
echo "assert OK: cursor on the missing registry row"

# 3. `b` opens rebind mode on a registry-backed row.
af_send b
af_wait_for 'New checkout path' "$AF_DRIVER_TIMEOUT" 'rebind prompt' || exit 1

# 4. A non-git path is refused and the rejection renders INLINE — the picker
#    stays in rebind mode for correction rather than closing or toasting.
af_send_literal "$NOGIT"
af_send Enter
af_wait_for 'git common directory' "$AF_DRIVER_TIMEOUT" 'inline daemon rejection' || { af_capture; exit 1; }
af_assert_screen 'New checkout path' || { af_capture; exit 1; }
echo "assert OK: daemon rejection is inline and rebind mode is still open"

# 5. Esc backs out to the list (clearing the field), `b` re-enters.
af_send Escape
af_wait_gone 'New checkout path' "$AF_DRIVER_TIMEOUT" 'rebind prompt closed' || exit 1
af_send b
af_wait_for 'New checkout path' "$AF_DRIVER_TIMEOUT" 'rebind prompt reopened' || exit 1

# 6. Submit the replacement checkout — and double-Enter on top of it. Pending
#    is marked in the same Update that hands the request off, so the second
#    Enter (and any edits) is inert: at most one rebind is in flight. On a
#    local daemon the answer lands in milliseconds, so the freeze window is
#    brief — the unit test carries the property; this drives the gesture.
af_send_literal "$MOVED"
af_send Enter Enter
af_wait_for 'Rebound project' "$AF_DRIVER_TIMEOUT" 'rebind success toast' || { af_capture; exit 1; }
echo "assert OK: success toast (double-Enter produced one request)"

newroot="$("$af" projects list | grep -o '"root": "[^"]*"' | head -1 | cut -d'"' -f4)"
newid="$("$af" projects list | grep -o '"id": "[^"]*"' | head -1 | cut -d'"' -f4)"
echo "=== after rebind: id=$newid root=$newroot ==="
[ "$newid" = "$pid" ] || { echo "FAIL: the stable id changed ($pid -> $newid)" >&2; exit 1; }
[ "$newroot" = "$MOVED" ] || { echo "FAIL: root is $newroot, want $MOVED" >&2; exit 1; }

# 7. Remote target: point the TUI at the sandbox daemon's HTTP listener via
#    AF_DAEMON_URL — a LIVE daemon that answers, so the TUI boots for real and
#    remote mode is decided exactly as for a remote host. The picker's rows
#    still come from the LOCAL registry (the exact mismatch the refusal guards:
#    this record's id DOES exist there — the worst case, where an unguarded
#    rebind would mutate it), and `b` must refuse with the actionable message
#    rather than send a client-side id + path.
af_quit 2>/dev/null || true
export AF_DRIVER_LAUNCH_ENV="AF_DAEMON_URL=http://127.0.0.1:9477"
af_boot || { echo "FAIL: remote-mode boot" >&2; exit 1; }
af_send C-p
af_wait_for 'Switch project' "$AF_DRIVER_TIMEOUT" 'project picker overlay (remote)' || { af_capture; exit 1; }

# Cursor onto the (still registry-backed, still local) proj-a row.
af_send k k k
i=0
until af_capture | grep -qE '▸.*proj-a'; do
    i=$((i + 1))
    [ "$i" -gt 10 ] && { echo "FAIL: remote picker never reached the proj-a row" >&2; af_capture; exit 1; }
    af_send j
    sleep "$AF_DRIVER_POLL"
done
af_send b
af_wait_for 'local registry' "$AF_DRIVER_TIMEOUT" 'remote rebind refusal' || { af_capture; exit 1; }
af_assert_screen 'af projects rebind' || { af_capture; exit 1; }
echo "assert OK: remote target refuses rebind with the actionable message"

echo "PASS: project picker rebind — missing marker, inline rejection, stable-id move, single-flight, remote refusal"
