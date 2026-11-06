package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// probeChurnFixedTime keeps every timestamp the projection copies out of the
// instance byte-stable so the golden rows below do not drift between runs.
var probeChurnFixedTime = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

// probeLocalInstance shapes the row the daemon status probe walks on every
// tick: a started local session holding the realistic tab spread — agent,
// shell, web and process tabs — plus a populated worktree. It is built
// field-by-field rather than through FromInstanceData so nothing binds a real
// tmux name, spawns a process, or stats a worktree: the cost under test is the
// projection itself, not the restore (#6845).
func probeLocalInstance(tb testing.TB) *Instance {
	tb.Helper()
	wt, err := git.NewGitWorktreeFromStorage(
		"/repo/probe-local",
		"/repo/probe-local/.worktrees/probe-local",
		"probe-local", "af/probe-local",
		"0123456789abcdef0123456789abcdef01234567",
		false, true)
	require.NoError(tb, err)
	return &Instance{
		ID:          "inst-probe-local-00001",
		Title:       "probe-local",
		Path:        "/repo/probe-local/.worktrees/probe-local",
		Branch:      "af/probe-local",
		Program:     "claude",
		liveness:    LiveRunning,
		started:     true,
		backend:     NewFakeBackend(),
		CreatedAt:   probeChurnFixedTime,
		UpdatedAt:   probeChurnFixedTime,
		Height:      24,
		Width:       80,
		gitWorktree: wt,
		Tabs: []*Tab{
			{ID: "tab-aaaaaaa1", Name: agentTabName, Kind: TabKindAgent, tmux: tmux.NewTmuxSessionFromSanitizedName("af_probe_local_agent", "claude")},
			{ID: "tab-aaaaaaa2", Name: shellTabName, Kind: TabKindShell, tmux: tmux.NewTmuxSessionFromSanitizedName("af_probe_local_shell", "bash")},
			{ID: "tab-aaaaaaa3", Name: "dev", Kind: TabKindWeb, URL: "http://localhost:3000"},
			{ID: "tab-aaaaaaa4", Name: "build", Kind: TabKindProcess, Command: "make dev", tmux: tmux.NewTmuxSessionFromSanitizedName("af_probe_local_proc", "make")},
		},
	}
}

// probeSandboxInstance shapes an off-box session as the probe sees it after a
// daemon restart: a docker row loaded inert and Lost, its agent tab tmux-less
// and its metadata tabs still staged in pendingMetadataTabs — the shape whose
// sandbox predicates ran on every tick.
func probeSandboxInstance(tb testing.TB) *Instance {
	tb.Helper()
	return &Instance{
		ID:               "inst-probe-sandbox-001",
		Title:            "probe-sandbox",
		Path:             "/repo/probe-sandbox",
		Program:          "codex",
		Host:             "probe-docker",
		liveness:         LiveLost,
		started:          false,
		backend:          newInertSandboxBackend("docker"),
		repoIdentityPath: "/repo/probe-sandbox",
		CreatedAt:        probeChurnFixedTime,
		UpdatedAt:        probeChurnFixedTime,
		Tabs: []*Tab{
			{ID: "tab-bbbbbbb1", Name: agentTabName, Kind: TabKindAgent},
		},
		pendingMetadataTabs: []TabData{
			{ID: "tab-bbbbbbb2", Name: "docs", Kind: TabKindWeb, URL: "https://docs.example.com"},
		},
	}
}

// probeArchivedInstance shapes a local archived row: tmux torn down, the
// worktree binding still recorded so a restore knows where it lives.
func probeArchivedInstance(tb testing.TB) *Instance {
	tb.Helper()
	wt, err := git.NewGitWorktreeFromStorage(
		"/repo/probe-archived",
		"/repo/probe-archived/.af-archive/probe-archived",
		"probe-archived", "af/probe-archived",
		"fedcba9876543210fedcba9876543210fedcba98",
		false, true)
	require.NoError(tb, err)
	return &Instance{
		ID:          "inst-probe-archived-01",
		Title:       "probe-archived",
		Path:        "/repo/probe-archived/.af-archive/probe-archived",
		Branch:      "af/probe-archived",
		Program:     "claude",
		liveness:    LiveArchived,
		started:     false,
		backend:     NewFakeBackend(),
		archivedAt:  probeChurnFixedTime,
		CreatedAt:   probeChurnFixedTime,
		UpdatedAt:   probeChurnFixedTime,
		gitWorktree: wt,
		Tabs: []*Tab{
			{ID: "tab-ccccccc1", Name: agentTabName, Kind: TabKindAgent, tmux: tmux.NewTmuxSessionFromSanitizedName("af_probe_arch_agent", "claude")},
			{ID: "tab-ccccccc2", Name: "docs", Kind: TabKindWeb, URL: "http://localhost:8080"},
		},
	}
}

// TestIsSandboxBackendTypeDoesNotAllocate pins the per-probe predicate at zero
// heap. The status poll asks it of every session's backend on every tick — in
// the heap profile for #6845 it built a fresh fmt.Errorf for every local row
// just to test err == nil, ~10% of toInstanceDataLocked's allocation volume for
// a yes/no answer.
func TestIsSandboxBackendTypeDoesNotAllocate(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote", "local", "bogus", ""} {
		allocs := testing.AllocsPerRun(64, func() {
			_ = isSandboxBackendType(backendType)
			_ = IsSandboxBackendType(backendType)
		})
		require.Zero(t, allocs,
			"isSandboxBackendType(%q) must answer from a lookup, not an error construction", backendType)
	}
}

