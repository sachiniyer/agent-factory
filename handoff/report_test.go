package handoff

import (
	"strings"
	"testing"
	"time"
)

func TestImmediateReport(t *testing.T) {
	for kind, want := range map[EventKind]bool{
		EventHalted:      true,
		EventMoveFailed:  true,
		EventWall:        false,
		EventMove:        false,
		EventNoAccount:   false,
		EventConfigDrift: false,
	} {
		if got := ImmediateReport(EventRow{Kind: kind}); got != want {
			t.Fatalf("ImmediateReport(%s) = %v, want %v", kind, got, want)
		}
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

func TestEventRowSignatureParamOrderInvariant(t *testing.T) {
	a := EventRow{Kind: EventWall, ReasonCode: "wall", SessionKey: "s",
		Params: map[string]string{"x": "1", "y": "2"}}
	b := EventRow{Kind: EventWall, ReasonCode: "wall", SessionKey: "s",
		Params: map[string]string{"y": "2", "x": "1"}}
	if EventRowSignature(a) != EventRowSignature(b) {
		t.Fatalf("param order divided one fact into two signatures")
	}
	if EventRowSignature(a) == EventRowSignature(EventRow{Kind: EventMove, ReasonCode: "wall", SessionKey: "s"}) {
		t.Fatalf("different kinds share a signature")
	}
}
