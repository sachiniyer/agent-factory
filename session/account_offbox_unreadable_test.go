package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// writeInvalidInRepoConfig drops a .agent-factory/config.json whose body is
// deliberately unparseable, so resolveRepoConfig fails on the repo.
func writeInvalidInRepoConfig(t *testing.T, repoRoot, body string) {
	t.Helper()
	dir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.ConfigFileName), []byte(body), 0o644))
}

// A listed host under an UNREADABLE project config must NOT be reported as
// "not listed": the resolve error is returned so the caller can surface the
// true cause instead of the unlisted-host remedy (#7078). Without the error, a
// listed host under an unresolvable config would be indistinguishable from a
// genuinely unlisted one.
func TestListedAccountHostReturnsErrorOnUnreadableConfig(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	writeInvalidInRepoConfig(t, f.repo, "{not json")

	_, ok, err := listedAccountHost(BackendSSH, f.repo, "box1")
	require.Error(t, err, "an unreadable config must surface its error, not false")
	assert.False(t, ok, "the licence is undeterminable, not granted")
}

// A genuinely unlisted host under a READABLE config returns ("", false, nil) —
// the error path is for "cannot determine", not for "determined: no".
func TestListedAccountHostReturnsFalseWithoutErrorForGenuinelyUnlisted(t *testing.T) {
	f := newListedFixture(t, oneListedHost)

	_, ok, err := listedAccountHost(BackendSSH, f.repo, "nope")
	require.NoError(t, err)
	assert.False(t, ok, "a host that is not in the list is not licensed — no error")
}

// af create --host <listed> --account <acct> on a repo whose project config
// cannot be parsed must surface the config error, NOT the "add the host under
// [hosts.<name>]" unlisted-host remedy — the host IS listed and already picked,
// so that remedy would send the operator to fix a host-list entry that is
// already correct and mask the real cause (#7078).
func TestCreateOnListedHostWithAccountSurfacesConfigErrorNotUnlistedRemedy(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	writeInvalidInRepoConfig(t, f.repo, "{not json")

	_, err := f.create(t, InstanceOptions{Account: "work", Host: "box1"})
	require.Error(t, err)
	msg := err.Error()

	assert.Contains(t, msg, "work", "the operator must see WHICH account was refused")
	assert.Contains(t, msg, "ssh", "the refusal must name the backend")
	assert.Contains(t, msg, "could not be resolved",
		"the refusal must surface the config-resolve failure as the cause")
	assert.NotContains(t, msg, "add the host under [hosts.<name>]",
		"the host IS listed and already picked; the unlisted-host remedy masks the real cause")
	assert.NotContains(t, msg, "global host list",
		"the refusal must not claim the host is not listed when it is")
	assert.Empty(t, f.spec.Account.Dir, "nothing was provisioned, so nothing was sent")
}

// af create --host <listed> --account <acct> on a repo with a READABLE config
// is admitted and the account is provisioned — the fix does not break the
// happy path.
func TestCreateOnListedHostWithAccountStillAdmittedOnReadableConfig(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	inst, err := f.create(t, InstanceOptions{Account: "work", Host: "box1"})
	require.NoError(t, err)
	assert.Equal(t, "work", inst.Account, "the session records the account it runs under")
	assert.Equal(t, f.accountDir, f.spec.Account.Dir, "the registry directory, resolved on the daemon box")
}

// af create --host <unlisted> --account <acct> on a repo with a READABLE
// config is still refused with the unlisted-host remedy — the fix does not
// break the genuine-unlisted refusal.
func TestCreateOnUnlistedHostWithAccountStillRefusedWithRemedy(t *testing.T) {
	f := newListedFixture(t, oneListedHost)

	_, err := f.create(t, InstanceOptions{Account: "work", Host: "nope"})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "global host list", "the reason: credentials go only to a host the operator listed")
	assert.Contains(t, msg, "[hosts.<name>]", "the remedy: list the host")
	assert.Contains(t, msg, "--host", "and pick it")
	assert.Contains(t, msg, "omit --account", "or drop the account")
}

// accountCarried on an unreadable config returns (false, err), not (false,
// nil) — the error is what lets the gate surface the true cause.
func TestAccountCarriedReturnsErrorOnUnreadableConfig(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	writeInvalidInRepoConfig(t, f.repo, "{not json")

	carried, err := accountCarried(BackendSSH, f.repo, "box1")
	require.Error(t, err)
	assert.False(t, carried, "the licence is undeterminable, not granted")
}

// accountCarried on a docker kind returns (true, nil) regardless of config —
// docker carries by mount, not by listed-host licence, so an unreadable config
// must not affect its answer.
func TestAccountCarriedReturnsTrueForDockerRegardlessOfConfig(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	writeInvalidInRepoConfig(t, f.repo, "{not json")

	carried, err := accountCarried(BackendDocker, f.repo, "")
	require.NoError(t, err)
	assert.True(t, carried, "docker carries by mount, not by listed-host licence")
}
