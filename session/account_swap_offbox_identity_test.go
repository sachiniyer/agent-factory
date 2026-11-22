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

// The same anonymous committed when the swap genuinely never ran. A
// pre-identity server has no serialized discard to bound the lost request
// with, so the reconcile fences — and leaves the staged scope for teardown —
// rather than reaching for an unordered remote rm (#7773 R2).
func TestSwapOffBoxAccountAnonymousStaleCommittedFencesKeepingTheStage(t *testing.T) {
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
	assert.DirExists(t, f.w.NextAccountDir(),
		"an old server has no ordered discard: the stage stays for teardown, never removed by an unordered rm")
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct)
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the host kept the old account")
}

// A rollback a pre-identity server answered is just as unattributable: the
// lost request could still be queued behind the answer, and that server has
// no serialized discard to bound it — so the session fences rather than risk
// a later commit it cannot attribute.
func TestSwapOffBoxAccountAnonymousRolledBackIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.anonymous = true
	f.srv.failAfter = errors.New("the new agent did not start")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
	assert.DirExists(t, f.w.NextAccountDir(), "the restored stage stays on the fenced host for teardown")
	assert.True(t, f.inst.StartupStateUnknown())
}

// A pre-identity server's "no swap has run" is no more attributable even when
// the markers agree — the queued request could still land and nothing on that
// server can bound it. Fence, and leave the stage.
func TestSwapOffBoxAccountAnonymousNoneIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.anonymous = true
	f.srv.loseBefore = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.True(t, f.inst.StartupStateUnknown())
	assert.DirExists(t, f.w.NextAccountDir(), "the stage stays for teardown — an old server has no ordered discard")
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

// --- the ordering the locks enforce (#7773 R1/R2) ---

// A swap request queued on the agent-server across BOTH status reads can still
// arrive after the verdict — and after a LATER swap's stage replaced the one it
// was sent for. The serialized discard's tombstone refuses it outright; and
// even with the tombstone lost to a server restart, the stage's own marker
// refuses a request that does not name the id the stage was written for.
// Either way it can only roll back — it can never rotate another swap's scope
// in under its own id.
func TestSwapOffBoxAccountHeldRequestCannotRotateALaterSwapsStage(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "the discard's tombstone refuses it"
		if restart {
			name = "a restarted server still refuses it on the stage's marker"
		}
		t.Run(name, func(t *testing.T) {
			f := newSwapFixture(t)
			f.srv.holdNext = 1 // the request is queued, never runs, through the reconcile

			_, err := f.inst.SwapOffBoxAccount("alt", false)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrAccountSwapIndeterminate,
				"the serialized discard resolved it: the id is tombstoned, its stage removed")
			require.Len(t, f.srv.held, 1)

			// A later swap stages ITS scope beside the live one — the stage the
			// queued request would find when it finally lands.
			f.registerThird(t)
			next, selErr := selectAccountInNamespace(tmux.ProgramClaude, "third")
			require.NoError(t, selErr)
			_, _, stageErr := f.w.stageAccountNext(time.Minute, next, "a-different-swap")
			require.NoError(t, stageErr)
			if restart {
				f.srv.rec = accountSwapRecorder{} // a restart forgets the tombstone — never the marker
			}

			_, err = f.srv.runHeldSwap(0)
			require.Error(t, err, "the queued request is refused — never rotated into another swap's stage")

			assert.Equal(t, secretWork, f.hostCred(t, "account"), "the live scope never moved")
			assert.Equal(t, secretThird, readOrEmpty(filepath.Join(f.w.NextAccountDir(), credFile)),
				"the later swap's stage is intact for its own request")
			o, _, id := f.srv.rec.status()
			assert.Equal(t, swapOutcomeRolledBack, o, "its only possible outcome is rolled back")
			assert.Equal(t, f.srv.held[0], id)
		})
	}
}

