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
