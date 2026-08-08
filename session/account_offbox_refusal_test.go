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
	assert.Contains(t, msg, "teardown", "the reason is that a refreshed token dies with the session dir")
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
			assert.Contains(t, msg, "teardown")
			assert.NotContains(t, msg, "no location it can prove",
				"af DOES control the session dir for sandbox and provision-mode hook — sandboxProvisioner "+
					"creates it — so a location-based reason is false the same way the old ssh wording was")
			assert.Contains(t, msg, "--account")
		})
	}
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
