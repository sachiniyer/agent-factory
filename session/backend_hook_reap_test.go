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
	writeHookScript(t, h.delete, fmt.Sprintf(`
mkdir -p '%s'
echo "$name" >> '%s'/delete-ran.log
%s
`, dir, dir, deleteBody))
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
// stdout exactly as a port-forward logging its activity does.
func TestHookProvisionKeepsASuccessfulLaunchsTunnelAlive(t *testing.T) {
	shrinkHookTimeouts(t, 10*time.Second, 5*time.Second)
	dir := t.TempDir()

	// The tunnel must be the thing that ACTUALLY SERVES the endpoint, owned by
	// launch_cmd — not a server the test holds. A test-owned server would stay up
	// even when the tunnel is reaped, so the reachability assertion would pass
	// against the very bug it exists to catch. (It did, on the first draft.)
	//
	// So: a real backgrounded HTTP server that ALSO writes to stdout, which is what
	// makes it a pipe-holder and therefore a target of any drain policy.
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
while True:                      # holds launch_cmd's stdout, like a forwarder logging
    print("tunnel forwarding", flush=True)
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
// liveness probe — it keeps ticking only while the child lives.
func TestHookLaunchDoesNotKillBackgroundedChildren(t *testing.T) {
	shrinkHookTimeouts(t, 5*time.Second, 5*time.Second)
	dir := t.TempDir()
	hb := filepath.Join(dir, "heartbeat")

	h := hookState{dir: dir, launch: filepath.Join(dir, "launch.sh"), delete: filepath.Join(dir, "delete.sh")}
	writeHookScript(t, h.launch, fmt.Sprintf(`
( for i in $(seq 1 100); do echo "still here"; echo tick >> %s; sleep 0.05; done ) &
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, shellsuggest.Arg(hb)))
	writeHookScript(t, h.delete, "true")

	p := newHookProvisioner(h, "background child")
	_, err := p.provisionOrReap()
	require.NoError(t, err)

	// The child must still be ticking well after the script exited.
	time.Sleep(250 * time.Millisecond)
	first, err := os.Stat(hb)
	require.NoError(t, err, "the backgrounded child never ran")
	time.Sleep(250 * time.Millisecond)
	second, err := os.Stat(hb)
	require.NoError(t, err)
	assert.True(t, second.ModTime().After(first.ModTime()),
		"the process launch_cmd backgrounded stopped writing — the output capture killed a child that was not ours to kill")
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
