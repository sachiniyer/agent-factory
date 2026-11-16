package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

// TestTabPaneSourceAsksForRowsAsDrawn pins the TUI half of #7406. The pane draws
// whatever this source fetches, so it must ask the daemon for the rows the pane
// draws: a capture that joins soft-wrapped rows glued an exact-width readline
// row ("$ echo hello-from-task") onto the output printed below it, and the pane
// showed one fused line. Visible and full-history fetches both draw.
func TestTabPaneSourceAsksForRowsAsDrawn(t *testing.T) {
	h := newTestHome(t)
	var requests []daemon.PreviewRequest
	h.previewFetcher = func(req daemon.PreviewRequest) (daemon.PreviewResponse, error) {
		requests = append(requests, req)
		return daemon.PreviewResponse{Content: "row"}, nil
	}
	inst := newStartedInstance(t, "rows")
	source := h.newTabPaneSource()

	for _, full := range []bool{false, true} {
		_, err := source(inst, 0, full)
		require.NoError(t, err)
	}
	require.Len(t, requests, 2)
	for _, req := range requests {
		require.True(t, req.Rows, "a fetch the pane draws must ask for rows as drawn (full=%v)", req.Full)
	}
}
