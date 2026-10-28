#!/bin/sh
# assert-binary-version.sh — fail unless a built af binary reports the release
# version it was supposed to be stamped with.
#
# Usage: assert-binary-version.sh <binary> <expected-version>
#
# <expected-version> is the bare version the release workflow built with, no
# leading "v" (e.g. "1.1.0" or "1.1.1-preview-3") — the same string handed to
# `go build -ldflags "-X main.version=..."`. The binary's `version` subcommand
# must print exactly "agent-factory version <expected-version>" as its first
# line.
#
# Exit status alone proves nothing here (#5666). An empty -X stamp does not
# fail the build or the binary: commands.NewRootCommand ignores an empty
# version and the package falls back to "dev", so `version` prints
# "agent-factory version dev" and exits 0. stable-release.yml's static build
# step shipped exactly that, because its smoke test only checked the exit
# status. Both release workflows run this one script, so stable and preview
# releases cannot drift apart the way their two copies of that step did.
#
# An empty or "dev" <expected-version> is refused rather than compared: both
# are what a smoke-test step that never received the release version would
# pass, and a check that matches the fallback proves nothing.
set -eu

if [ "$#" -ne 2 ]; then
	echo "usage: assert-binary-version.sh <binary> <expected-version>" >&2
	exit 2
fi

binary=$1
expected=$2

case "$expected" in
	"" | dev)
		echo "::error::cannot check ${binary}: the expected version is '${expected}', which is not a release version. The smoke-test step did not receive the release VERSION (#5666)." >&2
		exit 2
		;;
esac

if ! out=$("$binary" version); then
	echo "::error::'${binary} version' failed: the artifact cannot run." >&2
	exit 1
fi
printf '%s\n' "$out"

first=$(printf '%s\n' "$out" | sed -n '1p')
want="agent-factory version ${expected}"
if [ "$first" != "$want" ]; then
	hint=""
	if [ "$first" = "agent-factory version dev" ]; then
		hint=" It fell back to 'dev': the build step stamped no release version (an empty -X main.version, #5666)."
	fi
	echo "::error::${binary} reports '${first}', want '${want}'.${hint}" >&2
	exit 1
fi
echo "ok: ${binary} reports ${expected}"
