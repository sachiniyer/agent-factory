// Package doctor implements `af doctor` (#1044, #1104): a read-only sweep
// that detects the leak classes behind the 2026-07-03 outage — orphaned
// session descendants burning CPU forever, runaway children of live
// sessions, af_ tmux sessions with no backing record, stale temp
// agent-factory homes, and unhealthy daemons — and, with --fix, applies only
// the remediations whose ancestry is verified.
//
// It also validates the remote-hook backend (#1039) for the repository
// containing the current working directory: config completeness, hook-script
// presence/executability, and a bounded read-only connectivity probe via
// list_cmd. When no remote backend is configured — the common case — the
// remote checks record a single informational "n/a" line and add no findings,
// so local-only users see no new noise.
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
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
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
	// Section, Severity, and Remediation drive the grouped doctor renderer.
	// Empty values are filled from the Check slug so existing detectors stay
	// compact while new checks can be explicit.
	Section     string
	Severity    CheckStatus
	Remediation string
}

// CheckStatus is the user-facing status for one doctor row.
type CheckStatus string

const (
	StatusPass  CheckStatus = "PASS"
	StatusWarn  CheckStatus = "WARN"
	StatusFail  CheckStatus = "FAIL"
	StatusFixed CheckStatus = "FIXED"
)

// HeaderItem is one key-value fact printed before grouped checks.
type HeaderItem struct {
	Label string
	Value string
}

// CheckResult is one grouped doctor row that is not backed by a Finding.
type CheckResult struct {
	Section     string
	Name        string
	Status      CheckStatus
	Detail      string
	Remediation string
	Problem     bool
}

// Report is the result of a doctor run.
type Report struct {
	// Header holds the key environment facts printed before the sections.
	Header []HeaderItem
	// Checks holds sectioned PASS/WARN/FAIL rows that are not Finding-backed.
	Checks []CheckResult
	// OK holds informational healthy lines, grouped by section.
	OK []string
	// Findings holds detected problems in detection order.
	Findings []Finding
}

// UnresolvedCount returns how many findings remain un-remediated (either
// report-only, fix not requested, or fix failed).
func (r *Report) UnresolvedCount() int {
	n := 0
	for _, c := range r.Checks {
		if c.Problem && c.Status != StatusPass && c.Status != StatusFixed {
			n++
		}
	}
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
	// Version is the af version stamped into the root command.
	Version string
	// Fix applies the safe remediations instead of only reporting.
	Fix bool
	// Setup runs the onboarding profile only: local prerequisites, writable
	// AF home/config/state/log storage, daemon health, and remote-hook setup.
	// It intentionally skips the full process/tmux/temp-home leak sweep.
	Setup bool
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

	// remoteConfig resolves the remote-hook backend to validate and the repo
	// root it was loaded from; nil hooks mean no remote is configured and the
	// remote checks skip cleanly. Defaults to resolving the repo of the current
	// working directory (defaultRemoteConfig); tests inject a hermetic resolver.
	remoteConfig func() (*config.RemoteHooks, string, error)

	// The skew checks' injection points (#1044). Every one of them reaches for
	// real daemon/system state, so each is a func field the tests replace with
	// a fake: doctor's own tests must never depend on — let alone disturb — the
	// daemon or the installs on the machine running them.
	//
	// daemonHealth resolves daemon liveness and the version it reports;
	// defaults to daemon.Health.
	daemonHealth func() daemon.HealthStatus
	// autostartUnit reads the installed autostart unit; defaults to
	// daemon.InspectAutostart.
	autostartUnit func() daemon.AutostartUnitInfo
	// autostartSupervision probes the service manager; defaults to
	// daemon.AutostartSupervision.
	autostartSupervision func() daemon.SupervisionInfo
	// autostartServesHome reports whether the installed autostart unit's daemon
	// would serve the home under inspection; defaults to
	// daemon.AutostartUnitServesHome (#1919).
	autostartServesHome func(configDir string) (serves bool, installed bool, err error)
	// selfBinary resolves the path of the binary running this command;
	// defaults to os.Executable.
	selfBinary func() (string, error)
	// binaryCandidates lists af install paths to compare; defaults to
	// defaultBinaryCandidates.
	binaryCandidates func() []string
	// binaryVersion reports the version of the af binary at a path; defaults
	// to execBinaryVersion.
	binaryVersion func(path string) (string, error)

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
	// daemons memoizes the run's daemon scan (see daemonProcs); daemonsScanned
	// distinguishes "scanned, found none" from "not scanned yet".
	daemons        []daemonProc
	daemonsScanned bool
	// autostart scope memo (see autostartScope): whether the installed unit is
	// this home's at all.
	autostartServes    bool
	autostartInstalled bool
	autostartScopeErr  error
	autostartScoped    bool
}

