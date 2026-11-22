package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The agent pane finds the session's staged, sha256-verified `af` through a
// single PATH entry: the provision launch prepends it (`PATH=<dir>:"$PATH"`,
// #5571 W2.2) and the headless agent-server re-pins it first on the adopted
// login PATH (#5877). A ':' inside that entry is a PATH SEPARATOR when the
// shell consumes the assignment — shellQuote protects metacharacters, never
// PATH grammar — so a colon-bearing session dir splits in two: the staged `af`
// is never found, and the relative tail resolves against each pane's working
// directory, where a repo-controlled directory could shadow it (#7770).
//
// The session dir can carry a ':' through any of its ancestors: a listed
// host's workspace_root (config accepts ':' today), the remote login's $HOME,
// or a symlinked component resolveSessionDirSymlinks surfaces through
// `pwd -P`. Rejecting such roots at config time would drop hosts whose only
// symptom on master was a broken in-pane `af` — a total refusal for a partial
// breakage, and a refusal no working setup asked for. So af builds a
// colon-free indirection instead: a symlink to the session dir whose own
// spelling provably has no ':' — under `$HOME/.af-path` when that spelling is
// usable, else under an unpredictable 0700 `mktemp -d` root OUTSIDE $HOME — and
// puts THAT spelling on PATH. The session dir itself is untouched: the staged
// binary still launches as <sessionDir>/af, which the reattach token file, the
// argv[0] identity check and the sweep all depend on.
//
// The chosen link path is persisted on the runtime identity: provision writes
// it, and every later reader — the session's teardown in this or a restored
// process, an abandoned adoption, a restart re-ensuring it — names the same
// path. The leaf name carries a session-dir digest so two sessions can never
// collide inside a shared root.

// remotePathLinkDir is the remote subdirectory (under the login's $HOME) the
// indirection prefers when $HOME is itself a usable PATH-entry spelling. It is
// af-owned: created 0700 by the link step and removed (link first, then rmdir
// when empty) by the session's teardown.
const remotePathLinkDir = ".af-path"

// panePathLinkName is the indirection's leaf name for a colon-bearing session
// dir: a digest of the dir, so two sessions can never collide inside a shared
// root ($HOME/.af-path, or an unlucky repeat inside a re-created mktemp root).
func panePathLinkName(sessionDir string) string {
	sum := sha256.Sum256([]byte(sessionDir))
	return "sess-" + hex.EncodeToString(sum[:8])
}

// ensurePanePathDir computes the directory the launch puts first on the
// agent-server's PATH — and the agent-server re-pins first on the agent pane's
// adopted login PATH — and builds the indirection when the session dir itself
// cannot be that entry. It runs once the session dir is final (fresh creates:
// after mktemp and physical resolution; adoptions: after the preserved dir is
// verified) and BEFORE the workspace and binary are staged, so a refusal still
// reaps as a pre-launch attempt.
//
// A colon-free session dir IS the entry and costs no remote call. A
// colon-bearing one pays exactly one: the script decides where the link lives —
// w.panePathDir when a caller already carries the session's recorded link path
// (a restart re-ensuring what provision's launch script still names), else
// $HOME/.af-path when that is a usable spelling, else an unpredictable
// `mktemp -d` root under ${TMPDIR:-/tmp} so a ':'-bearing remote $HOME is
// handled rather than refused (#7770 R4). The remote commits nothing until it
// has PROVEN the result: the leaf must be a symlink whose readlink equals the
// session dir byte for byte.
func (w *sandboxWorkspace) ensurePanePathDir(timeout time.Duration) error {
	dir := w.SessionDir
	if !strings.Contains(dir, ":") {
		if !strings.HasPrefix(dir, "/") {
			return fmt.Errorf("the session directory %q is not an absolute path, so it cannot lead the agent pane's PATH", dir)
		}
		w.panePathDir = dir
		return nil
	}
	var script string
	if w.panePathDir != "" {
		// A reattach supplies the recorded link path — the persisted launch
		// script names it, so the link is re-created AT it, never a fresh
		// spelling. A malformed record is refused before the script exists.
		if !strings.HasPrefix(w.panePathDir, "/") || strings.Contains(w.panePathDir, ":") {
			return fmt.Errorf("the recorded pane PATH dir %q is unusable: a PATH entry must be an absolute path without ':'", w.panePathDir)
		}
		script = panePathEnsureAtScript(w.panePathDir, dir)
	} else {
		script = panePathChooseScript(dir)
	}
	// The link path is read from STDOUT ONLY (#7770 R3): a bannered rc or a
	// traced argv on stderr must never be parsed as the path, so stderr is
	// kept apart and consulted only to explain a failure (an ExitError
	// carries it). Inside stdout, the marker line still names the one line
	// af printed, so stdout-side noise cannot be read as the path either.
	out, err := w.shell.Run(timeout, script, nil, false)
	if err != nil {
		detail := strings.TrimSpace(string(out))
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if s := strings.TrimSpace(string(ee.Stderr)); s != "" {
				detail = s
			}
		}
		return fmt.Errorf("the session directory %q contains ':' and af could not build the colon-free PATH indirection the agent pane needs: %s: %w",
			dir, detail, err)
	}
	resolved := ""
	for _, line := range strings.Split(string(out), "\n") {
		if l, ok := strings.CutPrefix(strings.TrimSpace(line), "AF_PANE_LINK="); ok {
			resolved = l
		}
	}
	if !strings.HasPrefix(resolved, "/") || strings.Contains(resolved, ":") {
		return fmt.Errorf("af could not build a colon-free PATH indirection for the session directory %q (the remote reported %q)", dir, resolved)
	}
	w.panePathDir = resolved
	return nil
}

