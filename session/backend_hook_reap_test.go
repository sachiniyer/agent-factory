package session

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #1955 invariant: a launch_cmd that STARTED may have provisioned real
// infrastructure on the user's account, so every provisioning failure must run
// delete_cmd — "it failed" is not evidence that nothing was created. The mirror
// of destruction-requires-positive-evidence: absence of a success signal is not
// absence of a resource, and forgetting must be the safe direction.
//
// These tests mutate the package-level hook timeout vars, so none of them may
// run in parallel. Every script they write touches ONLY its own t.TempDir().

// hookState is a self-contained launch/delete script pair operating on a state
// tree inside t.TempDir(), standing in for the user's cloud provider: launch
// "provisions" a sandbox dir, delete reaps it and logs that it ran.
type hookState struct {
	dir    string // the state tree the scripts read/write — all under t.TempDir()
	launch string // path to launch.sh
	delete string // path to delete.sh
}

// sandbox is the "provisioned resource" for slug — the thing that leaks.
func (h hookState) sandbox(slug string) string {
	return filepath.Join(h.dir, "sandboxes", slug)
}

// deleteRan reports whether delete_cmd was invoked at all (it appends to a log
// before doing any work, so it records even a delete that then fails).
func (h hookState) deleteRan(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(h.dir, "delete-ran.log"))
	return err == nil
}

// deleteRunCount is how many times delete_cmd actually ran: it appends one line
// per invocation BEFORE any wedge/sleep, so a growing count proves a reap
// re-invoked delete_cmd rather than short-circuiting on a latch.
func (h hookState) deleteRunCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, "delete-ran.log"))
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// writeHookScript writes an executable bash script and returns its path. Paths
// are interpolated single-quoted, so the script never depends on cwd or $HOME.
func writeHookScript(t *testing.T, path, body string) string {
	t.Helper()
	const preamble = `#!/usr/bin/env bash
name=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --name) name="$2"; shift 2;;
    *) shift;;
  esac
done
`
	require.NoError(t, os.WriteFile(path, []byte(preamble+body), 0o755))
	return path
}

// newHookState builds the script pair. launchBody runs after the sandbox dir is
// created, so a test only has to say how launch_cmd FAILS; deleteBody defaults
// to a working, idempotent reap in the shape docs/remote-hooks.md recommends.
func newHookState(t *testing.T, launchBody, deleteBody string) hookState {
	t.Helper()
	dir := t.TempDir()
	h := hookState{
		dir:    dir,
		launch: filepath.Join(dir, "launch.sh"),
		delete: filepath.Join(dir, "delete.sh"),
	}
	if deleteBody == "" {
		// Slug-deterministic and idempotent: a slug it has never seen is a
		// success, not an error — the contract this fix now depends on.
		deleteBody = fmt.Sprintf(`rm -rf '%s'/sandboxes/"$name" 2>/dev/null || true`, dir)
	}
	writeHookScript(t, h.launch, fmt.Sprintf(`
mkdir -p '%s'/sandboxes/"$name"
echo "a VM that bills by the hour" > '%s'/sandboxes/"$name"/resource.txt
%s
`, dir, dir, launchBody))
	// No mkdir before the log line: dir is t.TempDir(), which already exists, and
	// on the wedged-reap paths that mkdir was a whole fork+exec standing between
	// the spawn and the only evidence the script ever writes (#2821).
	writeHookScript(t, h.delete, fmt.Sprintf(`
echo "$name" >> '%s'/delete-ran.log
%s
`, dir, deleteBody))
	return h
}

// shrinkHookTimeouts shrinks the production bounds so a test proves they FIRE
// without waiting the real budget, restoring them afterwards.
func shrinkHookTimeouts(t *testing.T, launch, del time.Duration) {
	t.Helper()
	ol, od := hookLaunchTimeout, hookDeleteTimeout
	hookLaunchTimeout, hookDeleteTimeout = launch, del
	t.Cleanup(func() { hookLaunchTimeout, hookDeleteTimeout = ol, od })
}

func newHookProvisioner(h hookState, title string) *hookProvisioner {
	return &hookProvisioner{
		hooks: config.RemoteHooks{LaunchCmd: h.launch, DeleteCmd: h.delete},
		spec:  ProvisionSpec{Title: title, CloneURL: "https://example.invalid/repo.git"},
		slug:  Slugify(title),
	}
}

// TestHookProvisionReapsPartiallyProvisionedLaunch is the headline #1955 test: a
// launch_cmd that provisions and THEN fails must not leak the sandbox. Each case
// is a different way to fail after the resource already exists.
func TestHookProvisionReapsPartiallyProvisionedLaunch(t *testing.T) {
	cases := []struct {
		name string
		// launchBody runs after the sandbox has been "provisioned".
		launchBody string
	}{
		{
			// (a) provisions, then hangs until the launch timeout kills it. The
			// `sleep` is a child that outlives the killed script and holds the
			// output pipe — the shape that made the timeout unbounded.
			name:       "provisions then hangs until timeout",
			launchBody: "sleep 3\n",
		},
		{
			// (b) provisions, then exits non-zero.
			name:       "provisions then exits non-zero",
			launchBody: "echo 'could not reach the agent-server' >&2\nexit 4\n",
		},
		{
			// The pre-existing covered case, kept as a lock: exits 0 having
			// printed no endpoint JSON.
			name:       "exits 0 with no endpoint JSON",
			launchBody: "echo 'all done, forgot the JSON'\nexit 0\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkHookTimeouts(t, 300*time.Millisecond, 5*time.Second)
			h := newHookState(t, tc.launchBody, "")
			p := newHookProvisioner(h, "bills by the hour")

			_, err := p.provisionOrReap()
			require.Error(t, err, "provisioning must fail")

			assert.True(t, h.deleteRan(t), "delete_cmd must run: launch_cmd started, so it may have provisioned")
			assert.NoDirExists(t, h.sandbox(p.slug),
				"the partially provisioned sandbox leaked — it is still billing the user with no record of it on our side")
		})
	}
}

