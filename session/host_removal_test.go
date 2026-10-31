package session

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// Removing a host from the global list must never strand a live session
// (#5571 W10.5). A session's transport — the reap, the -L tunnel's re-dial
// (W1.2) and the reattach after a daemon restart (W1.5) — is composed from the
// config persisted in its OWN row (SSHRuntimeCleanupData.Config, the posture and
// the dial pin), never by looking its host's name up in the list. HostName on the
// row is the operator's label, not a lookup key.

func TestRestoredSSHSessionDoesNotNeedItsHostInTheList(t *testing.T) {
	// A fresh home: no config.toml, so the host list is empty — exactly the state
	// after `af hosts remove` (and the daemon restart that follows it).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	require.Empty(t, cfg.Hosts)

	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "box1.tailnet.ts.net", User: "ubuntu", Port: 2222},
		SessionDir:          "/home/ubuntu/.af-sessions/s.abc123",
		HostName:            "removed-box",
		HostKeyVerification: config.SSHHostKeyStrict,
		DialAddress:         "100.64.0.9",
		RemoteAddr:          "127.0.0.1:41000",
		TokenRef:            "ref",
	}}
	b, teardown, err := restoreRuntimeCleanup("s", "ssh", data)
	require.NoError(t, err, "a row whose host was removed must still restore")
	require.NotNil(t, teardown)

	sb, ok := b.(*sshBackend)
	require.True(t, ok)
	p := sb.provisioner
	require.NotNil(t, p)
	// The command every later step rides — the tunnel's re-dial, the reattach
	// probe and the reap all build on p.sshCmd — carries the persisted machine.
	assert.Contains(t, p.sshCmd, "box1.tailnet.ts.net")
	assert.Contains(t, p.sshCmd, "ubuntu")
	assert.Contains(t, p.sshCmd, "2222")
	assert.Contains(t, p.sshCmd, "100.64.0.9", "the pinned machine, not a re-resolution")
	assert.Equal(t, "removed-box", sb.cleanup.HostName)
}

// The two files that implement re-dial and reattach must not grow a host-list
// lookup: that is the one change that would make `af hosts remove` strand live
// sessions, and the test above only proves the restore path as written today.
func TestRedialAndReattachNeverConsultTheHostList(t *testing.T) {
	forbidden := regexp.MustCompile(`chooseListedHost|ValidateHostPick|\.Hosts\b|LoadConfig|ResolveConfigForRepo|resolveRepoConfig`)
	for _, f := range []string{"sandbox_tunnel.go", "reattach_sandbox.go"} {
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.Empty(t, forbidden.FindAllString(string(src), -1),
			"%s must compose from the session's own persisted transport, not the host list (removing a host would strand live sessions)", f)
	}
}