// The staged-scope discard is serialized with the swap — the remote rm the bug
// report's reconcile ran unordered can never again walk a directory a rotation
// is renaming out from under it. Both orderings are terminal and clean: a
// rotation in flight finishes and the discard finds nothing to remove, or the
// discard finishes and the queued request can only roll back.
func TestSwapOffBoxAccountDiscardAndRotationAreSerialized(t *testing.T) {
	// The request's response was lost, and the request itself is in flight
	// when the reconcile reaches for the discard: the discard must wait out
	// the rotation, not walk the directory under it.
	t.Run("the discard waits out a rotation in flight", func(t *testing.T) {
		f := newSwapFixture(t)
		f.registerThird(t)
		f.srv.loseBefore = errors.New("remote agent-server: connection reset")
		f.srv.delayedAtStatusCall = 1 // the lost request lands after the first read
		f.srv.delayedInBackground = true
		f.srv.rotateGate = make(chan struct{})
		f.srv.rotateWaiting = make(chan struct{})
		f.srv.discardWaiting = make(chan struct{})
		f.srv.beforeDiscard = make(chan struct{})
		f.srv.discardStarted = make(chan struct{})

		done := make(chan error, 1)
		go func() {
			_, err := f.inst.SwapOffBoxAccount("third", false)
			done <- err
		}()

		<-f.srv.discardWaiting // the reconcile reached for the discard
		<-f.srv.rotateWaiting  // but the late request won the lock first: mid-rotation
		close(f.srv.beforeDiscard)
		select {
		case <-f.srv.discardStarted:
			t.Fatal("the discard entered the swap's lock while the rotation held it")
		case <-time.After(50 * time.Millisecond):
		}
		close(f.srv.rotateGate) // the rotation finishes — now the discard may run
		<-f.srv.discardStarted
		require.NoError(t, <-done)

		acct, _ := f.inst.AccountSelection()
		assert.Equal(t, "third", acct, "this swap's own record proves its commit")
		assert.Equal(t, secretThird, f.hostCred(t, "account"),
			"the live scope is whole — no rm ever walked it mid-rotation")
		assert.Equal(t, secretWork, readOrEmpty(filepath.Join(f.w.PrevAccountDir(), credFile)),
			"the outgoing scope is parked in .prev, complete")
	})

	// The mirror: the discard holds the lock when the lost request finally
	// lands — the request waits, finds its stage gone and its id tombstoned,
	// and can only roll back.
	t.Run("a queued request waits out a discard in flight", func(t *testing.T) {
		f := newSwapFixture(t)
		f.registerThird(t)
		f.srv.loseBefore = errors.New("remote agent-server: connection reset")
		f.srv.discardStarted = make(chan struct{})
		f.srv.discardGate = make(chan struct{})

		done := make(chan error, 1)
		go func() {
			_, err := f.inst.SwapOffBoxAccount("third", false)
			done <- err
		}()

		<-f.srv.discardStarted // the discard holds the swap's lock, mid-removal
		delayed := make(chan struct{})
		go func() { f.srv.runDelayedSwap(); close(delayed) }()
		select {
		case <-delayed:
			t.Fatal("the queued request ran while the discard held the lock")
		case <-time.After(50 * time.Millisecond):
		}
		close(f.srv.discardGate)
		<-delayed // the request lands now — tombstoned, it can only roll back
		err := <-done
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrAccountSwapIndeterminate)

		acct, _ := f.inst.AccountSelection()
		assert.Equal(t, "work", acct)
		assert.Equal(t, secretWork, f.hostCred(t, "account"), "the old account's scope is whole")
		assert.NoDirExists(t, f.w.NextAccountDir())
		o, _, id := f.srv.rec.status()
		assert.Equal(t, swapOutcomeRolledBack, o)
		assert.Equal(t, f.srv.lastID, id)
	})
}

// The staged scope is bound to the swap it was staged for (#7773 R1): a
// request rotates in only the scope whose marker names its own id. The binding
// is durable — it lives on the filesystem, so it survives the restart a
// recorder tombstone does not.
func TestScopeRotationBindsTheStageToItsSwap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marker  string // "" writes no marker file
		wantID  string
		wantErr string
	}{
		{"a matching id rotates", "swap-1", "swap-1", ""},
		{"a mismatched id refuses", "swap-1", "swap-2", "not staged for this swap"},
		{"no marker refuses a named swap", "", "swap-2", "not staged for this swap"},
		{"no marker allows a pre-identity request", "", "", ""},
		{"a marker allows a pre-identity request", "swap-1", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := rotationHost(t)
			if tc.marker != "" {
				require.NoError(t, os.WriteFile(
					filepath.Join(live+accountNextSuffix, accountSwapMarkerRel), []byte(tc.marker), 0o600))
			}
			_, err := newScopeRotation(tmux.ProgramClaude, tc.wantID)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// The rotation strips the swap's marker from the live scope: it bound the
// stage, not the account, and must not ride the scope into a write-back.
func TestScopeRotationStripsTheSwapMarker(t *testing.T) {
	live := rotationHost(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(live+accountNextSuffix, accountSwapMarkerRel), []byte("swap-1"), 0o600))
	rot, err := newScopeRotation(tmux.ProgramClaude, "swap-1")
	require.NoError(t, err)
	require.NoError(t, rot.rotate())
	assert.NoFileExists(t, filepath.Join(live, accountSwapMarkerRel))
	assert.NoFileExists(t, filepath.Join(live+accountPrevSuffix, accountSwapMarkerRel))
}

// An id the daemon resolved as not committed can never commit afterwards: the
// serialized discard tombstones it under the same lock the swap runs in, and
// the request is refused into rolled_back rather than rotated (#7773 R1).
func TestAccountSwapDiscardTombstonesTheSwap(t *testing.T) {
	var rec accountSwapRecorder
	rec.discardStaged("doomed")
	_, err := rec.run("doomed", func() (string, error) { return "claude", nil })
	require.Error(t, err)
	o, _, id := rec.status()
	assert.Equal(t, swapOutcomeRolledBack, o)
	assert.Equal(t, "doomed", id)

	_, err = rec.run("fine", func() (string, error) { return "claude", nil })
	require.NoError(t, err, "an unrelated id is untouched")
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
