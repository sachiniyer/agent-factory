// Package testguard fences test binaries off from the developer's real
// agent-factory environment. Test packages that can spawn af processes or
// write config files call ConfigTripwire from TestMain; if any test (or a
// child process it spawned) escapes its AGENT_FACTORY_HOME sandbox and
// touches the real config.json, the package run fails loudly instead of the
// user discovering days later that their settings were silently replaced
// (#837). TmuxTripwire does the same for real tmux sessions leaked onto the
// developer's tmux server, SandboxHome defaults a whole package into a
// throwaway AGENT_FACTORY_HOME, SandboxTmux defaults a whole package onto a
// private tmux server so no test can even see the developer's real one
// (#1122), and IsolateTmux gives a single test a private server of its own so
// nothing it creates can outlive it (#1056).
//
// This package deliberately has no dependency on the config package — config
// imports session/tmux, and the tripwire must be usable from that package's
// tests without an import cycle — so the config-dir resolution below mirrors
// config.GetConfigDir.
package testguard

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sockpath"
)

// ambientConfigPaths resolves the config files the test process could touch
// with its ambient (pre-test) environment — config.json and config.toml
// (#1030) in $AGENT_FACTORY_HOME when set, otherwise ~/.agent-factory.
// Returns nil when the directory cannot be resolved (no HOME — e.g. some CI
// sandboxes), in which case the tripwire is a no-op.
func ambientConfigPaths() []string {
	dir := ambientConfigDir()
	if dir == "" {
		return nil
	}
	return []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "config.toml")}
}

// ambientConfigDir resolves the config directory for ambientConfigPaths,
// mirroring config.GetConfigDir. Returns "" when unresolvable.
func ambientConfigDir() string {
	dir := os.Getenv("AGENT_FACTORY_HOME")
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		switch {
		case dir == "~":
			dir = home
		case strings.HasPrefix(dir, "~/"):
			dir = filepath.Join(home, dir[2:])
		default:
			// Malformed tilde form; GetConfigDir would error out, so there
			// is no real config at risk.
			return ""
		}
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".agent-factory")
	}
	return dir
}

func hashFile(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	sum := sha256.Sum256(data)
	return sum[:], true, nil
}

// ConfigTripwire snapshots the real config files (config.json and
// config.toml, #1030) and returns a verify func for TestMain to call after
// m.Run(). Verify returns a non-nil error when:
//   - a file existed at snapshot time and was modified or deleted, or
//   - a file did not exist and a test materialized one.
//
// On boxes without a resolvable home (or with AF_DISABLE_CONFIG_TRIPWIRE=1)
// both snapshot and verify are no-ops, so CI runs are unaffected.
func ConfigTripwire() func() error {
	if os.Getenv("AF_DISABLE_CONFIG_TRIPWIRE") == "1" {
		return func() error { return nil }
	}
	type snapshot struct {
		path    string
		before  []byte
		existed bool
	}
	var snaps []snapshot
	for _, path := range ambientConfigPaths() {
		before, existed, err := hashFile(path)
		if err != nil {
			// Unreadable real config (permissions?) — nothing we can guard
			// for this file.
			continue
		}
		snaps = append(snaps, snapshot{path: path, before: before, existed: existed})
	}
	return func() error {
		for _, snap := range snaps {
			after, exists, err := hashFile(snap.path)
			if err != nil {
				return fmt.Errorf("config tripwire: cannot re-read %s after the test run: %w", snap.path, err)
			}
			switch {
			case snap.existed && !exists:
				return fmt.Errorf("config tripwire: %s was DELETED during this package's test run — a test escaped its AGENT_FACTORY_HOME sandbox (#837)", snap.path)
			case snap.existed && !bytes.Equal(snap.before, after):
				return fmt.Errorf("config tripwire: %s was MODIFIED during this package's test run — a test escaped its AGENT_FACTORY_HOME sandbox (#837)", snap.path)
			case !snap.existed && exists:
				return fmt.Errorf("config tripwire: %s was CREATED during this package's test run — a test materialized config into the real config dir (#837)", snap.path)
			}
		}
		return nil
	}
}

// ambientAFSessions lists the af_-prefixed session names on the tmux server
// the current environment resolves to. A nil map means no reachable server —
// nothing to leak against.
func ambientAFSessions() map[string]bool {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil
	}
	sessions := make(map[string]bool)
	for _, name := range strings.Split(string(out), "\n") {
		// Anchor the prefix so a non-agent session like "my_af_project"
		// never trips the wire (same anchoring as tmux.CleanupSessions).
		if strings.HasPrefix(name, "af_") {
			sessions[name] = true
		}
	}
	return sessions
}

