package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestTabPlaceholderText pins the per-state copy of #7811: a resolved listener
// prints the address the daemon is actually bound to, and every state without
// a proven address points at the docs — never the built-in default, which
// would be a wrong address for anyone who moved network.listen_addr.
func TestTabPlaceholderText(t *testing.T) {
	vscode := &session.Tab{Kind: session.TabKindVSCode}
	web := &session.Tab{Kind: session.TabKindWeb, URL: "http://localhost:3000/app"}

	t.Run("resolved bound listener prints its URL", func(t *testing.T) {
		hint := WebUIHint{URL: "http://127.0.0.1:8443"}

		got, ok := tabPlaceholder(vscode, hint)
		require.True(t, ok)
		require.Equal(t, "Open this VS Code tab in the web UI at http://127.0.0.1:8443.", got)

		got, ok = tabPlaceholder(web, hint)
		require.True(t, ok)
		require.Equal(t, "http://localhost:3000/app\n\nWeb tab — open the URL in a browser or view it in the web UI at http://127.0.0.1:8443.", got)
	})

	t.Run("unresolved hints point at af daemon --help", func(t *testing.T) {
		// The zero hint covers every unresolvable state — no poll landed yet, a
		// failed health read, and a remote target, whose bound address would
		// name the wrong host anyway.
		hint := WebUIHint{}

		got, ok := tabPlaceholder(vscode, hint)
		require.True(t, ok)
		require.Equal(t, "Open this VS Code tab in the web UI — see `af daemon --help` for its address.", got)

		got, ok = tabPlaceholder(web, hint)
		require.True(t, ok)
		require.Equal(t, "http://localhost:3000/app\n\nWeb tab — open the URL in a browser or view it in the web UI — see `af daemon --help` for its address.", got)
	})

	t.Run("disabled listener names the off web server", func(t *testing.T) {
		hint := WebUIHint{Disabled: true}

		got, ok := tabPlaceholder(vscode, hint)
		require.True(t, ok)
		require.Equal(t, "Open this VS Code tab in the web UI — the web server is off (network.listen_addr is empty).", got)

		got, ok = tabPlaceholder(web, hint)
		require.True(t, ok)
		require.Equal(t, "http://localhost:3000/app\n\nWeb tab — open the URL in a browser or view it in the web UI — the web server is off (network.listen_addr is empty).", got)
	})

	t.Run("unresolved hints never name an address", func(t *testing.T) {
		for _, hint := range []WebUIHint{{}, {Disabled: true}} {
			got, _ := tabPlaceholder(vscode, hint)
			require.NotContains(t, got, "http://")
			require.NotContains(t, got, "https://")
			require.NotContains(t, got, "8443",
				"the built-in default must never stand in for an unproven listener")
		}
	})

	t.Run("renderable kinds get no placeholder", func(t *testing.T) {
		for _, kind := range []session.TabKind{session.TabKindAgent, session.TabKindShell, session.TabKindProcess} {
			_, ok := tabPlaceholder(&session.Tab{Kind: kind}, WebUIHint{URL: "http://127.0.0.1:8443"})
			require.False(t, ok, "kind %v is renderable, so it has no placeholder", kind)
		}
	})
}

// TestTabPaneVSCodePlaceholderRendersTheInstalledHint proves the pane consults
// the provider the app installs at creation, so a listener the health poll
// resolved reaches the rendered fallback — and a later read can swap the docs
// pointer for the real address (#7811).
func TestTabPaneVSCodePlaceholderRendersTheInstalledHint(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "vscode-hint", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddTabForTest("vscode", session.TabKindVSCode)
	inst.SetStartedForTest(true)

	hint := WebUIHint{}
	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	p.SetWebUIHint(func() WebUIHint { return hint })

	require.NoError(t, p.UpdateContent(inst, 1))
	require.Contains(t, p.String(), "af daemon --help",
		"an unresolved hint renders the docs pointer")
	require.NotContains(t, p.String(), "8443")

	hint = WebUIHint{URL: "http://127.0.0.1:9000"}
	require.NoError(t, p.UpdateContent(inst, 1))
	require.Contains(t, p.String(), "http://127.0.0.1:9000",
		"a resolved hint renders the bound listener")
	require.NotContains(t, p.String(), "8443",
		"the default address is never printed for a listener that moved")
}
