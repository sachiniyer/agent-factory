package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
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
// colon-free indirection instead: a symlink to the session dir under a
// directory af controls that provably has no ':' — `$HOME/.af-path` on the
// remote login — and puts THAT spelling on PATH. The session dir itself is
// untouched: the staged binary still launches as <sessionDir>/af, which the
// reattach token file, the argv[0] identity check and the sweep all depend on.
//
// The link name derives from the session dir, so provision, teardown, a
// restored cleanup handle and the orphan sweep all compute the same path
// without persisting it.

// remotePathLinkDir is the remote subdirectory (under the login's $HOME) that
// holds the per-session PATH indirection links. It is af-owned: created 0700
// by the link step and removed (link first, then rmdir when empty) by the
// session's teardown.
const remotePathLinkDir = ".af-path"

// panePathLinkName is the indirection's leaf name for a colon-bearing session
// dir: a digest of the dir, so two sessions can never collide and every later
// reader — a teardown composed in a fresh process, the orphan sweep — names
// the same link without a stored field.
func panePathLinkName(sessionDir string) string {
	sum := sha256.Sum256([]byte(sessionDir))
	return "sess-" + hex.EncodeToString(sum[:8])
}

// panePathLinkWord is the remote shell word naming the indirection link for
// sessionDir — `"$HOME/.af-path/<name>"` double-quoted so the remote shell
// expands $HOME while the spelling (and so the PATH entry) stays colon-free
// iff the login's $HOME spelling is. It is the SAME word provision creates and
// teardown removes.
func panePathLinkWord(sessionDir string) string {
	return `"$HOME/` + remotePathLinkDir + `/` + panePathLinkName(sessionDir) + `"`
}

// checkPanePathFeasibility refuses — before anything is staged on the host —
// the one setup that can never yield a colon-free pane PATH dir: a session dir
// that will contain ':' AND a remote login whose $HOME contains ':' too, so
// the indirection has no colon-free spelling to live under either. The refusal
// names the field and the character, stays reapable as a pre-launch attempt
// (it runs before launchAttempted is ever set), and is the ONLY new refusal:
// every other shape — including whitespace-bearing roots, which shellQuote
// already makes safe — proceeds exactly as on master.
func (w *sandboxWorkspace) checkPanePathFeasibility(timeout time.Duration) error {
	var underHome, rootColon bool
	if a := w.spec.Adopt; a != nil {
		// The preserved dir is what it is; only its spelling decides whether
		// the indirection is needed.
		rootColon = strings.Contains(a.SessionDir, ":")
	} else {
		underHome = w.spec.WorkspaceRoot == ""
		rootColon = strings.Contains(w.spec.WorkspaceRoot, ":")
	}
	if !underHome && !rootColon {
		// A colon-free workspace_root yields a colon-free session dir unless
		// physical resolution introduces one — which only ensurePanePathDir's
		// own refusal can know, after the dir exists.
		return nil
	}
	// The marker line keeps wrapper noise (a bannered ssh rc, a traced argv)
	// from being read as the login's home: only the line af printed is read.
	out, err := w.shell.Run(timeout, `printf 'AF_PANE_HOME=%s\n' "$HOME"`, nil, false)
	if err != nil {
		// An unanswered probe must not refuse a host that may be fine — the
		// same rule checkHostPrereqs keeps; the link step fail-closes later.
		log.WarningLog.Printf("pane PATH feasibility probe did not complete (%v); continuing", err)
		return nil
	}
	home := ""
	for _, line := range strings.Split(string(out), "\n") {
		if h, ok := strings.CutPrefix(strings.TrimSpace(line), "AF_PANE_HOME="); ok {
			home = h
		}
	}
	if !strings.Contains(home, ":") {
		return nil
	}
	switch {
	case w.spec.Adopt != nil:
		return fmt.Errorf("the preserved session directory %q contains ':' and the remote login's $HOME %q contains ':' too, so no colon-free PATH indirection exists for the agent pane — give the remote account a colon-free $HOME (a symlinked spelling is enough) and retry",
			w.spec.Adopt.SessionDir, home)
	case underHome:
		return fmt.Errorf("the remote login's $HOME %q contains ':': the session directory is created under it and a PATH entry can never contain ':', and af's indirection dir would have to live under $HOME too — give the remote account a colon-free $HOME (a symlinked spelling is enough), or provision through a listed host whose workspace_root is colon-free, and retry",
			home)
	default:
		return fmt.Errorf("the host's workspace_root %q contains ':': af would need a colon-free indirection under the remote login's $HOME for the agent pane's PATH, but $HOME %q contains ':' too — change workspace_root to a colon-free absolute path or give the remote account a colon-free $HOME (a symlinked spelling is enough), and retry",
			w.spec.WorkspaceRoot, home)
	}
}

