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

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7773: a reconciled commit is accepted only on the agent-server's terminal
// record for THIS swap — never on an earlier swap's stale outcome, never on a
// leftover rotation marker, and never on a server that cannot echo the swap's
// identity. These tests run two swaps through ONE agent-server recorder, the
// stale-record shape the single-swap tests never produce.

const secretThird = "token-for-third"

// registerThird adds the second swap's incoming account to the fixture's
// registry.
func (f *swapFixture) registerThird(t *testing.T) (thirdReg string) {
	t.Helper()
	dir, err := agentaccount.Register(os.Getenv("AGENT_FACTORY_HOME"), tmux.ProgramClaude, "third")
	require.NoError(t, err)
	thirdReg = filepath.Join(dir, credFile)
	require.NoError(t, os.WriteFile(thirdReg, []byte(secretThird), 0o600))
	return thirdReg
}

// failPrevCleanup makes the outgoing scope's removal fail during fn, so a
// completed first swap leaves its account.prev behind — the leftover marker a
// later reconcile must never read as evidence (#7781 review).
func (f *swapFixture) failPrevCleanup(t *testing.T, fn func()) {
	t.Helper()
	p := f.inst.currentBackend().(*sshBackend).provisioner
	real := p.runCommandFn
	p.runCommandFn = func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
		if stdin == nil && strings.HasPrefix(script, "rm -r") && containsStr(script, accountPrevSuffix) {
			return []byte("busy"), errors.New("rm failed")
		}
		return real(d, script, stdin, combined)
	}
	defer func() { p.runCommandFn = real }()
	fn()
}

// seedUnknown plants a terminal "the scope could not be put back" record for an
// earlier swap directly into the recorder, without wrecking the fixture's host.
func (f *swapFixture) seedUnknown(t *testing.T) {
	t.Helper()
	_, err := f.srv.rec.run("an-earlier-swap", func() (string, error) {
		return "", errors.New("restore failed; " + accountScopeIndeterminate)
	})
	require.Error(t, err)
}

// The #7781 P1 shape: swap #1 committed and its .prev cleanup failed, leaving
// the outgoing scope behind; swap #2's request is lost before the host runs
// anything. The stale committed record — and the leftover marker that reads
// "incoming" once .next is gone — are both attributed to the WRONG swap: the
// row stays on alt, which is what the host still runs.
func TestSwapOffBoxAccountStaleCommittedWithLeftoverPrevKeepsTheOldAccount(t *testing.T) {
	f := newSwapFixture(t)
	f.failPrevCleanup(t, func() {
		_, err := f.inst.SwapOffBoxAccount("alt", false)
		require.NoError(t, err)
	})
	require.FileExists(t, filepath.Join(f.w.PrevAccountDir(), credFile),
		"the first swap's failed cleanup leaves its outgoing scope as a marker")
	thirdReg := f.registerThird(t)
	f.srv.loseBefore = errors.New("remote agent-server: POST /v1/agent/swap-account: connection reset")

	_, err := f.inst.SwapOffBoxAccount("third", false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate,
		"the host provably runs the old account: a definite refusal, not a guess")

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct, "the row names the account the host runs")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"))
	assert.NoDirExists(t, f.w.NextAccountDir(), "the staged scope is discarded so a late request can only roll back")
	assert.Equal(t, secretWork, readOrEmpty(filepath.Join(f.w.PrevAccountDir(), credFile)),
		"the first swap's leftover stays put — it was never this swap's to remove")
	assert.False(t, f.inst.StartupStateUnknown(), "a definite answer does not fence the session")
	assert.Equal(t, secretAlt, readOrEmpty(f.altReg))
	assert.Equal(t, secretThird, readOrEmpty(thirdReg), "no write-back across accounts")
}