// panePathGuards is the pre-write guard block both link scripts share: the
// indirection's parent must be a real, af-owned directory — never a symlink or
// a non-directory a caller planted for af to write inside — and the leaf must
// not already exist as anything but a symlink, because `ln -sfn` into a real
// directory is implementation-defined (busybox creates the link INSIDE it and
// still exits 0). $root/$link are already set by the caller.
const panePathGuards = `if [ -L "$root" ]; then echo "af: $root is a symlink; refusing to place the pane PATH indirection inside it" >&2; exit 93; fi; ` +
	`if [ -e "$root" ] && [ ! -d "$root" ]; then echo "af: $root exists and is not a directory" >&2; exit 94; fi; ` +
	`if [ -e "$link" ] && [ ! -L "$link" ]; then echo "af: $link exists and is not a symlink; refusing to overwrite it" >&2; exit 95; fi; `

// panePathProve is the commit block both link scripts share: create the link,
// then prove it — the leaf must exist as a symlink whose readlink equals the
// session dir byte for byte before the path is reported. Anything less and the
// run fails instead of committing a PATH entry that does not reach the staged
// `af` (#7770 R1).
const panePathProve = `mkdir -p "$root" && chmod 700 "$root" && ln -sfn "$sess" "$link" && [ -L "$link" ] && [ "$(readlink "$link")" = "$sess" ] && printf 'AF_PANE_LINK=%s\n' "$link"`

// panePathEnsureAtScript recreates the indirection AT a path af already
// recorded — a restart, whose persisted launch script names this exact PATH
// entry. root is the link's parent; mkdir -p re-establishes it if the whole
// tree was cleaned.
func panePathEnsureAtScript(link, sess string) string {
	return `link=` + shellQuote(link) + `; sess=` + shellQuote(sess) + `; root="${link%/*}"; ` +
		panePathGuards + panePathProve
}

// panePathChooseScript picks where a fresh ':'-bearing session dir's link
// lives and creates it:
//   - $HOME/.af-path when the login's $HOME is an absolute, colon-free
//     spelling of a real directory — the af-owned shared root the teardown
//     also empties;
//   - else an unpredictable `mktemp -d` root under ${TMPDIR:-/tmp} — 0700 by
//     construction, collision-free by mktemp, and outside $HOME so a
//     ':'-bearing login home is handled rather than refused (#7770 R4).
//     ${TMPDIR:-/tmp} itself bearing ':' (or not absolute) falls back to the
//     literal /tmp, and the root mktemp prints is re-checked: a remote that
//     cannot produce one colon-free spelling anywhere is the only refusal
//     left, and it still happens before the workspace and binary are staged.
//
// The EXIT trap drops a mktemp root the run could not commit (an empty
// directory af alone could have made); on success it holds the link, so the
// rmdir can only fail — the teardown removes it for real.
func panePathChooseScript(sess string) string {
	return `sess=` + shellQuote(sess) + `; leaf=` + shellQuote(panePathLinkName(sess)) + `; root=; link=; made=; ` +
		`trap 'rc=$?; if [ "$rc" -ne 0 ] && [ -n "$made" ]; then rm -f "$link" 2>/dev/null; rmdir "$root" 2>/dev/null; fi' EXIT; ` +
		`case "$HOME" in /*) case "$HOME" in *:*) ;; *) if [ -d "$HOME" ]; then root="$HOME/` + remotePathLinkDir + `"; fi;; esac;; esac; ` +
		`if [ -z "$root" ]; then base="${TMPDIR:-/tmp}"; case "$base" in /*) ;; *) base=/tmp;; esac; case "$base" in *:*) base=/tmp;; esac; ` +
		`root=$(mktemp -d "$base/af-path.XXXXXX") || { echo "af: no colon-free pane PATH indirection can be built: the remote login's $HOME contains ':' or is relative, and mktemp -d under $base failed" >&2; exit 96; }; made=1; fi; ` +
		`case "$root" in /*) ;; *) echo "af: the indirection root $root is not an absolute path" >&2; exit 97;; esac; ` +
		`case "$root" in *:*) echo "af: no colon-free pane PATH indirection can be built: the remote login's $HOME contains ':' and the mktemp root $root contains ':' too" >&2; exit 98;; esac; ` +
		`link="$root/$leaf"; ` +
		// A mktemp root is af's own fresh 0700 dir — nothing can already be
		// inside it — so the planted-path guards apply only to the $HOME root.
		`if [ -z "$made" ]; then ` + panePathGuards + `fi; ` +
		panePathProve
}
