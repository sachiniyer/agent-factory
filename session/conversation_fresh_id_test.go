package session

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7211: every launch that starts a NEW Claude conversation must use an id no
// transcript in the store already holds. Claude Code refuses to start on a
// --session-id whose <id>.jsonl already exists in the launch directory's project
// store ("Error: Session ID <id> is already in use."), so a launch that re-uses
// one never boots. Only an instance's first launch may use the instance id: no
// conversation can have been filed under it yet. Every later launch may find
// the transcript that first launch wrote.

// freshIDFixture is a Claude session in a worktree whose launches resolve to
// stand-in executables, with a Claude store of its own.
type freshIDFixture struct {
	inst      *Instance
	work      string
	configDir string
}

func newFreshIDFixture(t *testing.T) freshIDFixture {
	t.Helper()
	agentHome(t)
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// Handoff preflight checks each target's executable exists.
	bin := t.TempDir()
	cfg := config.DefaultConfig()
	if cfg.ProgramOverrides == nil {
		cfg.ProgramOverrides = map[string]string{}
	}
	for _, agent := range []string{tmux.ProgramClaude, tmux.ProgramCodex} {
		stub := filepath.Join(bin, agent)
		require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755))
		cfg.ProgramOverrides[agent] = stub
	}
	require.NoError(t, config.SaveConfig(cfg))

	work := t.TempDir()
	inst, err := NewInstance(InstanceOptions{Title: "fresh-id", Path: work, Program: tmux.ProgramClaude})
	require.NoError(t, err)
	gw, err := git.NewGitWorktreeFromStorage(work, work, inst.Title, "main", "", true, false)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	return freshIDFixture{inst: inst, work: work, configDir: configDir}
}

// firstClaudeLaunch runs the create plan, commits its conversation the way the
// launch does, and files the transcript Claude writes for it.
func (f freshIDFixture) firstClaudeLaunch(t *testing.T) AgentConversationData {
	t.Helper()
	plan, err := (&LocalBackend{}).prepareCreateLaunch(f.inst)
	require.NoError(t, err)
	first := plan.conversation
	require.Equal(t, f.inst.ID, first.ID, "precondition: a first launch comes up on the instance id")
	require.Contains(t, plan.program, "--session-id "+first.ID)
	require.True(t, f.inst.SetAgentConversation(first))
	writeCarryFile(t, f.configDir, claudeTranscriptRel(f.work, first.ID), "{}\n")
	return first
}

var sessionIDArg = regexp.MustCompile(`--session-id[ =](\S+)`)

// requireUnheldSessionID asserts program starts a new Claude conversation on
// conv, an id no transcript in the store holds and not any of used.
func (f freshIDFixture) requireUnheldSessionID(t *testing.T, program string, conv AgentConversationData, used ...string) {
	t.Helper()
	match := sessionIDArg.FindStringSubmatch(program)
	require.NotNil(t, match, "the launch must name its new conversation: %s", program)
	require.Equal(t, conv.ID, match[1], "the launch is recorded on the id it launches with")
	require.Equal(t, tmux.ProgramClaude, conv.Agent)
	for _, id := range used {
		require.NotEqual(t, id, match[1], "a new conversation must not re-use an id a transcript already holds")
	}
	require.False(t, claudeConversationFiledHere(program, f.work, conv), "no transcript may already hold the id a new conversation launches on")
}

// claude → codex → claude: the second Claude launch is a handoff, and the
// instance id it used to launch on names the first Claude conversation.
func TestHandoffBackToClaudeLaunchesOnAnIDNoTranscriptHolds(t *testing.T) {
	f := newFreshIDFixture(t)
	first := f.firstClaudeLaunch(t)

	backend := &LocalBackend{}
	toCodex, err := backend.PrepareAgentSwap(f.inst, tmux.ProgramCodex)
	require.NoError(t, err)
	require.False(t, toCodex.conversation.HasID(), "codex records its conversation by capture, not by launch")

	back, err := backend.PrepareAgentSwap(f.inst, tmux.ProgramClaude)
	require.NoError(t, err)
	f.requireUnheldSessionID(t, back.program, back.conversation, first.ID, f.inst.ID)
}

// Two handoffs back to Claude each start a conversation of their own.
func TestEachHandoffToClaudeMintsItsOwnConversation(t *testing.T) {
	f := newFreshIDFixture(t)
	first := f.firstClaudeLaunch(t)
	backend := &LocalBackend{}

	once, err := backend.PrepareAgentSwap(f.inst, tmux.ProgramClaude)
	require.NoError(t, err)
	f.requireUnheldSessionID(t, once.program, once.conversation, first.ID)
	writeCarryFile(t, f.configDir, claudeTranscriptRel(f.work, once.conversation.ID), "{}\n")

	twice, err := backend.PrepareAgentSwap(f.inst, tmux.ProgramClaude)
	require.NoError(t, err)
	f.requireUnheldSessionID(t, twice.program, twice.conversation, first.ID, once.conversation.ID)
}

// A re-spawn that does not resume is never the instance's first launch either.
func TestFreshRespawnLaunchesOnAnIDNoTranscriptHolds(t *testing.T) {
	f := newFreshIDFixture(t)
	first := f.firstClaudeLaunch(t)

	program, _, _, err := respawnLaunchProgram(f.inst, tmux.ProgramClaude, tmux.ProgramClaude, false, false, nil)
	require.NoError(t, err)
	recorded := f.inst.AgentConversation()
	f.requireUnheldSessionID(t, program, recorded, first.ID, f.inst.ID)
}