// TestHookProvisionDoesNotReapWhenLaunchNeverStarted is the other half of the
// invariant, and the reason the naive "flip the bool unconditionally" fix is
// wrong: if launch_cmd never ran, nothing was provisioned and delete_cmd must not
// fire. Both spellings are covered because they fail DIFFERENTLY: only a bare
// command name goes through exec.LookPath (*exec.Error); a path — which is the
// documented launch_cmd shape — fails at StartProcess with *fs.PathError, so
// discriminating on *exec.Error alone would misread this case as "ran".
func TestHookProvisionDoesNotReapWhenLaunchNeverStarted(t *testing.T) {
	cases := []struct {
		name      string
		launchCmd func(h hookState) string
	}{
		{
			name:      "path does not exist",
			launchCmd: func(h hookState) string { return filepath.Join(h.dir, "no-such-launch.sh") },
		},
		{
			name: "exists but is not executable",
			launchCmd: func(h hookState) string {
				p := filepath.Join(h.dir, "not-executable.sh")
				require.NoError(t, os.WriteFile(p, []byte("#!/usr/bin/env bash\ntrue\n"), 0o644))
				return p
			},
		},
		{
			name:      "bare name not on PATH",
			launchCmd: func(h hookState) string { return "af-no-such-launch-binary-xyz" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkHookTimeouts(t, 300*time.Millisecond, 5*time.Second)
			h := newHookState(t, "exit 0\n", "")
			p := newHookProvisioner(h, "never started")
			p.hooks.LaunchCmd = tc.launchCmd(h)

			_, err := p.provisionOrReap()
			require.Error(t, err, "provisioning must fail")

			assert.False(t, h.deleteRan(t),
				"delete_cmd must NOT run: launch_cmd never started, so it provisioned nothing")
		})
	}
}

// TestHookProvisionReportsOrphanWhenDeleteFails covers the case where cleanup
// itself fails. A leak the user knows about is survivable; a silent one is not —
// so the failure has to reach the person creating the session, name the orphan,
// and say how to reap it by hand.
func TestHookProvisionReportsOrphanWhenDeleteFails(t *testing.T) {
	shrinkHookTimeouts(t, 300*time.Millisecond, 5*time.Second)
	h := newHookState(t,
		"echo 'provisioned, then died' >&2\nexit 4\n",
		"echo 'the VM is still in CREATING state' >&2\nexit 9\n")
	p := newHookProvisioner(h, "Orphan Me")

	_, err := p.provisionOrReap()
	require.Error(t, err)
	msg := err.Error()

	// The original failure survives...
	assert.Contains(t, msg, "launch_cmd failed", "the original provisioning error must not be swallowed")
	// ...and so does the orphan warning, with everything needed to act on it.
	assert.Contains(t, msg, "may still be running on your infrastructure")
	assert.Contains(t, msg, p.slug, "the warning must name the orphaned sandbox's slug")
	// The command, not a hand-built string: TestHookManualReapCommandIsPasteable
	// proves this one actually runs in a shell, which is the property that matters.
	assert.Contains(t, msg, p.manualReapCommand(), "the warning must give the exact command to reap by hand")
	assert.Contains(t, msg, h.delete, "the reap command must name the configured delete_cmd")
	assert.Contains(t, msg, "the VM is still in CREATING state", "delete_cmd's own output must be surfaced")

	// The resource really is still there — the message is not crying wolf.
	assert.DirExists(t, h.sandbox(p.slug))
}

// TestHookManualReapCommandIsPasteable is the #1966 gate: the reap command we
// print is one we are telling a user to paste into their shell while they are
// already cleaning up a failed launch, so it must survive a delete_cmd path with
// shell metacharacters in it.
//
// It asserts the EXECUTED EFFECT, not the printed string. A string assertion
// would happily bless a command that is wrong in a shell — the unquoted form
// looks perfectly reasonable printed, and only detonates when run.
func TestHookManualReapCommandIsPasteable(t *testing.T) {
	// A delete_cmd living under a path with a space AND an apostrophe: unquoted,
	// the apostrophe opens a quote the shell never closes.
	dir := t.TempDir()
	hookDir := filepath.Join(dir, "sachin's hooks $PATH `x`")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	state := filepath.Join(dir, "state")
	target := filepath.Join(state, "sandboxes", "bills-by-the-hour")
	bystander := filepath.Join(state, "sandboxes", "someone-elses-session")
	for _, d := range []string{target, bystander} {
		require.NoError(t, os.MkdirAll(d, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(d, "resource.txt"), []byte("a VM"), 0o644))
	}

	del := writeHookScript(t, filepath.Join(hookDir, "delete.sh"),
		fmt.Sprintf(`rm -rf '%s'/sandboxes/"$name"`, state))

	p := &hookProvisioner{
		hooks: config.RemoteHooks{DeleteCmd: del},
		spec:  ProvisionSpec{Title: "bills by the hour"},
		slug:  Slugify("bills by the hour"),
	}
	cmdLine := p.manualReapCommand()

	// Paste it into a real shell, exactly as a user would.
	out, err := exec.Command("sh", "-c", cmdLine).CombinedOutput()
	require.NoError(t, err,
		"the command we told the user to paste does not run: %s\noutput: %s", cmdLine, out)

	// It reaped exactly the target...
	assert.NoDirExists(t, target, "the pasted command did not reap the orphan it names")
	// ...and nothing else.
	assert.DirExists(t, bystander, "the pasted command reaped more than the orphan it names")

	// And the warning the user actually reads carries that same command.
	assert.Contains(t, p.orphanWarning(errors.New("boom")), cmdLine)
}

// TestShellQuoteSurvivesARealShell backs the helper manualReapCommand relies on.
// The existing TestShellQuote asserts the produced STRING; this asserts a real
// shell round-trips the value back unchanged, which is the property that actually
// matters and the only way to catch an idiom that merely looks right.
func TestShellQuoteSurvivesARealShell(t *testing.T) {
	cases := map[string]string{
		"space":        "a b",
		"single quote": "sachin's",
		"double quote": `say "hi"`,
		"dollar":       "$HOME and ${x}",
		"backtick":     "`whoami`",
		"newline":      "line1\nline2",
		"everything":   "a b'c\"d $e `f` \n g; echo pwned",
		"semicolon":    "x; echo pwned",
		"empty":        "",
	}
	// Every payload above is INERT on purpose. These strings are fed to a real
	// shell, so if shellQuote is broken the payload runs — a destructive one
	// would make this test's failure mode worse than the bug it guards.
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			// printf %s the quoted value: whatever the shell parses must be the
			// literal original, byte for byte.
			out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(raw)).CombinedOutput()
			require.NoError(t, err, "shellQuote(%q) produced a command the shell rejects: %s", raw, out)
			assert.Equal(t, raw, string(out), "shellQuote(%q) did not survive the shell verbatim", raw)
		})
	}
}

