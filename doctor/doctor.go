// Package doctor implements `af doctor` (#1044, #1104): a read-only sweep
// that detects the leak classes behind the 2026-07-03 outage — orphaned
// session descendants burning CPU forever, runaway children of live
// sessions, af_ tmux sessions with no backing record, stale temp
// agent-factory homes, and unhealthy daemons — and, with --fix, applies only
// the remediations whose ancestry is verified.
//
// The safety stance is asymmetric by design: detection is generous,
// remediation is conservative. A process is only ever killed when its
// AF_SESSION env marker proves it was spawned inside an af tmux session that
// no longer exists, and every signal re-verifies the (pid, starttime)
// identity. Anything ambiguous — a process that merely looks orphaned, a
// session that might belong to another agent-factory home — is reported,
// never touched.
package doctor

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// Finding is one detected problem. FixAction is empty for report-only
// findings; when set, applying opts.Fix runs the attached fix.
type Finding struct {
	// Check is the detector that produced this finding (stable slug).
	Check string
	// Detail is the human-readable description.
	Detail string
	// FixAction describes what --fix will do; empty means report-only.
	FixAction string
	fix       func() error
	// Fixed / FixErr record the outcome when a fix was attempted.
	Fixed  bool
	FixErr error
}

// Report is the result of a doctor run.
type Report struct {
	// OK holds informational healthy lines, grouped by section.
	OK []string
	// Findings holds detected problems in detection order.
	Findings []Finding
}

// UnresolvedCount returns how many findings remain un-remediated (either
// report-only, fix not requested, or fix failed).
func (r *Report) UnresolvedCount() int {
	n := 0
	for _, f := range r.Findings {
		if !f.Fixed {
			n++
		}
	}
	return n
}

// Options configures a doctor run. Zero values resolve to production
// defaults; tests override ConfigDir/TempDir/Exec to stay hermetic.
type Options struct {
	// Fix applies the safe remediations instead of only reporting.
	Fix bool
	// ConfigDir is the active agent-factory home; defaults to
	// config.GetConfigDir().
	ConfigDir string
	// TempDir is the temp root scanned for stale AF homes; defaults to
	// os.TempDir().
	TempDir string
	// Exec runs tmux commands; defaults to cmd.MakeExecutor().
	Exec cmd.Executor
	// MinTempHomeAge is how old an unused temp home must be before it
	// counts as stale; defaults to 7 days (long enough that no plausible
	// test or debug run is still coming back for it).
	MinTempHomeAge time.Duration

	// Escalation windows for --fix kills; default to 2s/2s.
	killGrace    time.Duration
	killTermWait time.Duration

	// snapshot overrides the /proc scan; tests inject a snapshot containing
	// only their own spawned processes so a --fix run can never act on
	// anything outside the test.
	snapshot func() (map[int]proctree.Process, error)
}

// scanContext carries the shared, immutable inputs of one run.
type scanContext struct {
	opts Options
	// snap is the /proc snapshot; nil when /proc is unavailable, in which
	// case the process-based checks degrade to no-ops.
	snap map[int]proctree.Process
	// selfAncestors holds our own PID and every ancestor — never proposed
	// for a kill, no matter what markers they carry.
	selfAncestors map[int]bool
}

