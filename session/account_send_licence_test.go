package session

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// Every send of an account to an off-box host is licensed by the host list as it
// is AT THE SEND, for the machine the send actually reaches (#7342): the
// destination the transport dials must be the destination the licensed [hosts]
// entry names now. These tests change the host list after the session's
// transport was fixed and watch what reaches a fake transport.

// reAddressed returns hosts with box1 pointing at another machine.
func reAddressed(hosts map[string]config.HostConfig) map[string]config.HostConfig {
	out := map[string]config.HostConfig{}
	for k, v := range hosts {
		out[k] = v
	}
	b := out["box1"]
	b.Host = "other-machine.example"
	out["box1"] = b
	return out
}

// Shape 1: the row's workspace is preserved on dev@box1.example. The operator
// edits [hosts.box1] to name another machine. The key still matches, so the
// adoption is licensed by name — and would sync the account over the recorded
// command to the old machine, which is now under no name in the list.
func TestAdoptionSendsNoAccountToAHostEntryThatWasReAddressed(t *testing.T) {
	for name, rowHost := range map[string]string{"a row pinned to box1": "box1", "an empty-host row under default_host = box1": ""} {
		t.Run(name, func(t *testing.T) {
			hosts := twoListedHosts(t.TempDir())
			f := newListedFixture(t, hosts)
			writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
			saveDefaultHost(t, reAddressed(hosts), "box1")

			scripts, _, err := adoptingRestore(t, f, rowHost)
			require.Error(t, err)
			assert.NotErrorIs(t, err, errStopAtLaunch, "the provision must not run past the account sync")
			assert.Empty(t, accountSyncs(scripts),
				"the account was synced over the recorded transport to dev@box1.example, but [hosts.box1] now names other-machine.example")
		})
	}
}

// A change that does not move the machine does not refuse: the same address,
// user and port under a new identity file and host-key posture still syncs.
func TestAdoptionStillSyncsWhenOnlyTheIdentityFileOrPostureChanged(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
	changed := map[string]config.HostConfig{"box2": hosts["box2"]}
	b := hosts["box1"]
	b.IdentityFile = filepath.Join(t.TempDir(), "a-new-key")
	b.HostKeyVerification = "accept-new"
	changed["box1"] = b
	saveDefaultHost(t, changed, "box1")

	scripts, licence, err := adoptingRestore(t, f, "box1")
	require.ErrorIs(t, err, errStopAtLaunch, "premise: the provision ran past the account sync to the launch")
	assert.Equal(t, "box1", licence)
	assert.Len(t, accountSyncs(scripts), 1, "the account is synced: the machine did not move")
}

// recordHostCommands wraps the instance's transport so every script that
// reaches the host is recorded.
func recordHostCommands(inst *Instance) *[]string {
	var scripts []string
	p := inst.sandboxProvisioner()
	run := p.runCommandFn
	p.runCommandFn = func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
		scripts = append(scripts, script)
		return run(d, script, stdin, combined)
	}
	return &scripts
}

// accountWrites returns the host commands that unpack an archive into an
// account scope (the live one or the staged next one).
func accountWrites(scripts []string) []string {
	var out []string
	for _, s := range scripts {
		if strings.Contains(s, "tar -xf - -C") && strings.Contains(s, "/account") {
			out = append(out, s)
		}
	}
	return out
}

// Shape 2: a session reattached after a daemon restart keeps the licence its
// cleanup record carries. After [hosts.box1] is removed or re-addressed, an
// account handoff (manual or limit failover) must not send the INCOMING account
// to the machine the session is on.
func TestAccountHandoffSendsNothingAfterItsHostEntryIsRemovedOrReAddressed(t *testing.T) {
	for name, edit := range map[string]func(map[string]config.HostConfig) map[string]config.HostConfig{
		"the entry is removed":      func(map[string]config.HostConfig) map[string]config.HostConfig { return nil },
		"the entry is re-addressed": reAddressed,
	} {
		t.Run(name, func(t *testing.T) {
			f := newSwapFixture(t)
			inst := restartedFrom(t, f, "box1")
			scripts := recordHostCommands(inst)
			saveDefaultHost(t, edit(oneListedHost), "")

			require.Error(t, inst.ValidateOffBoxAccountSwap("alt"), "admission refuses, so the scheduler never picks this session")
			_, err := inst.SwapOffBoxAccount("alt", false)
			require.Error(t, err)
			assert.Empty(t, accountWrites(*scripts), "the incoming account was sent to a machine the host list no longer names")
			assert.NoDirExists(t, filepath.Join(f.dir, "account.next"), "nothing was staged")
			assert.Equal(t, secretWork, f.hostCred(t, "account"), "the session keeps running on its current account")
			assert.Zero(t, f.srv.calls, "and the agent was never asked to rotate")
			acct, _ := inst.AccountSelection()
			assert.Equal(t, "work", acct)
		})
	}
}

