#!/bin/sh
# next-preview-version.sh — compute the next preview release version.
#
# Usage: git tag | next-preview-version.sh
#
# Reads existing git tags (one per line) on stdin and prints the next preview
# version without a leading "v", e.g. "1.0.138-preview-3".
#
# Release scheme (#1041): stable releases are vX.Y.Z and are cut manually.
# Previews are cut automatically off master as vX.Y.(Z+1)-preview-N, where
# X.Y.Z is the latest stable tag. Basing previews on the *next* patch keeps
# standard semver precedence across both channels:
#   1.2.0 < 1.2.1-preview-1 < 1.2.1-preview-2 < 1.2.1 <= any newer stable
# N increments per preview and resets to 1 whenever a new stable changes the
# base. Tags that match neither shape are ignored.
set -eu

tags=$(cat)

# Latest stable: highest vX.Y.Z tag by numeric (not lexical) comparison.
stable=$(printf '%s\n' "$tags" | awk '
	/^v[0-9]+\.[0-9]+\.[0-9]+$/ {
		split(substr($0, 2), p, ".")
		a = p[1] + 0; b = p[2] + 0; c = p[3] + 0
		if (!found || a > M || (a == M && (b > m || (b == m && c > s)))) {
			M = a; m = b; s = c; found = 1
		}
	}
	END { if (found) print M "." m "." s; else print "0.0.0" }
')

base=$(printf '%s\n' "$stable" | awk -F. '{ print $1 "." $2 "." $3 + 1 }')

next=$(printf '%s\n' "$tags" | awk -v base="$base" '
	BEGIN { pre = "v" base "-preview-" }
	index($0, pre) == 1 {
		n = substr($0, length(pre) + 1)
		if (n ~ /^[0-9]+$/ && n + 0 > z) z = n + 0
	}
	END { print z + 1 }
')

echo "${base}-preview-${next}"
