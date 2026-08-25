#!/usr/bin/env bash
# Real-TUI regression for #3421, via:
#
#   scripts/testbox.sh scenario scripts/tui-3421-scenario.sh
#
# The config list truncated a value by RUNE COUNT while the terminal renders in
# CELLS, so a value holding CJK or emoji ran past the pane: at a 72-column pane
# a CJK path rendered an 85-cell row. This drives the real overlay with two such
# values seeded in config.toml and lets a REAL terminal be the oracle — an
# over-wide row wraps, and the wrap is what the assertions below look for.
#
# Three facts per value, and the middle one is the discriminator:
#
#   1. the head of the value renders on the key's row (it is on screen at all),
#   2. the truncation ellipsis is on the SAME row as the key — pre-fix the row
#      overflowed and its tail, ellipsis included, wrapped onto the next row,
#   3. a fragment deep inside the value is nowhere on screen: the pre-fix
#      rune-count budget kept it (then spilled it), the cell budget cuts it.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# 120x40 puts the config modal at its preferred width (0.6 * 120 = 72 content,
# ~68 inside the padding) — wide enough that the overlay is the centered box a
# user sees rather than the #1821 full-screen fallback.
export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

# A path of the shape the issue reports: user directory, project directories, a
# versioned build output. 53 runes, 35 of them double-width.
CJK_VALUE='/Users/田中太郎/项目目录/代理工厂/二进制文件/代码服务器/版本一二三/编译输出/最终目录/bin'
CJK_HEAD='田中太郎'
CJK_DEEP='版本一二三'

# DEEPMARK sits past the cell budget but inside the old rune budget, so it is
# the exact byte the two arithmetics disagree about.
EMOJI_VALUE='echo 🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀DEEPMARK🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀'
EMOJI_HEAD='echo 🚀'
EMOJI_DEEP='DEEPMARK'

af_reset_sandbox
af_set_config "default_program = \"claude\"
vscode_server_binary = \"$CJK_VALUE\"
on_archive_command = \"$EMOJI_VALUE\"

[program_overrides]
claude = \"bash\""

# _row_with <literal> — the first captured screen row containing a literal
# string. -F, not -E: a CJK/emoji needle must never be read as a regex, and the
# sandbox's C/POSIX locale makes any bracket expression byte-wise anyway. The
# `|| true` keeps a no-match from killing the run under `set -e`+pipefail; the
# callers below report the empty result themselves.
_row_with() { af_capture | grep -F -- "$1" | head -1 || true; }

# _reveal <key> — scroll the config list until the key's row is on screen. The
# list is windowed, so a key below the fold is simply not in the capture.
_reveal() {
    local key="$1" i
    for i in $(seq 1 80); do
        [ -n "$(_row_with "$key")" ] && return 0
        af_send j
        sleep "$AF_DRIVER_POLL"
    done
    _af_fail "#3421: never scrolled '$key' into view in the config list"
    return 1
}

# _assert_fits <key> <head> <deep> — the three facts above for one seeded value.
_assert_fits() {
    local key="$1" head="$2" deep="$3" row
    _reveal "$key" || return 1
    row="$(_row_with "$key")"
    _af_log "#3421 row: $row"

    if ! printf '%s' "$row" | grep -qF -- "$head"; then
        _af_fail "#3421: '$key' row does not carry the head of its value ('$head') — an over-wide row is word-wrapped, which moves the whole value off the key's line and leaves the value column blank: [$row]"
        af_capture >&2
        return 1
    fi
    if ! printf '%s' "$row" | grep -qF -- '…'; then
        _af_fail "#3421: '$key' row carries no truncation ellipsis — the row overflowed the pane, so its tail wrapped onto the next line: [$row]"
        af_capture >&2
        return 1
    fi
    if af_capture | grep -qF -- "$deep"; then
        _af_fail "#3421: '$deep' is on screen — it sits past the pane's cell budget and only a rune-count budget would paint it:"
        af_capture >&2
        return 1
    fi
    _af_log "assert OK: '$key' renders its wide-character value inside the pane"
}

af_boot
af_open_config
af_assert_screen 'esc close' 'the config overlay is up'

# `a` reveals the advanced tier, where on_archive_command lives.
af_send a
sleep "$AF_DRIVER_POLL"

_assert_fits vscode_server_binary "$CJK_HEAD" "$CJK_DEEP"
_assert_fits on_archive_command "$EMOJI_HEAD" "$EMOJI_DEEP"

# The box itself must still be whole: a wrapped row pushes the frame, which is
# the reason a too-wide line matters beyond looking untidy.
af_assert_screen '╰' 'the config modal still draws its bottom border'
af_assert_screen 'esc close' 'the hint row survived the wide-character rows'

echo "PASS: #3421 CJK and emoji config values render inside the pane at ${AF_DRIVER_COLS}x${AF_DRIVER_ROWS}"
