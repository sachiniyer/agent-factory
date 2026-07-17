package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// These tests execute the launch incantation docs/remote-hooks.md tells users
// to copy, on whatever platform they run on. They exist because that line was
// wrong for every macOS reader for as long as the doc has existed (#1946): it
// said `setsid`, which is util-linux and simply does not exist on a Mac, so the
// recipe failed with `setsid: command not found` — an error naming a coreutil
// the reader never asked for, reported by af as its own failure.
//
// A doc example is only as true as the last platform someone ran it on, and
// nothing ran this one on darwin until the macOS job landed (#1931). So the
// example is executed here rather than reasoned about: on the macOS runner this
// is the verification that the replacement genuinely detaches, which is the one
// property `setsid` was there to provide and the one a careless substitute
// would silently drop.
//
// TestRemoteHookRoundTripMockRemote cannot cover this — it is skipped on darwin
// (#1945, clientless PTY streaming), which is exactly the platform whose answer
// we need. These tests touch no PTY, so they run everywhere.

// docLaunchLine is the detach used by docs/remote-hooks.md's launch.sh, and by
// the mock hook fixture. Keep the three in step: if this changes, the doc is
// wrong, and a Mac user pays for it.
const docLaunchLine = `nohup "$CHILD" >"$OUT" 2>"$ERR" &`

// TestDocumentedDetachToolExists is the flat contradiction of #1946: whatever
// the doc tells a reader to run must exist on the reader's machine.
//
// setsid fails this on darwin. If this ever fails, the recipe is handing
// someone a command not found.
func TestDocumentedDetachToolExists(t *testing.T) {
	if _, err := exec.LookPath("nohup"); err != nil {
		t.Fatalf("docs/remote-hooks.md tells users to run `nohup`, which is not on PATH on %s: %v — "+
			"the documented recipe cannot work here (#1946)", runtime.GOOS, err)
	}
}

// darwinBaseSystemBinDirs are the directories macOS's own userland ships in.
// Anything outside them is user-installed (Homebrew, MacPorts, /usr/local),
// which is a property of one machine and not of the platform.
var darwinBaseSystemBinDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}

// TestSetsidIsNotInDarwinsBaseSystem records the fact the doc fix rests on, so
// nobody has to take #1946's word for it (or re-litigate it from a Linux box).
//
// It checks the BASE SYSTEM directories, not $PATH, and that distinction is the
// whole correctness of the test. A developer or runner may well have setsid
// installed — `brew install util-linux` puts one on $PATH — and that says
// nothing about what a Mac user reading our docs has out of the box. Failing on
// a $PATH hit would mean "I did not find it where I expected" masquerading as
// "it does not exist", which is the same fabricated-negative shape this PR
// exists to remove; here it would just point the other way.
//
// So a user-installed setsid is reported and tolerated. Only setsid appearing
// in the BASE SYSTEM would falsify the doc, and that is worth failing over.
func TestSetsidIsNotInDarwinsBaseSystem(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("records darwin's userland; %s is not the platform in question", runtime.GOOS)
	}
	for _, dir := range darwinBaseSystemBinDirs {
		p := filepath.Join(dir, "setsid")
		if _, err := os.Stat(p); err == nil {
			t.Errorf("setsid exists in macOS's base system at %s — #1946's premise (setsid is "+
				"util-linux and ships only on Linux) is no longer true and docs/remote-hooks.md "+
				"needs re-reading", p)
		}
	}
	// Informational only: a user-installed setsid is normal and irrelevant.
	if p, err := exec.LookPath("setsid"); err == nil {
		t.Logf("note: this machine has a user-installed setsid at %s (outside the base system). "+
			"That does not contradict the doc — a Mac user who has not installed util-linux has no "+
			"setsid, which is who the recipe is written for.", p)
	}
}