// ensurePanePathDir computes the directory the launch puts first on the
// agent-server's PATH — and the agent-server re-pins first on the agent pane's
// adopted login PATH — and builds the indirection when the session dir itself
// cannot be that entry. It runs once the session dir is final (fresh creates:
// after mktemp and physical resolution; adoptions: after the preserved dir is
// verified) and BEFORE the workspace and binary are staged, so a refusal still
// reaps as a pre-launch attempt.
func (w *sandboxWorkspace) ensurePanePathDir(timeout time.Duration) error {
	dir := w.SessionDir
	if !strings.Contains(dir, ":") {
		if !strings.HasPrefix(dir, "/") {
			return fmt.Errorf("the session directory %q is not an absolute path, so it cannot lead the agent pane's PATH", dir)
		}
		w.panePathDir = dir
		return nil
	}
	// A ':' in the session dir would split into two PATH entries when the
	// launch's shell consumes the assignment — the staged `af` unreachable and
	// the relative tail resolved against each pane's cwd (#7770). The
	// indirection lives under the remote login's $HOME — the one path the
	// script knows without another probe — and its checks run BEFORE mkdir:
	// the root must be absolute and colon-free (a ':'-bearing $HOME is the
	// case checkPanePathFeasibility already refuses early; this re-check
	// fail-closes for a root that changed or could not be probed) and must not
	// be a symlink or a non-directory (a pre-planted path af must not write
	// inside). The leaf gets its own check: `ln -sfn` replaces a stale file or
	// symlink, but on some implementations (busybox) it instead creates the
	// link INSIDE a destination that is a real directory — so an existing
	// non-symlink is refused outright rather than left to `ln`'s variant
	// behavior. mkdir -p + chmod 700 then makes the root af-owned and private,
	// and the post-check re-proves a symlink landed at the leaf: no pre-planted
	// path survives as a way to redirect the pane's `af` lookup.
	script := `root="$HOME/` + remotePathLinkDir + `"; link="$root/` + panePathLinkName(dir) + `"; ` +
		`case "$root" in /*) ;; *) echo "af: the remote login's $HOME is not an absolute path, so no colon-free pane PATH indirection can be built under it" >&2; exit 91;; esac; ` +
		`case "$root" in *:*) echo "af: the remote login's $HOME contains ':', so no colon-free pane PATH indirection can be built under it" >&2; exit 92;; esac; ` +
		`if [ -L "$root" ]; then echo "af: $root is a symlink; refusing to place the pane PATH indirection inside it" >&2; exit 93; fi; ` +
		`if [ -e "$root" ] && [ ! -d "$root" ]; then echo "af: $root exists and is not a directory" >&2; exit 94; fi; ` +
		`if [ -e "$link" ] && [ ! -L "$link" ]; then echo "af: $link exists and is not a symlink; refusing to overwrite it" >&2; exit 95; fi; ` +
		`mkdir -p "$root" && chmod 700 "$root" && ln -sfn ` + shellQuote(dir) + ` "$link" && [ -L "$link" ] && printf 'AF_PANE_LINK=%s\n' "$link"`
	// stderr rides the combined output on purpose — the remote's own refusal
	// text belongs in the error — which means wrapper noise (a bannered rc, a
	// traced argv) arrives mixed into stdout too. The marker line names the
	// one line af printed, so noise can never be read as the link path.
	out, err := w.shell.Run(timeout, script, nil, true)
	if err != nil {
		return fmt.Errorf("the session directory %q contains ':' and af could not build the colon-free PATH indirection the agent pane needs: %s: %w",
			dir, strings.TrimSpace(string(out)), err)
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
