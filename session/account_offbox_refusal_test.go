package session

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The off-box account refusal is a DECISION, and the message has to say the true
// reason (#3103).
//
// The merged wording said af "cannot place a credential account on that machine"
// for every off-box kind. That is FALSE for ssh and provably so — the runtime
// streams af's own binary there, creates a per-session directory and starts a
// process in it. Placement was never the obstacle; the ROUND TRIP is. A refusal
// that misstates its reason invites the operator to conclude af just has not
// wired it up, and to file the same issue again.

// ssh must not be told af cannot put the account there, because it can.
func TestSSHAccountRefusalNamesTheRoundTripNotPlacement(t *testing.T) {
	err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendSSH})
	require.Error(t, err)
	msg := err.Error()

	assert.Contains(t, msg, "work", "the operator must see WHICH account was refused")
	assert.Contains(t, msg, "ssh")
	assert.NotContains(t, strings.ToLower(msg), "cannot place",
		"af CAN place a directory on an ssh host — claiming otherwise reads as unfinished wiring")
	assert.Contains(t, msg, "deliberately does not",
		"the refusal must read as a decision, not a gap")

	// The actual reason, in the operator's terms: writes do not come home.
	assert.Contains(t, msg, "cannot establish that those writes come back",
		"the reason is the write-back af cannot guarantee, stated as the property rather than a mechanism")
	assert.Contains(t, msg, "rotates refresh tokens",
		"and the worst case — invalidating the copy on THIS machine — is the part that decides it")
	assert.Contains(t, msg, "--account", "and it must name the way out")
}

// sandbox and hook must give the ROUND-TRIP reason, not a location one.
//
// The first cut of this change said they "provision through your own command or
// scripts, so af has no location it can prove is the account". That is false in
// the same way the old blanket wording was false for ssh: sandbox and
// provision-mode hook both run through sandboxProvisioner, which creates the
// session directory itself and streams af's binary into it, so af controls those
// locations exactly as it does for ssh. Only hook's launch_cmd mode leaves the
// machine's shape to the operator — and the round trip is what refuses all of
// them anyway.
func TestSandboxAndHookAccountRefusalGivesTheRoundTripReason(t *testing.T) {
	for _, kind := range []BackendKind{BackendSandbox, BackendHook} {
		t.Run(string(kind), func(t *testing.T) {
			err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: kind})
			require.Error(t, err)
			msg := err.Error()

			assert.Contains(t, msg, "rotates refresh tokens",
				"the reason these refuse is the round trip, the same one that refuses ssh")
			assert.Contains(t, msg, "cannot establish that those writes come back",
				"state the property af can ESTABLISH, not a mechanism of loss — hook's launch_cmd owns no "+
					"session directory af deletes, so a definite teardown claim is false there")
			assert.NotContains(t, msg, "destroyed with the session directory",
				"that names a lifecycle af controls for some of these and not others")
			assert.NotContains(t, msg, "no location it can prove",
				"af DOES control the session dir for sandbox and provision-mode hook — sandboxProvisioner "+
					"creates it — so a location-based reason is false the same way the old ssh wording was")
			assert.Contains(t, msg, "--account")
		})
	}
}

// The workaround must not promise WHICH identity omitting --account produces on
// hook, and this is the one place a wrong promise is dangerous rather than
// untidy: runHookScriptWithResolvedEnvironment hands launch_cmd the DAEMON's
// filtered agent-authentication environment, and the script decides what reaches
// the machine. So an operator told "omit --account to use that machine's own
// credentials" can end up running as the DAEMON HOST's ambient identity — the
// exact wrong-identity outcome account scoping exists to prevent — while
// believing the remote's identity was used.
func TestHookAccountRefusalDoesNotPromiseAnIdentityForTheWorkaround(t *testing.T) {
	hook := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendHook})
	require.Error(t, hook)
	assert.NotContains(t, hook.Error(), "to use that machine's own credentials",
		"af passes the daemon's filtered agent credentials to launch_cmd, so this would be false in the "+
			"direction account scoping exists to prevent")
	assert.Contains(t, hook.Error(), "up to your hooks",
		"it must say the identity depends on the hooks rather than assert one")

	// sandbox must not promise one either, and for its own reason: sandbox_ssh is
	// the operator's command and that backend deliberately preserves their
	// ssh_config, so a `SendEnv OPENAI_API_KEY` block copies the DAEMON's value to
	// the sandbox verbatim (measured in #3092).
	sandbox := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendSandbox})
	require.Error(t, sandbox)
	assert.NotContains(t, sandbox.Error(), "to use that machine's own credentials",
		"an operator's ssh_config can SendEnv the daemon's credentials into the sandbox")
	assert.Contains(t, sandbox.Error(), "sandbox_ssh",
		"the identity depends on their command, so name that rather than assert an outcome")

	// ssh IS immune, because af pins -F none there so no ssh_config is read at all —
	// so its accurate wording must stay rather than being deleted along with theirs.
	ssh := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendSSH})
	require.Error(t, ssh)
	assert.Contains(t, ssh.Error(), "own credentials",
		"backend=ssh reads no ssh_config (-F none), so the workaround CAN name the identity there")
}

