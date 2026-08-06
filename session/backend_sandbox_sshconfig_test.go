package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole reason this backend exists: `backend=ssh` uses
// golang.org/x/crypto/ssh and never reads ~/.ssh/config, so it cannot express a
// Host alias, a ProxyJump, or a bastion. `sandbox_ssh` runs the REAL ssh binary,
// which does all of that.
//
// That property is invisible to every other test here — the rest use a stub
// `ssh`, which would keep passing if the transport quietly stopped resolving
// through the real binary (a bad rebase, someone "simplifying" the sh -c
// composition into a strings.Fields split). This test uses the actual ssh
// binary, and NO network: `ssh -G` resolves a host through ssh_config and prints
// the result without connecting to anything.
func TestSandboxTransportResolvesThroughRealSSHConfig(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("no ssh binary on PATH, so the property this test exists to prove cannot be exercised here: %v", err)
	}

	// A config exercising exactly the features backend=ssh cannot express.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ssh_config")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
Host af-sandbox-alias
    HostName sandbox-real-host.invalid
    User sandbox-operator
    Port 2222
    ProxyJump bastion.invalid
`), 0o600))

	// `-G` makes ssh resolve and print, never connect. The alias, the rewritten
	// hostname, the user, the port and the jump host all come from ssh_config.
	p := &sandboxProvisioner{sshCmd: "ssh -F " + cfgPath + " -G af-sandbox-alias"}

	out, err := p.Run(20*time.Second, "true", nil, true)
	require.NoError(t, err, "ssh -G must resolve without connecting; output: %s", out)
	resolved := strings.ToLower(string(out))

	// If the transport ever stopped handing the operator's command to the real
	// ssh binary, none of these could appear.
	assert.Contains(t, resolved, "hostname sandbox-real-host.invalid",
		"the Host alias must be resolved BY SSH — this is what backend=ssh cannot do")
	assert.Contains(t, resolved, "user sandbox-operator")
	assert.Contains(t, resolved, "port 2222")
	assert.Contains(t, resolved, "proxyjump bastion.invalid",
		"ProxyJump/bastion support is the headline reason this backend exists (#2476)")
}

// The operator's own flags must survive composition. A naive strings.Fields()
// split of sandbox_ssh would destroy a quoted option with embedded spaces — the
// exact shape a real ProxyCommand takes — so this pins that they reach ssh
// intact, again by asking the real binary what it resolved.
func TestSandboxTransportKeepsQuotedProxyCommandIntact(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("no ssh binary on PATH: %v", err)
	}

	p := &sandboxProvisioner{
		sshCmd: `ssh -o "ProxyCommand=nc -X connect -x proxy.invalid:8080 %h %p" -G host.invalid`,
	}
	out, err := p.Run(20*time.Second, "true", nil, true)
	require.NoError(t, err, "output: %s", out)

	assert.Contains(t, strings.ToLower(string(out)), "proxycommand nc -x connect -x proxy.invalid:8080",
		"a quoted ProxyCommand with embedded spaces must reach ssh as ONE option, not be split into flags")
}
