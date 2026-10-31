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
