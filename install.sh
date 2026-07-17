#!/bin/sh
# install.sh — install the `af` (Agent Factory) binary without needing Go.
#
# Downloads the prebuilt release tarball for your OS/arch from GitHub and
# installs `af` into ${AF_INSTALL_DIR:-$HOME/.local/bin}. No GitHub API call
# or token is required: it fetches the stable "releases/latest/download/..."
# redirect.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | sh
#
#   # install into a custom directory
#   AF_INSTALL_DIR=/usr/local/bin sh install.sh
#
#   # pin a specific release tag (default: latest)
#   sh install.sh --version v1.0.114
#   AF_VERSION=v1.0.114 sh install.sh
#
# POSIX sh — no bashisms. Re-running upgrades in place.

set -eu

REPO="sachiniyer/agent-factory"
INSTALL_DIR="${AF_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${AF_VERSION:-latest}"

# --- argument parsing ------------------------------------------------------
while [ $# -gt 0 ]; do
	case "$1" in
		--version)
			if [ $# -lt 2 ]; then
				echo "error: --version requires a tag argument (e.g. --version v1.0.114)" >&2
				exit 1
			fi
			VERSION="$2"
			shift 2
			;;
		--version=*)
			VERSION="${1#--version=}"
			shift
			;;
		-h|--help)
			# Print the header comment block (skip the shebang, stop at the
			# first non-comment line).
			awk 'NR==1{next} /^#/{sub(/^# ?/,""); print; next} {exit}' "$0"
			exit 0
			;;
		*)
			echo "error: unknown argument: $1" >&2
			echo "usage: install.sh [--version <tag>]" >&2
			exit 1
			;;
	esac
done

# --- platform detection ----------------------------------------------------
uname_s="$(uname -s)"
case "$uname_s" in
	Linux) os="linux" ;;
	Darwin) os="darwin" ;;
	*)
		echo "error: unsupported operating system: $uname_s" >&2
		echo "Prebuilt binaries are available for Linux and macOS only." >&2
		echo "On other platforms, build from source: https://github.com/$REPO#building-from-source" >&2
		exit 1
		;;
esac

uname_m="$(uname -m)"
case "$uname_m" in
	x86_64|amd64) arch="amd64" ;;
	arm64|aarch64) arch="arm64" ;;
	*)
		echo "error: unsupported architecture: $uname_m" >&2
		echo "Prebuilt binaries are available for amd64 (x86_64) and arm64 only." >&2
		echo "On other architectures, build from source: https://github.com/$REPO#building-from-source" >&2
		exit 1
		;;
esac

asset="agent-factory-${os}-${arch}.tar.gz"
if [ "$VERSION" = "latest" ]; then
	url="https://github.com/$REPO/releases/latest/download/$asset"
else
	url="https://github.com/$REPO/releases/download/$VERSION/$asset"
fi

# --- downloader (curl, falling back to wget) -------------------------------
download() {
	# download <url> <dest>
	if command -v curl >/dev/null 2>&1; then
		# -f: fail on HTTP error, -S: show errors, -L: follow redirects.
		curl -fSL --connect-timeout 30 --max-time 300 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget --timeout=30 -O "$2" "$1"
	else
		echo "error: neither curl nor wget is installed; cannot download $1" >&2
		exit 1
	fi
}

# --- download into a temp dir, verify, install -----------------------------
tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/af-install.XXXXXX")"
cleanup() { rm -rf "$tmpdir"; }
trap cleanup EXIT INT TERM

tarball="$tmpdir/$asset"

echo "Downloading $asset ($VERSION) for $os/$arch..."
if ! download "$url" "$tarball"; then
	echo "error: download failed from $url" >&2
	echo "Check your network, or that the release/tag exists at https://github.com/$REPO/releases" >&2
	exit 1
fi

# Verify the body is a real gzip/tar archive, not an HTML error page or a
# truncated download. A broken install is worse than no install.
if ! tar tzf "$tarball" >/dev/null 2>&1; then
	echo "error: downloaded file is not a valid gzip/tar archive: $tarball" >&2
	echo "The download may be incomplete or the URL may have returned an error page." >&2
	echo "URL: $url" >&2
	exit 1
fi

echo "Extracting..."
tar xzf "$tarball" -C "$tmpdir"

# The release tarball contains a single binary named `agent-factory`; we
# install it under the friendlier name `af`.
if [ ! -f "$tmpdir/agent-factory" ]; then
	echo "error: archive did not contain the expected 'agent-factory' binary" >&2
	exit 1
fi

mkdir -p "$INSTALL_DIR"
chmod +x "$tmpdir/agent-factory"
# Move into place (atomic when on the same filesystem); fall back to cp for
# cross-filesystem temp dirs.
if ! mv "$tmpdir/agent-factory" "$INSTALL_DIR/af" 2>/dev/null; then
	cp "$tmpdir/agent-factory" "$INSTALL_DIR/af"
fi
chmod +x "$INSTALL_DIR/af"

echo "Installed af to $INSTALL_DIR/af"

# --- post-install: version + PATH hint -------------------------------------
installed_version="$("$INSTALL_DIR/af" version 2>/dev/null || echo "unknown")"
echo "Version: $installed_version"

if ! "$INSTALL_DIR/af" daemon restart --quiet; then
	echo "warning: installed af, but failed to restart the running daemon" >&2
	echo "         run '$INSTALL_DIR/af daemon restart' to retry" >&2
fi

case ":$PATH:" in
	*":$INSTALL_DIR:"*)
		echo "Next:"
		echo "  1. Make sure tmux, git, and an agent CLI (Claude Code, Codex, Aider, Gemini, Amp, or opencode) are installed."
		echo "  2. Run: af doctor --setup"
		echo "  3. In a git repository, run: af"
		;;
	*)
		echo ""
		echo "NOTE: $INSTALL_DIR is not on your PATH."
		echo "Add it to your shell profile, e.g.:"
		echo "    export PATH=\"$INSTALL_DIR:\$PATH\""
		echo "Then restart your shell and run: af doctor --setup"
		echo "You can also run the installed binary directly: $INSTALL_DIR/af doctor --setup"
		;;
esac