// Control for shape 2: with the entry unchanged the handoff runs.
func TestAccountHandoffStillRunsWhenItsHostEntryIsUnchanged(t *testing.T) {
	f := newSwapFixture(t)
	inst := restartedFrom(t, f, "box1")
	scripts := recordHostCommands(inst)

	require.NoError(t, inst.ValidateOffBoxAccountSwap("alt"))
	_, err := inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	assert.Len(t, accountWrites(*scripts), 1)
	assert.Equal(t, secretAlt, f.hostCred(t, "account"))
}

// The read-back is the exception. Credentials already on a host the operator
// has since removed from the list must still be readable back and removable:
// teardown reaches the recorded machine and takes them off it.
func TestTeardownStillRemovesTheAccountFromAHostThatLeftTheHostList(t *testing.T) {
	for name, edit := range map[string]func(map[string]config.HostConfig) map[string]config.HostConfig{
		"the entry is removed":      func(map[string]config.HostConfig) map[string]config.HostConfig { return nil },
		"the entry is re-addressed": reAddressed,
	} {
		t.Run(name, func(t *testing.T) {
			f := newSwapFixture(t)
			inst := restartedFrom(t, f, "box1")
			scripts := recordHostCommands(inst)
			saveDefaultHost(t, edit(oneListedHost), "")

			w := inst.sandboxProvisioner().account
			require.NoError(t, w.teardownAccount(time.Minute))
			var readBack bool
			for _, s := range *scripts {
				readBack = readBack || strings.Contains(s, "tar -cf -")
			}
			assert.True(t, readBack, "the scope is still read back from the recorded machine")
			_, err := os.Stat(filepath.Join(f.dir, "account"))
			assert.True(t, os.IsNotExist(err), "and the credentials are removed from it")
		})
	}
}

// saveBox1 rewrites the host list with box1 as entry (and nothing else).
func saveBox1(t *testing.T, entry config.HostConfig) {
	t.Helper()
	saveDefaultHost(t, map[string]config.HostConfig{"box1": entry}, "")
}

// The check itself, at a workspace licensed as box1 and dialling dev@box1.example.
// Every refusal sends nothing and names something the operator can do.
func TestAccountSendLicenceIsTheHostListAtTheSend(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	box1 := oneListedHost["box1"]
	send := func(t *testing.T) (*recordingShell, error) {
		t.Helper()
		sh := &recordingShell{}
		w := workspaceOn(t, acct, "box1")
		w.shell = sh
		return sh, w.syncAccountIn(time.Second)
	}
	refused := func(t *testing.T) string {
		t.Helper()
		sh, err := send(t)
		require.Error(t, err)
		assert.Empty(t, sh.scripts, "nothing reaches the transport")
		assert.Empty(t, sh.stdin, "and no credential byte is written to it")
		msg := err.Error()
		assert.Contains(t, msg, `account "work-claude" is not sent`)
		assert.Contains(t, msg, "Nothing was sent")
		assert.Contains(t, msg, "try again", "names something the operator can do")
		assert.NotContains(t, msg, "clear the session's account")
		assert.NotContains(t, msg, testSecretA)
		return msg
	}
	sent := func(t *testing.T) {
		t.Helper()
		sh, err := send(t)
		require.NoError(t, err)
		assert.Len(t, sh.scripts, 1, "the account is synced")
	}

	t.Run("unchanged entry", func(t *testing.T) { saveBox1(t, box1); sent(t) })

	t.Run("same machine, new identity file, posture and workspace root", func(t *testing.T) {
		e := box1
		e.IdentityFile, e.HostKeyVerification, e.WorkspaceRoot = filepath.Join(t.TempDir(), "k"), "accept-new", "/srv/af"
		saveBox1(t, e)
		sent(t)
	})
	t.Run("same machine, spelled in another case and with the default port written out", func(t *testing.T) {
		e := box1
		e.Host, e.Port = "BOX1.example", 22
		saveBox1(t, e)
		sent(t)
	})

	t.Run("re-addressed", func(t *testing.T) {
		e := box1
		e.Host = "other-machine.example"
		saveBox1(t, e)
		msg := refused(t)
		assert.Contains(t, msg, "[hosts.box1] now names dev@other-machine.example")
		assert.Contains(t, msg, "this session's workspace is on dev@box1.example")
		assert.Contains(t, msg, "Point [hosts.box1] back at dev@box1.example")
	})
	t.Run("another port", func(t *testing.T) {
		e := box1
		e.Port = 2222
		saveBox1(t, e)
		assert.Contains(t, refused(t), "dev@box1.example (port 2222)")
	})
	t.Run("another remote user", func(t *testing.T) {
		// The account scope lands in the remote user's home, so the user is part
		// of where a send goes.
		e := box1
		e.User = "someone-else"
		saveBox1(t, e)
		assert.Contains(t, refused(t), "someone-else@box1.example")
	})
	t.Run("removed", func(t *testing.T) {
		saveDefaultHost(t, map[string]config.HostConfig{"box2": {Host: "box2.example", User: "dev"}}, "")
		msg := refused(t)
		assert.Contains(t, msg, `host "box1" is no longer in the global host list`)
		assert.Contains(t, msg, "Re-add it under [hosts.box1]")
		assert.Contains(t, msg, "dev@box1.example", "and says what it has to name")
	})
	t.Run("now uses the operator's ssh_config", func(t *testing.T) {
		e := box1
		e.UseSSHConfig = true
		saveBox1(t, e)
		assert.Contains(t, refused(t), "use_ssh_config")
	})
	t.Run("the global config cannot be read", func(t *testing.T) {
		saveBox1(t, box1)
		dir, err := config.GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, config.TomlConfigFileName), []byte("hosts = [not toml"), 0o600))
		assert.Contains(t, refused(t), "the global config could not be read")
	})

	t.Run("a transport that recorded no destination", func(t *testing.T) {
		saveBox1(t, box1)
		sh := &recordingShell{}
		w := workspaceOn(t, acct, "box1")
		w.shell, w.spec.AccountSyncTarget = sh, accountSyncTarget{}
		err := w.syncAccountIn(time.Second)
		require.Error(t, err)
		assert.Empty(t, sh.scripts)
		assert.Contains(t, err.Error(), "bug in af")
	})
}

