package doctor

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// possibleOrphanCap mirrors the const maxPossibleOrphans in
// checkOrphanedProcesses. It is duplicated here deliberately: the cap is the
// boundary this regression guards (the bug is the overflow summary that fires
// past it), so if the production cap changes, this constant and `total` below
// must change together — a silent drift would let the bug regress against a
// new cap without a failing test.
const possibleOrphanCap = 15

// stageCPUFraction replaces the per-process CPU reader with a deterministic
// table, so the possible-orphan overflow summary — which describes the hidden
// tail by its measured max CPU — can be exercised without spawning real
// CPU-burning children. A busy loop's lifetime-average fraction depends on the
// host's free-core count at scan time (a concurrent set reads 0.3 or 1.0 of a
// core depending on the box) and so is flaky under contention; that is the
// same reason daemonProcessEnvLookup and its siblings are package vars. The
// seam is the only entry point for CPU in the possible-orphan path, so tests
// pin it while production keeps the default (proctree.CPUFraction). Restored
// on cleanup.
func stageCPUFraction(t *testing.T, fracs map[int]float64) {
	t.Helper()
	orig := cpuFraction
	cpuFraction = func(p proctree.Process) (float64, float64, error) {
		if f, ok := fracs[p.PID]; ok {
			return f, 60, nil
		}
		return 0, 60, nil
	}
	t.Cleanup(func() { cpuFraction = orig })
}

// spawnPossibles stages n idle children whose TMUX env names a server PID of
// 1<<30 — unreachable by construction (the kernel caps pids far below it), so
// in the filtered snapshot tmuxServerDead returns true and each child lands in
// the possibles path. Each is a single blocked `read line` shell (no
// descendants, reaped on cleanup) carrying no agent-factory marker, so doctor
// can prove no ownership and reports them advisory.
func spawnPossibles(t *testing.T, n int) []proctree.Process {
	t.Helper()
	out := make([]proctree.Process, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, spawnWithEnv(t, "sh", nil, map[string]string{
			"TMUX": fmt.Sprintf("/tmp/af-doctor-dead-tmux-%d,%d,0", i, 1<<30),
		}))
	}
	return out
}

// rankedTail replicates the production sort (descending frac, ascending-PID
// tiebreak) using the staged CPU seam, and returns the entries at or beyond the
// cap — the coldest set the overflow summary collapses rather than rendering
// individually. Reading the CPU through the seam ties the expected tail to the
// same values the production ranking loop used.
func rankedTail(t *testing.T, possibles []proctree.Process) []proctree.Process {
	t.Helper()
	type r struct {
		p    proctree.Process
		frac float64
	}
	rs := make([]r, 0, len(possibles))
	for _, p := range possibles {
		frac, _, _ := cpuFraction(p)
		rs = append(rs, r{p, frac})
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].frac != rs[j].frac {
			return rs[i].frac > rs[j].frac
		}
		return rs[i].p.PID < rs[j].p.PID
	})
	if len(rs) <= possibleOrphanCap {
		return nil
	}
	tail := make([]proctree.Process, 0, len(rs)-possibleOrphanCap)
	for _, x := range rs[possibleOrphanCap:] {
		tail = append(tail, x.p)
	}
	return tail
}

// TestPossibleOrphanOverflowDescribesTailByMeasuredCPU is the regression for the
// false "all idle or near-idle" claim. When more than maxPossibleOrphans (15)
// possibles exist, the descending-by-CPU sort hides the coldest tail behind one
// summary row, and that row MUST describe the hidden processes by their
// MEASURED CPU (the per-process frac the ranking loop already captured on
// `ranked`) rather than asserting "idle" unconditionally. Three tail shapes:
//
//   - idle tail (max frac 0): the truthful "all idle or near-idle" wording is
//     preserved — no regression to the case the data supports.
//   - moderate tail (max frac 0.33, the production-gate shape from the bug
//     report, where the 15 hottest sit at 33-78% over a minute): "up to 33% CPU
//     in the remainder" — the core fix.
//   - pegging tail (max frac 0.85, ≥ runawayCPUFraction): "some at up to 85%
//     CPU" — the strong-signal branch the old claim was most plainly wrong about.
//
// Every shape keeps possibles advisory (no --fix, no exit-code contribution),
// surfaces the 15 hottest individually with their measured CPU, and collapses
// the 2 coldest (naming only their count — never their PIDs — in the summary).
func TestPossibleOrphanOverflowDescribesTailByMeasuredCPU(t *testing.T) {
	testguard.IsolateTmux(t)

	const total = possibleOrphanCap + 2    // 17: 15 surfaced + 2 collapsed
	tailCount := total - possibleOrphanCap // 2

	t.Run("idle_tail", func(t *testing.T) {
		possibles := spawnPossibles(t, total)
		// Every possible reads idle, so the tail's max frac is 0 and the
		// summary must keep the truthful "all idle or near-idle" wording.
		stageCPUFraction(t, nil)
		assertPossibleOrphanOverflow(t, possibles, "all idle or near-idle", tailCount)
	})

	t.Run("moderate_tail", func(t *testing.T) {
		possibles := spawnPossibles(t, total)
		fracs := make(map[int]float64, total)
		// The 15 hottest sit at 0.78 … 0.36 over a minute; the 2 coldest
		// (the hidden tail) at 0.33 — well above "near-idle" by any reading.
		for i, p := range possibles {
			if i < possibleOrphanCap {
				fracs[p.PID] = 0.78 - float64(i)*0.03
			} else {
				fracs[p.PID] = 0.33
			}
		}
		stageCPUFraction(t, fracs)
		assertPossibleOrphanOverflow(t, possibles, "up to 33% CPU in the remainder", tailCount)
	})

	t.Run("pegging_tail", func(t *testing.T) {
		possibles := spawnPossibles(t, total)
		fracs := make(map[int]float64, total)
		// Every possible pegs a core (≥ runawayCPUFraction), so even the
		// hidden tail is CPU-burning — the case the old "all idle" claim
		// was most plainly false about.
		for i, p := range possibles {
			if i < possibleOrphanCap {
				fracs[p.PID] = 0.99 - float64(i)*0.01
			} else {
				fracs[p.PID] = 0.85
			}
		}
		stageCPUFraction(t, fracs)
		assertPossibleOrphanOverflow(t, possibles, "some at up to 85% CPU", tailCount)
	})
}

