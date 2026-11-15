package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// An adopting provision sends its account over the transport the archived row
// recorded, so the licence has to name THAT machine (#7336). These tests run the
// real licence step (sshProvisionHost) and the real provisioner over a fake
// transport, and watch what reaches it.

// errStopAtLaunch ends the fake provision at the agent-server launch, which is
// after the account sync.
var errStopAtLaunch = errors.New("fake transport: stopping at the agent-server launch")

// adoptOverFakeTransport is the ssh runtime with its transport replaced: the
// licence step and the provisioner are the real ones, and every host command
// lands in scripts instead of on a host.
type adoptOverFakeTransport struct {
	t       *testing.T
	afBin   string
	scripts *[]string
	// licence is the AccountSyncHost the licence step granted.
	licence *string
}

func (r adoptOverFakeTransport) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	cfg, err := resolveRepoConfig(spec.RepoRoot)
	require.NoError(r.t, err)
	if _, _, err := sshProvisionHost(&spec, cfg); err != nil {
		return ProvisionResult{}, err
	}
	*r.licence = spec.AccountSyncHost
	// As sshRuntime.Provision composes it: an adoption rides the recorded command.
	p := newSSHSandboxProvisioner(spec, adoptedTransportOr(spec, "ssh the-current-config-host"), r.afBin, tmux.ProgramClaude)
	stage := binaryStageReplies(r.t, r.afBin)
	p.runCommandFn = func(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
		*r.scripts = append(*r.scripts, script)
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		if out, ok := stage(script); ok {
			return out, nil
		}
		switch {
		case strings.Contains(script, `echo "PRESENT `):
			return []byte("PRESENT " + spec.Adopt.SessionDir + "/worktree\n"), nil
		case strings.Contains(script, "wc -c <"):
			return []byte("12\n"), nil
		case strings.Contains(script, "nohup"):
			return nil, errStopAtLaunch
		}
		return nil, nil
	}
	p.startTunnelFn = func(string) (string, error) { return "127.0.0.1:1", nil }
	_, err = p.provision()
	return ProvisionResult{}, err
}

// accountSyncs returns the host commands that unpack an account's credentials.
func accountSyncs(scripts []string) []string {
	var out []string
	for _, s := range scripts {
		if strings.Contains(s, "tar -xf - -C") && strings.Contains(s, "/account") {
			out = append(out, s)
		}
	}
	return out
}

// adoptingRestore archives nothing and contacts nothing: it builds the row the
// issue describes — account "work", preserved on box1 over a recorded transport
// — and runs the adopting re-provision over the fake transport.
func adoptingRestore(t *testing.T, f listedFixture, rowHost string) (scripts []string, licence string, err error) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.accountDir, ".credentials.json"), []byte(`{"token":"t"}`), 0o600))
	afBin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(afBin, []byte("fake-af-biny"), 0o755))
	t.Cleanup(SetRuntimeForTest(BackendSSH, func() Runtime {
		return adoptOverFakeTransport{t: t, afBin: afBin, scripts: &scripts, licence: &licence}
	}))
	const dir = "/home/dev/.af-sessions/s.AbCdEf"
	row := accountRow(f, rowHost, nil)
	row.archivedWorkspace = &ArchivedWorkspaceData{Host: rowHost, Dir: dir, Branch: "root/s"}
	err = row.reprovisionRemoteWith(&AdoptSpec{
		SessionDir: dir, Branch: "root/s",
		Transport: &RecordedTransport{
			SSHCmd: "ssh -F none dev@box1.example",
			// What the recorded command dials, as the cleanup record keeps it.
			SSH: &SSHRuntimeCleanupData{HostName: "box1", SessionDir: dir, Config: config.SSHConfig{Host: "box1.example", User: "dev"}},
		},
	})
	return scripts, licence, err
}