// TmuxTripwire snapshots the af_-prefixed sessions on the ambient tmux
// server and returns a verify func for TestMain to call after m.Run().
// Verify returns a non-nil error when the run left behind af_ sessions that
// were not there before — i.e. a test created a real agent tmux session on
// the developer's server and never killed it (#1056). Tests that need real
// tmux should call IsolateTmux, whose private server this tripwire cannot
// even see.
//
// Call it BEFORE any test changes TMUX_TMPDIR/TMUX so both snapshots target
// the ambient server. No-ops when tmux is not installed or with
// AF_DISABLE_TMUX_TRIPWIRE=1. Caveat for dev boxes running a real daemon:
// an af_ session legitimately created by the real daemon DURING the package
// run is indistinguishable from a leak; the error lists the session names so
// that case is recognizable, and the escape hatch covers it.
func TmuxTripwire() func() error {
	if os.Getenv("AF_DISABLE_TMUX_TRIPWIRE") == "1" {
		return func() error { return nil }
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return func() error { return nil }
	}
	before := ambientAFSessions()
	return func() error {
		var leaked []string
		for name := range ambientAFSessions() {
			if !before[name] {
				leaked = append(leaked, name)
			}
		}
		if len(leaked) == 0 {
			return nil
		}
		sort.Strings(leaked)
		return fmt.Errorf("tmux tripwire: this package's test run left tmux session(s) %v on the ambient tmux server — a test created a real af_ session without testguard.IsolateTmux or cleanup (#1056)", leaked)
	}
}

// SandboxHome points AGENT_FACTORY_HOME at a fresh temp dir for the whole
// package run and returns a restore func. Call it from TestMain AFTER
// ConfigTripwire (which must snapshot the real, pre-sandbox config) and
// BEFORE log.Initialize (so the package's log file lands in the sandbox
// instead of the production daemon log — #1056). Individual tests that
// t.Setenv their own home still win for their duration; this is the default
// for tests that never set one.
//
// It also scrubs the AF_SESSION/AF_HOME ancestry markers (#1120,
// session/tmux/envmarker.go — string literals here because testguard stays
// dependency-free). A test run launched from inside a production af pane
// inherits the pane's markers, so every child a test spawns would otherwise
// carry the production install's identity — which breaks any test asserting
// on marker absence (e.g. doctor's home-match gate) and misattributes test
// children to the real install.
func SandboxHome() func() {
	dir, err := os.MkdirTemp("", "af-test-home-")
	if err != nil {
		panic("testguard: cannot create sandbox AGENT_FACTORY_HOME: " + err.Error())
	}
	prev, had := os.LookupEnv("AGENT_FACTORY_HOME")
	if err := os.Setenv("AGENT_FACTORY_HOME", dir); err != nil {
		panic("testguard: cannot set sandbox AGENT_FACTORY_HOME: " + err.Error())
	}
	prevSession, hadSession := os.LookupEnv("AF_SESSION")
	prevAFHome, hadAFHome := os.LookupEnv("AF_HOME")
	_ = os.Unsetenv("AF_SESSION")
	_ = os.Unsetenv("AF_HOME")
	return func() {
		if had {
			_ = os.Setenv("AGENT_FACTORY_HOME", prev)
		} else {
			_ = os.Unsetenv("AGENT_FACTORY_HOME")
		}
		if hadSession {
			_ = os.Setenv("AF_SESSION", prevSession)
		} else {
			_ = os.Unsetenv("AF_SESSION")
		}
		if hadAFHome {
			_ = os.Setenv("AF_HOME", prevAFHome)
		} else {
			_ = os.Unsetenv("AF_HOME")
		}
		_ = os.RemoveAll(dir)
	}
}

