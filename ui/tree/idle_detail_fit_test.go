package tree

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// paneChurnInstance is a selected-row session whose idle detail reads
// "pane changed · 12m ago" under branch.
func paneChurnInstance(t *testing.T, branch string) *session.Instance {
	t.Helper()
	attemptedAt := time.Now().Add(-13 * time.Minute)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	inst.Branch = branch
	inst.SetStatusForTest(session.Ready)
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt))
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Minute), epoch))
	return inst
}

// branchRow renders inst selected at width and returns its secondary row with
// the padding trimmed, starting at the row's first non-space cell.
func branchRow(t *testing.T, inst *session.Instance, width int, marker string) string {
	t.Helper()
	r := NewInstanceRenderer()
	r.SetWidth(width)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), "")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, marker) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no row containing %q at width %d:\n%s", marker, width, out)
	return ""
}

// TestIdleDetailTooNarrowDropsToBareBranch pins #4956: when the remainder
// after the branch cannot hold the reason label whole, the row renders the
// branch alone rather than a bare separator ("· …") or a one-word fragment
// ("pane …") that reads as a rendering glitch.
func TestIdleDetailTooNarrowDropsToBareBranch(t *testing.T) {
	t.Parallel()

	inst := paneChurnInstance(t, "dev/beta")
	for _, tc := range []struct {
		name  string
		width int
	}{
		{"bare separator", 18},       // master: ⎇-dev/beta · …
		{"one-word label", 23},       // master: ⎇-dev/beta · pane …
		{"label one cell short", 28}, // master: ⎇-dev/beta · pane chang…
	} {
		assert.Equal(t, branchIcon+"-dev/beta", branchRow(t, inst, tc.width, branchIcon),
			"%s at width %d", tc.name, tc.width)
	}
}

// TestIdleDetailKeepsWholeSegmentsThatFit: the detail survives whenever its
// reason label fits, trimmed by whole " · " segments rather than by rune, and
// shows in full once it all fits.
func TestIdleDetailKeepsWholeSegmentsThatFit(t *testing.T) {
	t.Parallel()

	inst := paneChurnInstance(t, "dev/beta")
	// Exactly wide enough for the label: master cut it to "pane changed…".
	assert.Equal(t, branchIcon+"-dev/beta · pane changed", branchRow(t, inst, 29, branchIcon))
	// Room for the label and part of the age: master showed "· 12…".
	assert.Equal(t, branchIcon+"-dev/beta · pane changed", branchRow(t, inst, 35, branchIcon))
	// The whole detail fits and must still show.
	assert.Equal(t, branchIcon+"-dev/beta · pane changed · 12m ago", branchRow(t, inst, 39, branchIcon))
}

// TestIdleDetailRowNeverEndsInAFragment sweeps every width: the branch row is
// either the branch (possibly truncated with nothing after it) or the branch
// followed by whole detail segments — never a separator or a cut segment.
func TestIdleDetailRowNeverEndsInAFragment(t *testing.T) {
	t.Parallel()

	inst := paneChurnInstance(t, "dev/beta")
	allowed := map[string]bool{
		branchIcon + "-dev/beta":                          true,
		branchIcon + "-dev/beta · pane changed":           true,
		branchIcon + "-dev/beta · pane changed · 12m ago": true,
	}
	for width := 10; width <= 80; width++ {
		row := branchRow(t, inst, width, branchIcon)
		if cut, ok := strings.CutSuffix(row, "…"); ok {
			assert.True(t, strings.HasPrefix(branchIcon+"-dev/beta", cut),
				"width %d truncated past the branch into the detail: %q", width, row)
			continue
		}
		assert.True(t, allowed[row], "width %d rendered a partial detail: %q", width, row)
	}
}

// TestRestoreFailureDetailLeadAndTruncationUnchanged guards the one exception
// #4956 leaves alone: a restore-gave-up row leads with its actionable detail
// and still truncates by rune, so as much of the failure as fits stays visible.
func TestRestoreFailureDetailLeadAndTruncationUnchanged(t *testing.T) {
	t.Parallel()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	inst.Branch = "feature"
	inst.SetStatusForTest(session.Lost)
	require.True(t, inst.SetLostRestoreFailure(6, errors.New("agent exited at startup")))

	for width, want := range map[int]string{
		30: "restore gave up after 6…",
		70: "restore gave up after 6 attempts: agent exited at startup · fea…",
		75: "restore gave up after 6 attempts: agent exited at startup · feature",
	} {
		r := NewInstanceRenderer()
		r.SetWidth(width)
		out := ansiEscape.ReplaceAllString(r.Render(inst, 1, false, false, false), "")
		lines := strings.Split(out, "\n")
		require.GreaterOrEqual(t, len(lines), 2)
		assert.Equal(t, branchIcon+"-"+want, strings.TrimSpace(lines[1]), "width %d", width)
	}
}
