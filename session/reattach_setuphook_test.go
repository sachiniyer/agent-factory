package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reattach adopts the same runtime — it provisions nothing, so no new
// setup_commands hook runs. The recorded failure note is the same class of
// provision evidence as callbackUnavailable (#5974) and agentConfigSkipped
// (#6337): bindProvisionResult installs the result's note verbatim, and an
// empty one would read as a clean provision — silently dropping the
// operator-facing "af setup hook failed: …" warning the row's contract says
// must not be silent (#6667).

// TestReattachPreservesSetupHookFailed mirrors TestReattachPreservesAgentConfigSkipped
// (#6337) and TestReattachPreservesAPersistedCallbackReason (#5974): a reattach
// that provisions nothing must carry the prior note forward, and a later
// genuine provision that runs the hook clean still clears it.
func TestReattachPreservesSetupHookFailed(t *testing.T) {
	t.Run("a recorded note survives a plain reattach", func(t *testing.T) {
		inst, _ := reattachFixture(t, "")
		inst.setupHookFailed = "entry 1 failed"
		require.NoError(t, inst.ReattachSandbox())
		assert.Equal(t, "entry 1 failed", inst.SetupHookFailed(),
			"the note survives a reattach that provisions nothing new")
	})

	t.Run("an empty note stays empty", func(t *testing.T) {
		inst, _ := reattachFixture(t, "")
		require.NoError(t, inst.ReattachSandbox())
		assert.Empty(t, inst.SetupHookFailed(),
			"a session whose hook never failed stays silent after reattach")
	})

	t.Run("a later clean provision clears the note", func(t *testing.T) {
		// bindProvisionResult installs the result's note verbatim: a fresh
		// provision that ran the hook clean passes "" and clears any prior
		// note, so the carry-forward on reattach must not prevent the clear.
		inst, _ := reattachFixture(t, "")
		inst.setupHookFailed = "entry 1 failed"
		require.NoError(t, inst.ReattachSandbox())
		assert.Equal(t, "entry 1 failed", inst.SetupHookFailed())

		// A genuine reprovision that ran the hook clean clears the note.
		require.NoError(t, inst.bindProvisionResult(ProvisionResult{
			Backend:         newInertSandboxBackend("ssh"),
			SetupHookFailed: "",
		}))
		assert.Empty(t, inst.SetupHookFailed(),
			"a fresh provision that ran the hook clean clears the prior note")
	})
}
