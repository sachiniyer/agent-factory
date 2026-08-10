package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

// The human-facing half of #3169. The operator who archived working sessions was
// reading a terminal, not JSON, so the notice is the part that would have stopped
// them — and its exact wording is the fix.
func TestPreviewPartialNotice(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot daemon.PreviewResponse
		full     bool
		want     string
		why      string
	}{
		{
			name:     "measured scrollback names the count and the flag",
			snapshot: daemon.PreviewResponse{LinesAbove: 412, LinesAboveKnown: true},
			want:     "af: showing the visible screen; 412 more lines above — use --full to capture everything",
			why:      "the reader needs to know both that it was partial and how to see the rest",
		},
		{
			name:     "one line stays grammatical",
			snapshot: daemon.PreviewResponse{LinesAbove: 1, LinesAboveKnown: true},
			want:     "af: showing the visible screen; 1 more line above — use --full to capture everything",
			why:      "a one-line omission is the case most likely to be dismissed as noise",
		},
		{
			name:     "a measured zero is silent",
			snapshot: daemon.PreviewResponse{LinesAbove: 0, LinesAboveKnown: true},
			want:     "",
			why:      "the visible screen IS the pane, so flagging it would train the reader to ignore the line",
		},
		{
			name:     "unmeasured says so and never implies zero",
			snapshot: daemon.PreviewResponse{LinesAboveKnown: false},
			want:     "af: showing the visible screen; this pane's scrollback was not measured — use --full to capture everything",
			why:      "a remote sandbox does not carry the count; reporting it as none would be the original bug",
		},
		{
			name:     "full capture is silent even with scrollback measured",
			snapshot: daemon.PreviewResponse{LinesAbove: 412, LinesAboveKnown: true},
			full:     true,
			want:     "",
			why:      "--full captured it all, so nothing was omitted",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, previewPartialNotice(test.snapshot, test.full), test.why)
		})
	}
}
