package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// --- #5855: the read-modify-write is one hold of the config file lock ---

// useRealHostStore points the host-list seams at a real config.toml in a temp
// home, so edits go through the real exclusive file lock.
func useRealHostStore(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := config.LoadConfig() // materialize config.toml
	require.NoError(t, err)
	pl, pe, ps := hostsLoad, hostsEditApply, hostsConfigSet
	hostsEditApply = func(e config.HostsEdit) (daemon.SetConfigValueResponse, error) {
		r, _, err := config.EditGlobalHosts(e)
		return daemon.SetConfigValueResponse{Result: r}, err
	}
	hostsConfigSet = func(k, v string) (daemon.SetConfigValueResponse, error) {
		r, err := config.SetGlobalConfigValue(k, v)
		return daemon.SetConfigValueResponse{Result: r}, err
	}
	t.Cleanup(func() { hostsLoad, hostsEditApply, hostsConfigSet = pl, pe, ps })
}

func TestApplyHostsEdit_ConcurrentAddsAllSurvive(t *testing.T) {
	useRealHostStore(t)
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("box%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry := config.HostConfig{Host: name + ".example"}
			errs <- applyHostsEdit(config.HostsEdit{Op: config.HostsEditAdd, Name: name, Host: &entry})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "a writer must wait for the lock, never fail under contention")
	}
	hosts, _, err := loadGlobalHosts()
	require.NoError(t, err)
	assert.Len(t, hosts, n, "every concurrent add must survive")
}

func TestApplyHostsEdit_ConcurrentAddRemoveAndConfigSetNeitherLoseNorResurrect(t *testing.T) {
	useRealHostStore(t)
	for _, n := range []string{"keep", "drop"} {
		e := config.HostConfig{Host: n + ".example"}
		require.NoError(t, applyHostsEdit(config.HostsEdit{Op: config.HostsEditAdd, Name: n, Host: &e}))
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("new%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := config.HostConfig{Host: name + ".example"}
			require.NoError(t, applyHostsEdit(config.HostsEdit{Op: config.HostsEditAdd, Name: name, Host: &e}))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.NoError(t, applyHostsEdit(config.HostsEdit{Op: config.HostsEditRemove, Name: "drop"}))
	}()
	// An unrelated `af config set` takes the same lock and must not lose either.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := config.SetGlobalConfigValue("default_program", "claude")
		require.NoError(t, err)
	}()
	wg.Wait()
	hosts, _, err := loadGlobalHosts()
	require.NoError(t, err)
	assert.Contains(t, hosts, "keep")
	assert.NotContains(t, hosts, "drop", "a concurrent add must not resurrect a removed host")
	assert.Len(t, hosts, 9)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "claude", cfg.DefaultProgram)
}