// The contrast arm: the same stale record, but THIS swap's own response is the
// one that was lost — after the host genuinely rotated. The record now names
// this swap's id, which IS admissible proof of commit.
func TestSwapOffBoxAccountStaleRecordStillCreditsThisSwapsCommit(t *testing.T) {
	f := newSwapFixture(t)
	f.failPrevCleanup(t, func() {
		_, err := f.inst.SwapOffBoxAccount("alt", false)
		require.NoError(t, err)
	})
	thirdReg := f.registerThird(t)
	f.refreshOnHost(t, "alt-refreshed-on-host") // alt's agent rotated its token between the swaps
	f.srv.loseResponse = errors.New("remote agent-server: POST /v1/agent/swap-account: context deadline exceeded")

	res, err := f.inst.SwapOffBoxAccount("third", false)
	require.NoError(t, err)
	assert.True(t, res.Reconciled)

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "third", acct, "this swap's own record proves its commit")
	assert.Equal(t, secretThird, f.hostCred(t, "account"))
	assert.NoDirExists(t, f.w.PrevAccountDir(), "the outgoing scope is removed only under this swap's terminal commit")
	assert.Equal(t, "alt-refreshed-on-host", readOrEmpty(f.altReg), "the write-back still brings the outgoing account home")
	assert.Equal(t, secretThird, readOrEmpty(thirdReg))
}

// A stale rolled_back record says the same thing about this swap: not committed.
func TestSwapOffBoxAccountStaleRolledBackKeepsTheOldAccount(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.failAfter = errors.New("the new agent did not start")
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorContains(t, err, "did not start")
	f.srv.failAfter = nil
	thirdReg := f.registerThird(t)
	f.srv.loseBefore = errors.New("remote agent-server: connection reset")

	_, err = f.inst.SwapOffBoxAccount("third", false) // still on work: the first swap rolled back
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate)

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
	assert.NoDirExists(t, f.w.NextAccountDir())
	assert.False(t, f.inst.StartupStateUnknown())
	assert.Equal(t, secretThird, readOrEmpty(thirdReg))
}

// The delayed request that rotates between the status read and the staged
// scope's removal: the re-ask waits it out and answers under THIS swap's id —
// committed, because that is what the host did. A marker probe ordered the way
// the earlier fix read it would have credited it before it was terminal; the
// record cannot.
func TestSwapOffBoxAccountDelayedRequestCommitsUnderItsOwnIdentity(t *testing.T) {
	f := newSwapFixture(t)
	f.failPrevCleanup(t, func() {
		_, err := f.inst.SwapOffBoxAccount("alt", false)
		require.NoError(t, err)
	})
	thirdReg := f.registerThird(t)
	f.refreshOnHost(t, "alt-refreshed-on-host")
	f.srv.loseBefore = errors.New("remote agent-server: connection reset")
	f.srv.delayedAtStatusCall = 1 // lands between the first status read and the discard

	res, err := f.inst.SwapOffBoxAccount("third", false)
	require.NoError(t, err)
	assert.True(t, res.Reconciled, "the second status read waited for the running swap and found its own record")

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "third", acct)
	assert.Equal(t, secretThird, f.hostCred(t, "account"))
	assert.Equal(t, "alt-refreshed-on-host", readOrEmpty(f.altReg))
	assert.Equal(t, secretThird, readOrEmpty(thirdReg))
}

// The mirror: the delayed request lands only after the staged scope is gone, so
// it can never rotate in — its terminal record is this swap's rolled_back, and
// the host keeps the old account.
func TestSwapOffBoxAccountDelayedRequestAfterTheDiscardOnlyRollsBack(t *testing.T) {
	f := newSwapFixture(t)
	f.failPrevCleanup(t, func() {
		_, err := f.inst.SwapOffBoxAccount("alt", false)
		require.NoError(t, err)
	})
	thirdReg := f.registerThird(t)
	f.srv.loseBefore = errors.New("remote agent-server: connection reset")
	f.srv.delayedAtStatusCall = 2 // lands after the staged scope is discarded

	_, err := f.inst.SwapOffBoxAccount("third", false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate)

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct)
	assert.Equal(t, secretAlt, f.hostCred(t, "account"))
	o, _, id := f.srv.rec.status()
	assert.Equal(t, swapOutcomeRolledBack, o, "the late arrival could only roll back")
	assert.Equal(t, f.srv.lastID, id)
	assert.Equal(t, secretThird, readOrEmpty(thirdReg))
}