// Run executes all checks and (when opts.Fix) applies the safe remediations.
func Run(opts Options) (*Report, error) {
	if opts.ConfigDir == "" {
		dir, err := config.GetConfigDir()
		if err != nil {
			return nil, fmt.Errorf("resolving agent-factory home: %w", err)
		}
		opts.ConfigDir = dir
	}
	if opts.TempDir == "" {
		opts.TempDir = tempDirDefault()
	}
	if opts.Exec == nil {
		opts.Exec = cmd.MakeExecutor()
	}
	if opts.MinTempHomeAge == 0 {
		opts.MinTempHomeAge = 7 * 24 * time.Hour
	}
	if opts.killGrace == 0 {
		opts.killGrace = 2 * time.Second
	}
	if opts.killTermWait == 0 {
		opts.killTermWait = 2 * time.Second
	}

	if opts.snapshot == nil {
		opts.snapshot = proctree.Snapshot
	}

	ctx := &scanContext{opts: opts, selfAncestors: map[int]bool{}}
	if snap, err := opts.snapshot(); err == nil {
		ctx.snap = snap
		for pid := range selfAndAncestors(snap) {
			ctx.selfAncestors[pid] = true
		}
	}

	report := &Report{}
	checkDaemonHealth(ctx, report)
	checkOrphanedProcesses(ctx, report)
	checkRunawayChildren(ctx, report)
	checkLeakedTmuxSessions(ctx, report)
	checkStaleTempHomes(ctx, report)
	checkForeignDaemons(ctx, report)

	if opts.Fix {
		for i := range report.Findings {
			f := &report.Findings[i]
			if f.fix == nil {
				continue
			}
			if err := f.fix(); err != nil {
				f.FixErr = err
				log.WarningLog.Printf("doctor --fix: %s: %s: %v", f.Check, f.FixAction, err)
			} else {
				f.Fixed = true
				log.InfoLog.Printf("doctor --fix: %s: %s", f.Check, f.FixAction)
			}
		}
	}
	return report, nil
}

// selfAndAncestors walks from our own PID to init, collecting every PID on
// the way. Bounded by snapshot lookups, so a cyclic/garbled snapshot cannot
// loop forever.
func selfAndAncestors(snap map[int]proctree.Process) map[int]bool {
	out := map[int]bool{}
	pid := selfPID()
	for i := 0; i < 128; i++ {
		if pid <= 0 || out[pid] {
			break
		}
		out[pid] = true
		p, ok := snap[pid]
		if !ok {
			break
		}
		pid = p.PPID
	}
	return out
}

// Render writes the human-readable report. fixMode changes the trailing
// hints (no "--fix would..." when fixes were just applied).
func Render(w io.Writer, r *Report, fixMode bool) {
	if len(r.OK) > 0 {
		for _, line := range r.OK {
			fmt.Fprintf(w, "ok: %s\n", line)
		}
	}
	if len(r.Findings) == 0 {
		fmt.Fprintf(w, "\nNo problems found.\n")
		return
	}
	fmt.Fprintln(w)
	for _, f := range r.Findings {
		switch {
		case f.Fixed:
			fmt.Fprintf(w, "fixed: [%s] %s (%s)\n", f.Check, f.Detail, f.FixAction)
		case f.FixErr != nil:
			fmt.Fprintf(w, "fix-failed: [%s] %s (%s: %v)\n", f.Check, f.Detail, f.FixAction, f.FixErr)
		case f.FixAction != "" && !fixMode:
			fmt.Fprintf(w, "issue: [%s] %s (--fix will %s)\n", f.Check, f.Detail, f.FixAction)
		default:
			fmt.Fprintf(w, "issue: [%s] %s\n", f.Check, f.Detail)
		}
	}
	unresolved := r.UnresolvedCount()
	fixed := len(r.Findings) - unresolved
	switch {
	case fixMode && fixed > 0:
		fmt.Fprintf(w, "\n%d issue(s) found, %d fixed, %d remaining.\n", len(r.Findings), fixed, unresolved)
	case !fixMode && hasFixable(r):
		fmt.Fprintf(w, "\n%d issue(s) found. Re-run with --fix to apply the safe remediations.\n", len(r.Findings))
	default:
		fmt.Fprintf(w, "\n%d issue(s) found.\n", len(r.Findings))
	}
}

func hasFixable(r *Report) bool {
	for _, f := range r.Findings {
		if f.FixAction != "" {
			return true
		}
	}
	return false
}

// sortedKeys returns m's keys in stable order so report output is
// deterministic.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
