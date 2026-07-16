package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The web-selftest harness installs a FAKE code-server on PATH
// (scripts/container/web-selftest-entry.sh) and the daemon spawns it through the
// ordinary detection path, exactly as it would a real install. That makes the
// fake's argv parser a silent dependency of vscodeArgs — a test fixture that has
// to track production argv, with nothing but review connecting them.
//
// They drifted, and #1895 is what it cost: #1873 moved the editor onto a unix
// socket, the fake kept demanding the --bind-addr of the old TCP transport and
// exit(2)'d on every spawn, and BOTH changes were green on their own. The symptom
// was as far from the cause as it gets — the fake's error went nowhere (startOne
// discards the child's output, deliberately), so the only evidence was the vscode
// tab timing out 30s into a 3-minute container run. The suite is serial, so that
// one red also hid every test behind it and master's web gate went blind.
//
// These tests are the guard that keeps the next drift from hiding: they derive the
// contract from vscodeArgs itself and fail in `go test`, in milliseconds, naming
// the flag. Nothing here re-tests the fixture's behavior — the container run does
// that. It only tests that the fixture is still parsing the argv the daemon sends.

// fakeEditorScript is the harness fixture that must track vscodeArgs.
const fakeEditorScript = "../scripts/container/web-selftest-entry.sh"

// fixtureValueFlagsPattern captures the fake's value-flag set: the flags it knows
// consume the NEXT argv word. It is the fake's whole parsing contract — a flag
// missing from it does not just go unread, it shifts the parse, and the flag's
// value is mistaken for the positional worktree.
var fixtureValueFlagsPattern = regexp.MustCompile(`const valueFlags = new Set\(\[([^\]]*)\]\)`)

var fixtureFlagPattern = regexp.MustCompile(`"(--[a-z-]+)"`)

func readFakeEditorFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(fakeEditorScript))
	if err != nil {
		t.Fatalf("reading the web-selftest fixture %s failed: %v", fakeEditorScript, err)
	}
	return string(b)
}

// valueTakingArgs reports which flags in args consume the next word.
//
// Derived from the argv rather than listed, so this cannot itself go stale: the
// final element is the positional worktree, and in what remains a flag followed by
// a non-flag is exactly a flag with a value. (--disable-workspace-trust sits last
// among the flags precisely because the worktree follows it, so dropping the
// positional first is what keeps it from reading as a value flag.)
func valueTakingArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	rest := args[:len(args)-1] // drop the positional worktree
	var out []string
	for i := 0; i < len(rest); i++ {
		if !strings.HasPrefix(rest[i], "--") {
			continue
		}
		if i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "--") {
			out = append(out, rest[i])
			i++ // skip the value; it is not a flag
		}
	}
	return out
}

// TestWebSelftestFakeEditorParsesEveryValueFlag is the #1895 guard.
//
// Every flag the daemon passes WITH a value must be one the fake knows takes one.
// Miss one and the fake misparses in two ways at once: the flag's value is read as
// the positional worktree (so the #folder assertion reads a socket path), and the
// real worktree may never be seen at all.
func TestWebSelftestFakeEditorParsesEveryValueFlag(t *testing.T) {
	script := readFakeEditorFixture(t)

	m := fixtureValueFlagsPattern.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("no `const valueFlags = new Set([...])` in %s: the fake's argv parser was "+
			"restructured, so this guard no longer reads its contract — update the guard with it", fakeEditorScript)
	}
	got := map[string]bool{}
	for _, f := range fixtureFlagPattern.FindAllStringSubmatch(m[1], -1) {
		got[f[1]] = true
	}

	// The fixture installs the fake under the name "code-server", so the daemon
	// infers the code-server dialect (flavorForBinary) and sends this argv.
	args := vscodeArgs(flavorCodeServer, "/tmp/af-vscode-guard.sock", "/tmp/af-worktree")
	for _, flag := range valueTakingArgs(args) {
		if !got[flag] {
			t.Errorf("the daemon passes %s with a value, but the web-selftest fake code-server (%s) "+
				"does not list it in valueFlags: it will read %s's value as the positional worktree. "+
				"Add it there — a fixture that misparses the daemon's argv fails as a 30s iframe "+
				"timeout in the container, not as this test", flag, fakeEditorScript, flag)
		}
	}
}

// TestWebSelftestFakeEditorBindsTheDaemonsSocket pins the transport itself.
//
// The value-flag check above proves the fake READS --socket; this proves it
// LISTENS on it. Both are needed: a fake that parses the socket path perfectly and
// binds anything else is spawned, detected, and never reachable — which is #1895's
// exact shape. The daemon dials the socket it named and nothing else
// (newVSCodeTransport), so the fixture binding that path is the whole contract.
func TestWebSelftestFakeEditorBindsTheDaemonsSocket(t *testing.T) {
	script := readFakeEditorFixture(t)

	args := vscodeArgs(flavorCodeServer, "/tmp/af-vscode-guard.sock", "/tmp/af-worktree")
	if !strings.Contains(strings.Join(args, " "), "--socket ") {
		t.Fatalf("vscodeArgs no longer passes --socket for the code-server flavor: the fake "+
			"code-server in %s binds whatever --socket names, so it must be updated to match "+
			"the new transport", fakeEditorScript)
	}
	if !strings.Contains(script, "server.listen(socketPath") {
		t.Errorf("the web-selftest fake code-server (%s) does not listen on the --socket path the "+
			"daemon named: the proxy dials that socket and only that socket, so the vscode tab "+
			"will time out with no error anywhere", fakeEditorScript)
	}
}
