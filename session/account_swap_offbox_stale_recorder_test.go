package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Regression coverage for the off-box account swap's stale-recorder reconcile
// arm. The agent-server's accountSwapRecorder keeps only the terminal outcome of
// the most recent swap and is never reset to "none" after a swap commits, so a
// later swap whose request is lost BEFORE the host runs anything must NOT be
// reconciled as committed on the strength of that stale record — the host's own
// rotation markers (account.next staged, no account.prev) show the current swap
// never ran. reconcileSwap now verifies the host on the committed arm the way its
// none arm always has, instead of trusting a terminal record that names no swap.

const secretThird = "token-for-third"

// thirdAccount registers a "third" claude account in the fixture's af home and
// returns its registry credential file, mirroring how newSwapFixture registers
// "alt".
func thirdAccount(t *testing.T) (thirdDir, thirdReg string) {
	t.Helper()
	home := os.Getenv("AGENT_FACTORY_HOME")
	dir, err := agentaccount.Register(home, tmux.ProgramClaude, "third")
	require.NoError(t, err)
	reg := filepath.Join(dir, credFile)
	require.NoError(t, os.WriteFile(reg, []byte(secretThird), 0o600))
	return dir, reg
}

// TestSwapOffBoxAccountStaleCommittedRecorderReconcilesAsNotCommitted is the core
// regression: a first swap succeeds and leaves the recorder at "committed"; a
// second swap whose request is lost before the host rotates must be reconciled as
// NOT committed, so the session stays on the account the host actually runs.
func TestSwapOffBoxAccountStaleCommittedRecorderReconcilesAsNotCommitted(t *testing.T) {
	f := newSwapFixture(t)

	// Swap #1: work -> alt, fully successful. The recorder is left committed.
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	outcome, prog := f.srv.rec.status()
	assert.Equal(t, swapOutcomeCommitted, outcome, "swap #1 left the recorder committed")
	assert.Equal(t, "claude", prog)
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the live scope now holds alt")
	assert.NoDirExists(t, f.w.NextAccountDir())
	assert.NoDirExists(t, f.w.PrevAccountDir())
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct)

	// Register the second swap's target and arm a transport loss BEFORE the host
	// runs anything for swap #2, exactly the class swapOutcomeIndeterminate
	// reconciles.
	_, thirdReg := thirdAccount(t)
	f.srv.loseBefore = errors.New("remote agent-server: POST /v1/agent/swap-account: connection reset")

	res, err := f.inst.SwapOffBoxAccount("third", false)
	require.Error(t, err, "a swap the host never ran must NOT be reconciled as committed")
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate,
		"the host showed the old account live with the staged scope beside it: that is a fact, not a guess")

	// The daemon's record must match the host: the session stays on alt.
	assert.False(t, res.Reconciled, "the stale committed record is not taken as proof the current swap committed")
	assert.Equal(t, "alt", res.From)
	assert.Equal(t, "third", res.To)
	acct, _ = f.inst.AccountSelection()
	assert.Equal(t, "alt", acct, "the row still names the account the host actually runs")
	assert.Equal(t, "alt", f.w.spec.Account.Name, "the workspace spec still names alt")
	assert.Equal(t, "alt", f.inst.currentBackend().(*sshBackend).provisioner.spec.Account.Name,
		"the provisioner spec still names alt")
	assert.False(t, f.inst.StartupStateUnknown(),
		"the host showed the old account live: the session is not fenced")

	// The host: the live scope still holds alt, the staged third is discarded, no
	// account.prev (the current swap never rotated anything).
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "alt is still the live account on the host")
	assert.NoDirExists(t, f.w.NextAccountDir(),
		"the staged account.next holding third's credentials is discarded, not left on the host")
	assert.NoDirExists(t, f.w.PrevAccountDir())

	// Neither registry entry was touched by a swap the host never ran.
	assert.Equal(t, secretAlt, readOrEmpty(f.altReg), "alt's registry copy is untouched")
	assert.Equal(t, secretThird, readOrEmpty(thirdReg), "third's registry copy is untouched")
}

// TestSwapOffBoxAccountStaleCommittedRecorderStillReconcilesAGenuineCommit is the
// contrast arm: a pre-existing committed record (from an earlier swap) must NOT
// cause a genuinely-committed current swap to be rejected. When swap #2's
// response is lost AFTER the host rotated, the live scope holds the new account
// and account.prev holds the old one (scopeIncoming), so the committed arm's
// host-marker verification accepts the genuine commit even under a stale record.
func TestSwapOffBoxAccountStaleCommittedRecorderStillReconcilesAGenuineCommit(t *testing.T) {
	f := newSwapFixture(t)

	// Swap #1: work -> alt, fully successful. The recorder is left committed and
	// the host has only the live scope (alt) — account.prev was cleaned up by the
	// first swap's write-back path.
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	outcome, _ := f.srv.rec.status()
	require.Equal(t, swapOutcomeCommitted, outcome, "swap #1 left the recorder committed")
	assert.NoDirExists(t, f.w.PrevAccountDir(), "the first swap cleaned up its account.prev")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the live scope now holds alt")

	// alt's token was rotated before the second swap.
	f.refreshOnHost(t, "alt-refreshed")

	_, thirdReg := thirdAccount(t)
	// Swap #2: alt -> third, with the response lost AFTER the host rotated. The
	// host genuinely committed: account = third, account.prev = alt.
	f.srv.loseResponse = errors.New("remote agent-server: POST /v1/agent/swap-account: context deadline exceeded")

	res, err := f.inst.SwapOffBoxAccount("third", false)
	require.NoError(t, err, "a genuine commit verified by the host's markers reconciles as committed")
	assert.True(t, res.Reconciled, "the outcome was learned from the host's markers, not assumed")
	assert.Equal(t, "claude", res.Program)

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "third", acct, "recorded identity matches the running one")
	assert.Equal(t, secretThird, f.hostCred(t, "account"), "the live scope now holds third")
	assert.NoDirExists(t, f.w.NextAccountDir())
	assert.NoDirExists(t, f.w.PrevAccountDir(), "the outgoing alt scope was cleaned up after its write-back")
	assert.Equal(t, "alt-refreshed", readOrEmpty(f.altReg), "alt's refresh came home before alt was removed")
	// Teardown must not push third's credentials over alt's registry copy, and
	// third's own registry entry is untouched by the absence of a refresh on it.
	require.NoError(t, f.w.teardownAccount(time.Minute))
	assert.Equal(t, "alt-refreshed", readOrEmpty(f.altReg))
	assert.Equal(t, secretThird, readOrEmpty(thirdReg), "third's registry stays its staged value")
}