// TestIsSandboxBackendTypeAnswers pins the verdict itself: exactly the
// re-provisionable sandbox runtimes are admitted, everything else — the local
// backend and every unknown discriminator — is not.
func TestIsSandboxBackendTypeAnswers(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		require.True(t, isSandboxBackendType(backendType), backendType)
		require.True(t, IsSandboxBackendType(backendType), backendType)
	}
	for _, backendType := range []string{"local", "hook", "bogus", ""} {
		require.False(t, isSandboxBackendType(backendType), backendType)
		require.False(t, IsSandboxBackendType(backendType), backendType)
	}
}

// TestPollGateAccessorsMatchProjection pins the equivalence the daemon's
// persist/outage gates rely on: the cheap accessor reads must report exactly
// what ToInstanceData serializes for the same row — on every shape the probe
// walks, including a limit-parked row whose reset fields are liveness-gated
// and an unreachable row whose outage pair must round-trip (#6845).
func TestPollGateAccessorsMatchProjection(t *testing.T) {
	shapes := map[string]*Instance{
		"local-multi-tab": probeLocalInstance(t),
		"sandbox-inert":   probeSandboxInstance(t),
		"archived-local":  probeArchivedInstance(t),
	}
	// A limit-parked row carries the gated reset pair the persist gate
	// diffs every tick; an unreachable row carries the outage pair the
	// publish gate diffs.
	limited := probeLocalInstance(t)
	limited.liveness = LiveLimitReached
	limited.limitResetAt = probeChurnFixedTime.Add(time.Hour)
	limited.limitResetEvidence = "usage resets at 05:05"
	shapes["limit-reached"] = limited
	unreachable := probeLocalInstance(t)
	unreachable.SetUnreachable(errors.New("transport refused"))
	shapes["unreachable"] = unreachable

	for name, inst := range shapes {
		t.Run(name, func(t *testing.T) {
			data := inst.ToInstanceData()
			require.Equal(t, data.Liveness, inst.GetLiveness(), "liveness")
			require.Equal(t, data.InFlightOp, inst.GetInFlightOp(), "in-flight op")
			resetAt, _ := inst.LimitResetAt()
			require.True(t, data.LimitResetAt.Equal(resetAt),
				"LimitResetAt: projection %v vs accessor %v", data.LimitResetAt, resetAt)
			require.Equal(t, data.LimitResetEvidence, inst.LimitResetEvidence(), "limit evidence")
			require.Equal(t, data.Unreachable, inst.UnreachableSnapshot(), "unreachable")
		})
	}
}

// TestToInstanceDataProbeShapesGolden pins the serialized record the probe
// publishes — the event payload and the storage projection — for the local,
// sandbox, archived and multi-tab shapes, so the churn fix can prove it changed
// no observable output rather than merely claim it (#6845).
//
// Regenerate with AF_UPDATE_PROBE_GOLDEN=1 after an intentional output change
// and review the diff.
func TestToInstanceDataProbeShapesGolden(t *testing.T) {
	shapes := map[string]*Instance{
		"local-multi-tab": probeLocalInstance(t),
		"sandbox-inert":   probeSandboxInstance(t),
		"archived-local":  probeArchivedInstance(t),
	}
	doc := make(map[string]json.RawMessage, 2*len(shapes))
	for name, inst := range shapes {
		data := inst.ToInstanceData()
		wire, err := json.Marshal(data)
		require.NoError(t, err, name)
		doc[name] = wire
		stored, err := json.Marshal(data.ForStorage())
		require.NoError(t, err, name)
		doc[name+"-storage"] = stored
	}
	got, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)

	golden := filepath.Join("testdata", "probe_shapes.golden.json")
	if os.Getenv("AF_UPDATE_PROBE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(golden, append(got, '\n'), 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got),
		"probe snapshot output drifted; if intentional, AF_UPDATE_PROBE_GOLDEN=1 and review the diff")
}

// BenchmarkToInstanceDataProbeRow is the per-probe cost the daemon was paying
// on every tick for every session before #6845: a whole InstanceData built
// only to diff a handful of fields.
func BenchmarkToInstanceDataProbeRow(b *testing.B) {
	inst := probeLocalInstance(b)
	b.ReportAllocs()
	b.ResetTimer()
	var sink InstanceData
	for i := 0; i < b.N; i++ {
		sink = inst.ToInstanceData()
	}
	_ = sink
}

// BenchmarkPollGateAccessors is the cost the unchanged-path gate pays instead:
// the small accessor reads it diffs against the previous tick.
func BenchmarkPollGateAccessors(b *testing.B) {
	inst := probeLocalInstance(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = inst.GetLiveness()
		_ = inst.GetInFlightOp()
		_, _ = inst.LimitResetAt()
		_ = inst.LimitResetEvidence()
		_ = inst.UnreachableSnapshot()
		_ = inst.AccountLimitObservationsUnpersisted()
		_ = inst.ConversationUnpersisted()
	}
}

func BenchmarkIsSandboxBackendType(b *testing.B) {
	for _, backendType := range []string{"local", "docker"} {
		b.Run(backendType, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = isSandboxBackendType(backendType)
			}
		})
	}
}
