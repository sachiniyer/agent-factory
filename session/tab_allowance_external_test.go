package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTabKindAllowances_OffBoxOffersOnlyMetadataWeb guards both sides of the
// off-box menu contract. Web is admissible because an external HTTPS tab is
// metadata only: it owns no PTY and starts no process. Every process-backed or
// worktree-reading kind must remain refused on a machine that cannot serve it.
func TestTabKindAllowances_OffBoxOffersOnlyMetadataWeb(t *testing.T) {
	remote := remoteCaps()
	projected := make(map[string]TabKindAllowance)
	for _, allowance := range tabKindAllowances(remote) {
		projected[allowance.Kind] = allowance
	}

	web, ok := projected["web"]
	require.True(t, ok, "the web kind must be projected")
	require.True(t, web.Allowed,
		"an external HTTPS web tab is metadata-only: it owns no PTY and starts no process")

	for _, tc := range []struct {
		name string
		kind TabKind
	}{
		{name: "vscode", kind: TabKindVSCode},
		{name: "shell", kind: TabKindShell},
		{name: "process", kind: TabKindProcess},
		{name: "agent", kind: TabKindAgent},
		{name: "unknown", kind: TabKind(9999)},
	} {
		require.Error(t, remote.RefuseTabKind(tc.kind, ""),
			"off-box admission widened to %s, which needs a local editor, PTY, or process", tc.name)
		if allowance, projectedKind := projected[tc.name]; projectedKind {
			require.False(t, allowance.Allowed,
				"the menu advertised unsupported off-box kind %s", tc.name)
		}
	}
}
