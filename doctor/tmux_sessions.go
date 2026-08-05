package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Everything `af doctor` learns from the tmux SERVER, and the one rule that
// governs all of it: A FAILED READ IS NOT AN EMPTY RESULT.
//
// Split out of checks.go for the file-length lint, but the grouping is the real
// point (#2874). Four checks reason from this data by treating an ABSENCE as
// evidence — a session missing from the list is dead, a home no session names is
// abandoned, a session in no record leaked — and two of them spend that evidence
// on a kill or an rm -rf. Every read here is therefore three-valued, and a
// consumer that cannot get an answer does nothing at all.

// listTmuxSessions returns every session name on the current tmux server, or an
// error when tmux could not tell us.
//
// THE INVARIANT, stated rather than borrowed: A FAILED READ IS NOT AN EMPTY
// RESULT. Every consumer below treats a name's ABSENCE as evidence the session
// is dead, and two of them spend that evidence on a kill or an rm -rf.
//
// It DELEGATES rather than shelling out — the second read in this file to do so,
// after tmuxSessionHomeMarker, and for the same reason. The copy that lived here
// fell outside both invariants session/tmux maintains:
//
//   - the classification of tmux's ambiguous exit 1, which it got wrong: every
//     failure became an empty list, an unreachable socket read as "no sessions
//     are live", and checkOrphanedProcesses turned that into an armed
//     `kill pid N` for every live session's processes (#2874);
//   - the tmuxCommandTimeout BOUND, which it lacked entirely. `af doctor --fix`
//     re-lists before removing a stale home, so against a server that wedged
//     mid-run an unbounded listing hangs the cleanup instead of refusing the
//     removal it can no longer justify (#2910).
//
// Do not restore a "mirrors X" comment in either direction, and do not
// re-inline the shell-out. State the property; keep the mechanism in one place.
func listTmuxSessions(ctx *scanContext) ([]string, error) {
	return tmux.ListSessionNames(ctx.opts.Exec)
}

// tmuxSessions returns the run's memoized session listing, or the error that
// made it unavailable. A caller that treats a missing name as proof the session
// is DEAD must handle the error and do nothing; see the scanContext field
// comments. The doctor run is sequential, so a plain memo is enough (mirrors
// daemonProcs).
func (c *scanContext) tmuxSessions() ([]string, error) {
	if !c.tmuxScanned {
		c.tmuxNames, c.tmuxErr = listTmuxSessions(c)
		c.tmuxScanned = true
	}
	return c.tmuxNames, c.tmuxErr
}

// checkTmuxInspection turns the run's session listing into a report row, and is
// the ONE place its failure is announced — the checks that consume the list then
// stay silent rather than each repeating it, exactly as they do for
// checkProcessInspection and an unreadable process table (#1939).
//
// It must run BEFORE those checks, or the operator reads their silence before
// learning why they were silent.
func checkTmuxInspection(ctx *scanContext, report *Report) {
	names, err := ctx.tmuxSessions()
	if err == nil {
		report.Pass(sectionProcesses, "tmux-inspection",
			fmt.Sprintf("listed the tmux server's sessions (%d)", len(names)))
		return
	}
	report.Fail(sectionProcesses, "tmux-inspection",
		fmt.Sprintf("cannot list tmux sessions, so the orphaned-process, escaped-process, runaway-child, "+
			"leaked-session and stale-temp-home checks below could not establish which sessions are live: %v", err),
		"those checks are UNKNOWN, not clean — nothing was reported as orphaned or abandoned and `--fix` "+
			"proposed no kill or removal derived from session liveness on this run. Restore access to the "+
			"tmux server and re-run")
}

// checkRunawayChildren reports (never kills) descendants of live af_
// sessions that have averaged a pegged core for an extended period.
func checkRunawayChildren(ctx *scanContext, report *Report) {
	// See checkOrphanedProcesses: blindness is reported once — by
	// checkProcessInspection for the table, checkTmuxInspection for the session
	// list — rather than swallowed here. This check is report-only, so an
	// unavailable listing costs an under-report rather than a wrong action, but
	// an under-report that renders as "no runaway processes" is the very thing
	// this package refuses to print.
	names, listErr := ctx.tmuxSessions()
	if ctx.snap == nil || listErr != nil {
		return
	}
	unmeasurable := 0
	for _, name := range names {
		if !strings.HasPrefix(name, tmux.TmuxPrefix) {
			continue
		}
		procs := tmux.SessionProcessTrees(ctx.opts.Exec, name)
		sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
		for _, p := range procs {
			if ctx.selfAncestors[p.PID] {
				continue
			}
			frac, age, err := proctree.CPUFraction(p)
			if errors.Is(err, proctree.ErrCPUUnknown) {
				// Counted, not swallowed. A check that cannot answer must say
				// so: silently skipping every process would render "no runaway
				// processes" — the exact shape of the report this package
				// exists to stop printing. Reported once below rather than per
				// process, since the cause is usually systemic (a subset=pid
				// procfs hides /proc/uptime, so NO process has an age).
				unmeasurable++
				continue
			}
			if err != nil || frac < runawayCPUFraction || age < runawayMinAge.Seconds() {
				continue
			}
			report.addAdvisoryFinding(Finding{
				Check: "runaway-cpu",
				Detail: fmt.Sprintf("%s in live session %s has averaged a pegged core — "+
					"check the session; doctor never kills children of live sessions", describeProc(p), name),
			})
		}
	}
	if unmeasurable > 0 {
		report.Warn(sectionProcesses, "runaway-cpu",
			fmt.Sprintf("could not measure CPU for %s in live sessions, so this check reports nothing "+
				"about them", plural(unmeasurable, "process", "processes")),
			"a process pegging a core would not be spotted here; inspect the sessions yourself", false)
	}
}

