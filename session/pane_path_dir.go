package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path"
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
// spelling provably has no ':' — under a per-uid af-owned root when one
// qualifies ($HOME/.af-path, then $XDG_RUNTIME_DIR/.af-path, #7787), else an
// unpredictable 0700 `mktemp -d` root under ${TMPDIR:-/tmp} — and puts THAT
// spelling on PATH. The session dir itself is untouched: the staged binary
// still launches as <sessionDir>/af, which the reattach token file, the
// argv[0] identity check and the sweep all depend on.
//
// The chosen link path is persisted on the runtime identity: provision writes
// it, and every later reader — the session's teardown in this or a restored
// process, an abandoned adoption, a restart re-ensuring it — names the same
// path. The leaf name carries a session-dir digest so two sessions can never
// collide inside a shared root.
//
// The TRUST BOUNDARY is other local uids, not the session uid: code running as
// the session uid can already repoint <sessionDir>/af itself. The indirection
// differs from the session dir in two ways that other uids CAN reach (#7787):
// the link lives outside the reaped dir, and a recorded root under a directory
// other users can write (a /tmp mktemp root from an earlier run) can be
// squatted or repointed while the session is away. So a root is never re-used
// by name alone: every ensure verifies — after creation — that it is a real
// directory, never a symlink, owned by the session uid and exactly 0700, and
// mints a fresh root on any failure rather than refusing (a squatted name is
// abandoned, never fought over) or writing inside a stranger's dir. And the
// link's in-dir marker (panePathMarkerName) lets the orphan sweep find and
// reap it without the session row.

// remotePathLinkDir is the remote subdirectory name the indirection prefers
// inside a per-uid parent ($HOME, $XDG_RUNTIME_DIR). It is af-owned: created
// 0700 by the link step and removed (link first, then rmdir when empty) by the
// session's teardown.
const remotePathLinkDir = ".af-path"

// panePathMarkerName is the file the link step writes INSIDE the session dir,
// holding the link path it committed outside it. The session row is not the
// only record then: when the row is lost, the startup orphan sweep reads the
// marker from a proven orphan dir and reaps exactly the link — and its empty
// af-owned root — it names (#7787).
const panePathMarkerName = "af-pane-path"

// panePathLinkName is the indirection's leaf name for a colon-bearing session
// dir: a digest of the dir, so two sessions can never collide inside a shared
// root ($HOME/.af-path, $XDG_RUNTIME_DIR/.af-path, or an unlucky repeat inside
// a re-created mktemp root). It is also the sweep's proof that a marker-named
// link belongs to a given dir: the name is bound to the dir's own bytes.
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
// (a restart re-ensuring what provision's launch script still names), else the
// first per-uid root that verifies ($HOME/.af-path, then a uid-owned 0700
// $XDG_RUNTIME_DIR), else an unpredictable `mktemp -d` root under
// ${TMPDIR:-/tmp} (#7770 R4, #7787). A recorded root is never re-created by
// name alone: the script verifies the result is a real, uid-owned 0700
// directory and mints a fresh root on ANY failure — a squatted or repointed
// name is abandoned, not fought over or written inside. The remote commits
// nothing until it has PROVEN the result: the leaf must be a symlink whose
// readlink equals the session dir byte for byte, inside a root that still
// verifies.
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
		// script names it, so the link is re-created AT it when it still
		// verifies, else at a fresh root the script mints and reports. A
		// malformed record is refused before the script exists.
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

// The script fragments below are shared by panePathChooseScript and
// panePathEnsureAtScript. They read and write these shell variables:
//
//	sess   the session dir (the link's target)
//	leaf   the link's leaf name (a session-dir digest)
//	root   the link's parent dir — set or re-set by each block
//	link   the link path itself
//	made   nonempty once a mktemp root exists, arming the EXIT trap
//	nl     a literal newline, for the IFS-based candidate loop
//	cands  newline-separated candidate roots, most preferred first
//	r      the root under examination by the verify fragments
//
// panePathRootOwned PROVES $r is a usable indirection root: a real directory,
// never a symlink, owned by the session uid, exactly 0700. It runs AFTER any
// mkdir/chmod attempt (#7787): a name in a directory other uids can write is
// only ever reused once this passes — a squatter's dir fails ownership, a
// repointed name fails the no-symlink test, and either way the run mints a
// fresh root rather than refusing or writing inside. -O is the euid-ownership
// test every target shell provides (dash, bash, busybox, macOS sh); the mode
// is read through `ls -ld`, POSIX where `stat` is not.
const panePathRootOwned = `[ -d "$r" ] && [ ! -L "$r" ] && [ -O "$r" ] && [ "$(ls -ld "$r" 2>/dev/null | cut -c1-10)" = "drwx------" ]`