// The issue's sequence. The row records no host (an account swapped into an
// accountless default_host session leaves it empty). Its workspace is preserved
// on box1. box1 is then removed from the host list and default_host becomes
// box2. The restore's licence read resolves box2 — but the recorded transport
// still reaches box1, a host the operator removed. No account may go there.
func TestAdoptionSendsNoAccountToARecordedHostThatIsNoLongerTheLicensedOne(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
	saveDefaultHost(t, map[string]config.HostConfig{"box2": hosts["box2"]}, "box2")

	scripts, licence, err := adoptingRestore(t, f, "")
	require.Error(t, err)
	assert.Empty(t, accountSyncs(scripts),
		"the account was synced over the recorded transport to box1, which is not in the host list; the licence named box2")
	assert.Empty(t, licence, "no sync licence is granted for a machine the licence does not name")
	assert.Empty(t, scripts, "and the refusal comes before anything is run on the host")

	msg := err.Error()
	assert.Contains(t, msg, `account "work"`)
	assert.Contains(t, msg, `host "box1"`, "names the host the workspace is on")
	assert.Contains(t, msg, "no longer in the global host list", "says why")
	assert.Contains(t, msg, "[hosts.box1]", "says what to re-add")
	assert.Contains(t, msg, "clear the session's account", "or how to restore without the account")
	assert.NotContains(t, msg, "already retired", "an adoption reaps nothing")
}

// The recorded host is still listed, but the row's empty host now resolves to a
// different listed host. The licence still names a machine the transport does
// not reach, so nothing is sent — and the remedy is not "re-add it".
func TestAdoptionSendsNoAccountWhenTheLicenceResolvesAnotherListedHost(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
	saveDefaultHost(t, hosts, "box2")

	scripts, licence, err := adoptingRestore(t, f, "")
	require.Error(t, err)
	assert.Empty(t, accountSyncs(scripts))
	assert.Empty(t, licence)
	msg := err.Error()
	assert.Contains(t, msg, `host "box1"`)
	assert.Contains(t, msg, `host "box2"`, "names both hosts")
	assert.NotContains(t, msg, "no longer in the global host list", "box1 is still listed")
	assert.Contains(t, msg, "default_host")
}

// The converse: a recorded transport whose host is still listed and is the
// licensed host syncs exactly as before — for a row pinned to it and for an
// empty-host row that default_host still resolves to it.
func TestAdoptionSyncsTheAccountWhenTheRecordedHostIsTheLicensedHost(t *testing.T) {
	for name, rowHost := range map[string]string{"a row pinned to box1": "box1", "an empty-host row under default_host = box1": ""} {
		t.Run(name, func(t *testing.T) {
			hosts := twoListedHosts(t.TempDir())
			f := newListedFixture(t, hosts)
			writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
			saveDefaultHost(t, hosts, "box1")

			scripts, licence, err := adoptingRestore(t, f, rowHost)
			require.ErrorIs(t, err, errStopAtLaunch, "premise: the provision ran past the account sync to the launch")
			assert.Equal(t, "box1", licence, "licensed under the name of the machine the transport reaches")
			syncs := accountSyncs(scripts)
			require.Len(t, syncs, 1, "the account is synced once, over the recorded transport")
			assert.Contains(t, syncs[0], "/home/dev/.af-sessions/s.AbCdEf/account")
		})
	}
}

// An accountless adoption is untouched: it is never refused for its host, and
// it never syncs an account.
func TestAccountlessAdoptionIgnoresTheHostList(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
	saveDefaultHost(t, map[string]config.HostConfig{"box2": hosts["box2"]}, "box2")
	afBin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(afBin, []byte("fake-af-biny"), 0o755))
	var scripts []string
	var licence string
	t.Cleanup(SetRuntimeForTest(BackendSSH, func() Runtime {
		return adoptOverFakeTransport{t: t, afBin: afBin, scripts: &scripts, licence: &licence}
	}))
	const dir = "/home/dev/.af-sessions/s.AbCdEf"
	row := accountRow(f, "box1", nil)
	row.Account = ""
	row.archivedWorkspace = &ArchivedWorkspaceData{Host: "box1", Dir: dir, Branch: "root/s"}
	err := row.reprovisionRemoteWith(&AdoptSpec{
		SessionDir: dir, Branch: "root/s",
		Transport: &RecordedTransport{SSHCmd: "ssh -F none dev@box1.example", SSH: &SSHRuntimeCleanupData{HostName: "box1", SessionDir: dir}},
	})
	require.ErrorIs(t, err, errStopAtLaunch, "the adoption ran to the launch although box1 left the host list")
	assert.Empty(t, accountSyncs(scripts))
}
