#!/usr/bin/env bash
set -euo pipefail

cd /src
go test ./session/tmux \
  -run '^TestCheckAndHandleTrustPrompt_CodexSafetyWaitsForRealTmuxRender$' \
  -count=1
echo "PASS: #2673 delayed Codex rendering is verified on an isolated real tmux socket"
