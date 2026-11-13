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

// TestTunnelNeverSharesAConnection pins #7121's tunnel half against OpenSSH
// itself rather than a restatement of its rules. The operator here wants
// connection sharing everywhere: their ssh_config sets ControlMaster auto, a
// ControlPath and ControlPersist, and their sandbox_ssh command passes its own
// `-o ControlPath` and `-S` too. Under that, an ssh -N -L that finds a master
// hands the forward to it and exits, so supervision watches a dead process
// while the forward lives on in the master, past teardown.
//
// The tunnel is spawned through the real spawnTunnel, and its ssh runs with -G,
// which prints the configuration ssh resolved from exactly that command line and
// config and then exits without connecting. The tunnel must resolve to no
// ControlPath at all: that is what "never multiplexes" means to ssh.
func TestTunnelNeverSharesAConnection(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("needs an OpenSSH client to resolve the tunnel's configuration")
	}
	forceTunnelLifelineExe(t, "")
	dir := t.TempDir()
	config := filepath.Join(dir, "ssh_config")
	require.NoError(t, os.WriteFile(config, []byte("Host *\n"+
		"  ControlMaster auto\n"+
		"  ControlPath "+filepath.Join(dir, "config-cm-%C")+"\n"+
		"  ControlPersist 120\n"), 0o600))
	resolved := filepath.Join(dir, "resolved")
	// The operator's sandbox_ssh, standing in for `ssh -o ControlPath=… -S …
	// host`: a wrapper is the only way to send -G's report somewhere a test can
	// read, since af appends its flags after this command.
	wrapper := filepath.Join(dir, "operator-ssh")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\n"+
		"exec "+shellQuote(sshPath)+" -G -F "+shellQuote(config)+
		" -o ControlPath="+shellQuote(filepath.Join(dir, "flag-cm-%C"))+
		" -S "+shellQuote(filepath.Join(dir, "flag-s"))+
		` "$@" >`+shellQuote(resolved)+" 2>&1\n"), 0o755))

	p := &sandboxProvisioner{sshCmd: shellQuote(wrapper) + " sandbox.invalid"}
	child := spawnTestTunnel(t, p)
	select {
	case <-child.done:
	case <-time.After(10 * time.Second):
		t.Fatal("ssh -G did not exit")
	}
	data, err := os.ReadFile(resolved)
	require.NoError(t, err)
	out := string(data)
	require.Contains(t, out, "exitonforwardfailure yes", "ssh must have resolved the tunnel's whole command line: %s", out)

	for _, line := range strings.Split(out, "\n") {
		assert.False(t, strings.HasPrefix(line, "controlpath "),
			"the tunnel resolved %q: it would share a master's connection, and a -N -L that finds one hands it the forward and exits (#7121)", line)
	}
	assert.Contains(t, out, "\ncontrolmaster false\n", "and it never becomes a master itself")
}
