package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The listed-host licence is resolved ONCE in NewInstance and threaded into the
// backend factory and recordedAccountHost, so the admission gate, provisioning
// and the recorded host pin cannot disagree about whether a create's account may
// be carried after a concurrent config change lands between two reads. These
// tests pin that single-read invariant: the factory uses the gate's threaded
// decision (not its own re-read), it fails closed when an off-box account is not
// carried, and the record pins the host the gate resolved (not a third
// re-derivation).

// accountCarriedHost reports the host name AND carried flag in one read. For a
// kind that carries by another mechanism (docker's mount) the host name is empty
// even though carried is true; for a kind that does not carry (local, sandbox,
// hook, or ssh on an unlisted target) both are empty/false; an unreadable config
// surfaces as an error so a listed host is never misreported as unlisted.
func TestAccountCarriedHostReturnsHostAndCarriedFlag(t *testing.T) {
	f := newListedFixture(t, oneListedHost)

	// SSH on a listed host: carried, and the resolved name is the picked host.
	name, carried, err := accountCarriedHost(BackendSSH, f.repo, "box1")
	require.NoError(t, err)
	assert.True(t, carried, "a listed host carries the account")
	assert.Equal(t, "box1", name, "the resolved name is the pick")

	// SSH on a name that is not in the host list: not carried, no error.
	name, carried, err = accountCarriedHost(BackendSSH, f.repo, "nope")
	require.NoError(t, err)
	assert.False(t, carried)
	assert.Empty(t, name)

	// Docker: carried by mount, with no listed-host name (it does not use one).
	name, carried, err = accountCarriedHost(BackendDocker, f.repo, "")
	require.NoError(t, err)
	assert.True(t, carried, "docker carries by mount, not by listed-host licence")
	assert.Empty(t, name, "docker's carried-ness does not resolve a listed host")

	// sandbox, hook and local: not carried (sandbox/hook have no listed-host
	// licence; local resolves the account in the in-target shim, not here).
	for _, kind := range []BackendKind{BackendSandbox, BackendHook, BackendLocal} {
		name, carried, err = accountCarriedHost(kind, f.repo, "box1")
		require.NoError(t, err, kind)
		assert.False(t, carried, kind)
		assert.Empty(t, name, kind)
	}

	// An unreadable project config surfaces an error rather than reporting a
	// listed host as unlisted (#7078).
	writeInvalidInRepoConfig(t, f.repo, "{not json")
	name, carried, err = accountCarriedHost(BackendSSH, f.repo, "box1")
	require.Error(t, err, "an unreadable config must surface its error, not false")
	assert.False(t, carried, "the licence is undeterminable, not granted")
	assert.Empty(t, name)
}

// accountCarried is the bool-only view of the same single read, so it stays
// consistent with accountCarriedHost's carried flag and error across every
// kind and config state.
func TestAccountCarriedDelegatesToAccountCarriedHost(t *testing.T) {
	f := newListedFixture(t, oneListedHost)

	check := func(kind BackendKind, pick string) {
		t.Helper()
		wantHost, wantCarried, wantErr := accountCarriedHost(kind, f.repo, pick)
		gotCarried, gotErr := accountCarried(kind, f.repo, pick)
		assert.Equal(t, wantCarried, gotCarried, "carried mismatch for %s/%q", kind, pick)
		if wantErr == nil {
			assert.NoError(t, gotErr, "unexpected error for %s/%q", kind, pick)
		} else {
			assert.Error(t, gotErr, "expected an error for %s/%q", kind, pick)
		}
		_ = wantHost
	}

	check(BackendSSH, "box1")
	check(BackendSSH, "nope")
	check(BackendSSH, "")
	check(BackendDocker, "")
	check(BackendSandbox, "box1")
	check(BackendHook, "box1")
	check(BackendLocal, "box1")
}

