package doctor

import (
	"fmt"
	"io"
	"strings"
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
	for _, section := range orderedSections(rows) {
		fmt.Fprintf(w, "\n%s\n", section)
		for _, row := range rows {
			if row.section != section {
				continue
			}
			fmt.Fprintf(w, "  %-5s %-24s %s\n", row.status, row.name+":", row.detail)
			if row.remediation != "" && row.status != StatusPass && row.status != StatusFixed {
				fmt.Fprintf(w, "        fix: %s\n", row.remediation)
			}
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, summaryLine(r, rows, fixMode))
}

type renderRow struct {
	section     string
	name        string
	status      CheckStatus
	detail      string
	remediation string
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

func summarizedMoreCount(detail string) (int, bool) {
	fields := strings.Fields(detail)
	for i := 0; i+2 < len(fields); i++ {
		if fields[i] != "and" || fields[i+2] != "more" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(fields[i+1], "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
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

func summaryLine(r *Report, rows []renderRow, fixMode bool) string {
	counts := map[CheckStatus]int{}
	for _, row := range rows {
		counts[row.status]++
	}
	parts := []string{
		fmt.Sprintf("%d PASS", counts[StatusPass]),
		fmt.Sprintf("%d WARN", counts[StatusWarn]),
		fmt.Sprintf("%d FAIL", counts[StatusFail]),
	}
	if counts[StatusFixed] > 0 {
		parts = append(parts, fmt.Sprintf("%d FIXED", counts[StatusFixed]))
	}

	unresolved := r.UnresolvedCount()
	summary := fmt.Sprintf("Summary: %s; %s.", strings.Join(parts, ", "), issuePhrase(unresolved))
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
		return "1 underlying issue requires action"
	}
	return fmt.Sprintf("%d underlying issues require action", n)
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