// TestHookReapIsBounded proves the fix does not trade a leak for a hang. Gating
// cleanup on "launch started" runs delete_cmd on far more paths than before, so
// a delete_cmd that wedges must not wedge the caller with it.
func TestHookReapIsBounded(t *testing.T) {
	// delete_cmd hangs; its `sleep` child outlives the kill holding the output
	// pipe, which is what defeats the context timeout on its own.
	shrinkHookTimeouts(t, 300*time.Millisecond, 300*time.Millisecond)
	h := newHookState(t, "exit 0\n", "sleep 30\n")
	p := newHookProvisioner(h, "wedged delete")
	p.launchStarted = true

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- p.reap() }()

	select {
	case err := <-done:
		require.Error(t, err, "a delete_cmd killed at its timeout is a failed reap")
		assert.Less(t, time.Since(start), 5*time.Second,
			"reap must return at its own bound, not wait on a wedged delete_cmd")
	case <-time.After(20 * time.Second):
		t.Fatal("reap hung on a wedged delete_cmd: the delete bound does not fire")
	}
}

// TestHookReapTimeoutIsUnknownState is the #2529 regression: a delete_cmd killed
// at its timeout is proof of NOTHING — the remote workspace may still be running.
// So the reap error must be classified as unknown-state (wrap ErrWorkspaceStateUnknown)
// exactly as the docker backend does, so deleteSessionRecord RETAINS the record and
// the workspace stays reap-able. A plain error lets the record be deleted, leaking
// the workspace with nothing pointing at it — the #2440/#1955 false-success disease.
//
// Against master this FAILS: the timeout surfaces as "signal: killed", which reap
// wraps as a plain error, so TeardownStateUnknown is false and the record is deleted.
func TestHookReapTimeoutIsUnknownState(t *testing.T) {
	// delete_cmd wedges past its own short bound, so the reap is killed by the
	// timeout rather than answering.
	shrinkHookTimeouts(t, 300*time.Millisecond, 300*time.Millisecond)
	h := newHookState(t, "exit 0\n", "sleep 5\n")
	p := newHookProvisioner(h, "wedged delete unknown state")
	p.launchStarted = true

	err := p.reap()
	require.Error(t, err, "a delete_cmd killed at its timeout is a failed reap")
	assert.True(t, TeardownStateUnknown(err),
		"a timed-out delete_cmd leaves the workspace state UNKNOWN — the record must be retained, not deleted")
	assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"the reap error must wrap ErrWorkspaceStateUnknown so deleteSessionRecord refuses to delete the record")
	// The orphan the user needs to know about is still named.
	assert.Contains(t, err.Error(), p.slug, "the failure must still name the orphaned sandbox")
}

// TestHookReapTimeoutIsReRunnable is the #2529-review guard: a timed-out reap must
// NOT latch. The daemon's finishUserKill re-invokes reap() on the SAME provisioner
// every poll for a retained (unknown-state) record. sync.Once latched the timeout
// too, so the second reap skipped the closure and returned nil — deleteSessionRecord
// then deleted the record and the workspace leaked exactly one poll later, the same
// leak as master, one tick delayed. A second reap while delete_cmd is still wedged
// must (i) ACTUALLY re-run delete_cmd and (ii) keep returning the unknown sentinel.
//
// (i) is asserted on how long the reap BLOCKED, not on what the script logged,
// and that is the whole point of #2821. The script's log line was the old proof,
// and it is not kill-proof: delete_cmd is SIGKILLed at the bound, so on a loaded
// box the spawn can miss the budget and be killed before its first instruction —
// leaving no line, and no way for the count to tell "the product latched" from
// "this box was busy". The two look identical, so the count could only ever
// produce a false FAIL.
//
// Blocking time cannot be destroyed that way. A latch returns the CACHED error
// with no spawn and no wait, in microseconds; a real re-invocation runs the
// wedged script into the bound and blocks for it. Load pushes that duration UP,
// never down, so the assertion is one-sided in the safe direction. It is also the
// only thing that separates the two here: the error assertions cannot, because
// the cached error a latch returns IS the unknown-state error.
func TestHookReapTimeoutIsReRunnable(t *testing.T) {
	const bound = 300 * time.Millisecond
	shrinkHookTimeouts(t, bound, bound)
	h := newHookState(t, "exit 0\n", "sleep 5\n") // wedges until the bound kills it
	p := newHookProvisioner(h, "re-runnable wedged reap")
	p.launchStarted = true

	first, firstTook := timeReap(p)
	require.True(t, errors.Is(first, ErrWorkspaceStateUnknown), "first timed-out reap must be unknown-state")
	require.GreaterOrEqual(t, firstTook, bound,
		"the first reap must run the wedged delete_cmd into its bound")

	second, secondTook := timeReap(p)
	require.Error(t, second,
		"a second reap while delete_cmd is still wedged must not return nil — a nil would let deleteSessionRecord delete the record and orphan the workspace one poll later")
	require.True(t, errors.Is(second, ErrWorkspaceStateUnknown),
		"a re-run timed-out reap must keep returning ErrWorkspaceStateUnknown")
	require.GreaterOrEqual(t, secondTook, bound,
		"the second reap must ACTUALLY re-invoke delete_cmd, not skip it via a latch: it has to block on the still-wedged script for the full bound, where a latch would return the cached error immediately")
}

// timeReap runs a reap and reports how long it blocked, which is what tells a
// real invocation apart from a latch — see TestHookReapTimeoutIsReRunnable.
func timeReap(p *hookProvisioner) (error, time.Duration) {
	started := time.Now()
	err := p.reap()
	return err, time.Since(started)
}