// checkLeakedTmuxSessions reports af_ tmux sessions with no backing record
// in this home's storage. Report-only even under --fix: a session with no
// record here may be owned by another agent-factory home on the same tmux
// server, and killing someone else's live session is worse than a leak.
func checkLeakedTmuxSessions(ctx *scanContext, report *Report) {
	// This check reads TWO surfaces and calls a session leaked on the ABSENCE of
	// evidence in both, so neither may answer "absent" when it merely failed.
	// The session list is reported once by checkTmuxInspection; the record store
	// is this check's own input, so its failure is reported here.
	//
	// No --fix closure is armed either way, which is not the same as harmless:
	// every row prints a `tmux kill-session` command for the operator to run, so
	// an unreadable record store hands them a kill command for each of their
	// healthy sessions (#2874).
	names, listErr := ctx.tmuxSessions()
	if listErr != nil {
		return
	}
	recorded, recordErr := recordedTmuxNames()
	if recordErr != nil {
		report.Fail(sectionProcesses, "session-records",
			fmt.Sprintf("cannot read this home's session records, so no tmux session can be classified as "+
				"leaked: %v", recordErr),
			"the leaked-session check is UNKNOWN, not clean — repair or remove the unreadable records under "+
				"<AF_HOME>/instances and re-run")
		return
	}
	report.Pass(sectionProcesses, "session-records",
		fmt.Sprintf("read this home's session records (%d recorded tmux name(s))", len(recorded)))
	var leaked []string
	for _, name := range names {
		if strings.HasPrefix(name, tmux.TmuxPrefix) && !recorded[name] {
			leaked = append(leaked, name)
		}
	}
	sort.Strings(leaked)
	for _, name := range leaked {
		origin := "no ancestry marker"
		ownedByActiveHome := false
		switch home, present, markerErr := tmuxSessionHomeMarker(ctx, name); {
		case markerErr != nil:
			// Unanswered, not unmarked. Stays advisory below — the actionable
			// row asserts this install created the session, and we do not know
			// that (#2874).
			origin = fmt.Sprintf("ownership could not be read: %v", markerErr)
		case present && home != "" && filepath.Clean(home) == filepath.Clean(ctx.opts.ConfigDir):
			origin = "created by this install"
			ownedByActiveHome = true
		case present && home != "":
			origin = "created by another agent-factory home: " + home
		}
		finding := Finding{
			Check: "leaked-tmux-session",
			Detail: fmt.Sprintf("tmux session %s has no backing record in %s (%s); "+
				"kill it with: %s", name, ctx.opts.ConfigDir, origin,
				// "=name:" is tmux's exact-match target syntax — one argument, so
				// it is one piece the seam quotes as a whole.
				shellsuggest.Command("tmux", "kill-session", "-t", "="+name+":")),
		}
		if ownedByActiveHome {
			// This install's marker plus no record is a proven leak with an
			// exact manual remedy. --fix support is not required for a row to
			// be actionable.
			report.addActionableFinding(finding)
		} else {
			// A shared tmux server can expose another install's healthy session,
			// and a pre-marker session has unknown ownership.
			report.addAdvisoryFinding(finding)
		}
	}
}

