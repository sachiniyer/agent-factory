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
# All of it comes out of the hidden `af gen-docs` command. CI runs this script
# and fails if the committed output differs (see .github/workflows/docs.yml), so
# run it and commit the result whenever you add or change a command, a flag, an
# HTTP route, or the af usage text in session/systemprompt.go.
set -euo pipefail

cd "$(dirname "$0")/.."

out="docs/reference"
go run . gen-docs "$out" --plugin-root .