// TestHookReapAnsweredErrorIsKnownStateAndLatches is the other half: a delete_cmd
// that ANSWERS with a non-zero exit told us something, so it is known-state (the
// record may go) AND it latches — a second reap returns the cached error without
// re-running a delete that already reported. A GENEROUS delete timeout keeps a slow
// script spawn under load from being misread as a timeout (which would be
// unknown-state) — only the wedged tests above need the short bound (#2529 review).
func TestHookReapAnsweredErrorIsKnownStateAndLatches(t *testing.T) {
	shrinkHookTimeouts(t, 5*time.Second, 5*time.Second)
	h := newHookState(t, "exit 0\n", "echo 'nope' >&2\nexit 9\n")
	p := newHookProvisioner(h, "answered failure known state")
	p.launchStarted = true

	err := p.reap()
	require.Error(t, err)
	assert.False(t, TeardownStateUnknown(err),
		"a delete_cmd that answered with an error TOLD us something — it is known-state, not unknown")
	require.Equal(t, 1, h.deleteRunCount(t), "delete_cmd ran once")

	err2 := p.reap()
	require.Error(t, err2, "the latched known-state error is returned again")
	assert.Equal(t, 1, h.deleteRunCount(t),
		"an answered-error reap latches — delete_cmd is not re-run")
}

// TestHookProvisionFailureWithReapTimeoutPreservesUnknownSentinel locks #2529
// review P3-a: when provisioning fails AND the reap then times out, the provision
// error must keep reapErr's ErrWorkspaceStateUnknown sentinel classifiable
// (errors.Join, not flattened into %s text) — the hook/docker/ssh unknown-state
// parity this fix is about, matching ssh's reapProvisionFailure.
func TestHookProvisionFailureWithReapTimeoutPreservesUnknownSentinel(t *testing.T) {
	shrinkHookTimeouts(t, 300*time.Millisecond, 300*time.Millisecond)
	h := newHookState(t, "echo 'provisioned then died' >&2\nexit 4\n", "sleep 5\n")
	p := newHookProvisioner(h, "create then wedged reap")

	_, err := p.provisionOrReap()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"the provision error must keep the reap's unknown-state sentinel classifiable (#2529 review P3-a)")
	// The original launch failure and the human-actionable orphan warning survive too.
	assert.Contains(t, err.Error(), "launch_cmd failed", "the provisioning error is not swallowed")
	assert.Contains(t, err.Error(), "may still be running on your infrastructure")
}

// TestHookReapUnaffectedByCancelledCaller locks the one subtlety that would
// silently un-fix #1955: reap runs on the failure path, where the launch context
// is ALREADY expired. If reap's context were ever derived from that dead parent
// it would be born expired, delete_cmd would never spawn, and the sandbox would
// leak in silence — with the reap call still sitting there looking correct.
func TestHookReapUnaffectedByCancelledCaller(t *testing.T) {
	shrinkHookTimeouts(t, 50*time.Millisecond, 5*time.Second)
	h := newHookState(t, "sleep 3\n", "")
	p := newHookProvisioner(h, "dead parent")

	// Drive the real path: launch times out (so its context is expired and its
	// process killed) and only then does the reap run.
	_, err := p.provisionOrReap()
	require.Error(t, err)

	assert.True(t, h.deleteRan(t), "delete_cmd must still spawn after the launch context has expired")
	assert.NoDirExists(t, h.sandbox(p.slug))
}

// TestHookReapKillsAnOrphanedLaunchChildBeforeDeleting locks the ordering that
// makes the reap authoritative (#2440).
//
// runHookScript kills only the SCRIPT, so a launch_cmd killed at its bound
// leaves whatever it had in flight — the `terraform`/`gcloud` doing the actual
// provisioning — running. Before the fix, delete_cmd reaped only what existed at
// that instant and reported SUCCESS, and the orphan then created the resource
// AFTER the reap. Nothing surfaced it: provisioning failed so af keeps no record
// of the session, and orphanWarning fires only when the reap FAILS.
//
// This is what TestHookReapUnaffectedByCancelledCaller was hitting intermittently
// on CI, where the in-flight child was the fixture's own `mkdir` descheduled
// under load. Here the window is explicit instead of a scheduling accident, so
// the leak reproduces every run rather than one cell in four.
func TestHookReapKillsAnOrphanedLaunchChildBeforeDeleting(t *testing.T) {
	shrinkHookTimeouts(t, 50*time.Millisecond, 5*time.Second)
	h := newHookState(t, "", "")

	// The real shape of a launch_cmd: the provisioning is done by a CHILD, and
	// the script is still waiting on it when the launch bound fires.
	child := writeHookScript(t, filepath.Join(h.dir, "provision-child.sh"), fmt.Sprintf(`
sleep 0.25
mkdir -p '%s'/sandboxes/"$name"
echo "a VM that bills by the hour" > '%s'/sandboxes/"$name"/resource.txt
`, h.dir, h.dir))
	writeHookScript(t, h.launch, fmt.Sprintf(`'%s' --name "$name"`, child))

	p := newHookProvisioner(h, "orphaned child")
	_, err := p.provisionOrReap()
	require.Error(t, err, "launch_cmd is killed at its bound, so provisioning must fail")
	require.True(t, h.deleteRan(t), "delete_cmd must run: launch_cmd started")

	// Watch across the whole window in which the orphan would land its side
	// effect. A resource that appears at ANY point after the reap is the leak —
	// delete_cmd already reported success and will never run again.
	sandbox := h.sandbox(p.slug)
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(sandbox); statErr == nil {
			t.Fatalf("a launch_cmd child outlived the reap and re-created %s: "+
				"delete_cmd reported success, so this sandbox now bills with nothing pointing at it", sandbox)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHookQuiesceNeverSignalsAStalePgid pins the other half of the process-group
// kill: it may only ever target a group THIS attempt established.
//
// quiesceLaunchGroup runs on any provisioning failure, and its only guard is a
// non-zero launchPgid. If a provisioner could carry a pgid from an earlier
// launch into an attempt that never spawned, it would signal a group that is
// long gone — and whose id the kernel may since have handed to an unrelated
// process. Today no production path reuses a provisioner (hookRuntime.Provision
// builds a fresh one per call), so this is a latent hazard rather than a live
// bug; it is pinned because "the caller always builds a new one" is an invariant
// a future caller can break silently, and the failure mode is SIGKILLing a
// stranger's process tree.
//
// The sentinel below leads its OWN process group. That is not incidental: a kill
// aimed at the test runner's group would take the test process with it.
func TestHookQuiesceNeverSignalsAStalePgid(t *testing.T) {
	shrinkHookTimeouts(t, 50*time.Millisecond, 5*time.Second)

	// Stands in for whatever now owns a recycled pgid.
	sentinel := exec.Command("sleep", "30")
	sentinel.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, sentinel.Start())
	stalePgid := sentinel.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- sentinel.Wait() }()
	t.Cleanup(func() { _ = sentinel.Process.Kill() })

	h := newHookState(t, "", "")
	p := newHookProvisioner(h, "stale pgid")
	// A pgid left behind by an earlier launch on this provisioner.
	p.launchPgid = stalePgid
	// …and an attempt that never spawns, so it establishes no group of its own
	// and must not inherit the right to signal that one.
	p.hooks.LaunchCmd = filepath.Join(h.dir, "does-not-exist.sh")

	_, err := p.provisionOrReap()
	require.Error(t, err, "a launch_cmd that cannot be executed must fail the provision")

	assert.Zero(t, p.launchPgid,
		"a launch that never spawned must clear launchPgid, not inherit the previous attempt's")

	select {
	case werr := <-exited:
		t.Fatalf("quiesceLaunchGroup signalled a process group this attempt never established (%v); "+
			"on a real box that pgid may belong to an unrelated process tree", werr)
	case <-time.After(300 * time.Millisecond):
		// Still running — the stale pgid was never signalled.
	}
}