// SandboxTmux points the WHOLE package run at a private tmux server, exactly
// like SandboxHome does for AGENT_FACTORY_HOME: a fresh TMUX_TMPDIR socket dir
// with $TMUX cleared, set before any test runs. It is the structural backstop
// for #1122: a test that forgets IsolateTmux now runs against the package's
// private server instead of the developer's real one, so an escaped create or
// a CleanupSessions-style sweep cannot touch production af_ sessions. Tests
// that need a server of their own still call IsolateTmux; its t.Setenv wins
// for the test's duration and nests another level down, never back up to the
// ambient server.
//
// Call it from TestMain AFTER TmuxTripwire (which must snapshot the ambient
// server) and call the returned restore BEFORE the tripwire's verify (so the
// verify probes the ambient server again). The restore kills the package's
// private server, so anything a test leaked onto it dies there.
//
// No-op (returns a no-op restore) when tmux is not installed — there is no
// server to fence off, and resolving a socket dir would only mask that.
func SandboxTmux() func() {
	if _, err := exec.LookPath("tmux"); err != nil {
		return func() {}
	}
	dir, err := os.MkdirTemp("", "af-tmux-pkg-")
	if err != nil {
		panic("testguard: cannot create package tmux socket dir: " + err.Error())
	}
	prevTmpdir, hadTmpdir := os.LookupEnv("TMUX_TMPDIR")
	prevTmux, hadTmux := os.LookupEnv("TMUX")
	if err := os.Setenv("TMUX_TMPDIR", dir); err != nil {
		panic("testguard: cannot set package TMUX_TMPDIR: " + err.Error())
	}
	// Empty counts as unset for tmux's "am I inside tmux" check; keeping the
	// real value would make tmux ignore TMUX_TMPDIR entirely ($TMUX wins in
	// socket resolution).
	if err := os.Setenv("TMUX", ""); err != nil {
		panic("testguard: cannot clear TMUX: " + err.Error())
	}
	return func() {
		// Kill the package server BEFORE restoring the env so the kill
		// targets the private socket dir, then drop it.
		_ = exec.Command("tmux", "kill-server").Run()
		_ = os.RemoveAll(dir)
		if hadTmpdir {
			_ = os.Setenv("TMUX_TMPDIR", prevTmpdir)
		} else {
			_ = os.Unsetenv("TMUX_TMPDIR")
		}
		if hadTmux {
			_ = os.Setenv("TMUX", prevTmux)
		} else {
			_ = os.Unsetenv("TMUX")
		}
	}
}

