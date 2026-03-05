#!/usr/bin/env bash
# Build and install cs from local source.
# Usage: ./dev-install.sh

set -e

BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
BINARY_NAME="cs"

echo "Building cs from source..."
go build -o "$BINARY_NAME" .

echo "Installing to ${BIN_DIR}/${BINARY_NAME}..."
mkdir -p "$BIN_DIR"
mv "$BINARY_NAME" "${BIN_DIR}/${BINARY_NAME}"
chmod +x "${BIN_DIR}/${BINARY_NAME}"

echo "Installed successfully: $(${BIN_DIR}/${BINARY_NAME} version 2>/dev/null || echo "${BIN_DIR}/${BINARY_NAME}")"
