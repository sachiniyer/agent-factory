package daemon

import (
	"strings"
	"sync"
)

// tailBuffer is a small bounded ring of one script run's most recent output
// lines — stdout lines that did not become delivered events, plus stderr —
// kept so a failing run's log line can show WHY the script died instead of a
// bare exit status (#797). Bounded to watcherTailMaxLines lines and
// watcherTailMaxBytes total bytes, always retaining at least the newest line.
// Extracted from watcher.go to keep that file under its length ceiling (#1145).
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
	size  int
}

// add records one output line, trimming the line terminator and capping the
// line at watcherTailMaxBytes. Blank lines are skipped — they carry no
// diagnostics and would evict lines that do.
func (b *tailBuffer) add(line string) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return
	}
	if len(line) > watcherTailMaxBytes {
		line = truncateRunes(line, watcherTailMaxBytes)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	b.size += len(line)
	for len(b.lines) > 1 && (len(b.lines) > watcherTailMaxLines || b.size > watcherTailMaxBytes) {
		b.size -= len(b.lines[0])
		b.lines = b.lines[1:]
	}
}

// logSuffix renders the buffered output for appending to a failure log line:
// "; last output:" plus one indented line each, or "" when the run produced
// nothing to show (so failure logs never grow an empty trailer).
func (b *tailBuffer) logSuffix() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) == 0 {
		return ""
	}
	return "; last output:\n  " + strings.Join(b.lines, "\n  ")
}

// firstLine returns the oldest buffered line — usually the script's initial
// complaint — for the persisted status summary.
func (b *tailBuffer) firstLine() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) == 0 {
		return ""
	}
	return b.lines[0]
}

// failureSummary builds the status the crash-loop breaker persists:
// "errored: <exit error>: <first buffered line>", capped at
// watcherStatusSummaryMax. The "errored" prefix is what the TUI keys the
// supervision state off (ui.watchTaskStatus), so it must come first.
func failureSummary(runErr error, tail *tailBuffer) string {
	summary := "errored: " + runErr.Error()
	if first := tail.firstLine(); first != "" {
		summary += ": " + first
	}
	if len(summary) > watcherStatusSummaryMax {
		summary = truncateRunes(summary, watcherStatusSummaryMax) + "…"
	}
	return summary
}