// IsolateTmux points the test at a private tmux server: a fresh TMUX_TMPDIR
// socket dir with $TMUX cleared, so a test run from inside a tmux pane does
// not fall back to the surrounding server ($TMUX wins over TMUX_TMPDIR in
// tmux's socket resolution). A cleanup kills the private server, so every
// session the test created dies with it and nothing can leak onto the
// developer's real server (#1056). Child processes — exec'd af binaries and
// the daemons they spawn — inherit both variables through os.Environ().
//
// The socket dir lives under os.TempDir() rather than t.TempDir() because
// unix socket paths are length-limited (~104 bytes) and t.TempDir() embeds
// the full test name. Skips the test when tmux is not installed; must not be
// used with t.Parallel (t.Setenv forbids it).
func IsolateTmux(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	dir, err := os.MkdirTemp("", "af-tmux-")
	if err != nil {
		t.Fatalf("testguard: cannot create private tmux socket dir: %v", err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	// Empty counts as unset for tmux's "am I inside tmux" check; t.Setenv
	// cannot fully unset a variable.
	t.Setenv("TMUX", "")
	t.Cleanup(func() {
		// Runs before t.Setenv's env restore (cleanups are LIFO), so this
		// kill targets the private server, then removes its socket dir.
		_ = exec.Command("tmux", "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
}

// maxUnixSocketPathLen is the ceiling a TEST's socket path must respect: what
// fits on every platform we ship, not what fits here.
//
// sockpath.Portable rather than sockpath.Max on purpose. Max is this kernel's
// real limit (107 on Linux), and a harness held to that would let a 105-byte
// path sail through on Linux and fail on macOS — the exact shape of the bug
// class this repo keeps rediscovering. Holding tests to the portable floor
// makes a portability break fail on whichever runner reaches it first.
//
// It is imported now rather than duplicated (it used to be a second hardcoded
// 103): internal/sockpath is a leaf, so it raises none of the config import
// cycle this package's doc comment warns about.
const maxUnixSocketPathLen = sockpath.Portable

// SocketTempDir returns a fresh temp dir short enough to hold a Unix socket, and
// removes it when the test ends.
//
// Prefer it over t.TempDir() for any directory that will hold a .sock — whether
// that is the socket's own dir or an AGENT_FACTORY_HOME the daemon derives one
// from. t.TempDir() embeds the TEST'S NAME in the path, and on macOS it hangs
// that off a base that is already ~48 bytes (/var/folders/<hash>/T/). A
// descriptively-named test therefore lands ~140 bytes — past sun_path's 104 —
// and net.Listen fails with a bare "invalid argument" that names nothing (#1940).
// Linux hides this completely: its cap is 108 AND its base is /tmp, so the same
// path fits with room to spare. That is why it survived until the suite first ran
// on darwin.
//
// This is a harness concern, not a product one: af's real sockets live in
// AGENT_FACTORY_HOME (~/.agent-factory/daemon.sock), which is short by default.
func SocketTempDir(t *testing.T) string {
	t.Helper()
	// /tmp rather than $TMPDIR. macOS's $TMPDIR canonicalizes to a 56-byte
	// /private/var/folders/<hash>/T, which leaves too little room once a caller
	// appends its own structure: a vscode socket (…/af-home/vscode/<hash>.sock)
	// lands at 107 bytes and trips the editor's own guard even with no test name in
	// the path. /tmp canonicalizes to /private/tmp — 12 bytes — buying 44 bytes of
	// headroom and turning knife-edge margins into comfortable ones (that same
	// vscode socket: 63). Linux's /tmp is already the default, so nothing changes
	// there.
	dir, err := os.MkdirTemp("/tmp", "af-")
	if err != nil {
		// A sandbox with no writable /tmp: fall back to $TMPDIR rather than fail.
		// SocketPath's length check still catches an overrun loudly if one follows.
		dir, err = os.MkdirTemp("", "af-")
	}
	if err != nil {
		t.Fatalf("testguard: creating a socket-safe temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Canonicalize: macOS hands out /var/folders/…, and /var is a symlink to
	// /private/var. Production resolves what it is given to the physical path, so
	// an unresolved home makes every assertion about a path derived from it
	// compare /var/… against /private/var/… — the #1918 class, which cost this
	// package a second round of macOS failures after the socket fix landed. The
	// resolved spelling is the one production will report, so hand that out and
	// the class cannot recur through this helper. No-op on Linux.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("testguard: canonicalizing socket temp dir: %v", err)
	}
	return resolved
}

// SocketPath joins name onto a SocketTempDir and fails the test up front if the
// result would not fit in sun_path. Without the check the bind fails later as an
// opaque "invalid argument", which is exactly the diagnosis this helper exists to
// spare the next person.
func SocketPath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(SocketTempDir(t), name)
	if len(p) > maxUnixSocketPathLen {
		t.Fatalf("testguard: socket path %q is %d bytes, over the %d-byte sun_path ceiling; "+
			"shorten the socket name", p, len(p), maxUnixSocketPathLen)
	}
	return p
}

// RequireProcFS and HasProcFS lived here until #1939 gave proctree a real
// darwin backend. They are deliberately NOT replaced.
//
// They existed to skip, on macOS, exactly the tests that prove tmux orphan
// reaping and `af doctor`'s process mapping work — because on macOS those
// features did not work, and the tests were right to fail. The skip was honest
// about that (it said so in capitals), but a skip is still a place where the
// suite agrees to stop asking, and this one sat over the four tests that would
// have caught the defect on day one.
//
// If you find yourself wanting a "does this platform have /proc?" helper again,
// the answer is almost certainly a backend in internal/proctree, not a gate in
// the test tree. proctree_other.go is where an unsupported platform belongs, and
// it fails to compile rather than quietly returning nothing.

// CanonicalTempDir returns t.TempDir() resolved to its physical path, and is the
// spelling any test should use for a directory whose path it later compares
// against something the PRODUCT reports.
//
// macOS hands out temp dirs under /var/folders/…, and /var is a symlink to
// /private/var. Production resolves the paths it is given (git, worktree and repo
// resolution all report the physical path), so an expectation written in
// t.TempDir()'s unresolved spelling compares /var/… against /private/var/… and
// fails — on macOS only, which is why this survived until the suite first ran on
// darwin (#1918, and its recurrence in #1931).
//
// Canonicalizing at the SOURCE is the fix, not canonicalizing at each assertion:
// the expectation is then simply written in the same spelling production uses, so
// new assertions are correct by default. That is the precedent tempBinPath set in
// commands/tempbin_test.go for #1921. EvalSymlinks on an already-canonical path is
// a no-op, so this is inert on Linux.
func CanonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("testguard: canonicalizing temp dir: %v", err)
	}
	return dir
}

// SkipDarwinPTYStream skips a test that asserts bytes flow through the clientless
// PTY stream (tmux pipe-pane -> dd -> FIFO -> ptyBroker.readLoop).
//
// THIS SKIP HIDES A REAL GAP (#1945) — it is not a harness quirk. On the darwin CI
// runner that path delivers NO bytes: typed input never reaches the stream, and
// preview renders blank. It is not the transport (tests that never touch the
// agent-server fail identically), not tmux itself (capture-pane-based tests pass
// on the same runner), and not BSD dd buffering (dd write()s each partial read —
// which is why ptychannel_tmux.go picked it over cat).
//
// Read #1945 before acting on this: it is NOT established that live panes are
// broken for every macOS user — a real darwin user's panes work, so this may be
// environmental to the runner. What IS established is that these tests cannot run
// there, which is why they are skipped rather than deleted.
//
// TO REVERSE: grep for SkipDarwinPTYStream, delete this helper and its call sites,
// and let the macOS job confirm. Do not extend it to new tests.
func SkipDarwinPTYStream(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("clientless PTY streaming delivers no bytes on the darwin runner: typed input " +
			"never reaches the stream — see #1945 (REAL DEFECT, not a test-harness assumption; " +
			"this test times out rather than fails)")
	}
}
