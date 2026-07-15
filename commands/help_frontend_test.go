package commands

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentServerHelpDoesNotClaimToServeTheFrontend pins the boundary users kept
// tripping over: the web UI is served by the DAEMON (go:embed'd into it and
// exposed on listen_addr), while 'af agent-server' is only the headless
// per-workspace backend a daemon drives. Its help must not read as "this starts
// the web app", and must point at the daemon instead — otherwise someone runs
// agent-server, opens its port, and files a bug about the missing UI.
func TestAgentServerHelpDoesNotClaimToServeTheFrontend(t *testing.T) {
	help := agentServerCmd.Short + "\n" + agentServerCmd.Long
	lower := strings.ToLower(help)

	// It must actively disclaim the frontend and redirect to the daemon, not merely
	// stay silent — silence is what let the misconception form.
	assert.Contains(t, lower, "does not start the web ui",
		"agent-server help must explicitly disclaim serving the web UI")
	assert.Contains(t, lower, "af daemon",
		"agent-server help must name the daemon as what does serve the UI")
	assert.Contains(t, help, "8443",
		"agent-server help must point at the daemon's web address")

	// And it must never assert the opposite.
	for _, claim := range []string{
		"serves the web ui",
		"serves the frontend",
		"starts the web ui",
		"starts the frontend",
		"open your browser at",
	} {
		assert.NotContains(t, lower, claim,
			"agent-server must not claim to serve a frontend (%q)", claim)
	}
}

// TestDaemonHelpAdvertisesTheWebUI is the other half: the daemon is where the web
// UI lives, so its help must say so and name the URL. This is the discoverability
// path for a user who never reads docs/web.md.
func TestDaemonHelpAdvertisesTheWebUI(t *testing.T) {
	help := daemonCmd.Short + "\n" + daemonCmd.Long
	lower := strings.ToLower(help)

	assert.Contains(t, lower, "web ui", "af daemon help must mention the web UI it serves")
	assert.Contains(t, help, "http://localhost:8443", "af daemon help must name the URL to open")
	assert.Contains(t, lower, "agent-server", "af daemon help should disambiguate agent-server")
}

// TestRootHelpAdvertisesTheWebUI keeps the top-level entry point honest: 'af --help'
// is the first place a user looks, so the web UI and its URL must be discoverable
// there without reading any docs.
func TestRootHelpAdvertisesTheWebUI(t *testing.T) {
	require.NotEmpty(t, rootCmd.Long)
	lower := strings.ToLower(rootCmd.Long)

	assert.Contains(t, lower, "web ui", "af --help must mention the web UI")
	assert.Contains(t, rootCmd.Long, "http://localhost:8443", "af --help must name the URL to open")
	assert.Contains(t, lower, "no token by default",
		"af --help must say the web UI needs no token by default")
}
