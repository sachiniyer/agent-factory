#!/usr/bin/env bash
# Build and install af from local source, and optionally set up microclaw.
# Usage: ./dev-install.sh

set -e

BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
BINARY_NAME="af"

echo "Building af from source..."
go build -o "$BINARY_NAME" .

echo "Installing to ${BIN_DIR}/${BINARY_NAME}..."
mkdir -p "$BIN_DIR"
mv "$BINARY_NAME" "${BIN_DIR}/${BINARY_NAME}"
chmod +x "${BIN_DIR}/${BINARY_NAME}"

echo "Installed successfully: $(${BIN_DIR}/${BINARY_NAME} version 2>/dev/null || echo "${BIN_DIR}/${BINARY_NAME}")"

# --- MicroClaw setup ---

MICROCLAW_BIN="${BIN_DIR}/microclaw"
MICROCLAW_DIR="$HOME/.microclaw"
MICROCLAW_CONFIG="${MICROCLAW_DIR}/microclaw.config.yaml"
SYSTEMD_DIR="$HOME/.config/systemd/user"
MICROCLAW_SERVICE="${SYSTEMD_DIR}/microclaw.service"

# Download microclaw binary if not present
if [ ! -f "$MICROCLAW_BIN" ]; then
    echo ""
    echo "Setting up MicroClaw..."

    ARCH=$(uname -m)
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$ARCH" in
        x86_64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *)
            echo "Warning: unsupported architecture $ARCH for microclaw download, skipping."
            ARCH=""
            ;;
    esac

    if [ -n "$ARCH" ]; then
        MICROCLAW_URL="https://github.com/microclaw/microclaw/releases/latest/download/microclaw-${OS}-${ARCH}"
        echo "Downloading microclaw from ${MICROCLAW_URL}..."
        if curl -fSL -o "$MICROCLAW_BIN" "$MICROCLAW_URL" 2>/dev/null; then
            chmod +x "$MICROCLAW_BIN"
            echo "Installed microclaw to ${MICROCLAW_BIN}"
        else
            echo "Warning: failed to download microclaw. You can install it manually later."
            rm -f "$MICROCLAW_BIN"
        fi
    fi
else
    echo "MicroClaw binary already installed at ${MICROCLAW_BIN}"
fi

# Generate default config if not present
if [ ! -f "$MICROCLAW_CONFIG" ]; then
    mkdir -p "$MICROCLAW_DIR"
    cat > "$MICROCLAW_CONFIG" << 'YAML'
# MicroClaw configuration
# See https://github.com/microclaw/microclaw for documentation

llm:
  provider: anthropic
  api_key: "YOUR_API_KEY_HERE"
  model: claude-sonnet-4-20250514

server:
  port: 10961
  password: helloworld

slack:
  enabled: false
  # bot_token: "xoxb-..."
  # app_token: "xapp-..."
YAML
    echo "Created default config at ${MICROCLAW_CONFIG}"
    echo "  -> Edit ${MICROCLAW_CONFIG} and set your API key before starting microclaw."
fi

# Create systemd user service if not present (Linux only)
if [ "$OS" = "linux" ] || [ "$(uname -s)" = "Linux" ]; then
    if [ ! -f "$MICROCLAW_SERVICE" ] && [ -f "$MICROCLAW_BIN" ]; then
        mkdir -p "$SYSTEMD_DIR"
        cat > "$MICROCLAW_SERVICE" << EOF
[Unit]
Description=MicroClaw AI Assistant
After=network.target

[Service]
Type=simple
ExecStart=${MICROCLAW_BIN} start
Restart=on-failure
RestartSec=5
Environment=HOME=${HOME}

[Install]
WantedBy=default.target
EOF
        echo "Created systemd service at ${MICROCLAW_SERVICE}"
        echo "  -> Enable with: systemctl --user enable --now microclaw"
    fi
fi

echo ""
echo "Setup complete!"
if [ -f "$MICROCLAW_BIN" ]; then
    echo ""
    echo "MicroClaw quick start:"
    echo "  1. Edit ${MICROCLAW_CONFIG} and set your API key"
    echo "  2. Start microclaw: microclaw start"
    echo "  3. Use the TUI: af microclaw"
fi