// TestHookProvisionSucceedsWhenLaunchLeavesOutputPipeOpen guards the regression
// the bound itself could cause. A launch_cmd that exits 0 and leaves a tunnel or
// backgrounded daemon holding its stdout — a documented pattern, since the script
// must make the agent-server reachable — trips the drain grace and returns a
// non-nil error even though it succeeded. Success is the EXIT STATUS; treating
// that error as failure would reap a sandbox that came up fine, which is exactly
// the "destroyed a working sandbox" outcome this fix must not cause.
func TestHookProvisionSucceedsWhenLaunchLeavesOutputPipeOpen(t *testing.T) {
	shrinkHookTimeouts(t, 5*time.Second, 5*time.Second)
	h := newHookState(t,
		// The lingering child is the tunnel; the endpoint JSON is printed and the
		// script exits 0.
		"sleep 3 &\necho '{\"url\":\"http://10.0.0.7:8080\",\"token\":\"secret\"}'\nexit 0\n", "")
	p := newHookProvisioner(h, "tunnel holder")

	res, err := p.provisionOrReap()
	require.NoError(t, err, "launch_cmd exited 0 with a valid endpoint; a held-open pipe is not a failure")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.False(t, h.deleteRan(t), "a sandbox that came up fine must never be reaped")
	assert.DirExists(t, h.sandbox(p.slug), "the working sandbox must still exist")
}

// TestHookProvisionSelectsEndpointAmongJSONLogs covers launch_cmd programs that
// emit structured logs to stderr. stdout and stderr intentionally share one file,
// so endpoint selection must inspect JSON shape rather than take the first value.
func TestHookProvisionSelectsEndpointAmongJSONLogs(t *testing.T) {
	h := newHookState(t, `
echo '{"level":"info","msg":"connecting"}' >&2
echo '{"level":"info","url":"http://wrong.invalid","token":"logged-secret"}' >&2
echo '{"URL":"http://case-variant.invalid","TOKEN":"case-variant-secret"}' >&2
echo '{"url":"http://first-duplicate.invalid","url":"http://last-duplicate.invalid","token":"duplicate-secret"}' >&2
echo '{"level":"info","endpoint":{"url":"http://nested.invalid","token":"nested-secret"},INVALID}' >&2
echo '{"level":INVALID,"endpoint":{"url":"http://post-error.invalid","token":"post-error-secret"}}' >&2
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
echo '{"level":"info","msg":"tunnel ready"}' >&2
exit 0
`, "")
	p := newHookProvisioner(h, "json logger")

	res, err := p.provisionOrReap()
	require.NoError(t, err, "a JSON log record must not hide the later endpoint JSON")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
	assert.False(t, h.deleteRan(t), "valid endpoint output must not reap the working sandbox")
	assert.DirExists(t, h.sandbox(p.slug))
}

// TestHookProvisionRedactsEndpointTokenFromParseError covers the failure path
// where launch_cmd emits JSON but never supplies a usable endpoint. The raw
// aggregate output reaches durable error reporting, so bearer tokens in any JSON
// candidate must be redacted before the error leaves this package.
func TestHookProvisionRedactsEndpointTokenFromParseError(t *testing.T) {
	const secret = "must-not-reach-the-error"
	h := newHookState(t, `
echo '{"level":"info","msg":"connecting"}' >&2
echo '{"url":"","token":"must-not-reach-the-error"}'
exit 0
`, "")
	p := newHookProvisioner(h, "invalid endpoint")

	_, err := p.provisionOrReap()
	require.Error(t, err, "an empty endpoint URL must fail provisioning")
	assert.NotContains(t, err.Error(), secret, "agent-server bearer tokens must never enter reported errors")
	assert.Contains(t, err.Error(), "[REDACTED]", "the diagnostic should show that sensitive output was removed")
	assert.True(t, h.deleteRan(t), "invalid endpoint output must reap the provisioned sandbox")
	assert.NoDirExists(t, h.sandbox(p.slug))
}

// TestHookOutputSuffixPreservesNumbersWhileRedactingTokens guards diagnostic
// fidelity: token redaction must not decode and re-encode unrelated JSON values,
// which can round large integer resource IDs through float64.
func TestHookOutputSuffixPreservesNumbersWhileRedactingTokens(t *testing.T) {
	const resourceID = "9223372036854775807"
	const secret = "diagnostic-token-must-not-leak"

	suffix := hookOutputSuffix([]byte(`{"resource_id":9223372036854775807,"token":"diagnostic-token-must-not-leak"}`))
	assert.Contains(t, suffix, resourceID, "redaction must preserve the exact remote resource identifier")
	assert.NotContains(t, suffix, secret, "redaction must remove the bearer token")
	assert.Contains(t, suffix, "[REDACTED]")
}

