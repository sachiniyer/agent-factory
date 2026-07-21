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
	release_url="https://github.com/$REPO/releases/latest/download"
else
	release_url="https://github.com/$REPO/releases/download/$VERSION"
fi
url="$release_url/$asset"
checksums_url="$release_url/sha256sums.txt"

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

# v1.0.206 predates published checksum manifests, but install.sh is served from
# master immediately: requiring the new release asset unconditionally would
# break the documented `.../master/install.sh | sh` path until the next stable
# release exists. These are the SHA-256 digests GitHub records on the v1.0.206
# release assets. The `latest` fallback is still fail-closed after a newer
# release: only the exact v1.0.206 bytes can match these pinned values.
pinned_pre_manifest_checksums() {
	case "$VERSION" in
	latest|v1.0.206)
		cat <<'CHECKSUMS'
f28be8db1a63bbfa082eb801d8a782f2fc4dd03e9b05d4fc7fbe78a9f2b02ec2  agent-factory-darwin-amd64.tar.gz
a36f0cda68dd8838e8c4b990f84396665613cac35927b086259a56622b24e33a  agent-factory-darwin-arm64.tar.gz
6db75e6ff648c3b8d75172a4720b9eb8b8477bd52cd753a85b2495b67ffc633b  agent-factory-linux-amd64.tar.gz
bf62bb15e07e06594a34dfdc7c5a0408eb48e2d10fa858ea06d30689915ec982  agent-factory-linux-arm64.tar.gz
CHECKSUMS
		;;
	*)
		return 1
		;;
	esac
}

# --- download into a temp dir, verify, install -----------------------------
tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/af-install.XXXXXX")"
cleanup() { rm -rf "$tmpdir"; }
trap cleanup EXIT INT TERM

tarball="$tmpdir/$asset"
checksums="$tmpdir/sha256sums.txt"

echo "Downloading $asset ($VERSION) for $os/$arch..."
if ! download "$url" "$tarball"; then
	echo "error: download failed from $url" >&2
	echo "Check your network, or that the release/tag exists at https://github.com/$REPO/releases" >&2
	exit 1
fi
if ! download "$checksums_url" "$checksums"; then
	if ! pinned_pre_manifest_checksums > "$checksums"; then
		echo "error: checksum manifest download failed from $checksums_url" >&2
		echo "The release is incomplete or predates checksum verification; refusing to install it." >&2
		exit 1
	fi
	echo "Checksum manifest unavailable; verifying against pinned v1.0.206 checksums."
fi

# Select exactly one well-formed entry for this platform archive. A missing,
# malformed, or duplicate entry is an incomplete release, never permission to
# install without verification.
expected_checksum="$(awk -v target="$asset" '
	{
		if (NF != 2) next
		name = $2
		sub(/^\*/, "", name)
		if (name == target) {
			matches++
			digest = $1
		}
	}
	END {
		if (matches != 1 || length(digest) != 64 || digest ~ /[^[:xdigit:]]/) exit 1
		print tolower(digest)
	}
' "$checksums")" || {
	echo "error: checksum manifest has no single valid SHA-256 entry for $asset" >&2
	exit 1
}

if command -v sha256sum >/dev/null 2>&1; then
	actual_checksum="$(sha256sum "$tarball" | awk '{print tolower($1)}')"
elif command -v shasum >/dev/null 2>&1; then
	actual_checksum="$(shasum -a 256 "$tarball" | awk '{print tolower($1)}')"
elif command -v openssl >/dev/null 2>&1; then
	actual_checksum="$(openssl dgst -sha256 "$tarball" | awk '{print tolower($NF)}')"
else
	echo "error: sha256sum, shasum, or openssl is required to verify the release" >&2
	exit 1
fi

if [ "$actual_checksum" != "$expected_checksum" ]; then
	echo "error: checksum mismatch for $asset" >&2
	echo "Expected: $expected_checksum" >&2
	echo "Actual:   $actual_checksum" >&2
	echo "The download may be corrupted or tampered with; refusing to install it." >&2
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
