//go:build !windows

package sessionenv

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A PerPane capability has no clock expiry, so these are the ways it ends. The
// explicit Revoke is covered where sessions end (session/tmux); this file covers
// the two ends nobody calls Revoke for.

func perPaneCap(session string) Capability {
	c := testCap()
	c.Session = session
	c.EnvironmentOnly = true
	c.PerPane = true
	return c
}

// A session lost without af asking (its tmux server died, someone killed it)
// never reaches the teardown that revokes. The sweep is what ends its record, and
// it must not end the record of a session tmux could not answer for.
func TestCapability_SweepRevokesPerPaneRecordsOfLostSessions(t *testing.T) {
	b, _ := startBroker(t, paneOf("live", os.Getpid()))
	for _, s := range []string{"live", "lost", "unknown"} {
		token, err := b.Mint(perPaneCap(s))
		require.NoError(t, err)
		b.Activate(token)
	}
	oneShot := testCap()
	oneShot.Session = "lost"
	_, err := b.Mint(oneShot)
	require.NoError(t, err)

	b.Sweep(context.Background(), func(_ context.Context, session string) (bool, bool) {
		switch session {
		case "live":
			return true, true
		case "lost":
			return false, true
		}
		return false, false // tmux did not answer
	})

	_, live := b.Capabilities("live")
	_, unknown := b.Capabilities("unknown")
	_, lost := b.Capabilities("lost")
	require.Equal(t, 1, live, "a live session keeps its per-pane capability")
	require.Equal(t, 1, unknown, "an unanswered liveness probe is never read as the session being gone")
	require.Zero(t, lost, "a lost session's per-pane capability must end with it")
	total, _ := b.Capabilities("lost")
	require.Equal(t, 1, total, "the sweep ends per-pane records only; a single-use one still has its own expiry")
}

// The broker lives in the daemon. A restart drops every record, so a token an
// older process minted (the one a surviving tmux session's default-command still
// names) claims nothing, and the restore that follows mints a new one.
func TestCapability_ARestartedBrokerRefusesAnOlderProcessesToken(t *testing.T) {
	old, _ := startBroker(t, paneOf("s", os.Getpid()))
	token, err := old.Mint(perPaneCap("s"))
	require.NoError(t, err)

	restarted, sock := startBroker(t, paneOf("s", os.Getpid()))
	_, err = claimAs(t, sock, token, "%1")
	require.ErrorContains(t, err, "unknown or already used")
	_, perPane := restarted.Capabilities("s")
	require.Zero(t, perPane)
}

// A long-lived daemon's claim socket sits under the system temp dir, which macOS
// prunes by age. The periodic keep-alive must move both the socket's and its
// directory's access and modification times to now, or the endpoint disappears
// while sessions still name it in their default-command (#6384).
func TestCapability_KeepClaimEndpointFreshRefreshesAgedSocketAndDir(t *testing.T) {
	dir := t.TempDir()
	// The keep-alive only touches a directory that is still ours, 0700 like the real one.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "claim.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	for _, p := range []string{socket, dir} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	keepClaimEndpointFresh(socket, dir, now)
	for _, p := range []string{socket, dir} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if age := now.Sub(fi.ModTime()); age > time.Second {
			t.Fatalf("%s mtime is %v old after the keep-alive, want fresh", p, age)
		}
	}
	// A path that is already gone must not panic or error out the sweep goroutine.
	keepClaimEndpointFresh(filepath.Join(dir, "missing.sock"), filepath.Join(dir, "missing"), now)
}