// TestHookOutputSuffixRedactsTokenAfterUnmatchedQuote covers arbitrary prose
// before endpoint output. A stray quote must not phase-shift the scanner and
// make every later JSON string look like the end of the preceding one.
func TestHookOutputSuffixRedactsTokenAfterUnmatchedQuote(t *testing.T) {
	const secret = "misaligned-token-must-not-leak"

	suffix := hookOutputSuffix([]byte("warning: unmatched \"\n" +
		`{"url":"","token":"misaligned-token-must-not-leak"}`))
	assert.NotContains(t, suffix, secret, "malformed diagnostic text must not bypass token redaction")
	assert.Contains(t, suffix, "[REDACTED]")
}

// TestHookOutputSuffixRedactsAfterMalformedTokenCandidate covers overlapping
// quotes where prose ends in a valid-looking "token" string but the same closing
// quote is also the opening boundary of the real token key.
func TestHookOutputSuffixRedactsAfterMalformedTokenCandidate(t *testing.T) {
	const secret = "overlapping-token-must-not-leak"

	suffix := hookOutputSuffix([]byte(`warning: "token"token":"overlapping-token-must-not-leak"`))
	assert.NotContains(t, suffix, secret, "a malformed token candidate must not hide the next real token field")
	assert.Contains(t, suffix, "[REDACTED]")
}

// TestHookOutputSuffixRedactsAfterOverlappingValueBoundary covers malformed
// text where the closing quote of a decoy token value is also the opening quote
// of the real token key. Successful redaction must not skip that boundary.
func TestHookOutputSuffixRedactsAfterOverlappingValueBoundary(t *testing.T) {
	const secret = "overlapping-value-token-must-not-leak"

	suffix := hookOutputSuffix([]byte(`warning: "token":"decoy"token":"overlapping-value-token-must-not-leak"`))
	assert.NotContains(t, suffix, secret, "a redacted decoy value must not hide the next real token field")
	assert.Contains(t, suffix, "[REDACTED]")
}

// TestHookOutputSuffixHandlesEscapedQuoteFlood keeps malformed diagnostic
// scanning linear. Every quote below is escaped, so rescanning the remaining
// suffix from each one is quadratic even though there is no token to redact.
func TestHookOutputSuffixHandlesEscapedQuoteFlood(t *testing.T) {
	output := strings.Repeat(`\"`, 50_000)
	started := time.Now()
	suffix := hookOutputSuffix([]byte(output))
	assert.Less(t, time.Since(started), time.Second, "escaped-quote diagnostics must be scanned in linear time")
	assert.Equal(t, "; its output was:\n"+output, suffix)
}

// TestHookOutputSuffixRedactsJSONEscapedToken covers structured loggers that
// serialize endpoint JSON into a string field. The inner bearer token is still
// sensitive even though its delimiters are escaped by the outer JSON object.
func TestHookOutputSuffixRedactsJSONEscapedToken(t *testing.T) {
	const secret = "json-escaped-token-must-not-leak"
	output := `{"endpoint":"{\"url\":\"\",\"token\":\"json-escaped-token-must-not-leak\"}"}`

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, secret, "a serialized endpoint object must not expose its bearer token")
	assert.Contains(t, suffix, "[REDACTED]")
}

// TestHookOutputSuffixRedactsTruncatedJSONEscapedToken covers a structured log
// whose string field contains only the prefix of an endpoint document. The
// complete outer log must not make the incomplete inner token safe to report.
func TestHookOutputSuffixRedactsTruncatedJSONEscapedToken(t *testing.T) {
	const secret = "truncated-json-escaped-token-must-not-leak"
	output := `{"endpoint":"{\"url\":\"\",\"token\":\"truncated-json-escaped-token-must-not-leak\""}`

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, secret, "a truncated serialized endpoint must not expose its bearer token")
	assert.Contains(t, suffix, "[REDACTED]")
}

// TestHookOutputSuffixRedactsTopLevelSerializedEndpoint covers log pipelines
// that JSON-encode the endpoint document itself as a complete string record.
// It has no enclosing object for the recursive map walk to discover.
func TestHookOutputSuffixRedactsTopLevelSerializedEndpoint(t *testing.T) {
	const secret = "top-level-json-string-token-must-not-leak"
	output := "{\"level\":\"info\"}\n" +
		`"{\"url\":\"\",\"token\":\"top-level-json-string-token-must-not-leak\"}"`

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, secret, "a serialized top-level endpoint must not expose its bearer token")
	assert.Contains(t, suffix, "[REDACTED]")
}

// TestHookOutputSuffixRedactsPrefixedSerializedEndpoint covers conventional
// logger metadata around a JSON string literal containing endpoint output.
func TestHookOutputSuffixRedactsPrefixedSerializedEndpoint(t *testing.T) {
	const secret = "prefixed-json-string-token-must-not-leak"
	output := `INFO endpoint="{\"url\":\"\",\"token\":\"prefixed-json-string-token-must-not-leak\"}" ready`

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, secret, "a prefixed serialized endpoint must not expose its bearer token")
	assert.Contains(t, suffix, "[REDACTED]")
}

