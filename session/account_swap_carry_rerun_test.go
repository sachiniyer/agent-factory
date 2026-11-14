package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// rerunFixture is one conversation carried from an ambient source home into
// a registered account under a throwaway AF home, where the carry keeps its
// watermark and quarantine records (#7270).
type rerunFixture struct {
	agent   string
	afHome  string
	srcHome string
	dstDir  string
	dstRoot carryRoot
	rel     string
}

func newRerunFixture(t *testing.T, agent string) rerunFixture {
	t.Helper()
	f := rerunFixture{agent: agent, afHome: t.TempDir(), srcHome: t.TempDir()}
	require.NoError(t, os.MkdirAll(filepath.Join(f.afHome, "instances"), 0o700))
	f.dstDir = filepath.Join(f.afHome, "accounts", agent, "work")
	require.NoError(t, os.MkdirAll(f.dstDir, 0o700))
	var err error
	f.dstRoot, err = accountCarryRoot(f.afHome, agent, "work", f.dstDir)
	require.NoError(t, err)
	f.rel = "sessions/2026/11/10/rollout-" + carryTestID + ".jsonl"
	if agent == tmux.ProgramClaude {
		f.rel = carryTestRel
	}
	return f
}

func (f rerunFixture) carry(committed bool) *conversationCarry {
	c := &conversationCarry{
		agent: f.agent, id: carryTestID,
		srcHome: f.srcHome, dstHome: f.dstDir,
		srcRoot: ambientCarryRoot(f.srcHome), dstRoot: f.dstRoot,
		transcript: f.rel, committed: committed,
	}
	if f.agent == tmux.ProgramClaude {
		c.aux = strings.TrimSuffix(f.rel, ".jsonl")
	}
	return c
}

func (f rerunFixture) srcPath() string { return filepath.Join(f.srcHome, f.rel) }
func (f rerunFixture) dstPath() string { return filepath.Join(f.dstDir, f.rel) }

func (f rerunFixture) watermarks(t *testing.T) map[string]int64 {
	t.Helper()
	prefixes, err := handoff.NewStore(filepath.Join(f.afHome, "instances", handoff.StateFileName)).CarriedPrefixes()
	require.NoError(t, err)
	return prefixes
}

func (f rerunFixture) railsStore() *handoff.RailsStore {
	return handoff.NewRailsStore(
		filepath.Join(f.afHome, "instances", handoff.RailsFileName),
		filepath.Join(f.afHome, "instances", handoff.StateFileName))
}

func appendCarryFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

// A committed re-run whose source can no longer vouch for the landed copy
// covers the boundary af durably recorded for that path — never the target's
// own appends past it. Probe A: a zero-byte first carry is a record too, so
// a re-run after the target appended keeps the watermark at zero.
func TestCommittedCarryRerunKeepsTheRecordedBoundary(t *testing.T) {
	const own = "the target account's own usage record\n"
	for _, agent := range []string{tmux.ProgramCodex, tmux.ProgramClaude} {
		t.Run(agent+" after a zero-byte first carry", func(t *testing.T) {
			f := newRerunFixture(t, agent)
			writeCarryFile(t, f.srcHome, f.rel, "")
			require.NoError(t, f.carry(false).copy())
			require.Contains(t, f.watermarks(t), f.dstPath(),
				"a carry that copied zero bytes must record that, or a re-run cannot tell it from a carry af never recorded")
			require.Zero(t, f.watermarks(t)[f.dstPath()])

			appendCarryFile(t, f.dstPath(), own)
			require.NoError(t, f.carry(true).copy())
			require.Zero(t, f.watermarks(t)[f.dstPath()],
				"the target's own appends must not become the source account's provenance")
			require.Equal(t, own, readCarryFile(t, f.dstPath()))
		})
	}

	// Probe B: the first carry recorded 12 bytes, the target appended, then
	// the source lost the conversation. The watermark only grows, so a
	// re-run covering the whole file would attribute the tail for good.
	for _, mode := range []string{"truncated", "deleted"} {
		t.Run("codex after the source is "+mode, func(t *testing.T) {
			f := newRerunFixture(t, tmux.ProgramCodex)
			const carried = "source line\n"
			writeCarryFile(t, f.srcHome, f.rel, carried)
			require.NoError(t, f.carry(false).copy())
			require.Equal(t, int64(len(carried)), f.watermarks(t)[f.dstPath()])

			appendCarryFile(t, f.dstPath(), own)
			if mode == "truncated" {
				require.NoError(t, os.Truncate(f.srcPath(), 0))
			} else {
				require.NoError(t, os.Remove(f.srcPath()))
			}
			require.NoError(t, f.carry(true).copy())
			require.Equal(t, int64(len(carried)), f.watermarks(t)[f.dstPath()],
				"the re-run must keep the boundary the first carry recorded")
			require.Equal(t, carried+own, readCarryFile(t, f.dstPath()))
		})
	}

	// With no watermark, the quarantine marker a failed bookkeeping write
	// left is the boundary af recorded: the re-run turns it into a watermark
	// of the same span and retires the marker.
	for _, mode := range []string{"empty", "missing"} {
		t.Run("codex with only a quarantine marker and a "+mode+" source", func(t *testing.T) {
			f := newRerunFixture(t, tmux.ProgramCodex)
			const carried = "source line\n"
			writeCarryFile(t, f.dstDir, f.rel, carried+own)
			if mode == "empty" {
				writeCarryFile(t, f.srcHome, f.rel, "")
			}
			_, err := f.railsStore().NoteUnrecordedCarry(f.dstPath(), f.dstDir, int64(len(carried)), time.Now())
			require.NoError(t, err)

			require.NoError(t, f.carry(true).copy())
			require.Equal(t, int64(len(carried)), f.watermarks(t)[f.dstPath()],
				"the marker's span is the boundary af recorded; the tail past it is the target's")
			rails, err := f.railsStore().Load()
			require.NoError(t, err)
			require.NotContains(t, rails.UnrecordedCarries, f.dstPath(),
				"a watermark covering the marker's span retires it")
		})
	}

	// Only a path af holds no record for at all is covered as a whole.
	for _, mode := range []string{"empty", "missing"} {
		t.Run("codex with no record and a "+mode+" source", func(t *testing.T) {
			f := newRerunFixture(t, tmux.ProgramCodex)
			const landed = "landed by a binary that kept no record\n"
			writeCarryFile(t, f.dstDir, f.rel, landed)
			if mode == "empty" {
				writeCarryFile(t, f.srcHome, f.rel, "")
			}
			require.NoError(t, f.carry(true).copy())
			require.Equal(t, int64(len(landed)), f.watermarks(t)[f.dstPath()],
				"with no record, every byte is unverifiable provenance")
		})
	}
}

