package doctor

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/ui"
)

const (
	sectionEnvironment = "Environment"
	sectionConfig      = "Config & Storage"
	sectionDaemon      = "Daemon"
	sectionRemote      = "Remote Hooks"
	sectionProcesses   = "Session & Process Health"
)

var sectionOrder = []string{
	sectionEnvironment,
	sectionConfig,
	sectionDaemon,
	sectionRemote,
	sectionProcesses,
}

// AddHeader adds one top-level fact to the doctor heading.
func (r *Report) AddHeader(label, value string) {
	if value == "" {
		value = "unknown"
	}
	r.Header = append(r.Header, HeaderItem{Label: label, Value: value})
}

// AddCheck adds one grouped non-finding row. problem controls whether a
// non-pass row contributes to the process exit code.
func (r *Report) AddCheck(section, name string, status CheckStatus, detail, remediation string, problem bool) {
	if section == "" {
		section = sectionEnvironment
	}
	r.Checks = append(r.Checks, CheckResult{
		Section:     section,
		Name:        name,
		Status:      status,
		Detail:      detail,
		Remediation: remediation,
		Problem:     problem,
	})
	if status == StatusPass {
		r.OK = append(r.OK, fmt.Sprintf("%s: %s", name, detail))
	}
}

func (r *Report) Pass(section, name, detail string) {
	r.AddCheck(section, name, StatusPass, detail, "", false)
}

func (r *Report) Warn(section, name, detail, remediation string, problem bool) {
	r.AddCheck(section, name, StatusWarn, detail, remediation, problem)
}

func (r *Report) Fail(section, name, detail, remediation string) {
	r.AddCheck(section, name, StatusFail, detail, remediation, true)
}

