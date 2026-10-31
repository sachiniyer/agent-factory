#!/usr/bin/env bash
# Regenerate the app/testdata/design goldens a change moved (#4180's Max runs
# row), inside the playtest sandbox. Captures the full scene matrix, reports
# which goldens differ from the committed files, then emits the differing
# .svg/.ansi pairs as a base64 tarball on stdout for the caller to unpack and
# inspect before committing.
set -euo pipefail

OUT=/tmp/design-capture
rm -rf "$OUT" && mkdir -p "$OUT"
cd /src

AF_TUI_DESIGN_CAPTURE="$OUT" go test -buildvcs=false ./app -run 'TestDesignDriverScenes' -count=1

echo "=== goldens differing from app/testdata/design/ ===" >&2
changed=0
for f in "$OUT"/*.svg; do
	name=$(basename "$f")
	if ! cmp -s "$f" "/src/app/testdata/design/$name"; then
		echo "DIFFERS: $name" >&2
		changed=$((changed + 1))
	fi
done
echo "=== $changed changed ===" >&2

cd "$OUT" && tar czf - *.svg *.ansi | base64