// The other senders ask the same question at their own send: the handoff's
// staging (after admission already passed) and the operator's agent config.
func TestEverySenderIsLicensedAtItsOwnSend(t *testing.T) {
	t.Run("staging the incoming account", func(t *testing.T) {
		f := newSwapFixture(t)
		require.NoError(t, f.inst.ValidateOffBoxAccountSwap("alt"), "premise: admission passes while the entry is unchanged")
		next, err := selectAccountInNamespace("claude", "alt")
		require.NoError(t, err)
		saveDefaultHost(t, reAddressed(oneListedHost), "") // …and the entry moves before the stage
		*f.shellCalls = nil

		_, _, err = f.w.stageAccountNext(time.Minute, next)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `account "alt" is not sent`)
		assert.Empty(t, accountWrites(*f.shellCalls))
		assert.NoDirExists(t, filepath.Join(f.dir, "account.next"))
	})

	t.Run("a handoff on a live session", func(t *testing.T) {
		f := newSwapFixture(t)
		saveDefaultHost(t, nil, "")
		*f.shellCalls = nil
		_, err := f.inst.SwapOffBoxAccount("alt", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no longer in the global host list")
		assert.Empty(t, accountWrites(*f.shellCalls))
		assert.Zero(t, f.srv.calls)
		assert.Equal(t, secretWork, f.hostCred(t, "account"), "the session keeps its current account")
	})

	t.Run("the operator's agent config", func(t *testing.T) {
		operatorHome(t)
		w := configWorkspace(t, "claude", "box1", "claude:settings.json")
		sh := &recordingShell{}
		w.shell = sh
		saveDefaultHost(t, reAddressed(oneListedHost), "")
		_, err := w.syncAgentConfigIn(time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config is not placed on the host")
		assert.Empty(t, sh.scripts)
		assert.Empty(t, sh.stdin)

		saveDefaultHost(t, oneListedHost, "")
		_, err = w.syncAgentConfigIn(time.Second)
		require.NoError(t, err)
		assert.Len(t, sh.scripts, 1, "control: with the entry back, the config is placed")
	})
}

// A sandbox command names no listed entry's machine. A workspace rebuilt from a
// sandbox cleanup record therefore has no destination to check a send against:
// it can be read back and removed, and nothing more is sent to it.
func TestRestoredSandboxWorkspaceCanBeReadBackButNotSentTo(t *testing.T) {
	f := newSwapFixture(t)
	data := &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh sandbox.invalid", SessionDir: f.dir, AccountSyncHost: "box1",
	}}
	backend, _, err := restoreRuntimeCleanupWithAccount("s", "sandbox", data, restoredAccount{Name: "work", Agent: "claude", Program: "claude"})
	require.NoError(t, err)
	w := backend.(*sandboxBackend).provisioner.account
	require.NotNil(t, w)
	ok, err := w.accountSyncApplies()
	require.NoError(t, err)
	assert.True(t, ok, "the read-back keeps its recorded licence")
	ok, err = w.accountSendApplies()
	assert.False(t, ok)
	require.Error(t, err, "a send has no destination to be licensed for")
}
