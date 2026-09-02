package git

import (
	"errors"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// maxStderrDetail bounds how much captured stderr reaches an error message.
//
// These errors are rendered into single daemon log lines (the PR-info sweep
// warns once per failing branch), so an unbounded copy of a pathological
// stderr — a paginated API dump, a shell that printed a whole environment —
// would flood the log and bury the very line it was meant to explain. 1 KiB is
// far more than any real `gh`/`git` diagnostic and still fits a log line.
const maxStderrDetail = 1024

// commandFailureDetail renders the explanation a failed command actually
// printed, or "" when there is none to add.
//
// This is the fix for the #3392 class. exec.Cmd.Output captures the child's
// stderr into (*exec.ExitError).Stderr, but Error() on that value is only
// "exit status N" — so wrapping a failed command with %w alone throws the
// diagnosis away and reports the same opaque sentence whether gh hit an auth
// failure, a rate limit, an unreachable network, or a repo with no GitHub
// remote. Those want completely different responses from whoever reads the
// log.
//
// Callers keep wrapping the original error with %w as well, so errors.Is and
// errors.As still see through to the *exec.ExitError and its exit code; this
// only adds the human-readable half that was being discarded.
//
// Returns "" for a nil error, for an error that is not an *exec.ExitError
// (a start failure or a context cancellation carries no captured stderr), and
// for an ExitError whose Stderr is empty — which is what CombinedOutput
// produces, since it routes stderr into the output slice instead. In every one
// of those cases the caller's plain %w wrap is already the whole truth.
func commandFailureDetail(err error) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ""
	}
	return collapseDetail(string(exitErr.Stderr))
}

// collapseDetail trims, flattens, and bounds captured output for use inside a
// one-line error message.
//
// Flattening matters as much as bounding: git and gh both print multi-line
// diagnostics ("error: …" followed by a hint block), and a raw newline in the
// message would split one log line into several, so the continuation lines
// arrive with no timestamp, no level, and no indication of which failure they
// belong to.
func collapseDetail(raw string) string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' })
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return truncateDetail(strings.Join(parts, "; "))
}

// truncateDetail caps s at maxStderrDetail, cutting on a rune boundary so a
// multi-byte character split by the limit cannot emit a replacement character
// into the log.
func truncateDetail(s string) string {
	if len(s) <= maxStderrDetail {
		return s
	}
	cut := maxStderrDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "… (truncated)"
}
