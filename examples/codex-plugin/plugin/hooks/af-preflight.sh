#!/usr/bin/env bash
# Report whether the af CLI is on PATH. Read-only on purpose.
#
# This hook deliberately does NOT download or install anything. A plugin hook
# runs as the user, before the user has any reason to trust it, so fetching and
# executing a release binary from here is the wrong shape — see the exploration
# issue this example belongs to. Installing af stays an explicit user action.
set -euo pipefail

if command -v af >/dev/null 2>&1; then
	# `af version` can print a second "an upgrade is available" line; the hook
	# only wants the version itself.
	version=$(af version 2>/dev/null | head -n 1)
	echo "${version:-af (version unknown)} is available."
else
	echo "af is not installed. Install it with:"
	echo "  curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | bash"
fi
