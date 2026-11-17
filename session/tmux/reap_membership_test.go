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
	// Cleans to the home, but resolves through a REGULAR FILE — path
	// traversal through a non-directory fails with ENOTDIR on every platform,
	// so nothing can prove the names name the same place. A missing-directory
	// variant was tried first; platform fs behavior around ENOENT-in-lookup
	// made it unreliable (#7435 CI on Ubuntu 26.04).
	notDir := filepath.Join(filepath.Dir(home), "not-a-dir")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))
	lexicalAlias := filepath.Join(notDir, "..", filepath.Base(home))
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

// #7435 item 5 — the cohort gate admits only what kernel evidence proves:
// capture inside the proven live tree, a SID one of the pane roots led, a
// (pid, StartID) recorded at spawn, or descent from a proven member. No
// evidence at all means the gate is off, never silently on.
func TestCohortMembershipAdmits(t *testing.T) {
	member := proctree.Process{PID: 100, SID: 40, StartID: 9}

	require.True(t, cohortMembership{}.admits(nil, member), "no evidence: gate off")
	require.True(t, cohortMembership{PaneSIDs: map[int]bool{}}.admits(nil, member),
		"an EMPTY sid set is still no evidence: gate off")
	require.True(t, cohortMembership{PaneSIDs: map[int]bool{40: true}}.admits(nil, member))
	require.True(t, cohortMembership{Recorded: map[int]uint64{100: 9}}.admits(nil, member))
	require.True(t, cohortMembership{Proven: map[int]uint64{100: 9}}.admits(nil, member),
		"captured inside the proven live tree is membership proof on its own")
	require.False(t, cohortMembership{PaneSIDs: map[int]bool{41: true}}.admits(nil, member),
		"a foreign SID is not membership however the markers read")
	require.False(t, cohortMembership{Recorded: map[int]uint64{100: 8}}.admits(nil, member),
		"a recycled pid with a different StartID is not the recorded process")
	require.False(t, cohortMembership{Recorded: map[int]uint64{101: 9}}.admits(nil, member),
		"a different pid carrying the same StartID is not the recorded process")
	require.False(t, cohortMembership{Proven: map[int]uint64{100: 8}}.admits(nil, member),
		"a recycled pid with a different StartID was never inside the tree")
}

// A process never itself captured still proves membership by descending from
// one that was — the shape a refresh pass finds when a proven leftover forks a
// child during the sweep.
func TestCohortMembershipAdmitsByDescentFromTheProvenTree(t *testing.T) {
	proven := proctree.Process{PID: 50, PPID: 1, SID: 50, StartID: 7}
	child := proctree.Process{PID: 60, PPID: 50, SID: 60, StartID: 8}
	snap := map[int]proctree.Process{50: proven, 60: child}
	membership := cohortMembership{Proven: map[int]uint64{50: 7}}

	require.True(t, membership.admits(snap, child),
		"a child of a proven member is inside the proven tree whatever the env scan says")
	require.False(t, cohortMembership{Proven: map[int]uint64{50: 99}}.admits(snap, child),
		"a reused ancestor pid with a different StartID proves nothing")
	require.False(t, cohortMembership{PaneSIDs: map[int]bool{40: true}}.admits(snap, child),
		"descent cannot help without a proven member to reach")
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

// #7435 Codex — an UNPROVABLE home is never "not ours, carry on". A survivor
// whose AF_HOME is relative (.af) can name this home or another's, so the
// vanished-session sweep must refuse on it: error, not an empty survivor set
// — the empty answer is what licensed deleting the worktree underneath it.
func TestVanishedSessionSweepRefusesAnUnprovableHomeSurvivor(t *testing.T) {
	name := "af_unproven_home"
	ownHome := t.TempDir()
	gen := "gen-unproven"
	survivor := spawnCohortSleeper(t, name, ".af", gen, false)

	err := reapVanishedSessionProcesses(name, ownHome,
		[]proctree.Process{survivor}, nil, false)
	require.Error(t, err, "an unprovable home must refuse the sweep, not silently clear it")
	require.Contains(t, err.Error(), "cannot be proven",
		"the refusal must name the attribution failure, not a generic skip")
	require.True(t, proctree.AliveSame(survivor),
		"the survivor stays alive AND the cleanup is not authorized — both halves of fail-closed")

	// The same process read as a candidate directly shows the per-pid split:
	// foreign is skipped by proof, unproven blocks.
	marked, inspectErr := markedOrphanProcesses(
		[]proctree.Process{survivor}, name, ownHome, fixtureCohort(gen), cohortMembership{})
	require.Error(t, inspectErr)
	require.Contains(t, inspectErr.Error(), "cannot be proven")
	require.Empty(t, marked)
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
