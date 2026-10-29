package envcommand

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		policy Policy
		want   Invocation
		bad    bool
	}{
		{name: "attached unset", args: []string{"-uCODEX_HOME", "codex"}, want: Invocation{Mutations: []Mutation{{Name: "CODEX_HOME", Unset: true}}, CommandIndex: 1}},
		{name: "separate unset", args: []string{"-u", "CODEX_HOME", "codex"}, want: Invocation{Mutations: []Mutation{{Name: "CODEX_HOME", Unset: true}}, CommandIndex: 2}},
		{name: "attached chdir", args: []string{"-C/tmp", "codex"}, want: Invocation{Chdir: "/tmp", CommandIndex: 1}},
		{name: "separate chdir", args: []string{"-C", "/tmp", "codex"}, want: Invocation{Chdir: "/tmp", CommandIndex: 2}},
		{name: "clear assign and chdir", args: []string{"-i", "--chdir=/tmp", "HOME=/home/agent", "codex"}, policy: Policy{AllowAssignments: true}, want: Invocation{ClearEnvironment: true, Chdir: "/tmp", Mutations: []Mutation{{Name: "HOME", Value: "/home/agent"}}, CommandIndex: 3}},
		{name: "dash clears and runs plain command", args: []string{"-", "claude"}, want: Invocation{ClearEnvironment: true, CommandIndex: 1}},
		{name: "dash terminates before ignore flag", args: []string{"-", "-i", "claude"}, want: Invocation{ClearEnvironment: true, CommandIndex: 1}},
		{name: "dash terminates before chdir flag", args: []string{"-", "-C", "/evilpath", "claude"}, want: Invocation{ClearEnvironment: true, CommandIndex: 1}},
		{name: "i keeps options active for chdir", args: []string{"-i", "-C", "/tmp", "claude"}, want: Invocation{ClearEnvironment: true, Chdir: "/tmp", CommandIndex: 3}},
		{name: "guard rejects assignment", args: []string{"HOME=/tmp", "codex"}, bad: true},
		{name: "resolver rejects dynamic assignment", args: []string{"HOME=$OTHER", "codex"}, policy: Policy{AllowAssignments: true}, bad: true},
		{name: "resolver rejects dynamic chdir", args: []string{"-C", "$OTHER", "codex"}, policy: Policy{AllowAssignments: true}, bad: true},
		{name: "reject glob chdir star", args: []string{"-C", "/tmp/af-repro-glob*", "codex"}, bad: true},
		{name: "reject glob chdir question", args: []string{"-C", "/tmp/a?", "codex"}, bad: true},
		{name: "reject glob chdir bracket", args: []string{"-C", "/tmp/a[b]", "codex"}, bad: true},
		{name: "reject glob chdir attached star", args: []string{"-C/tmp/x*", "codex"}, bad: true},
		{name: "reject glob chdir long attached star", args: []string{"--chdir=/tmp/x*", "codex"}, bad: true},
		{name: "reject glob assignment value", args: []string{"HOME=/tmp/x*", "codex"}, policy: Policy{AllowAssignments: true}, bad: true},
		{name: "reject glob unset name", args: []string{"-u", "FOO*", "codex"}, bad: true},
		{name: "literal chdir without glob still accepted", args: []string{"-C", "/opt/tools/codex", "codex"}, want: Invocation{Chdir: "/opt/tools/codex", CommandIndex: 2}},
		{name: "reject split string", args: []string{"-S", "codex"}, bad: true},
		{name: "reject help before command", args: []string{"--help", "codex"}, bad: true},
		{name: "reject version before command", args: []string{"--version", "codex"}, bad: true},
		{name: "reject short null with command", args: []string{"-0", "codex"}, bad: true},
		{name: "reject long null with command", args: []string{"--null", "codex"}, bad: true},
		{name: "reject unknown option", args: []string{"--future", "codex"}, bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.args, tc.policy)
			if tc.bad {
				require.ErrorIs(t, err, ErrUnsupported)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestIsLiteral pins the character set IsLiteral refuses. The launch shell
// (/bin/sh -c) acts on every one of these before env executes, so accepting any
// of them would let af model a different cwd or value from the one the process
// actually uses, silently breaking capture/correlation. The glob metacharacters
// * ? [ are refused for the same reason internal/sessionenv refuses them
// (pathname expansion at exec time), not just the shell expansion/control
// syntax carried over from the earlier literalEnvironmentValue helper.
func TestIsLiteral(t *testing.T) {
	for _, value := range []string{
		// Shell expansion and control syntax (pre-existing refusals).
		"$FOO", "`cmd`", "a;b", "a|b", "a&b", "a<b", "a>b", "a\nb", "a\rb",
		// Word-leading tilde (expands to a home at exec time).
		"~", "~/x",
		// Glob metacharacters: /bin/sh -c pathname-expands these before env's
		// -C/--chdir or assignment value is applied, so a glob chdir operand
		// must not be modeled as a provable cwd.
		"/tmp/af-repro-glob*", "/tmp/a?", "/tmp/a[b]", "a*b", "a?b", "a[b]c",
		"*", "?", "[", "[abc]",
	} {
		require.Falsef(t, IsLiteral(value), "IsLiteral(%q) should be false (shell acts on it at exec time)", value)
	}
	for _, value := range []string{
		"", "/opt/tools/codex", "/tmp/a b", "relative/path", "codex", "-C",
		"/tmp/=weird/name", "/opt/tools/codex-1", "a]b", // ] alone is not a glob opener
	} {
		require.Truef(t, IsLiteral(value), "IsLiteral(%q) should be true (literal text)", value)
	}
}