// A stale "the scope could not be put back" is not a definite answer either: the
// host's live scope is in doubt from the earlier wreck, so the session fences
// and nothing is discarded from it.
func TestSwapOffBoxAccountStaleUnknownIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	f.seedUnknown(t)
	f.registerThird(t)
	f.srv.loseBefore = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("third", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.True(t, f.inst.StartupStateUnknown())
	assert.DirExists(t, f.w.NextAccountDir(), "nothing is discarded from a host in doubt")
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
}

// --- pre-identity agent-servers (version skew) ---

// An agent-server that cannot echo the swap's identity can never prove a
// commit — even when this swap really committed and its response was lost. The
// row fences: indeterminate, never a stale-shape commit.
func TestSwapOffBoxAccountAnonymousCommittedIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.anonymous = true
	thirdReg := f.registerThird(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded") // this swap really committed

	_, err := f.inst.SwapOffBoxAccount("third", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate,
		"an outcome with no identity proves nothing about this swap")
	assert.True(t, f.inst.StartupStateUnknown())

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct, "the row records no account it cannot show the host runs")
	assert.Equal(t, secretWork, readOrEmpty(f.workReg), "no write-back from a scope of unknown identity")
	assert.Equal(t, secretAlt, readOrEmpty(f.altReg))
	assert.Equal(t, secretThird, readOrEmpty(thirdReg))
	assert.Equal(t, secretWork, readOrEmpty(filepath.Join(f.w.PrevAccountDir(), credFile)),
		"the rotated-out scope stays for whatever inspects the fenced session")
}

// The same anonymous committed when the swap genuinely never ran: still
// indeterminate — unattributable — but the staged scope is removed so the lost
// request cannot land late and make the fence a divergence.
func TestSwapOffBoxAccountAnonymousStaleCommittedBoundsThenFences(t *testing.T) {
	f := newSwapFixture(t)
	f.failPrevCleanup(t, func() {
		_, err := f.inst.SwapOffBoxAccount("alt", false)
		require.NoError(t, err)
	})
	f.srv.anonymous = true
	f.registerThird(t)
	f.srv.loseBefore = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("third", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.True(t, f.inst.StartupStateUnknown())
	assert.NoDirExists(t, f.w.NextAccountDir(),
		"the staged scope is still removed: an unattributable commit cannot land late either")
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct)
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the host kept the old account")
}

// A rollback is one answer a pre-identity server CAN give definitely once the
// staged scope is gone: whichever swap it belongs to, the host put the old
// scope back, and this swap can never rotate in now.
func TestSwapOffBoxAccountAnonymousRolledBackIsNotCommitted(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.anonymous = true
	f.srv.failAfter = errors.New("the new agent did not start")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorContains(t, err, "did not start")
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate)

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
	assert.NoDirExists(t, f.w.NextAccountDir())
	assert.False(t, f.inst.StartupStateUnknown())
}

// --- the none arm keeps its host check ---

// A "no swap has run" answer (a restarted server forgets every record) is only
// trusted when the host's markers agree. A leftover .prev contradicts it: fence
// rather than discard into a host in doubt.
func TestSwapOffBoxAccountNoRecordWithContradictingMarkersIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	stalePrev := f.w.PrevAccountDir()
	require.NoError(t, os.MkdirAll(stalePrev, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stalePrev, credFile), []byte(secretWork), 0o600))
	f.srv.loseBefore = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.True(t, f.inst.StartupStateUnknown())
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
	assert.DirExists(t, stalePrev, "the unexplained marker is left for the fence, not trusted")
}

// The session env the rotation needs is still the live scope after swap #1: the
// second swap's staging and rotation read it the same way the first's did.
func TestSwapOffBoxAccountSecondSwapStagesAgainstTheNewLiveScope(t *testing.T) {
	f := newSwapFixture(t)
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	f.registerThird(t)

	res, err := f.inst.SwapOffBoxAccount("third", false)
	require.NoError(t, err)
	assert.Equal(t, secretThird, f.srv.atCall.next, "the second swap staged the incoming account beside the live one")
	assert.Equal(t, "alt", res.From)
	assert.Equal(t, "third", res.To)
}
