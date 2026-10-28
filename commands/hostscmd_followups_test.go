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

// --- #5855: the read-modify-write is atomic against concurrent writers ---

// useRealHostStore points the host-list seams at a real config.toml in a temp
// home, so the writes go through the real file lock and compare-and-set.
func useRealHostStore(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := config.LoadConfig() // materialize config.toml
	require.NoError(t, err)
	pl, ps, pw := hostsLoad, hostsConfigSet, hostsWriteIf
	hostsConfigSet = func(k, v string) (daemon.SetConfigValueResponse, error) {
		r, err := config.SetGlobalConfigValue(k, v)
		return daemon.SetConfigValueResponse{Result: r}, err
	}
	hostsWriteIf = func(k, v string, ifc *string) (daemon.SetConfigValueResponse, error) {
		r, _, err := config.SetGlobalConfigValueIfWithDigest(k, v, ifc)
		return daemon.SetConfigValueResponse{Result: r}, err
	}
	t.Cleanup(func() { hostsLoad, hostsConfigSet, hostsWriteIf = pl, ps, pw })
}

func TestMutateHosts_ConcurrentAddsAllSurvive(t *testing.T) {
	useRealHostStore(t)
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("box%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- mutateHosts(func(hosts map[string]config.HostConfig, _ string) error {
				if _, ok := hosts[name]; ok {
					return errors.New("exists")
				}
				hosts[name] = config.HostConfig{Host: name + ".example"}
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	hosts, _, err := loadGlobalHosts()
	require.NoError(t, err)
	assert.Len(t, hosts, n, "every concurrent add must survive; a lost update drops entries")
}

func TestMutateHosts_ConcurrentAddAndRemoveNeitherLosesNorResurrects(t *testing.T) {
	useRealHostStore(t)
	require.NoError(t, mutateHosts(func(h map[string]config.HostConfig, _ string) error {
		h["keep"], h["drop"] = config.HostConfig{Host: "keep.example"}, config.HostConfig{Host: "drop.example"}
		return nil
	}))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("new%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, mutateHosts(func(h map[string]config.HostConfig, _ string) error {
				h[name] = config.HostConfig{Host: name + ".example"}
				return nil
			}))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.NoError(t, mutateHosts(func(h map[string]config.HostConfig, _ string) error {
			delete(h, "drop")
			return nil
		}))
	}()
	wg.Wait()
	hosts, _, err := loadGlobalHosts()
	require.NoError(t, err)
	assert.Contains(t, hosts, "keep")
	assert.NotContains(t, hosts, "drop", "a concurrent add must not resurrect a removed host")
	assert.Len(t, hosts, 7)
}

func TestMutateHosts_ConflictRedoesTheEditAgainstTheFreshTable(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}}
	installHostsFake(t, f)
	calls := 0
	err := mutateHosts(func(h map[string]config.HostConfig, _ string) error {
		calls++
		if calls == 1 {
			// Another writer lands between this edit's read and its write.
			f.hosts["theirs"] = config.HostConfig{Host: "theirs.example"}
		}
		h["mine"] = config.HostConfig{Host: "mine.example"}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Contains(t, f.hosts, "theirs")
	assert.Contains(t, f.hosts, "mine")
}

func TestMutateHosts_ADuplicateAddedMeanwhileIsRefusedNotOverwritten(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}}
	installHostsFake(t, f)
	calls := 0
	err := mutateHosts(func(h map[string]config.HostConfig, _ string) error {
		calls++
		if calls == 1 {
			f.hosts["box1"] = config.HostConfig{Host: "other.example"}
		}
		if _, ok := h["box1"]; ok {
			return errors.New("already listed")
		}
		h["box1"] = config.HostConfig{Host: "mine.example"}
		return nil
	})
	require.Error(t, err)
	assert.Equal(t, "other.example", f.hosts["box1"].Host, "the concurrent winner is untouched")
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
	f.reply = daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{{Name: "box1", Probed: true, Reachable: true}}}
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
