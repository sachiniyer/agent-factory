#!/usr/bin/env bash
# Install the standalone Codex CLI inside the throwaway play-test container.
# This is opt-in because the release archive is large and ordinary harness
# self-tests deliberately stay cheap.
set -euo pipefail

INSTALLER_URL="${AF_PLAYTEST_CODEX_INSTALLER_URL:-https://github.com/openai/codex/releases/latest/download/install.sh}"
RELEASE="${AF_PLAYTEST_CODEX_RELEASE:-latest}"
INSTALLER="$(mktemp)"
trap 'rm -f "$INSTALLER"' EXIT

echo "play-test: installing Codex $RELEASE with the official standalone installer …"
curl -fsSL "$INSTALLER_URL" -o "$INSTALLER"
CODEX_INSTALL_DIR="$HOME/bin" \
    CODEX_NON_INTERACTIVE=true \
    CODEX_RELEASE="$RELEASE" \
    sh "$INSTALLER"
