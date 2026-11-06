package handoff

import (
	"strings"
	"testing"
	"time"
)

// #6818 review: shadow mode has no immediate-delivery path — halted and
// move-failed rows batch through the digest like every other kind.
// Immediate delivery returns with the live mover in slice 6.
func TestHaltedAndMoveFailedBatchInDigest(t *testing.T) {
	rows := []EventRow{
		{Kind: EventHalted, ReasonCode: "mover-halted", Mode: "shadow"},
		{Kind: EventMoveFailed, ReasonCode: "carry", Mode: "shadow"},
	}
	digest := ReportDigest(rows)
	if !strings.HasPrefix(digest, "[af handoff digest] 2 events\n") {
		t.Fatalf("digest header: %q", digest)
	}
	if !strings.Contains(digest, "kind=halted") || !strings.Contains(digest, "kind=move-failed") {
		t.Fatalf("halted/move-failed missing from digest: %q", digest)
	}
}

func TestReportLineCarriesOnlyStructuredFields(t *testing.T) {
	row := EventRow{
		TS:   time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC),
		Kind: EventWall, SessionKey: "sess-1", Agent: "codex",
		Title: "fix login", From: "work", ReasonCode: "usage-reading",
		Params: map[string]string{"window": "primary", "percent": "98"},
		Mode:   "shadow",
	}
	line := ReportLine(row)
	for _, want := range []string{
		"[af handoff]", "kind=wall", "reason=usage-reading",
		`session="fix login"`, "key=sess-1", "agent=codex", "from=work",
		"mode=shadow", "percent=98", "window=primary",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("line missing %q: %s", want, line)
		}
	}
	// Params sort canonically — map order never varies the text.
	if strings.Index(line, "percent=98") > strings.Index(line, "window=primary") {
		t.Fatalf("params not sorted: %s", line)
	}
}

func TestReportLineSanitizesLabels(t *testing.T) {
	// The event-log writer sanitizes its own copies; the report path must
	// normalize the same fields itself (#6818 review).
	long := strings.Repeat("x", 200)
	row := EventRow{
		Kind: EventWall, ReasonCode: "wall", Mode: "shadow",
		Title:      long + "\n injected",
		SessionKey: "sess\n-1",
	}
	line := ReportLine(row)
	if strings.Contains(line, "\n") || strings.Contains(line, "injected") {
		t.Fatalf("control characters leaked into the report: %q", line)
	}
	if strings.Contains(line, strings.Repeat("x", 121)) {
		t.Fatalf("title was not capped at %d chars", eventLabelCap)
	}
}

func TestReportDigestHeaderAndLines(t *testing.T) {
	rows := []EventRow{
		{Kind: EventWall, ReasonCode: "wall", Mode: "shadow"},
		{Kind: EventMove, ReasonCode: "wall", From: "work", To: "personal", Mode: "shadow"},
	}
	digest := ReportDigest(rows)
	if !strings.HasPrefix(digest, "[af handoff digest] 2 events\n") {
		t.Fatalf("digest header: %q", digest)
	}
	if strings.Count(digest, "[af handoff] kind=") != 2 {
		t.Fatalf("digest lines: %q", digest)
	}
	if !strings.HasPrefix(ReportDigest(rows[:1]), "[af handoff digest] 1 event\n") {
		t.Fatalf("singular header: %q", ReportDigest(rows[:1]))
	}
}

// #6818 review: a raw title must not close the session field and forge
// report fields — the quoted slot escapes quotes and backslashes.
func TestReportLineEscapesQuotedTitle(t *testing.T) {
	row := EventRow{
		Kind: EventNoAccount, ReasonCode: "none", Mode: "shadow",
		Title: `evil " kind=halted reason=x`,
	}
	line := ReportLine(row)
	if !strings.Contains(line, `session="evil \" kind=halted reason=x"`) {
		t.Fatalf("title was not quote-escaped: %q", line)
	}
}

// #6818 review: a fleet-wide batch must never render as one huge prompt —
// the digest caps rows and bytes and reports the overflow.
func TestReportDigestCapsRowsAndBytes(t *testing.T) {
	rows := make([]EventRow, reportDigestMaxRows+10)
	for i := range rows {
		rows[i] = EventRow{
			Kind: EventWall, ReasonCode: "wall", Mode: "shadow",
			Title: strings.Repeat("y", 100), SessionKey: "s",
		}
	}
	digest := ReportDigest(rows)
	if len(digest) > reportDigestMaxBytes+64 {
		t.Fatalf("digest ran %d bytes", len(digest))
	}
	if strings.Count(digest, "[af handoff] kind=") > reportDigestMaxRows {
		t.Fatalf("digest carried more than %d rows", reportDigestMaxRows)
	}
	if !strings.Contains(digest, "more not shown") {
		t.Fatalf("digest dropped rows without an overflow marker: %q", digest[len(digest)-120:])
	}
}