// applyDefaults resolves every zero-valued option to its production default.
// It must run to completion before the Options are copied into a scanContext:
// a default applied afterwards would land on this copy only and never reach
// the checks, silently disabling whatever it guards (see #1785).
func (o *Options) applyDefaults() error {
	if o.ConfigDir == "" {
		dir, err := config.GetConfigDir()
		if err != nil {
			return fmt.Errorf("resolving agent-factory home: %w", err)
		}
		o.ConfigDir = dir
	}
	if o.TempDir == "" {
		o.TempDir = tempDirDefault()
	}
	if o.Exec == nil {
		o.Exec = cmd.MakeExecutor()
	}
	if o.MinTempHomeAge == 0 {
		o.MinTempHomeAge = 7 * 24 * time.Hour
	}
	if o.killGrace == 0 {
		o.killGrace = 2 * time.Second
	}
	if o.killTermWait == 0 {
		o.killTermWait = 2 * time.Second
	}
	if o.snapshot == nil {
		o.snapshot = proctree.Snapshot
	}
	if o.daemonHealth == nil {
		o.daemonHealth = daemon.Health
	}
	if o.autostartUnit == nil {
		o.autostartUnit = daemon.InspectAutostart
	}
	if o.autostartSupervision == nil {
		o.autostartSupervision = daemon.AutostartSupervision
	}
	if o.autostartServesHome == nil {
		o.autostartServesHome = daemon.AutostartUnitServesHome
	}
	if o.selfBinary == nil {
		o.selfBinary = resolvedSelfBinary
	}
	if o.binaryCandidates == nil {
		o.binaryCandidates = defaultBinaryCandidates
	}
	if o.binaryVersion == nil {
		o.binaryVersion = execBinaryVersion
	}
	return nil
}

// newScanContext defaults the options and seals them into the run's context.
// Every caller must go through here: scanContext holds its own copy of the
// Options, so this is the one place where defaulting is guaranteed to happen
// before that copy is taken.
func newScanContext(opts Options) (*scanContext, error) {
	if err := opts.applyDefaults(); err != nil {
		return nil, err
	}
	return &scanContext{opts: opts, selfAncestors: map[int]bool{}}, nil
}

// Run executes all checks and (when opts.Fix) applies the safe remediations.
// Past the constructor, ctx.opts is the only source of truth for the run's
// options — reading or mutating the `opts` argument here would diverge from
// what the checks actually see (#1785).
func Run(opts Options) (*Report, error) {
	ctx, err := newScanContext(opts)
	if err != nil {
		return nil, err
	}

	report := &Report{}
	if ctx.opts.Setup {
		checkSetup(ctx, report)
		return report, nil
	}

	cfg := checkConfigAndStorage(ctx, report)
	checkEnvironment(ctx, report, cfg)

	if snap, err := ctx.opts.snapshot(); err == nil {
		ctx.snap = snap
		for pid := range selfAndAncestors(snap) {
			ctx.selfAncestors[pid] = true
		}
	}

	// One health probe feeds every daemon check: each call dials the control
	// socket, and three checks asking the same daemon the same question could
	// disagree if a restart landed between them.
	health := ctx.opts.daemonHealth()
	checkDaemonHealth(ctx, report, health)
	checkDaemonVersionSkew(ctx, report, health)
	checkDuplicateDaemons(ctx, report)
	checkHTTPSocket(ctx, report, health)
	checkAutostartPath(ctx, report)
	checkSplitBrainBinaries(ctx, report)
	checkStaleSockets(ctx, report, health)
	checkAutostartSupervision(ctx, report)
	checkOrphanedProcesses(ctx, report)
	checkRunawayChildren(ctx, report)
	checkLeakedTmuxSessions(ctx, report)
	checkStaleTempHomes(ctx, report)
	checkForeignDaemons(ctx, report)
	checkRemoteSetup(ctx, report)

	if ctx.opts.Fix {
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
