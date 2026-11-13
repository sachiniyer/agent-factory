package tmux

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// #4797: with several sessions on one account home, "resume the latest" names a
// sibling's conversation. An exact-only session leaves such an agent fresh, and
// still resumes the agents whose latest is scoped to the working directory.
func TestResumeProgramExactOnly(t *testing.T) {
	for _, program := range []string{
		"codex",
		"CODEX_HOME=/h codex -c k=v",
		"amp",
	} {
		assert.Equal(t, program, resumeProgramExactOnly(program), program)
		assert.NotEqual(t, program, resumeProgram(program), "premise: the default rewrite is the latest one: %s", program)
	}
	assert.Equal(t, "claude --continue", resumeProgramExactOnly("claude"))
	assert.Equal(t, "codex resume abc", resumeProgramExactOnly("codex resume abc"))
}

func TestResumeRewriteFollowsTheExactOnlyFlag(t *testing.T) {
	ts := &TmuxSession{program: "codex", sanitizedName: "t"}
	assert.Equal(t, "codex resume --last", ts.resumeRewrite()("codex"), "default keeps today's behaviour")
	ts.SetExactResumeOnly(true)
	assert.Equal(t, "codex", ts.resumeRewrite()("codex"))
	ts.SetExactResumeOnly(false)
	assert.Equal(t, "codex resume --last", ts.resumeRewrite()("codex"))
}

// #7209 review: a re-spawn whose recorded conversation af judged and refused
// starts fresh, whatever the agent. A worktree-scoped "latest" (claude
// --continue) would re-answer the question the refusal answered: after a
// /clear it picks whichever conversation Claude wrote last. A refusal that only
// says the command cannot name the id keeps the agent's own resume.
func TestResumeRewriteStartsFreshAfterAJudgedRefusal(t *testing.T) {
	judged := ExactResumeRefusal{Session: "s", ID: "id", Reason: "stale", Judged: true}
	for _, program := range []string{"claude", "codex", "claude --dangerously-skip-permissions"} {
		ts := &TmuxSession{program: program, sanitizedName: "t"}
		ts.SetExactResumeOnly(true)
		ts.SetExactResumeRefusal(judged)
		assert.Equal(t, program, ts.resumeRewrite()(program), program)
	}

	ts := &TmuxSession{program: "claude", sanitizedName: "t"}
	ts.SetExactResumeOnly(true)
	ts.SetExactResumeRefusal(ExactResumeRefusal{Session: "s", ID: "id", Reason: "its launch command cannot resume a recorded codex conversation"})
	assert.Equal(t, "claude --continue", ts.resumeRewrite()("claude"),
		"an id the command cannot name says nothing about the worktree's own conversation")

	ts.SetExactResumeRefusal(judged)
	ts.SetExactResumeOnly(false)
	assert.Equal(t, "claude --continue", ts.resumeRewrite()("claude"), "a session that is not exact-only keeps today's rewrite")
}

// #6768: the retry-limit response answers "did the conversation carry" from
// the program a respawn launches, and only a provable resume counts — a
// worktree scope for codex, an explicit continue form for amp, the
// history restore aider still permits, and the worktree-scoped resumes every
// other supported agent's rewrite applies. Anything af cannot prove resumes
// an existing conversation answers false, so the response can name the fresh
// start rather than claim it.
func TestProgramResumesConversation(t *testing.T) {
	for _, tc := range []struct {
		program string
		resumes bool
	}{
		{"claude", true},
		{"claude --model sonnet", true},
		{"claude --continue", true},
		{"codex", false},
		{"codex resume --last", true},
		{"codex resume 019f386f-7206-7fc2-803b-f7045e07a242", true},
		{"codex resume --all", false},
		{"codex resume --include-non-interactive", false},
		{"codex exec resume 019f386f-7206-7fc2-803b-f7045e07a242", true},
		{"codex exec", false},
		{"amp", false},
		{"amp threads continue --last", true},
		{"amp last", true},
		{"amp review", false},
		{"aider", true},
		{"aider --no-restore-chat-history", false},
		{"gemini", true},
		{"opencode", true},
		{"devin", true},
		{"bash", false},
		{"", false},
		{"env FOO=1 claude", true},
	} {
		assert.Equal(t, tc.resumes, ProgramResumesConversation(tc.program), tc.program)
	}
}
