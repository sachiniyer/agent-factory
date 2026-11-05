package probe

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Codex probe, argv and model. Every word is a fixed literal: nothing
// the caller says — not the account name, not a path — can reach the command
// line of a process that runs under credentials.
//
// The least-privilege set (codex exec --help, CLI 0.163):
//
//   - --ignore-user-config: the account's config.toml never loads, so an
//     operator's sandbox_mode = "danger-full-access", approval_policy,
//     mcp_servers, hooks, notify, or model can never reach the run. Auth
//     still resolves from CODEX_HOME — that is the credential the probe
//     exists to exercise.
//   - --ignore-rules: no user or project execpolicy .rules file contributes
//     permission grants.
//   - --ephemeral: the rollout is never persisted under the account's
//     sessions/ — the probe leaves no resumable transcript behind.
//   - -s read-only: model-generated shell calls get no write authority
//     anywhere; combined with --ignore-user-config no config layer can
//     widen it.
//   - -c approval_policy="never": no tool call can ever be approved —
//     nothing in this run may pause for, or receive, a grant.
//   - -c mcp_servers={} / notify=[] / check_for_update_on_startup=false:
//     no MCP servers from any config layer, no notify program exec, no
//     phone-home update check.
//   - --disable <feature> for every tool-authority surface the CLI offers
//     a fixed literal for: hooks, plugins (incl. remote/sharing), apps,
//     multi-agent spawn, skill search + MCP dependency install, tool
//     suggest/elicitation, sleep, browser/computer/image/view, realtime
//     conversation, in-app browser, worktrees, workspace deps, goals,
//     daemon auto-start, memories.
//   - --skip-git-repo-check: the private run dir is not a repo; the flag
//     keeps the refusal ours, not the CLI's.
//
// What the CLI offers no fixed literal to remove is disclosed instead:
// the account's own $CODEX_HOME/skills may still be discovered (no flag
// disables host skill discovery in 0.163), and shell_tool stays enabled
// — a bare "reply ok" turn needs no tool call, but disabling the tool
// risks the turn machinery itself; read-only sandbox plus approval never
// is the authority bound either way.
//
// --output-last-message names the file the CLI writes its final message to.
// The name is a bare literal: it resolves inside the per-run cwd, so the
// file is the probe's private success-token channel and the argv stays
// constant. If the CLI resolves it against some other root the file simply
// is not found — a miss reads as Unknown, never as health.
const codexProbeModel = "gpt-5.1-codex-max"

var codexDisabledFeatures = []string{
	"apps",
	"browser_use",
	"browser_use_external",
	"browser_use_full_cdp_access",
	"computer_use",
	"daemon_auto_start",
	"goals",
	"hooks",
	"image_generation",
	"in_app_browser",
	"memories",
	"multi_agent",
	"plugin_sharing",
	"plugins",
	"realtime_conversation",
	"recommended_plugins",
	"remote_plugin",
	"skill_mcp_dependency_install",
	"skill_search",
	"sleep_tool",
	"tool_call_mcp_elicitation",
	"tool_suggest",
	"view_image",
	"worktrees",
	"workspace_dependencies",
}

var codexProbeArgv = buildCodexProbeArgv()

func buildCodexProbeArgv() []string {
	argv := []string{
		"codex", "exec",
		"--skip-git-repo-check",
		"--ignore-user-config",
		"--ignore-rules",
		"--ephemeral",
		"-s", "read-only",
		"-m", codexProbeModel,
		"-c", `model_reasoning_effort="low"`,
		"-c", `approval_policy="never"`,
		"-c", `mcp_servers={}`,
		"-c", `notify=[]`,
		"-c", `check_for_update_on_startup=false`,
	}
	for _, feature := range codexDisabledFeatures {
		argv = append(argv, "--disable", feature)
	}
	return append(argv,
		"--output-last-message", codexMessageFile,
		"Reply with exactly the single word: ok",
	)
}

const (
	codexMessageFile = "last-message.txt"
	codexSuccessText = "ok"
)

var codexProbeSpec = agentProbeSpec{
	agent:       "codex",
	model:       codexProbeModel,
	argv:        codexProbeArgv,
	messageFile: codexMessageFile,
	successText: codexSuccessText,
	wall:        codexLimit,
}

