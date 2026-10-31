//go:build !windows

package sessionenv

import (
	"context"
	"os"
	"testing"

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
