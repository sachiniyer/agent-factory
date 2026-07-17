package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
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
func shrinkHookTimeouts(t *testing.T, launch, del, drain time.Duration) {
	t.Helper()
	ol, od, og := hookLaunchTimeout, hookDeleteTimeout, hookOutputDrainGrace
	hookLaunchTimeout, hookDeleteTimeout, hookOutputDrainGrace = launch, del, drain
	t.Cleanup(func() {
		hookLaunchTimeout, hookDeleteTimeout, hookOutputDrainGrace = ol, od, og
	})
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
			shrinkHookTimeouts(t, 300*time.Millisecond, 5*time.Second, 300*time.Millisecond)
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
			shrinkHookTimeouts(t, 300*time.Millisecond, 5*time.Second, 300*time.Millisecond)
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
	shrinkHookTimeouts(t, 300*time.Millisecond, 5*time.Second, 300*time.Millisecond)
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
	assert.Contains(t, msg, h.delete+" --name "+p.slug, "the warning must give the exact command to reap by hand")
	assert.Contains(t, msg, "the VM is still in CREATING state", "delete_cmd's own output must be surfaced")

	// The resource really is still there — the message is not crying wolf.
	assert.DirExists(t, h.sandbox(p.slug))
}

// TestHookReapIsBounded proves the fix does not trade a leak for a hang. Gating
// cleanup on "launch started" runs delete_cmd on far more paths than before, so
// a delete_cmd that wedges must not wedge the caller with it.
func TestHookReapIsBounded(t *testing.T) {
	// delete_cmd hangs; its `sleep` child outlives the kill holding the output
	// pipe, which is what defeats the context timeout on its own.
	shrinkHookTimeouts(t, 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond)
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
	shrinkHookTimeouts(t, 50*time.Millisecond, 5*time.Second, 300*time.Millisecond)
	h := newHookState(t, "sleep 3\n", "")
	p := newHookProvisioner(h, "dead parent")

	// Drive the real path: launch times out (so its context is expired and its
	// process killed) and only then does the reap run.
	_, err := p.provisionOrReap()
	require.Error(t, err)

	assert.True(t, h.deleteRan(t), "delete_cmd must still spawn after the launch context has expired")
	assert.NoDirExists(t, h.sandbox(p.slug))
}

// TestHookProvisionSucceedsWhenLaunchLeavesOutputPipeOpen guards the regression
// the bound itself could cause. A launch_cmd that exits 0 and leaves a tunnel or
// backgrounded daemon holding its stdout — a documented pattern, since the script
// must make the agent-server reachable — trips the drain grace and returns a
// non-nil error even though it succeeded. Success is the EXIT STATUS; treating
// that error as failure would reap a sandbox that came up fine, which is exactly
// the "destroyed a working sandbox" outcome this fix must not cause.
func TestHookProvisionSucceedsWhenLaunchLeavesOutputPipeOpen(t *testing.T) {
	shrinkHookTimeouts(t, 5*time.Second, 5*time.Second, 300*time.Millisecond)
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

// TestHookLaunchIsBounded proves the launch bound fires at all. It is the
// precondition for the whole fix: if launch_cmd never returns, Provision hangs
// and the reap #1955 asks for can never run, no matter how it is gated.
func TestHookLaunchIsBounded(t *testing.T) {
	shrinkHookTimeouts(t, 300*time.Millisecond, 5*time.Second, 300*time.Millisecond)
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