// The system can remove the claim socket, even its whole directory, from under a
// live daemon (macOS prunes the temp dir by age, and a Mac that slept through the
// window runs the cleanup on wake, before any keep-alive can touch the socket). A
// session's default-command names that exact path, so the endpoint must come back
// AT THAT PATH and keep serving claims, with no new token handed out (#6384).
func TestCapability_EndpointRecreatesItsSocketAndDirAtTheSamePath(t *testing.T) {
	t.Setenv("TMPDIR", shortSocketDir(t))
	prev := capabilityEnsureInterval
	capabilityEnsureInterval = time.Hour // ensure() is driven by hand: no recovery races the deletion checks
	t.Cleanup(func() { capabilityEnsureInterval = prev })

	b, socket, err := startLaunchEndpoint(paneOf("s", os.Getpid()), nil)
	require.NoError(t, err)
	t.Cleanup(ShutdownLaunchCapabilities)
	ep := launchEP.Load()
	dir := filepath.Dir(socket)

	claims := func(stage string) {
		t.Helper()
		token, err := b.Mint(testCap())
		require.NoError(t, err, stage)
		_, err = claimAs(t, socket, token, "%1")
		require.NoError(t, err, "%s: claim through the ORIGINAL path %s", stage, socket)
	}
	unreachable := func(stage string) {
		t.Helper()
		_, err := claimAs(t, socket, "x", "%1")
		require.ErrorContains(t, err, "unreachable", stage)
	}

	claims("fresh endpoint")

	require.NoError(t, os.Remove(socket))
	unreachable("the deleted socket must really be gone")
	ep.ensure()
	claims("after the socket was deleted")

	require.NoError(t, os.RemoveAll(dir))
	unreachable("the deleted directory must really be gone")
	ep.ensure()
	claims("after the whole directory was deleted")

	fi, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), "a recreated directory keeps the private mode")
}

// The periodic check is what heals the endpoint without anyone calling ensure().
func TestCapability_EndpointHealsItselfOnTheTick(t *testing.T) {
	t.Setenv("TMPDIR", shortSocketDir(t))
	prev := capabilityEnsureInterval
	capabilityEnsureInterval = 20 * time.Millisecond
	t.Cleanup(func() { capabilityEnsureInterval = prev })

	b, socket, err := startLaunchEndpoint(paneOf("s", os.Getpid()), nil)
	require.NoError(t, err)
	t.Cleanup(ShutdownLaunchCapabilities)
	require.NoError(t, os.RemoveAll(filepath.Dir(socket)))
	require.Eventually(t, func() bool {
		token, err := b.Mint(testCap())
		if err != nil {
			return false
		}
		_, err = claimAs(t, socket, token, "%1")
		return err == nil
	}, 5*time.Second, 25*time.Millisecond)
}

// Re-creating the directory after a temp cleanup must not adopt what another local
// user planted at the predictable path: the directory's mode is the only thing
// protecting claim tokens, which sit in argv. A symlink, or a directory that is not
// exactly 0700, must be refused (and nothing bound in it); once the path is clear
// the endpoint comes back at the same path (#6384).
func TestCapability_EndpointRefusesAPlantedDirectoryAndRecoversWhenClear(t *testing.T) {
	t.Setenv("TMPDIR", shortSocketDir(t))
	prev := capabilityEnsureInterval
	capabilityEnsureInterval = time.Hour // drive ensure() by hand
	t.Cleanup(func() { capabilityEnsureInterval = prev })

	b, socket, err := startLaunchEndpoint(paneOf("s", os.Getpid()), nil)
	require.NoError(t, err)
	t.Cleanup(ShutdownLaunchCapabilities)
	ep := launchEP.Load()
	dir := filepath.Dir(socket)

	unreachable := func(stage string) {
		t.Helper()
		_, err := claimAs(t, socket, "x", "%1")
		require.ErrorContains(t, err, "unreachable", stage)
	}

	// A symlink to a directory the attacker controls.
	attacker := shortSocketDir(t)
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.Symlink(attacker, dir))
	ep.ensure()
	unreachable("symlinked directory")
	entries, err := os.ReadDir(attacker)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing may be bound inside the attacker's directory")

	// A real directory that is not 0700.
	require.NoError(t, os.Remove(dir))
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	ep.ensure()
	unreachable("0755 directory")
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing may be bound in a directory that is not ours")
	fi, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), fi.Mode().Perm(), "a directory that is not ours is not repaired")

	// Clear path: a normal re-create works and serves a claim at the same path.
	require.NoError(t, os.Remove(dir))
	ep.ensure()
	token, err := b.Mint(testCap())
	require.NoError(t, err)
	_, err = claimAs(t, socket, token, "%1")
	require.NoError(t, err)
}