// assertPossibleOrphanOverflow runs doctor over possibles and pins the
// possible-orphan findings: exactly possibleOrphanCap individual rows plus one
// overflow summary whose Detail carries expect. It also pins the
// no-regression invariants — advisory only, no --fix, no exit-code
// contribution, the hottest possibles surfaced individually with their
// measured CPU, and the coldest tail collapsed (never named) in the summary.
func assertPossibleOrphanOverflow(
	t *testing.T,
	possibles []proctree.Process,
	expect string,
	tailCount int,
) {
	t.Helper()
	pids := make([]int, len(possibles))
	for i, p := range possibles {
		pids[i] = p.PID
	}
	tailPIDs := map[int]bool{}
	for _, p := range rankedTail(t, possibles) {
		tailPIDs[p.PID] = true
	}
	require.Len(t, tailPIDs, tailCount,
		"the staged tail must collapse exactly tailCount processes, not more or fewer")

	report, err := Run(testOptions(t, true, pids...))
	require.NoError(t, err)

	findings := findByCheck(report, "possible-orphan")
	// possibleOrphanCap individual rows + 1 collapsed summary.
	require.Len(t, findings, possibleOrphanCap+1,
		"the cap emits %d individual possible-orphan rows then one overflow summary", possibleOrphanCap)

	// The overflow summary is the LAST possible-orphan finding (emission
	// order is the loop order, and the `break` at the cap ends it).
	overflow := findings[len(findings)-1]
	require.Contains(t, overflow.Detail,
		fmt.Sprintf("… and %d more processes of dead tmux servers", tailCount),
		"the summary must name how many possibles it hides: %q", overflow.Detail)
	require.Contains(t, overflow.Detail, "("+expect+";",
		"the summary must describe the hidden tail by its measured CPU, not assert idle unconditionally: %q", overflow.Detail)

	// The three descriptions are mutually exclusive branches; assert the
	// summary did not cross-contaminate them.
	switch expect {
	case "all idle or near-idle":
		require.NotContains(t, overflow.Detail, "% CPU",
			"an idle tail must not be dressed up with a measured percentage: %q", overflow.Detail)
	case "up to 33% CPU in the remainder":
		require.NotContains(t, overflow.Detail, "all idle or near-idle",
			"a measured-nonzero tail must not be called idle: %q", overflow.Detail)
		require.NotContains(t, overflow.Detail, "some at",
			"a tail below runawayCPUFraction must use the moderate wording, not the pegging wording: %q", overflow.Detail)
	case "some at up to 85% CPU":
		require.NotContains(t, overflow.Detail, "all idle or near-idle",
			"a pegging tail must not be called idle: %q", overflow.Detail)
		require.NotContains(t, overflow.Detail, "in the remainder",
			"a tail at/above runawayCPUFraction must use the pegging wording, not the moderate one: %q", overflow.Detail)
	}

	// No-regression invariants: possibles are advisory by definition, so
	// they never drive a health probe, carry no --fix, keep the exit clean,
	// and are left untouched by a --fix run.
	for _, f := range findings {
		require.False(t, f.Actionable,
			"a possible-orphan is never a failed health check: %q", f.Detail)
		require.Empty(t, f.FixAction,
			"doctor cannot prove ownership, so possibles carry no --fix: %q", f.Detail)
	}
	require.Zero(t, report.UnresolvedCount(),
		"advisory possibles must not contribute to the exit code")
	for _, p := range possibles {
		require.True(t, alive(p),
			"possibles are report-only — none may be killed by a --fix run: pid %d", p.PID)
	}

	// The hottest possibles are surfaced individually (their PIDs appear in
	// some finding Detail, each rendering its measured "X% CPU over 1m" so
	// the operator sees the high-CPU orphans); the coldest tail is collapsed
	// and must NOT be named in any finding Detail — the summary is the only
	// row about those processes.
	var renderedPIDs strings.Builder
	for _, f := range findings {
		renderedPIDs.WriteString(f.Detail)
		renderedPIDs.WriteByte('\n')
	}
	for _, p := range possibles {
		needle := fmt.Sprintf("pid %d", p.PID)
		if tailPIDs[p.PID] {
			require.NotContains(t, renderedPIDs.String(), needle,
				"a hidden tail PID must not surface in any finding — the summary collapses it: pid %d", p.PID)
		} else {
			require.Contains(t, renderedPIDs.String(), needle,
				"a hottest-possible PID must be reported individually so the operator sees the high-CPU orphans: pid %d", p.PID)
		}
	}

	// Every individual row renders its measured CPU through the same seam
	// the ranking used, so the operator is not shown a CPU-burning orphan as
	// "0%": describeProc and the sort read one fact, not two.
	individuals := findings[:possibleOrphanCap]
	for _, f := range individuals {
		require.Contains(t, f.Detail, "% CPU over 1m",
			"each surfaced possible must render its measured CPU: %q", f.Detail)
	}
}