// A config change that flips the empty host pick from a listed host to a
// repo-supplied [ssh] host AFTER the admission gate admitted the account must
// NOT make the backend factory silently drop it. The factory uses the gate's
// threaded decision (opts.AccountCarried) instead of re-reading the licence, so
// the account is resolved onto the provision even though a re-read would now
// say "not carried" — closing the gate-to-factory window the bug opened. The
// recorded host is pinned to the name the gate resolved before the flip, not a
// third re-derivation the flip would have left empty.
//
// This test FAILS on the pre-fix code: the factory's independent accountCarried
// re-read returns false after the flip, spec.Account stays zero, and inst.Host
// is "" (recordedAccountHost's third read also sees the flip).
func TestFactoryResolvesTheAccountEvenWhenConfigFlipsUnlistedAfterAdmission(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	// default_host = box1; no [ssh] section: the empty pick resolves through
	// default_host to the listed box1, so the gate admits the account.
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.DefaultHost = "box1"
	require.NoError(t, config.SaveConfig(cfg))
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})

	// Wrap the factory so the in-repo config flips to a repo-supplied [ssh]
	// host AFTER the admission gate read (which saw box1 listed and admitted)
	// but BEFORE the real factory's account resolution. This is the exact
	// state change that made the OLD factory's independent accountCarried
	// re-read return unlisted and silently drop the account.
	restore := SetProvisionResultFactoryForTest(func(opts InstanceOptions, absPath string, kind BackendKind) (ProvisionResult, error) {
		writeInRepoConfig(t, absPath, map[string]any{
			"backend": "ssh",
			"ssh":     map[string]any{"host": "repo-chosen.example"},
		})
		return defaultBackendFactoryForKind(opts, absPath, kind)
	})
	t.Cleanup(restore)

	*f.spec = ProvisionSpec{}
	inst, err := f.create(t, InstanceOptions{Account: "work"}) // no --host: default_host applies
	require.NoError(t, err)

	assert.Equal(t, "work", inst.Account,
		"the row records the account the gate admitted")
	assert.Equal(t, f.accountDir, f.spec.Account.Dir,
		"the factory resolved the account from the gate's threaded decision, not a re-read the flip made unlisted")
	assert.Equal(t, "work", f.spec.Account.Name,
		"the provisioned spec carries the account name")
	assert.Equal(t, "box1", inst.Host,
		"the record pins the host the gate resolved before the flip, not a third re-derivation the flip would have left empty")
}

// A create that names an account on an off-box kind the licence did not carry
// must fail closed rather than silently provision on ambient credentials while
// the row would record the account. The admission gate refuses or drops an
// uncarried account BEFORE the factory runs, so reaching the factory with an
// off-box account the licence did not carry means the gate's threaded decision
// was not honoured; the factory mirrors reprovisionRemote's "under account X or
// not at all" instead of failing open.
func TestFactoryFailsClosedWhenAnOffBoxAccountIsNotCarried(t *testing.T) {
	f := newListedFixture(t, oneListedHost)

	// Directly exercise the factory with a (impossible-in-production) call
	// whose threaded decision says "not carried" yet names an account on an
	// off-box kind. The factory must refuse, not provision silently.
	opts := InstanceOptions{
		Title:          "fail-closed",
		Path:           f.repo,
		Program:        tmux.ProgramClaude,
		Account:        "work",
		AccountCarried: false,
	}
	_, err := defaultBackendFactoryForKind(opts, f.repo, BackendSSH)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"work"`, "the refusal names the account")
	assert.Contains(t, err.Error(), "ssh", "the refusal names the backend")
	assert.Contains(t, err.Error(), "listed-host licence",
		"the refusal states the licence was not carried")
	assert.Empty(t, f.spec.Account.Dir,
		"nothing was provisioned, so no credential was sent")
}

// A LOCAL create with a named account is the ONE case that reaches the factory
// with AccountCarried false on purpose: the account is resolved by the
// in-target shim, not placed by the factory, so the factory must NOT resolve it
// onto the provision (and must NOT fail closed). This pins that the fail-closed
// guard does not over-reach onto the local path.
func TestFactoryLeavesLocalAccountUnplaced(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	// Capture the spec the local runtime was handed so the test can assert the
	// factory left Account zero.
	spec := &ProvisionSpec{}
	t.Cleanup(SetRuntimeForTest(BackendLocal, func() Runtime { return capturingRuntime{got: spec} }))
	opts := InstanceOptions{
		Title:          "local-acct",
		Path:           f.repo,
		Program:        tmux.ProgramClaude,
		Account:        "work",
		AccountCarried: false, // local never carries via the licence
	}
	res, err := defaultBackendFactoryForKind(opts, f.repo, BackendLocal)
	require.NoError(t, err, "a local create with a named account is not a factory error")
	assert.NotNil(t, res.Backend)
	assert.Empty(t, spec.Account.Dir, "the factory leaves spec.Account zero; the shim resolves it")
}

// The recorded host is pinned to the name the admission gate resolved, so a
// concurrent config change between the gate and the record cannot move a
// session, and its credentials, to a different target on restore. This is the
// third-read divergence the single-read fix closes: recordedAccountHost reuses
// the gate's answer instead of re-deriving it.
func TestRecordedAccountHostIsTheGatesResolvedName(t *testing.T) {
	// An explicit --host is recorded verbatim.
	assert.Equal(t, "box1", recordedAccountHost(InstanceOptions{Host: "box1", Account: "work"}, "ignored"))
	// An accountless create records the pick verbatim (empty here).
	assert.Equal(t, "", recordedAccountHost(InstanceOptions{Host: "", Account: ""}, "box1"))
	// An account-scoped create reached through default_host records the
	// gate-resolved name, not the empty pick.
	assert.Equal(t, "box1", recordedAccountHost(InstanceOptions{Host: "", Account: "work"}, "box1"))
	// When the gate resolved no listed host (e.g. an unlisted empty pick), the
	// record stays empty rather than pinning a name the gate never admitted.
	assert.Equal(t, "", recordedAccountHost(InstanceOptions{Host: "", Account: "work"}, ""))
}