// panePathRootSettle is the best-effort establishment of $r before the verify,
// and it is deliberately narrow: an EXISTING name is never chmodded or written
// through, because a mode change there is a confused deputy — a symlink
// another uid swapped in after a `[ ! -L ]` check would make `chmod` rewrite
// the TARGET's mode (#7787 R1). af chmods only a root its own `mkdir` just
// created: that object is provably af's, a real directory at the instant it
// exists. Anything already present is judged by panePathRootOwned alone —
// an af-owned 0700 dir verifies, and everything else is skipped, never
// repaired in place. `mkdir` without -p suffices: every parent here ($HOME,
// XDG_RUNTIME_DIR, the recorded root's parent) is an established directory.
const panePathRootSettle = `[ -e "$r" ] || { mkdir "$r" 2>/dev/null && chmod 700 "$r" 2>/dev/null; }; `

// panePathSharedRoots builds $cands, the newline-separated list of af-owned
// roots the link may live under, most preferred first, and $nl for the
// candidate loop's IFS:
//   - $HOME/.af-path, when the login's $HOME is an absolute, colon-free
//     spelling of a real directory — the shared per-uid root the teardown
//     also empties.
//   - $XDG_RUNTIME_DIR/.af-path, when XDG_RUNTIME_DIR is set to an absolute,
//     colon-free real directory the session uid owns at mode 0700 AND linger
//     is proven on (#7787 R2): the freedesktop per-uid runtime dir, which no
//     other local uid can write. The linger gate exists because pam_systemd
//     deletes /run/user/$UID when the uid's last logind session closes, and
//     every af ssh Run is its own short-lived session — without linger the
//     link dies with the command that created it, and the pane's first PATH
//     entry vanishes mid-session. The proofs are the linger marker file and
//     `loginctl show-user -p Linger`, either sufficing; on a host with
//     neither, XDG is skipped and the mktemp root — which survives — is used.
//     AF_LINGER_FILE overrides the marker path for the real-script tests;
//     it is same-uid evidence only, like every other env the script reads.
//
// Only when NO per-uid candidate qualifies does the caller mint a mktemp root
// under ${TMPDIR:-/tmp}.
const panePathSharedRoots = `nl='
'; cands=; ` +
	`case "$HOME" in /*) case "$HOME" in *:*) ;; *) [ -d "$HOME" ] && cands=$HOME/` + remotePathLinkDir + `;; esac;; esac; ` +
	`if [ -n "${XDG_RUNTIME_DIR:-}" ]; then case "$XDG_RUNTIME_DIR" in /*) case "$XDG_RUNTIME_DIR" in *:*) ;; *) ` +
	`if [ -d "$XDG_RUNTIME_DIR" ] && [ -O "$XDG_RUNTIME_DIR" ] && [ "$(ls -ld "$XDG_RUNTIME_DIR" 2>/dev/null | cut -c1-10)" = "drwx------" ]; then ` +
	`linger=${AF_LINGER_FILE:-/var/lib/systemd/linger/$(id -un 2>/dev/null)}; ` +
	`if [ -f "$linger" ] || [ "$(loginctl show-user -p Linger 2>/dev/null)" = "Linger=yes" ]; then ` +
	`cands="$cands$nl$XDG_RUNTIME_DIR/` + remotePathLinkDir + `"; fi; fi;; esac;; esac; fi; `

// panePathTryShared takes the first $cands entry that becomes a verified root
// holding a free-or-symlink leaf and sets $root to it; unusable candidates are
// skipped with a stderr note (only ever surfaced when the run as a whole
// fails). A leaf that exists as anything but a symlink disqualifies its root —
// `ln -sfn` into a real directory is implementation-defined (busybox creates
// the link INSIDE it and still exits 0), and nothing af did not make is
// written inside. $root stays empty when no candidate qualifies, for the
// caller to mint a mktemp root.
const panePathTryShared = `IFS="$nl"; for cand in $cands; do unset IFS; r=$cand; ` + panePathRootSettle +
	panePathRootOwned + ` || { echo "af: the pane PATH root $r is not a private af-owned directory; trying another" >&2; continue; }; ` +
	`[ -e "$r/$leaf" ] && [ ! -L "$r/$leaf" ] && { echo "af: $r/$leaf exists and is not a symlink; trying another root" >&2; continue; }; ` +
	`root=$r; break; done; unset IFS; `

// panePathMintTmp mints the unpredictable 0700 `mktemp -d` root under
// ${TMPDIR:-/tmp} — the last resort when no per-uid candidate qualified
// (#7770 R4): a ':'-bearing or relative TMPDIR falls back to the literal /tmp.
// made=1 arms the EXIT trap so a root the run could not commit is reaped.
const panePathMintTmp = `base="${TMPDIR:-/tmp}"; case "$base" in /*) ;; *) base=/tmp;; esac; case "$base" in *:*) base=/tmp;; esac; ` +
	`root=$(mktemp -d "$base/af-path.XXXXXX") || { echo "af: no colon-free pane PATH indirection can be built: no per-uid directory qualified and mktemp -d under $base failed" >&2; exit 96; }; made=1; `