func TestApplyHostsEdit_DuplicateAndDefaultAreDecidedInsideTheLock(t *testing.T) {
	useRealHostStore(t)
	e := config.HostConfig{Host: "a.example"}
	require.NoError(t, applyHostsEdit(config.HostsEdit{Op: config.HostsEditAdd, Name: "a", Host: &e}))
	err := applyHostsEdit(config.HostsEdit{Op: config.HostsEditAdd, Name: "a", Host: &config.HostConfig{Host: "other.example"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already listed")
	hosts, _, _ := loadGlobalHosts()
	assert.Equal(t, "a.example", hosts["a"].Host, "the existing entry is untouched")

	_, err = hostsConfigSet("default_host", "a")
	require.NoError(t, err)
	err = applyHostsEdit(config.HostsEdit{Op: config.HostsEditRemove, Name: "a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--clear-default")

	require.NoError(t, applyHostsEdit(config.HostsEdit{Op: config.HostsEditRemove, Name: "a", ClearDefault: true}))
	hosts, def, _ := loadGlobalHosts()
	assert.Empty(t, hosts)
	assert.Empty(t, def, "the default is cleared in the same lock hold as the removal")
}

// --- #5854: with --json every outcome is one envelope on stdout ---

func runHostsJSON(t *testing.T, c *cobra.Command, args ...string) (stdout, stderr string, env apiproto.Envelope, err error) {
	t.Helper()
	hostsJSONFlag = true
	var out, errb bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errb)
	root := &cobra.Command{Use: "af"}
	root.AddCommand(&cobra.Command{Use: "hosts"})
	err = c.RunE(c, args)
	stdout, stderr = out.String(), errb.String()
	if strings.TrimSpace(stdout) != "" {
		require.NoError(t, json.Unmarshal([]byte(stdout), &env), "stdout must be exactly one JSON envelope: %q", stdout)
	}
	return
}

func TestHostsAddJSON_ProbeFailureIsOneEnvelopeWithTheErrorInside(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}, checkEr: errors.New("unknown method CheckHost")}
	installHostsFake(t, f)
	_, stderr, env, err := runHostsJSON(t, hostsAddCmd, "box1", "box1.ts.net")
	require.Error(t, err, "the exit code stays non-zero")
	require.NotNil(t, env.Error, "not a success envelope")
	assert.Equal(t, apiproto.ErrorCodeMutationCommitted, env.Error.Code, "a script can tell the host was saved")
	assert.Contains(t, env.Error.Message, "af daemon restart")
	assert.NotNil(t, env.Data, "the saved entry rides beside the error")
	assert.Empty(t, stderr, "nothing on stderr in place of the JSON")
	assert.Contains(t, f.hosts, "box1")
}

func TestHostsAddJSON_NotReadyAndSuccessAreSingleEnvelopes(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}, reply: daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{
		{Name: "box1", Probed: true, Reachable: true, Problems: []string{"git is missing"}},
	}}}
	installHostsFake(t, f)
	_, stderr, env, err := runHostsJSON(t, hostsAddCmd, "box1", "box1.ts.net")
	require.ErrorIs(t, err, errHostsNotReady)
	require.NotNil(t, env.Error)
	assert.Equal(t, hostsCodeNotReady, env.Error.Code)
	assert.NotNil(t, env.Data, "the probe result is the data")
	assert.Empty(t, stderr)

	f.hosts = map[string]config.HostConfig{}
	f.reply = daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{{Name: "box1", Probed: true, Reachable: true, Complete: true}}}
	_, stderr, env, err = runHostsJSON(t, hostsAddCmd, "box1", "box1.ts.net")
	require.NoError(t, err)
	assert.Nil(t, env.Error)
	assert.Empty(t, stderr)
}

func TestHostsJSON_EarlyFailuresAndCheckAreEnvelopesOnStdout(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{"taken": {Host: "x.example"}}}
	installHostsFake(t, f)
	_, stderr, env, err := runHostsJSON(t, hostsAddCmd, "taken", "y.example")
	require.Error(t, err)
	require.NotNil(t, env.Error)
	assert.Contains(t, env.Error.Message, "already listed")
	assert.Empty(t, stderr, "a refused add puts its error envelope on stdout too")

	f.reply = daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{{Name: "taken", Probed: true, Error: "down"}}}
	_, stderr, env, err = runHostsJSON(t, hostsCheckCmd)
	require.ErrorIs(t, err, errHostsUnreachableOrBusy)
	require.NotNil(t, env.Error)
	assert.Equal(t, hostsCodeNotReady, env.Error.Code)
	assert.NotNil(t, env.Data)
	assert.Empty(t, stderr)
}

// An incomplete probe is one envelope on stdout with the error beside the probe
// results — a script can tell the host is not ready without scraping stderr
// (#5841). The host_not_ready code is the same code a blocked host uses.
func TestHostsCheckJSON_IncompleteIsOneEnvelopeNotReady(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{"a": {Host: "a.example"}}, reply: daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{
		{Name: "a", Probed: true, Reachable: true},
	}}}
	installHostsFake(t, f)
	_, stderr, env, err := runHostsJSON(t, hostsCheckCmd)
	require.ErrorIs(t, err, errHostsCheckIncomplete, "the exit code stays non-zero")
	require.NotNil(t, env.Error)
	assert.Equal(t, hostsCodeNotReady, env.Error.Code)
	assert.NotNil(t, env.Data, "the probe result is the data")
	assert.Empty(t, stderr, "nothing on stderr in place of the JSON")
}

// --- #5852: every command af prints for the operator is valid as printed ---

