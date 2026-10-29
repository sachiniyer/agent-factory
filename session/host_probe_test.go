package session

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

func withHostCheckShell(t *testing.T, sh sandboxShell) *int {
	t.Helper()
	prev := hostCheckShell
	built := 0
	hostCheckShell = func(context.Context, config.HostConfig, string) (sandboxShell, error) { built++; return sh, nil }
	t.Cleanup(func() { hostCheckShell = prev })
	return &built
}

func TestCheckHost_RefusesAnUnlistedNameBeforeDialing(t *testing.T) {
	built := withHostCheckShell(t, &prereqShell{out: "git=ok\ntmux=tmux 3.4\ngh=ok\n"})
	hosts := map[string]config.HostConfig{"box": {Host: "box.example"}}
	for _, name := range []string{"", "other", "-oProxyCommand=x"} {
		_, err := CheckHost(context.Background(), name, hosts, "", time.Second)
		require.Error(t, err, name)
	}
	assert.Zero(t, *built, "no ssh shell may even be composed for a name outside the list")
}

func TestCheckHost_RecordsByListedName(t *testing.T) {
	hosts := map[string]config.HostConfig{"ok-box": {Host: "ok.example"}, "bad-box": {Host: "bad.example"}}

	withHostCheckShell(t, &prereqShell{out: "git=ok\ntmux=tmux 3.4\ngh=ok\n"})
	c, err := CheckHost(context.Background(), "ok-box", hosts, "", time.Second)
	require.NoError(t, err)
	assert.True(t, c.Reachable && c.Complete)
	assert.False(t, c.Blocked())
	assert.Equal(t, HostReadinessReady, ReadHostReadiness(HostReadinessKey("ok-box")).State)

	withHostCheckShell(t, &prereqShell{out: "git=missing\ntmux=tmux 3.4\ngh=ok\n"})
	c, err = CheckHost(context.Background(), "bad-box", hosts, "", time.Second)
	require.NoError(t, err)
	assert.True(t, c.Blocked())
	assert.Equal(t, HostReadinessBlocked, ReadHostReadiness(HostReadinessKey("bad-box")).State)
}

// An unreachable or unreadable probe records nothing, so it can neither mark the
// host ready nor overwrite a blocked verdict.
func TestCheckHost_UnansweredProbeNeverOverwritesABlock(t *testing.T) {
	hosts := map[string]config.HostConfig{"flaky": {Host: "flaky.example"}}
	key := HostReadinessKey("flaky")
	RecordHostReadiness(key, errors.New("git is missing"))

	withHostCheckShell(t, &prereqShell{err: errors.New("ssh: connection refused")})
	c, err := CheckHost(context.Background(), "flaky", hosts, "", time.Second)
	require.NoError(t, err)
	assert.False(t, c.Reachable)
	assert.Contains(t, c.Error, "connection refused")
	assert.Equal(t, HostReadinessBlocked, ReadHostReadiness(key).State)

	withHostCheckShell(t, &prereqShell{out: "git=ok\ntmux=tmux 3.4\n"})
	c, err = CheckHost(context.Background(), "flaky", hosts, "", time.Second)
	require.NoError(t, err)
	assert.True(t, c.Reachable)
	assert.False(t, c.Complete)
	assert.Equal(t, HostReadinessBlocked, ReadHostReadiness(key).State, "an incomplete answer is not evidence the host was fixed")
}

func TestCheckAllHosts_SkipsAndContinuesPastARefusal(t *testing.T) {
	withHostCheckShell(t, &prereqShell{out: "git=ok\ntmux=tmux 3.4\ngh=ok\n"})
	hosts := map[string]config.HostConfig{"a-all": {Host: "a.example"}, "b-all": {Host: "b.example"}, "c-all": {Host: ""}}
	out, errs := CheckAllHosts(context.Background(), hosts, "", time.Second, func(n string) bool { return n == "b-all" })
	assert.Contains(t, out, "a-all")
	assert.NotContains(t, out, "b-all", "skipped")
	assert.Contains(t, errs, "c-all", "an invalid entry is refused without stopping the rest")
}

// A create on a listed host records its prerequisite verdict under host:<name>,
// the key the catalog and doctor read, and not under the per-create ssh command
// (#5840). An unnamed sandbox.ssh host keeps the command-string key.
func TestProvisionerRecordsUnderTheListedHostKey(t *testing.T) {
	probe := func(out string) func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return func(time.Duration, string, io.Reader, bool) ([]byte, error) { return []byte(out), nil }
	}
	const blocked = "git=missing\ntmux=tmux 3.4\ngh=ok\n"
	const ready = "git=ok\ntmux=tmux 3.4\ngh=ok\n"

	cmd := "ssh -o pin=1 create-key-box.example"
	key := HostReadinessKey("create-key-box")
	p := &sandboxProvisioner{sshCmd: cmd, readinessKey: key, runCommandFn: probe(blocked)}
	_, err := p.provision()
	require.Error(t, err)
	assert.Equal(t, HostReadinessBlocked, ReadHostReadiness(key).State, "the create proved the host blocked")
	assert.Equal(t, HostReadinessUnprobed, ReadHostReadiness(cmd).State, "nothing is recorded under the command key")

	// A later create that finds the host fixed clears the stale block.
	p = &sandboxProvisioner{sshCmd: cmd, readinessKey: key, runCommandFn: probe(ready)}
	_, _ = p.provision()
	assert.Equal(t, HostReadinessReady, ReadHostReadiness(key).State)

	// Unnamed sandbox.ssh: the command string stays the key.
	cmd2 := "ssh unnamed-create-box.example"
	p = &sandboxProvisioner{sshCmd: cmd2, runCommandFn: probe(blocked)}
	_, err = p.provision()
	require.Error(t, err)
	assert.Equal(t, HostReadinessBlocked, ReadHostReadiness(cmd2).State)
}
