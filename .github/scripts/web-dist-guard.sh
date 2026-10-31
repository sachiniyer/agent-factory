#!/usr/bin/env bash
# Validate an UNTRUSTED web/dist tree before the write-capable commit job copies
# it into the repository (web-dist-sync.yml, #6447).
#
# The tree was produced by `npm ci && make web-build` in a job that runs repo
# scripts, so the commit job treats it as data: only plain files and directories,
# names from a small safe alphabet, a bounded size, and index.html present. Any
# other shape (a symlink, a device, a `..` or leading-dash name, an oversize file)
# fails closed, because the next step copies this tree into a commit pushed with a
# token that can write to the repo.
#
# Usage: web-dist-guard.sh <dir>
set -euo pipefail

dir="${1:?usage: web-dist-guard.sh <dir>}"
max_files=200
max_total=$((24 * 1024 * 1024))
max_file=$((8 * 1024 * 1024))

if [ ! -d "$dir" ] || [ -L "$dir" ]; then
	echo "guard: $dir is not a real directory" >&2
	exit 1
fi

# Anything that is not a regular file or a directory (symlink, fifo, socket,
# device, hard-link oddities included) is refused.
odd="$(find "$dir" -mindepth 1 ! -type f ! -type d -print -quit)"
if [ -n "$odd" ]; then
	echo "guard: refusing non-regular entry: $odd" >&2
	exit 1
fi

name_re='^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$'
export LC_ALL=C

# Names relative to the tree: letters, digits, dot, underscore, dash, slash. No
# empty, `.`/`..` component, no leading dash or dot component (so no .git, no
# .gitattributes, no option-looking names).
while IFS= read -r -d '' p; do
	rel="${p#"$dir"/}"
	# A whole-string match: grep is line-based and would pass "a<newline>b".
	if ! [[ "$rel" =~ $name_re ]]; then
		echo "guard: refusing unsafe name: $rel" >&2
		exit 1
	fi
done < <(find "$dir" -mindepth 1 -print0)

count="$(find "$dir" -type f | wc -l)"
if [ "$count" -gt "$max_files" ]; then
	echo "guard: $count files exceeds $max_files" >&2
	exit 1
fi

total=0
while IFS= read -r -d '' f; do
	size="$(wc -c <"$f")"
	if [ "$size" -gt "$max_file" ]; then
		echo "guard: $f is $size bytes (> $max_file)" >&2
		exit 1
	fi
	total=$((total + size))
done < <(find "$dir" -type f -print0)
if [ "$total" -gt "$max_total" ]; then
	echo "guard: tree is $total bytes (> $max_total)" >&2
	exit 1
fi

[ -f "$dir/index.html" ] || { echo "guard: no index.html: not a web bundle" >&2; exit 1; }
echo "guard: ok ($count files, $total bytes)"