func TestSSHRemedyCommandParsesWithSSHG(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary")
	}
	hostsPosture = func() string { return "" }
	t.Cleanup(func() { hostsPosture = globalHostKeyPosture })
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cases := map[string]struct {
		dest, wantHost, wantUser, wantPort string
	}{
		"bare host":        {"box1.tailnet.ts.net", "box1.tailnet.ts.net", "", "22"},
		"user@host":        {"ubuntu@box1.example", "box1.example", "ubuntu", "22"},
		"custom port":      {"ubuntu@box1.example:2222", "box1.example", "ubuntu", "2222"},
		"bare host + port": {"10.0.0.7:2200", "10.0.0.7", "", "2200"},
		"ssh url":          {"ssh://ubuntu@box1.example:2222", "box1.example", "ubuntu", "2222"},
		"ssh url no port":  {"ssh://box1.example/", "box1.example", "", "22"},
		"ipv6 literal":     {"2001:db8::1", "2001:db8::1", "", "22"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			entry, err := parseHostDestination(tc.dest)
			require.NoError(t, err)
			require.NoError(t, config.ValidateHostEntry("box", entry), "the list must accept this shape")
			printed := sshRemedyCommand(entry)
			require.False(t, strings.Contains(printed, entry.Host+":"+tc.wantPort) && tc.wantPort != "22" && !strings.Contains(printed, "-p"),
				"never the host:port shorthand")

			// `ssh -G` evaluates the config and exits without connecting: the same
			// command, parsed exactly as an operator's shell would run it.
			probe := strings.Replace(printed, "ssh ", "ssh -G ", 1)
			out, err := exec.Command("sh", "-c", probe).CombinedOutput()
			require.NoError(t, err, "printed command must parse: %s\n%s", printed, out)
			got := map[string]string{}
			for _, line := range strings.Split(string(out), "\n") {
				if k, v, ok := strings.Cut(line, " "); ok {
					if _, seen := got[k]; !seen {
						got[k] = strings.TrimSpace(v)
					}
				}
			}
			assert.Equal(t, tc.wantHost, got["hostname"], printed)
			assert.Equal(t, tc.wantPort, got["port"], printed)
			if tc.wantUser != "" {
				assert.Equal(t, tc.wantUser, got["user"], printed)
			}
		})
	}
}

func TestUnreachableRemedyPrintsAValidCommandForACustomPort(t *testing.T) {
	hostsPosture = func() string { return "" }
	t.Cleanup(func() { hostsPosture = globalHostKeyPosture })
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	var out bytes.Buffer
	renderHostCheck(&out, daemon.HostCheckResult{Name: "box", Probed: true, Error: "refused"}, config.HostConfig{Host: "box.example", User: "ubuntu", Port: 2222})
	assert.Contains(t, out.String(), "-p 2222")
	assert.NotContains(t, out.String(), "ubuntu@box.example:2222")
}

// #6096: removing the default with --clear-default is exactly ONE write to the
// host list (clear and removal together) and never a separate default_host
// write, so a refusal can never follow a partial mutation.
func TestHostsRemoveClearDefault_IsASingleEditNotAClearThenARemoval(t *testing.T) {
	var edits []config.HostsEdit
	var sets []string
	pe, ps, pf := hostsEditApply, hostsConfigSet, hostsRemoveClearDefault
	t.Cleanup(func() { hostsEditApply, hostsConfigSet, hostsRemoveClearDefault = pe, ps, pf })
	hostsEditApply = func(e config.HostsEdit) (daemon.SetConfigValueResponse, error) {
		edits = append(edits, e)
		return daemon.SetConfigValueResponse{}, nil
	}
	hostsConfigSet = func(k, v string) (daemon.SetConfigValueResponse, error) {
		sets = append(sets, k)
		return daemon.SetConfigValueResponse{}, nil
	}
	hostsRemoveClearDefault = true
	require.NoError(t, hostsRemoveCmd.RunE(hostsRemoveCmd, []string{"box1"}))
	require.Equal(t, []config.HostsEdit{{Op: config.HostsEditRemove, Name: "box1", ClearDefault: true}}, edits)
	require.Empty(t, sets, "no standalone default_host write may precede a capability-gated edit")
}
