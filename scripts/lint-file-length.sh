#!/usr/bin/env bash
# lint-file-length.sh — structural-health lint (#1145).
#
# Fails if any tracked Go source file is longer than its line limit, unless
# the file is grandfathered in scripts/file-length-allowlist.txt.
#
# WHAT WE COUNT: total physical lines (`wc -l`), not non-blank/non-comment
# lines. Total lines is exactly what a reader scrolls through and what an
# editor's gutter reports; it needs no Go parsing, and it's stable and
# unambiguous. A file padded with blanks/comments to 1000+ lines is still a
# file worth a second look, so counting them is a feature, not a bug.
#
# LIMITS:
#   production .go : 1000 lines
#   *_test.go      : 1500 lines  — table-driven tests run legitimately longer,
#                                  so they get a higher ceiling but are still
#                                  bounded (a 3000-line test file is still a
#                                  decomposition candidate).
#
# ALLOWLIST (scripts/file-length-allowlist.txt): files already over their
# limit when this lint landed. Each entry pins a CEILING = the file's size at
# grandfathering time. A grandfathered file may not grow past its ceiling
# (ratchet — the big files can only shrink), and once decomposed back under
# the base limit its entry must be removed (this script fails if a
# grandfathered file has dropped to/under the limit, so the list can't rot).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MAX_PROD=1000
MAX_TEST=1500
ALLOWLIST="scripts/file-length-allowlist.txt"

# Report a failure line and flip the exit flag. Uses ::error:: so GitHub
# Actions surfaces it as an annotation; harmless as plain text locally.
fail=0
err() {
	echo "::error::$*"
	fail=1
}

base_limit() {
	case "$1" in
	*_test.go) echo "$MAX_TEST" ;;
	*) echo "$MAX_PROD" ;;
	esac
}

# Load the allowlist into path -> ceiling.
declare -A CEIL
if [[ -f "$ALLOWLIST" ]]; then
	while read -r path ceil _; do
		[[ -z "${path:-}" || "$path" == \#* ]] && continue
		if ! [[ "$ceil" =~ ^[0-9]+$ ]]; then
			err "$ALLOWLIST: malformed entry for '$path' (expected '<path> <ceiling>')"
			continue
		fi
		CEIL["$path"]="$ceil"
	done <"$ALLOWLIST"
fi

# 1. Enforce limits across every tracked Go file.
while IFS= read -r path; do
	lines=$(wc -l <"$path")
	limit=$(base_limit "$path")
	if [[ -n "${CEIL[$path]+x}" ]]; then
		ceil="${CEIL[$path]}"
		if ((lines > ceil)); then
			err "$path has $lines lines, over its grandfathered ceiling of $ceil. Do not grow a file already past the $limit-line limit — decompose it instead (#1145)."
		fi
	elif ((lines > limit)); then
		err "$path has $lines lines, over the $limit-line limit. Split it into smaller files; only grandfather it in $ALLOWLIST if a split is genuinely infeasible (#1145)."
	fi
done < <(git ls-files -- '*.go')

# 2. Keep the allowlist honest: entries must exist and must still be over the
#    base limit (otherwise the file has been decomposed and the entry is dead).
for path in "${!CEIL[@]}"; do
	if [[ ! -f "$path" ]]; then
		err "$ALLOWLIST lists '$path', which no longer exists — remove that entry."
		continue
	fi
	lines=$(wc -l <"$path")
	limit=$(base_limit "$path")
	if ((lines <= limit)); then
		err "$path is now $lines lines (<= $limit) — it's under the limit, so remove its entry from $ALLOWLIST."
	fi
done

if ((fail)); then
	echo "file-length lint failed. See docs/file-length-lint.md" >&2
	exit 1
fi

echo "file-length lint passed ($(git ls-files -- '*.go' | wc -l | tr -d ' ') Go files checked; ${#CEIL[@]} grandfathered)."
