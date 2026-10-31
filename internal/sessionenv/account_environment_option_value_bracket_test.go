package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #6261: an attached `--opt=value` on a wrapper tail is refused only when the
// value names a denied variable or carries a denied assignment. A bracket in
// free text (a glob or character class) is no evidence of a mutation, and the
// separate-word spelling of the same value is never judged.
func TestAttachedOptionValueBracketIsFreeTextUnlessSubscriptShaped(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	cases := []struct {
		command string
		want    bool
	}{
		{"ls --ignore='[a-c]*'", false},
		{"ls --ignore '[a-c]*'", false},
		{"rg --iglob='[a-z]+' foo", false},
		{"rg --iglob '[a-z]+' foo", false},
		{"nice ls --ignore='[a-c]*'", false},
		{"nohup rg --glob='*.[ch]' x", false},
		{"timeout 5 grep --include='[[:alpha:]]*.go' x", false},
		{"ls --hide='*[0-9]'", false},
		// Still refused: the value names a denied variable or assignment.
		{"nice ls '--opt=CODEX_HOME[0]=x'", true},
		{"nice ls '--opt=CODEX_HOME[0]'", true},
		{"nice ls --opt=CODEX_HOME=/o", true},
		{"nice ls --opt='CODEX_HOME=[a]'", true},
		{"nice ls --opt=OPENAI_API_KEY+=x", true},
		{"nice ls '--opt=FOO[CODEX_HOME=1]'", true},
		{"nice ls '--opt=x[0]=1'", true},
		{"nice ls --opt=PORT", false},
	}
	for _, c := range cases {
		require.Equal(t, c.want, commandMutatesAccountEnvironment(c.command, names), "command %q", c.command)
	}
}