// TestHookOutputSuffixRedactsSerializedEndpointVariants covers JSON-equivalent
// encodings where the decoded document begins with whitespace or an escaped
// opening brace rather than a literal raw delimiter.
func TestHookOutputSuffixRedactsSerializedEndpointVariants(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		output string
	}{
		{
			name:   "leading whitespace",
			secret: "whitespace-json-string-token-must-not-leak",
			output: `"  {\"token\":\"whitespace-json-string-token-must-not-leak\"}"`,
		},
		{
			name:   "unicode escaped opener",
			secret: "escaped-opener-token-must-not-leak",
			output: `"\u007b\"token\":\"escaped-opener-token-must-not-leak\"}"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			suffix := hookOutputSuffix([]byte(test.output))
			assert.NotContains(t, suffix, test.secret, "a JSON-equivalent serialized endpoint must redact its token")
			assert.Contains(t, suffix, "[REDACTED]")
		})
	}
}

// TestExtractJSONAtHandlesMalformedDelimiterFlood keeps endpoint selection
// linear after a valid JSON log. No unmatched opener can produce a complete
// value, so retrying a suffix scan from each one is pure quadratic work.
func TestExtractJSONAtHandlesMalformedDelimiterFlood(t *testing.T) {
	const logRecord = `{"level":"info"}`
	output := logRecord + strings.Repeat("{", 50_000)

	first, next := extractJSONAt(output, 0)
	require.Equal(t, logRecord, first)
	started := time.Now()
	value, end := extractJSONAt(output, next)
	assert.Less(t, time.Since(started), time.Second, "unmatched delimiters must be scanned in linear time")
	assert.Empty(t, value)
	assert.Equal(t, len(output), end)

	balanced := strings.Repeat("[", 50_000) + "not-json" + strings.Repeat("]", 50_000)
	started = time.Now()
	value, end = extractJSONAt(balanced, 0)
	assert.Less(t, time.Since(started), time.Second, "balanced malformed nesting must be scanned in linear time")
	assert.Empty(t, value)
	assert.Equal(t, len(balanced), end)

	recoverable := strings.Repeat("[bad", 20_000) + logRecord + strings.Repeat("]", 20_000)
	started = time.Now()
	value, _ = extractJSONAt(recoverable, 0)
	assert.Less(t, time.Since(started), time.Second, "nested recovery must walk each candidate once")
	assert.Equal(t, logRecord, value)
}

// TestHookProvisionRedactsTokenFromIncompleteEndpointOutput covers a launch
// killed or interrupted while writing its endpoint JSON. The unmatched tail is
// still diagnostic output, but a complete quoted token value inside it is just
// as sensitive as one in a complete JSON object.
func TestHookProvisionRedactsTokenFromIncompleteEndpointOutput(t *testing.T) {
	const secret = "truncated-token-must-not-leak"
	h := newHookState(t, `
echo '{"level":"info","msg":"connecting"}' >&2
echo '{"url":"","token":"truncated-token-must-not-leak"'
exit 0
`, "")
	p := newHookProvisioner(h, "truncated endpoint")

	_, err := p.provisionOrReap()
	require.Error(t, err, "an incomplete endpoint object must fail provisioning")
	assert.NotContains(t, err.Error(), secret, "an incomplete JSON tail must not expose its bearer token")
	assert.Contains(t, err.Error(), "[REDACTED]", "the diagnostic should show that sensitive output was removed")
	assert.True(t, h.deleteRan(t), "invalid endpoint output must reap the provisioned sandbox")
	assert.NoDirExists(t, h.sandbox(p.slug))
}

// TestHookProvisionKeepsASuccessfulLaunchsTunnelAlive is the #1966-review P2: a
// launch_cmd that SUCCEEDS and leaves a tunnel running must end with a REACHABLE
// endpoint. The tunnel is not a leak — it is the product, the thing making the
// endpoint dialable — so nothing in the capture path may reap it.
//
// It asserts REACHABILITY, not the absence of a kill: the endpoint working is the
// actual claim, and "we did not send a signal" would not prove it (the tunnel dies
// of SIGPIPE when our read end closes, no signal from us involved).
//
// The stand-in tunnel is a real HTTP server, backgrounded by launch_cmd, holding
// STDERR exactly as a port-forward logging its activity does. stderr is the
// stream a background process may still inherit — stdout became the endpoint
// record's alone in #2845 — and it is the one that matters here anyway: both are
// af-owned files the capture could wait on or drain, so a tunnel writing to
// either is a target of any such policy.
func TestHookProvisionKeepsASuccessfulLaunchsTunnelAlive(t *testing.T) {
	shrinkHookTimeouts(t, 10*time.Second, 5*time.Second)
	dir := t.TempDir()

	// The tunnel must be the thing that ACTUALLY SERVES the endpoint, owned by
	// launch_cmd — not a server the test holds. A test-owned server would stay up
	// even when the tunnel is reaped, so the reachability assertion would pass
	// against the very bug it exists to catch. (It did, on the first draft.)
	//
	// So: a real backgrounded HTTP server that ALSO writes to launch_cmd's stderr,
	// which is what makes it a pipe-holder and therefore a target of any drain
	// policy.
	tunnel := filepath.Join(dir, "tunnel.py")
	require.NoError(t, os.WriteFile(tunnel, []byte(`
import http.server, socketserver, sys, threading, time
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"agent-server alive")
    def log_message(self, *a): pass
srv = socketserver.TCPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(srv.server_address[1]))
threading.Thread(target=srv.serve_forever, daemon=True).start()
while True:                      # holds launch_cmd's stderr, like a forwarder logging
    print("tunnel forwarding", file=sys.stderr, flush=True)
    time.sleep(0.05)
`), 0o644))
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("python3 is not installed, so this test cannot stand up a real tunnel and would " +
			"assert reachability against a server it owns itself — which passes against the bug. " +
			"Install python3 rather than weakening this test.")
	}

	portFile := filepath.Join(dir, "port")
	h := hookState{dir: dir, launch: filepath.Join(dir, "launch.sh"), delete: filepath.Join(dir, "delete.sh")}
	writeHookScript(t, h.launch, fmt.Sprintf(`
