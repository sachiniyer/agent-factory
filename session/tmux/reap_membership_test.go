package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #7435 item 2 — classifyHomeDir replaces the lexical filepath.Clean compare
// on every kill-path AF_HOME check. Only literal equality or a SameFile stat
// proves "same directory"; a path that merely Cleans to the home, a relative
// marker, or anything unstat-able is unproven — and unproven is never same.
func TestClassifyHomeDir(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(home, link))
	// Cleans to the home, but resolves through a directory that does not
	// exist: stat fails, so nothing can prove the names name the same place —
	// and if the middle directory later appears, they may name DIFFERENT ones.
	lexicalAlias := filepath.Join(home+"-missing", "..", filepath.Base(home))
	require.Equal(t, home, filepath.Clean(lexicalAlias),
		"the fixture must only differ lexically, not after Clean")

	for _, tc := range []struct {
		name string
		a, b string
		want homeMatch
	}{
		{"literal equality", home, home, homeSame},
		{"a symlink to the home stats to it", link, home, homeSame},
		{"two different existing dirs", other, home, homeForeign},
		{"a lexical alias that cannot be stat'd", lexicalAlias, home, homeUnproven},
		{"a missing marker path", filepath.Join(home, "missing"), home, homeUnproven},
		{"a relative marker", ".af", home, homeUnproven},
		{"a relative home", home, ".af", homeUnproven},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyHomeDir(tc.a, tc.b))
		})
	}
}

// #7435 item 5 — the cohort gate admits only what the kernel session evidence
// proves: a SID one of the pane roots led, or a (pid, StartID) recorded at
// spawn. No evidence at all means the gate is off, never silently on.
func TestCohortMembershipAdmits(t *testing.T) {
	member := proctree.Process{PID: 100, SID: 40, StartID: 9}

	require.True(t, cohortMembership{}.admits(member), "no evidence: gate off")
	require.True(t, cohortMembership{PaneSIDs: map[int]bool{}}.admits(member),
		"an EMPTY sid set is still no evidence: gate off")
	require.True(t, cohortMembership{PaneSIDs: map[int]bool{40: true}}.admits(member))
	require.True(t, cohortMembership{Recorded: map[int]uint64{100: 9}}.admits(member))
	require.False(t, cohortMembership{PaneSIDs: map[int]bool{41: true}}.admits(member),
		"a foreign SID is not membership however the markers read")
	require.False(t, cohortMembership{Recorded: map[int]uint64{100: 8}}.admits(member),
		"a recycled pid with a different StartID is not the recorded process")
	require.False(t, cohortMembership{Recorded: map[int]uint64{101: 9}}.admits(member),
		"a different pid carrying the same StartID is not the recorded process")
}

// #7435 item 5 — inherited markers are never membership proof. Two live
// helpers carry identical AF_SESSION/AF_HOME/AF_SESSION_GEN markers: the one
// still in the proven pane's kernel session is a cohort member; the one that
// setsid'd out of it — the af-daemon-child shape — is not.
func TestMarkedOrphanProcessesRequiresSessionMembershipBeyondMarkers(t *testing.T) {
	name := "af_membership"
	home := t.TempDir()
	gen := "gen-membership"

	member := spawnCohortSleeper(t, name, home, gen, false)
	setsid := spawnCohortSleeper(t, name, home, gen, true)
	membership := cohortMembership{PaneSIDs: map[int]bool{member.SID: true}}

	marked, err := markedOrphanProcesses(
		[]proctree.Process{member, setsid}, name, home,
		fixtureCohort(gen), membership)
	require.NoError(t, err)
	require.Equal(t, []proctree.Process{member}, marked,
		"the setsid'd helper carries the same markers but left the proven session — it must be skipped")

	require.True(t, proctree.AliveSame(member))
	require.True(t, proctree.AliveSame(setsid))
}

// spawnCohortSleeper starts a live sleeper carrying this session's full
// marker environment, setsid'd out of it when asked.
func spawnCohortSleeper(t *testing.T, name, home, gen string, newSession bool) proctree.Process {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.Env = append(os.Environ(),
		EnvMarkerSession+"="+name,
		EnvMarkerHome+"="+home,
		EnvMarkerGeneration+"="+gen,
	)
	if newSession {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	var p proctree.Process
	var err error
	require.Eventually(t, func() bool {
		p, err = proctree.Lookup(cmd.Process.Pid)
		return err == nil && p.SID > 0
	}, 5*time.Second, 20*time.Millisecond)
	if newSession {
		require.Equal(t, p.PID, p.SID, "the setsid'd helper must lead its own session")
	}
	return p
}
