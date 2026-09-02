#!/usr/bin/env bash
set -uo pipefail
source /src/scripts/tui-driver.sh
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"
export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
af_reset_sandbox
af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'
af_boot
af_new_instance 'fam👨‍👩‍👧‍👦zzz' || af_new_instance famzzz

_cell_col() {
    python3 - "$1" "$2" <<'PY'
import sys, unicodedata
line, needle = sys.argv[1], sys.argv[2]
i = line.find(needle)
if i < 0:
    print(-1); raise SystemExit
def w(ch):
    if ch == '‍' or unicodedata.combining(ch):
        return 0
    return 2 if unicodedata.east_asian_width(ch) in ('W','F') else 1
print(sum(w(c) for c in line[:i]))
PY
}

echo "PROBE === open the kill confirmation ==="
af_ensure_nav; af_focus_tree
# walk to a session row
for i in 1 2 3 4 5 6; do
    af_capture | grep -qF 'D kill' && break
    af_send j; sleep 0.3
done
af_send D
sleep 1.5
echo "PROBE --- confirmation on screen ---"
af_capture | grep -nE 'confirm|cancel' | head -5

row=$(af_capture | grep -nF 'cancel' | head -1 | cut -d: -f1)
line=$(af_capture | sed -n "${row}p")
col=$(_cell_col "$line" 'cancel')
echo "PROBE row=$row col=$col"
echo "PROBE line=[$line]"

echo "PROBE === click FAR from the zone (col 2) ==="
af_click 2 "$row"; sleep 0.8
af_capture | grep -qE 'confirm|cancel' && echo "PROBE still open (good)" || echo "PROBE CLOSED on far click (bad oracle)"

echo "PROBE === click ON the cancel words ==="
af_click "$((col + 2))" "$row"; sleep 1.2
af_capture | grep -qE 'confirm|cancel' && echo "PROBE STILL OPEN after clicking the words" || echo "PROBE DISMISSED by clicking the words"