// recordedTmuxNames loads every persisted tmux session name (agent + tabs)
// from this home's storage, read-only. Legacy records without an explicit
// TmuxName fall back to the derived repo-scoped name.
// recordedTmuxNames returns every tmux session name this home's records claim,
// or an error when the record store could not be read.
//
// The error is not optional detail: the only consumer decides a session is
// LEAKED by its absence from this set, so a read failure that returned an empty
// set would indict every live session (#2874).
//
// It takes no configDir: doctor may be pointed at a ConfigDir other than the
// ambient one only in tests, which also set AGENT_FACTORY_HOME, and
// LoadAllRepoInstances always reads the ambient home.
func recordedTmuxNames() (map[string]bool, error) {
	names := map[string]bool{}
	all, skipped, err := config.LoadAllRepoInstancesReportingSkips()
	if err != nil {
		return nil, err
	}
	// A repo the loader could not read contributes no names, and every one of
	// its live sessions would then look leaked. Only the ReportingSkips form
	// makes those repos visible at all — the plain loader drops them silently.
	if len(skipped) > 0 {
		return nil, fmt.Errorf("session records for %s could not be read", strings.Join(skipped, ", "))
	}
	type tabRec struct {
		TmuxName string `json:"tmux_name"`
	}
	type instRec struct {
		Title    string   `json:"title"`
		Path     string   `json:"path"`
		TmuxName string   `json:"tmux_name"`
		Tabs     []tabRec `json:"tabs"`
	}
	for repoID, raw := range all {
		var instances []instRec
		if err := json.Unmarshal(raw, &instances); err != nil {
			// Same rule one level down: a repo whose records will not parse
			// names no sessions, so continuing here would indict every session
			// it owns.
			return nil, fmt.Errorf("session records for %s could not be parsed: %w", repoID, err)
		}
		for _, inst := range instances {
			if inst.TmuxName != "" {
				names[inst.TmuxName] = true
			} else if inst.Title != "" {
				names[tmux.NewTmuxSessionForRepo(inst.Title, inst.Path, "").SanitizedName()] = true
			}
			for _, tab := range inst.Tabs {
				if tab.TmuxName != "" {
					names[tab.TmuxName] = true
				}
			}
		}
	}
	return names, nil
}

// liveTmuxHomes returns the AF homes that live tmux sessions claim, or an error
// when that set cannot be established.
//
// The error matters more here than anywhere else in this file: this set is the
// ONLY guard between a temp home with live tmux sessions and an rm -rf, because
// such a home holds no daemon lock for the authoritative probe to find. An
// incomplete set is indistinguishable from an empty one at the call site, so
// "incomplete" has to be an error — both when the session list is unavailable
// and when a listed session's ownership marker cannot be read.
func liveTmuxHomes(ctx *scanContext) (map[string]bool, error) {
	names, err := ctx.tmuxSessions()
	if err != nil {
		return nil, err
	}
	return tmuxHomesFor(ctx, names)
}

// liveTmuxHomesNow is liveTmuxHomes against a FRESH listing rather than the
// run's memo, for the fix-time recheck.
//
// The memo is right for detection — one run, one view. It is wrong for the
// recheck, and the difference is the whole point of rechecking: findings are
// applied after detection, so the session that must veto an rm -rf is precisely
// the one that STARTED in that window, and it cannot appear in a listing taken
// before the window opened. Re-reading markers for the old list would miss it.
func liveTmuxHomesNow(ctx *scanContext) (map[string]bool, error) {
	names, err := listTmuxSessions(ctx)
	if err != nil {
		return nil, err
	}
	return tmuxHomesFor(ctx, names)
}

func tmuxHomesFor(ctx *scanContext, names []string) (map[string]bool, error) {
	homes := map[string]bool{}
	var unreadable []string
	for _, name := range names {
		if !strings.HasPrefix(name, tmux.TmuxPrefix) {
			continue
		}
		home, present, markerErr := tmuxSessionHomeMarker(ctx, name)
		if markerErr != nil {
			// tmux did not answer for this session, so it cannot be ruled OUT as
			// the claimant of any home. A session that answered and carries no
			// marker (present=false, nil error) claims nothing and is skipped —
			// collapsing those two is the bug this function exists to avoid.
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", name, markerErr))
			continue
		}
		if present && home != "" {
			homes[filepath.Clean(home)] = true
		}
	}
	if len(unreadable) > 0 {
		return nil, fmt.Errorf("live tmux session(s) would not report which AF home they belong to: %s",
			strings.Join(unreadable, "; "))
	}
	return homes, nil
}

// tmuxSessionHomeMarker asks which AF home a session belongs to, three-valued:
// (home, true, nil) it said so, ("", false, nil) it answered and claims none,
// and a non-nil error when it did not answer.
//
// It delegates to session/tmux rather than shelling out here. The version it
// replaces was `if err != nil { return "", false }`, which reported an
// unanswered query as a session that claims no home — and doctor treats "claims
// no home" as clearance to rm -rf that home (#2874). The shared probe also
// carries the timeout bound and the multiline-forgery guard this one lacked.
func tmuxSessionHomeMarker(ctx *scanContext, name string) (string, bool, error) {
	return tmux.SessionHomeMarker(ctx.opts.Exec, name)
}
