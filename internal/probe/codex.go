package probe

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Codex probe: the moral equivalent of the watcher's
// `codex exec --skip-git-repo-check -c model_reasoning_effort="low"
// "Reply with exactly the single word: ok"`, with two deliberate deltas:
//
//   - its CODEX_HOME=… prefix is NOT copied — env arrives only through
//     sessionenv's account math, never as a command-local assignment;
//   - its -C becomes the private per-run cwd, carried on exec.Cmd.Dir rather
//     than in argv — argv stays a byte-identical literal for every run.
//
// --output-last-message names the file the CLI writes its final message to.
// The name is a bare literal: it resolves inside the per-run cwd, so the
// file is the probe's private success-token channel and the argv stays
// constant. If the CLI resolves it against some other root the file simply
// is not found — a miss reads as Unknown, never as health.
var codexProbeArgv = []string{
	"codex", "exec",
	"--skip-git-repo-check",
	"-c", `model_reasoning_effort="low"`,
	"--output-last-message", codexMessageFile,
	"Reply with exactly the single word: ok",
}

const (
	codexMessageFile = "last-message.txt"
	codexSuccessText = "ok"
)

var codexProbeSpec = agentProbeSpec{
	agent:       "codex",
	argv:        codexProbeArgv,
	messageFile: codexMessageFile,
	successText: codexSuccessText,
	wall:        codexWall,
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

// codexWall reports whether the captured tail shows Codex's usage-limit
// message, and parses its reset phrase when present — "try again at <ts>"
// preferred, then "try again in <duration>" against now.
func codexWall(output []byte, now time.Time) (time.Time, bool) {
	loc := codexWallDetect.FindIndex(output)
	if loc == nil {
		return time.Time{}, false
	}
	tail := string(output[loc[0]:])
	if m := codexResetAt.FindStringSubmatch(tail); m != nil {
		if t, ok := parseCodexAbsolute(strings.TrimSpace(m[1]), now); ok {
			return t, true
		}
	}
	if m := codexResetIn.FindStringSubmatch(tail); m != nil {
		if d, ok := parseRelativeDuration(m[1]); ok {
			return now.Add(d), true
		}
	}
	return time.Time{}, true
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