// panePathRootChecks re-verifies the shape every PATH entry needs — an
// absolute, colon-free spelling — on whatever $root selection produced. The
// shared candidates are filtered on it up front; the mktemp output is checked
// here, and a remote that cannot produce one colon-free spelling anywhere is
// the only refusal left, still before the workspace and binary are staged.
const panePathRootChecks = `case "$root" in /*) ;; *) echo "af: the indirection root $root is not an absolute path" >&2; exit 97;; esac; ` +
	`case "$root" in *:*) echo "af: no colon-free pane PATH indirection can be built: the indirection root $root contains ':'" >&2; exit 98;; esac; `

// panePathCommit is the commit block both link scripts share: create the link,
// then PROVE the result before any path is reported — the leaf must exist as a
// symlink whose readlink equals the session dir byte for byte AND the root
// must still verify af-owned. The root check runs again AFTER the ln because a
// root in a directory other uids can write could be swapped between the first
// verify and this one (#7787). Anything less fails the run rather than
// committing a PATH entry that does not reach the staged `af` (#7770 R1). The
// in-dir marker — best-effort, the link stands without it — is what lets the
// orphan sweep find and reap the link and its empty root without the session
// row (#7787).
const panePathCommit = `ln -sfn "$sess" "$link" && [ -L "$link" ] && [ "$(readlink "$link")" = "$sess" ] && r=$root && ` + panePathRootOwned + ` && ` +
	`{ printf '%s\n' "$link" > "$sess"/` + panePathMarkerName + ` 2>/dev/null || :; } && printf 'AF_PANE_LINK=%s\n' "$link"`

// panePathEnsureAtScript recreates the indirection AT a path af already
// recorded — a restart, whose persisted launch script names this exact PATH
// entry. The recorded root is re-established, then VERIFIED: a real, uid-owned
// 0700 directory holding a free-or-symlink leaf. On ANY failure the script
// mints a fresh root the same way a fresh provision chooses it (per-uid
// candidates, then mktemp) and reports the new link path — a name in a
// directory other uids can write is abandoned, never re-created by name or
// written inside (#7787). The caller records the reported path; the link
// inside a verified recorded root is left exactly where the record put it.
func panePathEnsureAtScript(link, sess string) string {
	return `link=` + shellQuote(link) + `; sess=` + shellQuote(sess) + `; leaf=` + shellQuote(panePathLinkName(sess)) + `; ` +
		`root="${link%/*}"; made=; ok=1; ` +
		`trap 'rc=$?; if [ "$rc" -ne 0 ] && [ -n "$made" ]; then rm -f "$link" 2>/dev/null; rmdir "$root" 2>/dev/null; fi' EXIT; ` +
		`r=$root; ` + panePathRootSettle + panePathRootOwned + ` || ok=; ` +
		`[ -n "$ok" ] && [ -e "$link" ] && [ ! -L "$link" ] && ok=; ` +
		`if [ -z "$ok" ]; then root=; ` + panePathSharedRoots + panePathTryShared +
		`if [ -z "$root" ]; then ` + panePathMintTmp + `fi; ` + panePathRootChecks +
		`link="$root/$leaf"; fi; ` + panePathCommit
}

// panePathChooseScript picks where a fresh ':'-bearing session dir's link
// lives and creates it: the first per-uid candidate that verifies
// ($HOME/.af-path, then $XDG_RUNTIME_DIR/.af-path), else the mktemp root. See
// the fragment comments for the rules; a candidate is skipped, never fought
// over, when it cannot be proven a private af-owned directory.
//
// The EXIT trap drops a mktemp root the run could not commit (an empty
// directory af alone could have made); on success it holds the link, so the
// rmdir can only fail — the teardown removes it for real.
func panePathChooseScript(sess string) string {
	return `sess=` + shellQuote(sess) + `; leaf=` + shellQuote(panePathLinkName(sess)) + `; root=; link=; made=; ` +
		`trap 'rc=$?; if [ "$rc" -ne 0 ] && [ -n "$made" ]; then rm -f "$link" 2>/dev/null; rmdir "$root" 2>/dev/null; fi' EXIT; ` +
		panePathSharedRoots + panePathTryShared +
		`if [ -z "$root" ]; then ` + panePathMintTmp + `fi; ` + panePathRootChecks +
		`link="$root/$leaf"; ` + panePathCommit
}

// sweepPanePathLink validates the marker an orphan session dir records against
// the dir itself, for the startup orphan sweep (#7787): it must name an
// absolute colon-free path whose leaf carries THIS dir's digest — proof af
// staged the indirection for this session, not a stranger's file a same-uid
// process wants deleted — inside one of the af-owned root shapes (.af-path or
// a mktemp af-path.* root). Anything else is not af's to remove and is left.
func sweepPanePathLink(dir, marker string) (link string, ok bool) {
	link = strings.TrimSpace(marker)
	if link == "" || !path.IsAbs(link) || strings.Contains(link, ":") {
		return "", false
	}
	if path.Base(link) != panePathLinkName(dir) {
		return "", false
	}
	switch parent := path.Base(path.Dir(link)); {
	case parent == remotePathLinkDir, strings.HasPrefix(parent, "af-path."):
		return link, true
	}
	return "", false
}
