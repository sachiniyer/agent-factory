#!/bin/sh
# Build the release checksum manifest from the complete four-platform artifact set.
# Kept in one script so stable and preview releases cannot drift.

set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: write-sha256sums.sh <artifact-directory>" >&2
	exit 2
fi

artifact_dir=$1
expected_assets="
agent-factory-darwin-amd64.tar.gz
agent-factory-darwin-arm64.tar.gz
agent-factory-linux-amd64.tar.gz
agent-factory-linux-arm64.tar.gz
"

for asset in $expected_assets; do
	if [ ! -f "$artifact_dir/$asset" ]; then
		echo "error: release artifact is missing: $artifact_dir/$asset" >&2
		exit 1
	fi
done

set -- "$artifact_dir"/agent-factory-*.tar.gz
if [ "$#" -ne 4 ]; then
	echo "error: expected exactly 4 release archives in $artifact_dir, found $#" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	hash_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	hash_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	echo "error: sha256sum or shasum is required to build release checksums" >&2
	exit 1
fi

manifest_tmp="$artifact_dir/.sha256sums.txt.$$"
cleanup() { rm -f "$manifest_tmp"; }
trap cleanup EXIT INT TERM

: > "$manifest_tmp"
for asset in $expected_assets; do
	digest=$(hash_file "$artifact_dir/$asset")
	printf '%s  %s\n' "$digest" "$asset" >> "$manifest_tmp"
done
mv "$manifest_tmp" "$artifact_dir/sha256sums.txt"
trap - EXIT INT TERM
