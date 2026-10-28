package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

type hostsFake struct {
	hosts   map[string]config.HostConfig
	def     string
	sets    [][2]string
	checks  []daemon.CheckHostRequest
	reply   daemon.CheckHostResponse
	checkEr error
	setErr  error
}

func installHostsFake(t *testing.T, f *hostsFake) {
	t.Helper()
	pl, ps, pc := hostsLoad, hostsConfigSet, hostsCheckProbe
	hostsLoad = func() (map[string]config.HostConfig, string, error) {
		out := map[string]config.HostConfig{}
		for k, v := range f.hosts {
			out[k] = v
		}
		return out, f.def, nil
	}
	hostsConfigSet = func(key, value string) (daemon.SetConfigValueResponse, error) {
		if f.setErr != nil {
			return daemon.SetConfigValueResponse{}, f.setErr
		}
		f.sets = append(f.sets, [2]string{key, value})
		switch key {
		case "hosts":
			var m map[string]config.HostConfig
			require.NoError(t, json.Unmarshal([]byte(value), &m))
			f.hosts = m
		case "default_host":
			f.def = value
		}
		return daemon.SetConfigValueResponse{}, nil
	}
	hostsCheckProbe = func(req daemon.CheckHostRequest) (daemon.CheckHostResponse, error) {
		f.checks = append(f.checks, req)
		return f.reply, f.checkEr
	}
	t.Cleanup(func() {
		hostsLoad, hostsConfigSet, hostsCheckProbe = pl, ps, pc
		hostsAddIdentityFile, hostsAddWorkspaceRoot, hostsAddHostKeyPosture = "", "", ""
		hostsAddDefault, hostsAddNoCheck, hostsRemoveClearDefault, hostsJSONFlag = false, false, false, false
	})
}

func runHosts(t *testing.T, c *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	err := c.RunE(c, args)
	return out.String(), err
}

func TestParseHostDestination(t *testing.T) {
	cases := map[string]config.HostConfig{
		"box1.ts.net":          {Host: "box1.ts.net"},
		"ubuntu@box1.ts.net":   {Host: "box1.ts.net", User: "ubuntu"},
		"ubuntu@10.0.0.7:2222": {Host: "10.0.0.7", User: "ubuntu", Port: 2222},
		"2001:db8::1":          {Host: "2001:db8::1"},
	}
	for in, want := range cases {
		got, err := parseHostDestination(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := parseHostDestination("box:notaport")
	assert.Error(t, err)
}

func TestHostsAdd_SavesThenReportsMissingToolsWithFixes(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}, reply: daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{
		{Name: "box1", Probed: true, Reachable: true, Problems: []string{"git is not on the host's non-interactive PATH — install git there"}},
	}}}
	installHostsFake(t, f)
	out, err := runHosts(t, hostsAddCmd, "box1", "ubuntu@box1.ts.net")
	require.ErrorIs(t, err, errHostsNotReady, "a saved-but-not-ready host exits non-zero")
	assert.Contains(t, out, "saved host box1 (ubuntu@box1.ts.net)")
	assert.Contains(t, out, "fix: git is not on the host's non-interactive PATH")
	assert.Contains(t, out, "af hosts check box1")
	assert.Equal(t, "box1.ts.net", f.hosts["box1"].Host, "the entry is kept")
	assert.Equal(t, []daemon.CheckHostRequest{{Host: "box1"}}, f.checks)
}

func TestHostsAdd_UnreachableKeepsTheEntryAndNamesTheOperatorOwnedFix(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}, reply: daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{
		{Name: "box1", Probed: true, Error: "connection refused"},
	}}}
	installHostsFake(t, f)
	out, err := runHosts(t, hostsAddCmd, "box1", "box1.ts.net")
	require.ErrorIs(t, err, errHostsNotReady)
	assert.Contains(t, out, "connection refused")
	assert.Contains(t, out, "af opens no tunnel and edits no ssh config")
	assert.Contains(t, f.hosts, "box1")
}

