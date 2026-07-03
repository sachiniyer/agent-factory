#!/bin/sh
# validate-stable-version.sh — validate a manually requested stable version.
#
# Usage: git tag | validate-stable-version.sh <version>
#
# <version> is the bare semver, no leading "v" (e.g. "1.1.0"). Existing git
# tags are read one per line on stdin. Exits 0 if the version is well-formed,
# not already tagged, and strictly greater than the latest stable tag;
# exits 1 with a reason on stderr otherwise.
#
# Preview tags (vX.Y.Z-preview-N, see next-preview-version.sh) are ignored
# when finding the latest stable: releasing the current preview base as a
# stable (e.g. 1.0.138 while 1.0.138-preview-9 exists) is a supported way to
# promote what the preview channel has been testing.
set -eu

new="${1:?usage: git tag | validate-stable-version.sh <version>}"
tags=$(cat)

if ! printf '%s\n' "$new" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
	echo "error: version '$new' must be X.Y.Z with no leading v (e.g. 1.1.0)" >&2
	exit 1
fi

if printf '%s\n' "$tags" | grep -qx "v$new"; then
	echo "error: tag v$new already exists" >&2
	exit 1
fi

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

newer=$(awk -v n="$new" -v s="$stable" 'BEGIN {
	split(n, a, "."); split(s, b, ".")
	for (i = 1; i <= 3; i++) {
		if (a[i] + 0 > b[i] + 0) { print "yes"; exit }
		if (a[i] + 0 < b[i] + 0) { print "no"; exit }
	}
	print "no"
}')

if [ "$newer" != "yes" ]; then
	echo "error: version $new must be greater than the latest stable $stable" >&2
	exit 1
fi

echo "ok: v$new (latest stable: v$stable)"
