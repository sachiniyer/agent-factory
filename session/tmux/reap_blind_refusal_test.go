package tmux

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The blind refusal is the one an operator has to adjudicate by hand, so the
// message it leaves behind has to carry everything that decision needs: which
// pids, which generation each carries, and the read that separates a leftover
// from a live same-name replacement (#3706). It must NOT say "kill these" — the
// callers that reach this branch hold no claim on the session name and cannot
// tell those two apart.
func TestBlindVanishedSessionRefusalTellsOperatorHowToTell(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const (
		name       = "af_blind_refusal_guidance"
		generation = "b84f384df43d72ac54e4d3616c45336b"
	)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	marked := spawnMarkedSessionWithEscapee(t, name, home, generation)

	err := reapVanishedSessionProcesses(name, home, nil, nil, false)
	require.Error(t, err, "a blind sweep with no captured predecessor must refuse")
	require.True(t, proctree.AliveSame(marked),
		"the blind refusal must not signal a process it cannot attribute")

	message := err.Error()
	require.Contains(t, message, strconv.Itoa(marked.PID),
		"the refusal must name the marked pid the operator has to adjudicate")
	require.Contains(t, message, generation,
		"the refusal must name the generation that pid carries")
	require.Contains(t, message, name,
		"the refusal must name the vanished session")
	require.Contains(t, message,
		shellsuggest.Command("tmux", "show-environment", "-t", exactTarget(name), EnvMarkerGeneration),
		"the refusal must give the read that answers which generation the live session of that name carries")
	require.Contains(t, message, "safe to kill",
		"the refusal must say what to do with a generation no live session claims")
	require.Contains(t, message, "leave that pid alone",
		"the refusal must say what to do with a generation the live session claims")
}

// The synthetic half of the pin above: the message is built from a refusal this
// test constructs, so it can assert the exact pid/generation pairing without a
// live tmux session deciding which pids exist. A message that named the pids but
// dropped a generation — or paired the wrong one — would still satisfy the
// end-to-end test when every process happens to share a generation.
func TestBlindGenerationRefusalNamesEveryPidAndItsGeneration(t *testing.T) {
	const name = "af_0f8fc14c_root"
	marked := []markedGeneration{
		{pid: 1091038, generation: "8d6d4cf664efb073354d5f41b3e5f207"},
		{pid: 1091041, generation: "b84f384df43d72ac54e4d3616c45336b"},
	}

	message := blindGenerationRefusal(name, marked).Error()

	for _, process := range marked {
		require.Contains(t, message,
			fmt.Sprintf("pid %d (%s=%s)", process.pid, EnvMarkerGeneration, process.generation),
			"every marked pid must be named with the generation IT carries")
	}
	require.Contains(t, message, name, "the refusal must name the vanished session")
	require.Contains(t, message,
		shellsuggest.Command("tmux", "show-environment", "-t", exactTarget(name), EnvMarkerGeneration),
		"the refusal must give the read that answers what the live session of that name carries")
	require.Contains(t, message, shellsuggest.Command("af", "sessions", "list", "--json"),
		"the refusal must say how to find the af session behind that tmux name")
	require.Contains(t, message, "leave that pid alone",
		"a generation the live session claims must be left alone")
	require.Contains(t, message, "safe to kill",
		"a generation no live session claims must be named as safe to kill")
	require.Contains(t, message, "Nothing was signalled and nothing was removed",
		"the refusal must say what it is — a declined signal, not a broken workspace")
}

// A sanitized tmux name deliberately preserves `%`, so it must ride every
// suggestion as an argument and never be spliced into a format string (#1211).
func TestBlindGenerationRefusalKeepsPercentInSessionName(t *testing.T) {
	const name = "af_0f8fc14c_100%-done"

	message := blindGenerationRefusal(name, []markedGeneration{{pid: 4242, generation: "gen"}}).Error()

	require.Contains(t, message, name, "the session name must survive verbatim")
	require.NotContains(t, message, "%!", "a spliced name would leave fmt's bad-verb markers behind")
}