python3 %s %s &
for i in $(seq 1 100); do [ -s %s ] && break; sleep 0.05; done
echo "{\"url\":\"http://127.0.0.1:$(cat %s)\",\"token\":\"secret\"}"
exit 0
`, shellsuggest.Arg(tunnel), shellsuggest.Arg(portFile), shellsuggest.Arg(portFile), shellsuggest.Arg(portFile)))
	writeHookScript(t, h.delete, fmt.Sprintf(`echo "$name" >> %s/delete-ran.log`, shellsuggest.Arg(dir)))

	p := newHookProvisioner(h, "tunnel holder")
	// Own the tunnel's lifetime, exactly as TestHookLaunchDoesNotKillBackgroundedChildren
	// below owns its child's. The production code is right to leave this process alone —
	// that is the whole claim — but "nothing in the capture path reaps it" is not the same
	// as "nobody reaps it". Without this the tunnel outlives the test forever: t.TempDir()
	// removes the directory out from under a python3 that keeps looping and holding a
	// listening socket, detached, owned by no one (#2842 — 65 of them accumulated on the
	// dev box, the oldest six days old).
	//
	// This cannot weaken the claim: t.Cleanup runs after the test body, so the reachability
	// assertion has already passed by the time the kill fires. And t.Cleanup is LIFO, so it
	// runs BEFORE the TempDir removal registered by t.TempDir() above — no writer survives
	// into os.RemoveAll. launchPgid is populated by provisionOrReap below and read here at
	// cleanup time.
	//
	// This signals, and relies on PID 1 to reap. It cannot wait(2) the tunnel: launch_cmd
	// backgrounded it and then exited, so the test was never its parent — and it must not
	// be, because a test-owned server would survive a reap and pass against the very bug
	// this test exists to catch (see the comment on the script above). Every environment
	// the suite runs in supplies a reaping init: systemd on dev boxes and CI runners, and
	// tini in the container harness, which passes --init for exactly this reason
	// (scripts/testbox.sh). Under a PID 1 that ignores SIGCHLD the killed tunnel would
	// linger as a defunct entry until that PID 1 exits — no socket, no CPU, and no way
	// around it from here short of giving the tunnel an owner the test is not allowed to be.
	t.Cleanup(func() {
		if p.launchPgid != 0 {
			_ = syscall.Kill(-p.launchPgid, syscall.SIGKILL)
		}
	})
	res, err := p.provisionOrReap()
	require.NoError(t, err, "a launch_cmd that exits 0 with a valid endpoint must succeed")
	require.NotNil(t, res.Endpoint)

	// Give any pipe-holder policy every chance to fire before we check.
	time.Sleep(400 * time.Millisecond)

	// THE CLAIM: the endpoint we just handed back actually works.
	resp, err := http.Get(res.Endpoint.URL)
	require.NoError(t, err,
		"the provisioned endpoint %s is unreachable — the launch SUCCEEDED and something reaped the tunnel that makes it dialable",
		res.Endpoint.URL)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "agent-server alive", string(body), "the endpoint must serve the agent-server, not a corpse")

	assert.False(t, h.deleteRan(t), "a successful provision must never reap")
}

// TestHookLaunchDoesNotKillBackgroundedChildren is the mechanism behind the test
// above, pinned separately so a regression names its own cause: the capture must
// not kill a process launch_cmd deliberately backgrounded. A heartbeat file is the
// liveness probe — it keeps ticking only while the child lives, so requiring the
// mtime to keep advancing after the script exits proves the capture left the child
// alone.
//
// The test also OWNS that child's lifetime (the t.Cleanup reap below): a
// backgrounded writer left running races t.TempDir()'s os.RemoveAll and fails the
// test in cleanup with an ENOTEMPTY — the actual flake this file hit on CI, which
// looks like a passing body followed by "TempDir RemoveAll cleanup: ... directory
// not empty".
func TestHookLaunchDoesNotKillBackgroundedChildren(t *testing.T) {
	shrinkHookTimeouts(t, 5*time.Second, 5*time.Second)
	dir := t.TempDir()
	hb := filepath.Join(dir, "heartbeat")

	h := hookState{dir: dir, launch: filepath.Join(dir, "launch.sh"), delete: filepath.Join(dir, "delete.sh")}
	writeHookScript(t, h.launch, fmt.Sprintf(`
( for i in $(seq 1 400); do echo "still here"; echo tick >> %s; sleep 0.05; done ) &
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, shellsuggest.Arg(hb)))
	writeHookScript(t, h.delete, "true")

	p := newHookProvisioner(h, "background child")
	// Own the backgrounded child's lifetime. launch_cmd backgrounds a process that
	// keeps writing into dir; if it is still writing when t.TempDir()'s cleanup runs
	// os.RemoveAll(dir), the recreated entry makes rmdir return ENOTEMPTY and testing
	// fails the test IN CLEANUP (Linux never retries — testing.removeAll only retries
	// Windows-transient errors). Reap the launch process group so no writer survives
	// into the removal. t.Cleanup is LIFO, so this runs BEFORE the TempDir removal
	// registered by t.TempDir() above; launchPgid is populated by provisionOrReap
	// below and read here at cleanup time.
	t.Cleanup(func() {
		if p.launchPgid != 0 {
			_ = syscall.Kill(-p.launchPgid, syscall.SIGKILL)
		}
	})
	_, err := p.provisionOrReap()
	require.NoError(t, err)

	// provisionOrReap has returned, so launch_cmd itself is already gone and what
	// follows measures only the BACKGROUNDED child. Wait for its first heartbeat
	// on the CONDITION rather than on a fixed nap: that write is gated on a
	// process spawn, which has no upper bound on a loaded box, and a nap long
	// enough on an idle one turns a busy machine into a failed assertion (#2879).
	// The ceiling here is a failure deadline, not an expectation.
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(hb)
		return statErr == nil
	}, 30*time.Second, 10*time.Millisecond, "the backgrounded child never ran")

	// Then require the heartbeat to keep advancing. Asserting SUSTAINED ticking —
	// several distinct advances at this later checkpoint, not a single early write
	// — is deliberate: a capture-path kill lands at or just after Run() returns and
	// freezes the mtime, so a first-advance-wins probe could observe one stale
	// write and miss the regression. A brief scheduling stall is tolerated by the
	// generous deadline; a killed child can never reach the required count.

	const wantAdvances = 3
	var last time.Time
	advances := 0
	deadline := time.Now().Add(3 * time.Second)
	for advances < wantAdvances && time.Now().Before(deadline) {
		if fi, statErr := os.Stat(hb); statErr == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
			advances++
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, advances, wantAdvances,
		"the process launch_cmd backgrounded stopped ticking — the output capture killed a child that was not ours to kill")
}

// TestHookLaunchIsBounded proves the launch bound fires at all. It is the
// precondition for the whole fix: if launch_cmd never returns, Provision hangs
// and the reap #1955 asks for can never run, no matter how it is gated.
func TestHookLaunchIsBounded(t *testing.T) {
	shrinkHookTimeouts(t, 300*time.Millisecond, 5*time.Second)
	h := newHookState(t, "sleep 30\n", "")
	p := newHookProvisioner(h, "wedged launch")

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := p.launch()
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "launch_cmd failed")
		assert.Less(t, time.Since(start), 5*time.Second, "launch must return at its own bound")
		assert.True(t, p.launchStarted, "launch_cmd ran before it was killed, so the reap must be armed")
	case <-time.After(20 * time.Second):
		t.Fatal("launch hung past its timeout: the launch bound does not fire")
	}
}