var (
	// codexWallDetect is the same banner text the pane detector keys on
	// (task/limit.go codexLimitDetect), matched here WITHOUT that file's
	// line anchor. The anchor is pane evidence — it proves the provider
	// rendered the words rather than a user or agent quoting them on a
	// shared screen — and it answers a question exec output does not pose:
	// every byte in the captured tail came from the CLI itself, so "ERROR:
	// " prefixes and log framing are legitimate leads. The full leading
	// sentence is still required, for the same reason it is there: prose
	// quoting "hit your usage limit" is not a wall.
	codexWallDetect = regexp.MustCompile(`You['’]ve hit your usage limit`)

	// A banner that continues "for <model>" AND offers "Switch to another
	// model" names a per-model quota — the account still serves other
	// models, so it is NOT an account wall. Both markers are required:
	// "for <plan>" phrasing alone can introduce an account-scoped limit
	// ("usage limit for your workspace"), and the switch suggestion alone
	// could appear in unrelated guidance. Only the pair is model-scoped.
	codexModelLimitFor    = regexp.MustCompile(`(?i)usage limit for\b`)
	codexModelLimitSwitch = regexp.MustCompile(`(?i)switch to another model`)

	// codexResetAt / codexResetIn / the parse helpers below are the codex
	// reset grammar, ported verbatim from task/limit.go's parseCodexReset
	// and its helpers. They live here rather than in a shared import for
	// two reasons: task's public entry point is line-anchored pane matching
	// (it would drop a log-prefixed banner, above), and task imports
	// session/tmux — a pane package this leaf must not pull in. Keep the
	// grammars aligned: both parse the same provider text.
	codexResetAt = regexp.MustCompile(`(?is)try again at\s+(.+?[ap]m)\b`)
	codexResetIn = regexp.MustCompile(`(?is)try again in\s+([0-9a-z\s]+?)\.`)

	relDays  = regexp.MustCompile(`(?i)(\d+)\s*day`)
	relHours = regexp.MustCompile(`(?i)(\d+)\s*hour`)
	relMins  = regexp.MustCompile(`(?i)(\d+)\s*min`)

	clockTime     = regexp.MustCompile(`(?i)\b(\d{1,2})(?::(\d{2}))?\s*([ap])m\b`)
	ordinalSuffix = regexp.MustCompile(`(?i)(\d{1,2})(st|nd|rd|th)`)
)

// limitVerdict is the three-way read of the captured tail: no limit, a
// model-scoped quota (evidence, not a wall), or an account-wide wall.
type limitVerdict int

const (
	limitNone limitVerdict = iota
	limitModelWall
	limitAccountWall
)

// codexLimit classifies the tail: a model-scoped limit reports limitModelWall
// (and its reset when parseable — still evidence about that model), an
// account wall reports limitAccountWall with its reset, and everything else
// reports limitNone.
func codexLimit(output []byte, now time.Time) (time.Time, limitVerdict) {
	loc := codexWallDetect.FindIndex(output)
	if loc == nil {
		return time.Time{}, limitNone
	}
	tail := string(output[loc[0]:])
	if codexModelLimitFor.MatchString(tail) && codexModelLimitSwitch.MatchString(tail) {
		return parseCodexReset(tail, now), limitModelWall
	}
	return parseCodexReset(tail, now), limitAccountWall
}

// parseCodexReset extracts the reset phrase — "try again at <ts>" preferred,
// then "try again in <duration>" against now. Absent or unparseable yields
// the zero time; the verdict itself already stands.
func parseCodexReset(tail string, now time.Time) time.Time {
	if m := codexResetAt.FindStringSubmatch(tail); m != nil {
		if t, ok := parseCodexAbsolute(strings.TrimSpace(m[1]), now); ok {
			return t
		}
	}
	if m := codexResetIn.FindStringSubmatch(tail); m != nil {
		if d, ok := parseRelativeDuration(m[1]); ok {
			return now.Add(d)
		}
	}
	return time.Time{}
}

// parseCodexAbsolute parses a "try again at" timestamp in now's location —
// the weekly "Jul 25, 2026 5:55 PM" form first, then the time-only form,
// whose next occurrence after now is minted.
func parseCodexAbsolute(ts string, now time.Time) (time.Time, bool) {
	loc := now.Location()
	cleaned := ordinalSuffix.ReplaceAllString(ts, "$1")
	if t, err := time.ParseInLocation("Jan 2, 2006 3:04 PM", cleaned, loc); err == nil {
		return t, true
	}
	if hour, minute, ok := parseClockTime(cleaned); ok {
		return nextTimeOfDay(now, loc, hour, minute), true
	}
	return time.Time{}, false
}

// parseRelativeDuration sums the day/hour/minute components of a countdown
// like "4 days 2 hours 46 minutes".
func parseRelativeDuration(s string) (time.Duration, bool) {
	var d time.Duration
	found := false
	if m := relDays.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * 24 * time.Hour
		found = true
	}
	if m := relHours.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * time.Hour
		found = true
	}
	if m := relMins.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * time.Minute
		found = true
	}
	return d, found
}

// parseClockTime extracts the first 12-hour clock token and returns its
// 24-hour (hour, minute).
func parseClockTime(s string) (hour, minute int, ok bool) {
	m := clockTime.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	hour, _ = strconv.Atoi(m[1])
	if hour < 1 || hour > 12 {
		return 0, 0, false
	}
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
		if minute > 59 {
			return 0, 0, false
		}
	}
	if strings.EqualFold(m[3], "p") {
		if hour != 12 {
			hour += 12
		}
	} else if hour == 12 {
		hour = 0
	}
	return hour, minute, true
}

// nextTimeOfDay returns the next instant in loc whose clock reads
// hour:minute and is strictly after now.
func nextTimeOfDay(now time.Time, loc *time.Location, hour, minute int) time.Time {
	base := now.In(loc)
	candidate := time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}