func TestHostsAdd_ReadyAndFlags(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}, reply: daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{
		{Name: "gpu", Probed: true, Reachable: true, Readiness: "ready"},
	}}}
	installHostsFake(t, f)
	hostsAddDefault, hostsAddIdentityFile = true, "/home/me/.ssh/gpu"
	out, err := runHosts(t, hostsAddCmd, "gpu", "me@10.0.0.7:2222")
	require.NoError(t, err)
	assert.Contains(t, out, "gpu: ready")
	assert.Equal(t, config.HostConfig{Host: "10.0.0.7", User: "me", Port: 2222, IdentityFile: "/home/me/.ssh/gpu"}, f.hosts["gpu"])
	assert.Equal(t, "gpu", f.def)
}

func TestHostsAdd_RefusesBadInputBeforeWritingAnything(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{"taken": {Host: "x.example"}}}
	installHostsFake(t, f)
	for _, args := range [][]string{{"Bad Name", "box.example"}, {"ok", "-oProxyCommand=x"}, {"taken", "y.example"}} {
		_, err := runHosts(t, hostsAddCmd, args...)
		require.Error(t, err, args)
	}
	assert.Empty(t, f.sets, "nothing is written for a refused add")
	assert.Empty(t, f.checks)
}

func TestHostsAdd_NoCheckSkipsTheProbe(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}}
	installHostsFake(t, f)
	hostsAddNoCheck = true
	_, err := runHosts(t, hostsAddCmd, "box1", "box1.ts.net")
	require.NoError(t, err)
	assert.Empty(t, f.checks)
}

func TestHostsAdd_CheckThatCannotRunStillSaves(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{}, checkEr: errors.New("unknown method CheckHost")}
	installHostsFake(t, f)
	_, err := runHosts(t, hostsAddCmd, "box1", "box1.ts.net")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "af daemon restart")
	assert.Contains(t, f.hosts, "box1")
}

func TestHostsRemove_DefaultNeedsAnExplicitClear(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{"a": {Host: "a.example"}, "b": {Host: "b.example"}}, def: "a"}
	installHostsFake(t, f)
	_, err := runHosts(t, hostsRemoveCmd, "a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--clear-default")
	assert.Contains(t, f.hosts, "a")

	hostsRemoveClearDefault = true
	_, err = runHosts(t, hostsRemoveCmd, "a")
	require.NoError(t, err)
	assert.NotContains(t, f.hosts, "a")
	assert.Equal(t, "", f.def)
	require.Equal(t, "default_host", f.sets[0][0], "the default is cleared before the list is rewritten")

	_, err = runHosts(t, hostsRemoveCmd, "ghost")
	assert.Error(t, err, "an unlisted name is refused")
}

func TestHostsCheck_ExitCodesSeparateBlockedFromUnreachable(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{"a": {Host: "a.example"}}}
	installHostsFake(t, f)

	f.reply = daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{{Name: "a", Probed: true, Reachable: true}}}
	_, err := runHosts(t, hostsCheckCmd)
	assert.NoError(t, err)

	f.reply = daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{{Name: "a", Probed: true, Reachable: true, Problems: []string{"x"}}}}
	_, err = runHosts(t, hostsCheckCmd, "a")
	assert.ErrorIs(t, err, errHostsCheckHasProblems)

	f.reply = daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{{Name: "a", Probed: true, Error: "down"}}}
	_, err = runHosts(t, hostsCheckCmd)
	assert.ErrorIs(t, err, errHostsUnreachableOrBusy)
}

func TestHostsList(t *testing.T) {
	f := &hostsFake{hosts: map[string]config.HostConfig{"b": {Host: "b.example", Port: 22}, "a": {Host: "a.example", User: "u"}}, def: "b"}
	installHostsFake(t, f)
	out, err := runHosts(t, hostsListCmd)
	require.NoError(t, err)
	assert.Contains(t, out, "a\tu@a.example\n")
	assert.Contains(t, out, "b\tb.example:22 (default)\n")
}
