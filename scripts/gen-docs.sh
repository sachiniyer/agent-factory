#!/usr/bin/env bash
#
# Regenerate every generated artifact in the repo, so none of them can drift
# from the code:
#
#   docs/reference/cli.md            the whole Cobra command tree
#   docs/reference/api.md            the daemon's HTTP route catalog
#   plugins/**                       the installable per-agent af plugins
#   .agents/plugins/marketplace.json the Codex marketplace serving them
#   .claude-plugin/marketplace.json  the Claude Code marketplace serving them
#
# Interface tokens and the style guide come from the standalone Go generator.
# The reference and plugin artifacts come from the hidden `af gen-docs` command.
# CI runs this script
# and fails if the committed output differs (see .github/workflows/docs.yml), so
# run it and commit the result whenever you add or change a command, a flag, an
# HTTP route, or the af usage text in session/systemprompt.go.
set -euo pipefail

cd "$(dirname "$0")/.."

out="docs/reference"
go run . gen-docs "$out" --plugin-root .

# Staged design contract; does not load af state or change live screens.
go run ./scripts/gen-design

# The TUI recovery gallery (docs/design/tui-recovery-stills.md) embeds the SAME
# SVGs that app/recovery_test.go asserts against app/testdata/recovery. Two
# copies of one artifact drift, and this pair drifted twice in three days: #4025
# refreshed 10 of the 14 scenes and left `zero-accounts`/`remote-accounts` still
# advertising the `N new remote` shortcut it had just retired, and #4031 changed
# the no-daemon copy in the golden without touching the published twin. Both went
# unnoticed because only the app/testdata half has a test behind it.
#
# So derive the gallery from the asserted half rather than hand-copying it. This
# is a copy, not a recapture: it never runs the TUI, so it cannot re-baseline a
# real regression — it can only make the published still equal the one CI already
# checks against the code. Regenerating the goldens themselves is still
# `AF_TUI_RECOVERY_CAPTURE` inside the testbox (docs/design/tui-recovery-stills.md).
#
# Remove obsolete SVGs so renamed or deleted goldens cannot linger in the gallery.
# The .ansi twins beside them are deliberately untouched: they are asserted by
# nothing and are stale in their own right (#4067 decides whether they are
# asserted or dropped). Copying them would publish that staleness.
recovery_gallery="docs/assets/recovery/tui-model-driver"
rm -f "$recovery_gallery"/*.svg
for golden in app/testdata/recovery/*.svg; do
	cp "$golden" "$recovery_gallery/$(basename "$golden")"
done