// TestDocumentedLaunchDetachesAndSurvivesTheScript is the verification Sachin
// asked for, run on the platform in question rather than assumed from a Linux
// box.
//
// The whole job of the detach is that the launched server OUTLIVES launch_cmd:
// the script exits the moment it has the banner, and af then talks to a server
// that must still be running. A substitute that does not detach would fail
// later and more confusingly than the missing binary it replaced — the script
// would succeed, hand back a URL, and the server would be gone.
//
// So this runs the documented shape end to end and asserts the child is alive
// AFTER the script has exited, using proctree (which since #1939 can actually
// see processes on darwin — before that this assertion could not have been
// written here at all).
func TestDocumentedLaunchDetachesAndSurvivesTheScript(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")

	// A stand-in for `af agent-server`: prints a banner, then stays up. If the
	// detach works, it is still here after launch.sh returns.
	child := writeScript(t, filepath.Join(dir, "child.sh"), `
echo '{"addr":"127.0.0.1:1"}'
exec sleep 300
`)

	launch := writeScript(t, filepath.Join(dir, "launch.sh"), `
CHILD="`+child+`"
OUT="`+filepath.Join(dir, "banner.json")+`"
ERR="`+filepath.Join(dir, "server.log")+`"
`+docLaunchLine+`
echo $! > "`+pidFile+`"
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  grep -q '"addr"' "$OUT" 2>/dev/null && break
  sleep 0.1
done
grep -q '"addr"' "$OUT" || { echo "child printed no banner:" >&2; cat "$ERR" >&2; exit 1; }
`)

	// CombinedOutput is exactly how af runs launch_cmd
	// (session/backend_hook_backend.go). It reads the pipes to EOF, so if the
	// child inherited them this call hangs until the child exits — the failure
	// mode the doc's redirects exist to prevent. A 30s bound turns that hang
	// into a readable failure instead of a timed-out package.
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		defer close(done)
		out, runErr = exec.Command(launch).CombinedOutput()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("launch script did not return within 30s: af reads launch_cmd's output to EOF, so a " +
			"child holding stdout/stderr open hangs provisioning for the full 5-minute timeout")
	}
	if runErr != nil {
		t.Fatalf("the documented launch recipe failed on %s: %v\noutput:\n%s", runtime.GOOS, runErr, out)
	}

	pid := readPIDFile(t, pidFile)
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})

	// The script has exited. The child must not have.
	snap, err := proctree.Snapshot()
	if err != nil {
		t.Fatalf("reading the process table on %s: %v", runtime.GOOS, err)
	}
	p, ok := snap[pid]
	if !ok {
		t.Fatalf("the launched child (pid %d) is gone after launch.sh exited on %s — the documented "+
			"detach does not survive its parent here, which is the one thing it must do (#1946)",
			pid, runtime.GOOS)
	}

	// Reparented, i.e. genuinely orphaned rather than still owned by a shell
	// that happens to linger. The script's own pid is gone, so the child cannot
	// still name it as a parent.
	if p.PPID == 0 {
		t.Errorf("child %d reports ppid 0 after the script exited; expected reparenting to init/launchd", pid)
	}
}

// TestDocumentedLaunchSurfacesTheShellsError pins the other half of #1946: when
// the launch command cannot run at all, the reader must be told WHAT THE SHELL
// SAID, not just that no banner appeared.
//
// This is why the doc's failure branch cats the server's log. af reports what
// the script prints and nothing else, so a recipe that swallows its own stderr
// leaves the user staring at "printed no banner" with the actual cause —
// "command not found" — discarded. The shape is verified here, on the same
// script shape a Mac user runs.
func TestDocumentedLaunchSurfacesTheShellsError(t *testing.T) {
	dir := t.TempDir()

	launch := writeScript(t, filepath.Join(dir, "launch.sh"), `
CHILD="definitely-not-a-real-command-af"
OUT="`+filepath.Join(dir, "banner.json")+`"
ERR="`+filepath.Join(dir, "server.log")+`"
: > "$OUT"
`+docLaunchLine+`
sleep 0.5
grep -q '"addr"' "$OUT" || { echo "af agent-server printed no banner; its output was:" >&2; cat "$ERR" >&2; exit 1; }
`)

	out, err := exec.Command(launch).CombinedOutput()
	if err == nil {
		t.Fatalf("launch script succeeded with a nonexistent command; want failure. output:\n%s", out)
	}
	// The shell's own diagnosis must reach af's captured output. This is the
	// property that was in doubt: the redirect sends it to $ERR, and the failure
	// branch is what brings it back out.
	//
	// Asserted on the COMMAND NAME rather than the wording, deliberately. GNU
	// nohup says "failed to run command", BSD nohup (macOS) does not, and bash's
	// own miss is "command not found" — pinning any one phrasing would pass on
	// Linux and fail on darwin, which is the bug class this whole PR is about.
	// What must hold everywhere is that the failure names the thing that failed.
	if !strings.Contains(string(out), "definitely-not-a-real-command-af") {
		t.Errorf("the launch failure did not name the command that could not run; af would report only "+
			"the downstream symptom and the real cause would reach nobody (#1946).\ncaptured:\n%s", out)
	}
	if !strings.Contains(string(out), "printed no banner") {
		t.Errorf("the launch failure lost the script's own summary line.\ncaptured:\n%s", out)
	}
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing pid file %q: %v", data, err)
	}
	return pid
}