// Recording zero is bookkeeping, not a guard: a carry that copied nothing
// has no source bytes to quarantine, so a zero record that cannot be written
// must not fail it (the carry succeeded before zero records existed).
func TestZeroByteCarryProceedsWhenItsRecordCannotLand(t *testing.T) {
	f := newRerunFixture(t, tmux.ProgramCodex)
	instances := filepath.Join(f.afHome, "instances")
	require.NoError(t, os.RemoveAll(instances))
	require.NoError(t, os.WriteFile(instances, []byte("not a directory"), 0o600))
	writeCarryFile(t, f.srcHome, f.rel, "")

	require.NoError(t, f.carry(false).copy())
	require.FileExists(t, f.dstPath())
}

// Every branch that hands back a kept destination as the conversation to
// resume leaves it owner-writable (#4504): the resumed provider appends its
// next turn there. Probe C, for each reason a committed re-run keeps the
// copy rather than install the source.
func TestCommittedCarryRerunKeepsTheDestinationWritable(t *testing.T) {
	for _, mode := range []string{"empty", "missing", "shorter"} {
		t.Run(mode+" source", func(t *testing.T) {
			src, dst := carryHomes(t)
			switch mode {
			case "empty":
				writeCarryFile(t, src, carryTestRel, "")
			case "shorter":
				writeCarryFile(t, src, carryTestRel, "a\n")
			}
			dstPath := writeCarryFile(t, dst, carryTestRel, "a\nb\n")
			require.NoError(t, os.Chmod(dstPath, 0o400))

			_, err := carryConversationFileOutcome(ambientCarryRoot(src), plainCarryRoot(dst), carryTestRel, false)
			require.NoError(t, err)
			info, err := os.Stat(dstPath)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(carryFileMode), info.Mode().Perm(),
				"a kept destination must stay appendable")
			require.Equal(t, "a\nb\n", readCarryFile(t, dstPath))
		})
	}

	// The refusals hand nothing back, so they leave the destination exactly
	// as they found it — its mode included.
	for _, mode := range []string{"empty", "missing"} {
		t.Run("a new carry refusing a "+mode+" source", func(t *testing.T) {
			src, dst := carryHomes(t)
			if mode == "empty" {
				writeCarryFile(t, src, carryTestRel, "")
			}
			dstPath := writeCarryFile(t, dst, carryTestRel, "a\nb\n")
			require.NoError(t, os.Chmod(dstPath, 0o400))

			_, err := carryConversationFileOutcome(ambientCarryRoot(src), plainCarryRoot(dst), carryTestRel, true)
			require.Error(t, err)
			info, err := os.Stat(dstPath)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o400), info.Mode().Perm(), "a refused carry must not touch the destination")
			require.Equal(t, "a\nb\n", readCarryFile(t, dstPath))
		})
	}
}