// Render writes the human-readable report. fixMode changes remediation hints
// so a --fix run reports attempted outcomes rather than promising future work.
// verbose includes each process finding; the default collapses high-volume
// process classes into summary rows.
func Render(w io.Writer, r *Report, fixMode, verbose bool) {
	fmt.Fprintln(w, "Agent Factory Doctor")
	for _, item := range r.Header {
		fmt.Fprintf(w, "%s: %s\n", item.Label, item.Value)
	}

	rows := renderRows(r, fixMode, verbose)
	color := colorEnabled(w)
	for _, section := range orderedSections(rows) {
		fmt.Fprintf(w, "\n%s\n", section)
		for _, row := range rows {
			if row.section != section {
				continue
			}
			fmt.Fprintf(w, "  %s %-24s %s\n", renderStatus(row.status, color), row.name+":", row.detail)
			if row.remediation != "" && row.status != StatusPass && row.status != StatusFixed {
				fmt.Fprintf(w, "        fix: %s\n", row.remediation)
			}
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, summaryLine(r, rows, fixMode))
}

// renderStatus renders the status column, padded to a fixed width so check
// names stay aligned down the page. The padding goes inside the styled span:
// ANSI codes have no display width, but %-5s would count them, so padding a
// pre-colored string is what breaks the column.
func renderStatus(s CheckStatus, color bool) string {
	text := fmt.Sprintf("%-5s", string(s))
	if !color {
		return text
	}
	theme := ui.CurrentTheme()
	var c lipgloss.Color
	switch s {
	case StatusPass, StatusFixed:
		c = theme.Success
	case StatusWarn:
		c = theme.Warning
	case StatusFail:
		c = theme.Error
	default:
		return text
	}
	return lipgloss.NewStyle().Foreground(c).Render(text)
}

// colorEnabled reports whether w is a terminal that should receive color.
// Deciding from the writer — rather than from the ambient color profile — is
// what keeps `af doctor > file`, `| grep`, and the tests byte-identical and
// free of escape codes, while an interactive run still gets color.
func colorEnabled(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

type renderRow struct {
	section     string
	name        string
	status      CheckStatus
	detail      string
	remediation string
	// actionable reports whether this row requires the user to do something —
	// the same judgement that drives the nonzero exit. It is NOT "status is not
	// PASS": doctor emits advisory WARNs (no autostart unit installed, a legacy
	// config that still loads) that carry a helpful remedy while leaving the run
	// healthy. Without this, a script cannot tell those from a WARN that means
	// the daemon is broken (#1044).
	actionable bool
}

func renderRows(r *Report, fixMode, verbose bool) []renderRow {
	rows := make([]renderRow, 0, len(r.Checks)+len(r.Findings)+len(r.OK))
	for _, c := range r.Checks {
		rows = append(rows, renderRow{
			section:     c.Section,
			name:        c.Name,
			status:      c.Status,
			detail:      c.Detail,
			remediation: c.Remediation,
			// Mirrors UnresolvedCount's rule for checks, so "actionable" and the
			// exit code can never disagree.
			actionable: c.Problem && c.Status != StatusPass && c.Status != StatusFixed,
		})
	}
	if len(r.Checks) == 0 {
		for _, ok := range r.OK {
			rows = append(rows, renderRow{
				section: sectionEnvironment,
				name:    "ok",
				status:  StatusPass,
				detail:  ok,
			})
		}
	}
	if verbose {
		for _, f := range r.Findings {
			rows = append(rows, findingRow(f, fixMode))
		}
		return rows
	}

	collapsed := collapseProcessFindings(r.Findings, fixMode)
	for _, f := range r.Findings {
		if collapsibleFinding(f.Check) {
			continue
		}
		rows = append(rows, findingRow(f, fixMode))
	}
	rows = append(rows, collapsed...)
	return rows
}

func collapsibleFinding(check string) bool {
	switch check {
	case "orphaned-process", "escaped-process", "possible-orphan", "runaway-cpu":
		return true
	default:
		return false
	}
}

func collapseProcessFindings(findings []Finding, fixMode bool) []renderRow {
	byCheck := map[string][]Finding{}
	for _, f := range findings {
		if collapsibleFinding(f.Check) {
			byCheck[f.Check] = append(byCheck[f.Check], f)
		}
	}
	var rows []renderRow
	for _, check := range []string{"orphaned-process", "escaped-process", "possible-orphan", "runaway-cpu"} {
		group := byCheck[check]
		if len(group) == 0 {
			continue
		}
		rows = append(rows, collapsedProcessRow(check, group, fixMode))
	}
	return rows
}

func collapsedProcessRow(check string, findings []Finding, fixMode bool) renderRow {
	total := collapsedCount(findings)
	fixable := 0
	fixed := 0
	failed := 0
	for _, f := range findings {
		if f.Fixed {
			fixed++
		}
		if f.FixErr != nil {
			failed++
		}
		if f.FixAction != "" && !f.Fixed {
			fixable++
		}
	}

	status := StatusWarn
	if fixable > 0 || failed > 0 {
		status = StatusFail
	}
	if fixed == len(findings) && failed == 0 {
		status = StatusFixed
	}

	return renderRow{
		section:     sectionProcesses,
		name:        collapsedProcessName(check),
		status:      status,
		detail:      collapsedProcessDetail(check, total, fixable, fixed, failed, fixMode),
		remediation: collapsedProcessRemediation(check, fixable, fixMode),
		// One row standing in for several findings is actionable while any of
		// them is still unresolved.
		actionable: fixed < len(findings),
	}
}

func collapsedProcessName(check string) string {
	switch check {
	case "orphaned-process":
		return "orphaned-processes"
	case "escaped-process":
		return "escaped-processes"
	case "possible-orphan":
		return "possible-orphans"
	case "runaway-cpu":
		return "runaway-cpu"
	default:
		return check
	}
}

func collapsedProcessDetail(check string, total, fixable, fixed, failed int, fixMode bool) string {
	switch check {
	case "orphaned-process":
		parts := []string{fmt.Sprintf("%s from dead sessions", plural(total, "orphaned process", "orphaned processes"))}
		if fixable > 0 {
			parts = append(parts, fmt.Sprintf("%d safe to clean", fixable))
		}
		if fixMode && fixed > 0 {
			parts = append(parts, fmt.Sprintf("%d fixed", fixed))
		}
		if failed > 0 {
			parts = append(parts, fmt.Sprintf("%d fix failed", failed))
		}
		return strings.Join(parts, ", ")
	case "escaped-process":
		return fmt.Sprintf("%s escaped live session pane trees", plural(total, "process", "processes"))
	case "possible-orphan":
		return fmt.Sprintf("%s belong to dead tmux servers without agent-factory markers", plural(total, "process", "processes"))
	case "runaway-cpu":
		return fmt.Sprintf("%s averaged a pegged CPU core inside live sessions", plural(total, "process", "processes"))
	default:
		return fmt.Sprintf("%s reported", plural(total, "finding", "findings"))
	}
}

func collapsedProcessRemediation(check string, fixable int, fixMode bool) string {
	switch check {
	case "orphaned-process":
		if fixable > 0 && !fixMode {
			return "run `af doctor --fix` to clean the safe processes; rerun with `--verbose` for details"
		}
		return "rerun with `--verbose` for per-process details; inspect report-only processes manually"
	case "escaped-process":
		return "rerun with `--verbose` for details; inspect the live session or stop the process manually"
	case "possible-orphan":
		return "rerun with `--verbose` for details; verify ownership before killing anything manually"
	case "runaway-cpu":
		return "rerun with `--verbose` for details; inspect the live session before stopping processes"
	default:
		return "rerun with `--verbose` for details"
	}
}

func collapsedCount(findings []Finding) int {
	total := 0
	for _, f := range findings {
		if n, ok := summarizedMoreCount(f.Detail); ok {
			total += n
			continue
		}
		total++
	}
	return total
}

// countUnresolvedFindings counts the TRUE number of underlying issues the
// un-remediated findings represent. It mirrors collapsedCount's expansion of a
// "… and N more" summary into N, so the total the summary reports and the
// per-check counts a reader sees are the same arithmetic — 1 + 15 + 62, not
// 1 + 15 + 16 (#1979). Without this the summary's total counts collapsed ROWS
// while the per-check details count PROCESSES, and the two never add up.
func countUnresolvedFindings(findings []Finding) int {
	n := 0
	for _, f := range findings {
		if f.Fixed {
			continue
		}
		if c, ok := summarizedMoreCount(f.Detail); ok {
			n += c
			continue
		}
		n++
	}
	return n
}

// summarizedMoreCount reads N back out of a "… and N more …" roll-up detail —
// the one finding that stands for several (see checkOrphanedProcesses' cap).
//
// It is ANCHORED to the "… and N more" prefix that roll-up is written with, not
// matched anywhere in the string, because a finding's detail can embed a
// process's cmdline (describeProc) and a command line containing the words "and
// 5 more" would otherwise be read as a roll-up of five. That miscount now
// reaches the exit-code-driving total (#1979), so the match has to be precise
// rather than opportunistic.
func summarizedMoreCount(detail string) (int, bool) {
	fields := strings.Fields(detail)
	if len(fields) < 4 || fields[0] != "…" || fields[1] != "and" || fields[3] != "more" {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(fields[2], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

func plural(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func findingRow(f Finding, fixMode bool) renderRow {
	status := findingStatus(f)
	detail := f.Detail
	if f.FixErr != nil {
		detail = fmt.Sprintf("%s (fix failed: %v)", detail, f.FixErr)
	}
	return renderRow{
		section:     findingSection(f),
		name:        f.Check,
		status:      status,
		detail:      detail,
		remediation: findingRemediation(f, fixMode),
		// Mirrors UnresolvedCount's rule for findings: every un-remediated
		// finding is a real problem — findings are only ever raised for those.
		actionable: !f.Fixed,
	}
}

func findingStatus(f Finding) CheckStatus {
	switch {
	case f.Fixed:
		return StatusFixed
	case f.FixErr != nil:
		return StatusFail
	case f.Severity != "":
		return f.Severity
	case f.FixAction != "":
		return StatusFail
	default:
		return StatusWarn
	}
}

func findingSection(f Finding) string {
	if f.Section != "" {
		return f.Section
	}
	switch f.Check {
	case "daemon-health":
		return sectionDaemon
	case "remote-config", "remote-hook-script", "remote-connectivity":
		return sectionRemote
	case "config", "af-home", "state-dir", "repo-state-dir", "log-storage", "git-repo", "worktree-root":
		return sectionConfig
	case "git", "git-identity", "tmux", "agent-program":
		return sectionEnvironment
	default:
		return sectionProcesses
	}
}

func findingRemediation(f Finding, fixMode bool) string {
	switch {
	case f.Fixed:
		return ""
	case f.FixErr != nil:
		if f.FixAction != "" {
			return fmt.Sprintf("resolve the error, then rerun `af doctor --fix` to %s", f.FixAction)
		}
		return "resolve the reported error, then rerun `af doctor`"
	case f.Remediation != "":
		return f.Remediation
	case f.FixAction != "" && !fixMode:
		return fmt.Sprintf("run `af doctor --fix` to %s", f.FixAction)
	}
	switch f.Check {
	case "escaped-process", "runaway-cpu":
		return "inspect the live session or stop the process manually"
	case "possible-orphan":
		return "verify ownership, then kill the process manually if stale"
	case "orphaned-process":
		return "run `af doctor` with the owning AGENT_FACTORY_HOME active, or inspect manually"
	case "leaked-tmux-session":
		return "verify ownership, then run the tmux kill command shown in the message"
	case "foreign-daemon":
		return "verify the other AGENT_FACTORY_HOME is intentional, then stop that daemon if stale"
	case "daemon-health":
		return "run `af daemon restart`; if the socket is stale, remove it after verifying no daemon is running"
	case "remote-config":
		return "edit the repo .agent-factory config and rerun `af doctor`"
	case "remote-hook-script":
		return "fix the hook path or executable bit and rerun `af doctor`"
	case "remote-connectivity":
		return "run the listed hook command by hand and fix the remote connection"
	case "agent-program":
		return "install the agent binary or set the matching program_overrides entry"
	default:
		return "inspect the message above and rerun `af doctor`"
	}
}

func orderedSections(rows []renderRow) []string {
	present := map[string]bool{}
	for _, row := range rows {
		present[row.section] = true
	}
	var out []string
	for _, section := range sectionOrder {
		if present[section] {
			out = append(out, section)
			delete(present, section)
		}
	}
	for _, section := range sortedKeys(present) {
		out = append(out, section)
	}
	return out
}

// summaryLine LEADS with the actionable count, then gives the status-row
// breakdown (#1979). A reader who stops at "0 FAIL" must not conclude "healthy"
// while `af doctor` exits nonzero: the exit code keys on the actionable count
// (UnresolvedCount), so that count is the headline and the exit code cannot
// contradict it. The PASS/WARN/FAIL tally is a different axis — it counts ROWS,
// not issues, which is why it trails in parentheses and why one collapsed WARN
// row can stand for many issues.
func summaryLine(r *Report, rows []renderRow, fixMode bool) string {
	counts := map[CheckStatus]int{}
	for _, row := range rows {
		counts[row.status]++
	}
	breakdown := []string{
		fmt.Sprintf("%d PASS", counts[StatusPass]),
		fmt.Sprintf("%d WARN", counts[StatusWarn]),
		fmt.Sprintf("%d FAIL", counts[StatusFail]),
	}
	if counts[StatusFixed] > 0 {
		breakdown = append(breakdown, fmt.Sprintf("%d FIXED", counts[StatusFixed]))
	}

	unresolved := r.UnresolvedCount()
	summary := fmt.Sprintf("Summary: %s (%s).", issuePhrase(unresolved), strings.Join(breakdown, ", "))
	switch {
	case !fixMode && fixableCount(r) > 0:
		summary = strings.TrimSuffix(summary, ".") +
			fmt.Sprintf("; %d fixable with `af doctor --fix`.", fixableCount(r))
	case fixMode && fixedCount(r) > 0:
		summary = strings.TrimSuffix(summary, ".") +
			fmt.Sprintf("; %d fixed.", fixedCount(r))
	}
	return summary
}

func issuePhrase(n int) string {
	if n == 0 {
		return "no issues require action"
	}
	if n == 1 {
		return "1 issue requires action"
	}
	return fmt.Sprintf("%d issues require action", n)
}

func fixableCount(r *Report) int {
	n := 0
	for _, f := range r.Findings {
		if f.FixAction != "" && !f.Fixed {
			n++
		}
	}
	return n
}

func fixedCount(r *Report) int {
	n := 0
	for _, f := range r.Findings {
		if f.Fixed {
			n++
		}
	}
	return n
}