// CarriesAccount's rationale must not keep the premise the refusal dropped: af
// DOES control the session dir for sandbox and provision-mode hook, so "af does
// not decide the shape of those machines" is false there too. Their false answer
// is the missing write-back, same as ssh's.
func TestCarriesAccountRationaleDoesNotClaimAfLacksControl(t *testing.T) {
	src, err := os.ReadFile("runtime.go")
	require.NoError(t, err)
	i := strings.Index(string(src), "// CarriesAccount reports whether")
	require.GreaterOrEqual(t, i, 0)
	rationale := string(src)[i:min(i+1600, len(string(src)))]

	assert.NotContains(t, rationale, "af does not decide the shape of those machines",
		"sandboxProvisioner.provision creates the session dir, and provision-mode hook reuses it — "+
			"this is the same false premise the refusal message dropped")
	assert.Contains(t, rationale, "writes",
		"the reason every off-box kind answers false is that the agent's writes cannot come home")
}

// An unassessed backend added later must still refuse, and must say it is
// UNPROVEN rather than impossible — nobody has evaluated it.
func TestUnknownBackendAccountRefusalStaysConservative(t *testing.T) {
	err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendKind("future-thing")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has not established",
		"an unassessed backend is unproven, not impossible; the default is refusal either way")
}

// The two that must NOT be refused, so this change moved only the wording.
func TestAccountStillAllowedOnLocalAndDocker(t *testing.T) {
	assert.NoError(t, refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendLocal}))
	assert.NoError(t, refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendDocker}))
	assert.NoError(t, refuseOffBoxAccount(InstanceOptions{Account: "work"}),
		"an empty backend means local, which needs no placing")
	assert.NoError(t, refuseOffBoxAccount(InstanceOptions{Backend: BackendSSH}),
		"no account means nothing to refuse")
}

// Every off-box kind must have a reason written for it. A kind that falls to the
// default gets the honest 'unproven' wording, which is correct for something
// nobody assessed and WRONG for one we deliberately decided — so the three we
// decided must each say something specific.
func TestEachDecidedBackendHasItsOwnReason(t *testing.T) {
	generic := offBoxAccountRefusal(BackendKind("future-thing"))
	for _, kind := range []BackendKind{BackendSSH, BackendSandbox, BackendHook} {
		assert.NotEqual(t, generic, offBoxAccountRefusal(kind),
			"%s was decided deliberately, so it must not fall through to the unassessed wording", kind)
	}
}

// CarriesAccount's contract is SAFE WRITE-BACK, not physical placement (#3103
// review). ssh can physically place an account and still answers false, so a
// contract phrased as placement would invite a future backend author to answer
// true for a copy-only mechanism — exactly the unsafe behaviour this refuses.
//
// Asserted against the doc comment because the contract IS the comment: nothing
// else states what a `true` promises, and a future author reads it before adding
// a case.
func TestCarriesAccountContractIsStatedAsWriteBack(t *testing.T) {
	src, err := os.ReadFile("runtime.go")
	require.NoError(t, err)
	doc := string(src)
	i := strings.Index(doc, "// CarriesAccount reports whether")
	require.GreaterOrEqual(t, i, 0, "the contract comment must exist")
	contract := doc[i:min(i+900, len(doc))]

	assert.Contains(t, contract, "SAFELY HONOUR",
		"the predicate is safe honouring including writes, not physical placement")
	assert.Contains(t, contract, "WRITES",
		"and the writes are the part that makes ssh answer false despite being able to place one")
	assert.NotContains(t, contract, "reports whether this kind's provisioner can place a registered",
		"the placement phrasing invites a copy-only backend to answer true")
}

// The refusal must not present COPYING as unavoidable (#3103 review). hook's
// launch_cmd may provision anything and only has to return an agent-server
// endpoint — it could use shared storage, or run on the daemon host — and a
// mount-like transport for ssh or sandbox would qualify too. What af lacks is the
// GUARANTEE of write-back, not the possibility of some mechanism providing it.
//
// This is the ninth finding in the same class on this change, and they all share
// a shape: the message asserted something af had not established. So the test
// asserts the absence of the over-claim rather than a particular phrasing.
func TestRefusalDoesNotPresentCopyingAsUnavoidable(t *testing.T) {
	for _, kind := range []BackendKind{BackendSSH, BackendSandbox, BackendHook} {
		err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: kind})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot establish that those writes come back",
			"%s must refuse on the missing GUARANTEE, which is the part af can state", kind)
	}

	src, err := os.ReadFile("runtime.go")
	require.NoError(t, err)
	i := strings.Index(string(src), "// CarriesAccount reports whether")
	require.GreaterOrEqual(t, i, 0)
	rationale := string(src)[i:min(i+1800, len(string(src)))]
	assert.NotContains(t, rationale, "every off-box kind",
		"docker is off-box and answers TRUE, so off-box is the wrong axis")
	assert.Contains(t, rationale, "not because a copy is the only thing they could ever do",
		"a future mount-like transport or a launch_cmd using shared storage would qualify")
}