// The empty-source refusal is about the conversation transcript. Claude's
// per-session tool-result files are detail: an empty one beside a non-empty
// destination copy keeps that copy and reports nothing failed (probe D).
func TestCarryTreeKeepsAnAuxFileWhoseSourceIsEmpty(t *testing.T) {
	src, dst := carryHomes(t)
	aux := "projects/-repo/" + carryTestID
	writeCarryFile(t, src, aux+"/tool-results/a.txt", "")
	writeCarryFile(t, src, aux+"/tool-results/b.txt", "bee\n")
	dstA := writeCarryFile(t, dst, aux+"/tool-results/a.txt", "earlier content\n")

	require.NoError(t, carryConversationTree(ambientCarryRoot(src), plainCarryRoot(dst), aux),
		"an empty auxiliary file is not a failed carry")
	require.Equal(t, "earlier content\n", readCarryFile(t, dstA))
	require.Equal(t, "bee\n", readCarryFile(t, filepath.Join(dst, aux, "tool-results/b.txt")))
}

// The copy-time refusals for a source that cannot vouch for the destination
// carry the same refusal code as the plan-time missing one, and name the file
// the way the agent does: a Codex rollout, a Claude transcript.
func TestCarrySourceRefusalsAreCodedAndWordedPerAgent(t *testing.T) {
	for reason, want := range map[string]string{
		carryTranscriptMissingReason: handoff.CarryMissing,
		carryRolloutMissingReason:    handoff.CarryMissing,
		carryTranscriptEmptyReason:   handoff.CarryMissing,
		carryRolloutEmptyReason:      handoff.CarryMissing,
	} {
		require.Equal(t, want, carryRefusalCode(reason), "%q", reason)
	}

	for _, tc := range []struct {
		agent, source, want string
	}{
		{tmux.ProgramCodex, "empty", carryRolloutEmptyReason},
		{tmux.ProgramCodex, "missing", carryRolloutMissingReason},
		{tmux.ProgramClaude, "empty", carryTranscriptEmptyReason},
		{tmux.ProgramClaude, "missing", carryTranscriptMissingReason},
	} {
		t.Run(tc.agent+" "+tc.source+" source", func(t *testing.T) {
			f := newRerunFixture(t, tc.agent)
			if tc.source == "empty" {
				writeCarryFile(t, f.srcHome, f.rel, "")
			}
			const stale = "a copy an earlier carry left\n"
			writeCarryFile(t, f.dstDir, f.rel, stale)

			err := f.carry(false).copy()
			require.Error(t, err)
			require.Equal(t, tc.want, carryFailureReason(err))
			require.Equal(t, stale, readCarryFile(t, f.dstPath()))
			require.Empty(t, f.watermarks(t), "a refused carry records nothing")
		})
	}
}

// The empty-source refusal end to end for Claude: the source transcript is
// emptied between the plan and the copy while the new account holds a copy an
// earlier trip left. The swap starts a stated fresh conversation, coded as a
// missing transcript, and the new account's copy is left as it was.
func TestClaudeCarryOfAnEmptiedTranscriptStartsAStatedFreshConversation(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramClaude)
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	rel := claudeTranscriptRel(inst.GetWorktreePath(), carryTestID)
	const turn = "{\"type\":\"user\"}\n"
	sourcePath := writeCarryFile(t, filepath.Join(home, ".claude"), rel, turn)
	targetPath := writeCarryFile(t, target, rel, turn)
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NotNil(t, inst.accountSwapLaunch.carry)

	require.NoError(t, os.Truncate(sourcePath, 0))
	require.NoError(t, inst.CarryAccountSwapConversation(), "a refused copy must not abort the swap")

	plan := inst.accountSwapLaunch
	require.Nil(t, plan.carry)
	require.Equal(t, carryTranscriptEmptyReason, plan.carryFallback)
	require.Equal(t, handoff.CarryMissing, carryRefusalCode(plan.carryFallback))
	require.Contains(t, plan.program, "--session-id "+plan.conversation.ID)
	require.NotContains(t, plan.program, "--resume")
	require.Equal(t, turn, readCarryFile(t, targetPath), "the new account's copy is not af's to change")
	afHome := os.Getenv("AGENT_FACTORY_HOME")
	prefixes, err := handoff.NewStore(filepath.Join(afHome, "instances", handoff.StateFileName)).CarriedPrefixes()
	require.NoError(t, err)
	require.NotContains(t, prefixes, targetPath, "a refused carry records no watermark")
}
