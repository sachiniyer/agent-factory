package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #5487/#5504's shared compare: a configured command is trusted — and a drift
// warning skipped — only when it is the SAME literal argv, so quoting a word
// differently must not matter and nothing else may.
func TestLiteralCommandArgv(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		want    []string
		ok      bool
		why     string
	}{
		{"bare agent", "claude", []string{"claude"}, true,
			"a single literal word"},
		{"unquoted path and flag", "/opt/af/claude --dangerously-skip-permissions",
			[]string{"/opt/af/claude", "--dangerously-skip-permissions"}, true, ""},
		{"single-quoted path", "'/opt/af/claude' --x",
			[]string{"/opt/af/claude", "--x"}, true,
			"quoting a word does not change the word"},
		{"double-quoted path", `"/opt/af/claude" --x`,
			[]string{"/opt/af/claude", "--x"}, true, ""},
		{"quoted path with a space", "'/opt/af space/claude' --x",
			[]string{"/opt/af space/claude", "--x"}, true,
			"a space inside quotes is still one word"},
		{"quoted flag", `'/opt/af/claude' '--x'`,
			[]string{"/opt/af/claude", "--x"}, true,
			"quoting applies to any word, not only the executable"},
		{"exec prefix kept", "exec /opt/af/claude --x",
			[]string{"exec", "/opt/af/claude", "--x"}, true,
			"exec is a literal word of the command; dropping it would misdescribe the argv"},
		{"single-quoted dollar text", `claude 'a $b'`,
			[]string{"claude", "a $b"}, true,
			"single quotes make the $ literal text"},
		{"empty word", `claude ''`,
			[]string{"claude", ""}, true, "an empty literal word is a word"},

		{"command substitution", "claude $(id)", nil, false,
			"the text on disk is not the word that runs"},
		{"command substitution inside quotes", `claude "$(id)"`, nil, false,
			"double quotes still expand"},
		{"backtick substitution", "claude `id`", nil, false, ""},
		{"parameter expansion", "claude $HOME", nil, false, ""},
		{"ansi-c quoting", `claude $'x'`, []string{"claude", "$x"}, true,
			"POSIX-mode parse keeps the $ as literal text, so the word can never equal " +
				"the decoded spelling a bash-variant parse would produce"},
		{"assignment prefix", "HOME=/tmp claude --x", nil, false,
			"an assignment mutates the command's environment; it is not a plain word list"},
		{"two commands", "claude; cat", nil, false, ""},
		{"pipe", "claude | cat", nil, false, ""},
		{"and", "claude && cat", nil, false, ""},
		{"redirection", "claude >log", nil, false, ""},
		{"background", "claude &", nil, false, ""},
		{"negated", "! claude", nil, false, ""},
		{"empty", "", nil, false, ""},
		{"whitespace only", "   ", nil, false, ""},
		{"unterminated quote", "claude 'x", nil, false,
			"does not parse"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := LiteralCommandArgv(test.command)
			require.Equal(t, test.ok, ok, test.why)
			if test.ok {
				require.Equal(t, test.want, got, test.why)
			}
		})
	}
}

func TestLiteralCommandsEqual(t *testing.T) {
	const detected = "'/opt/af/claude' --dangerously-skip-permissions"
	for _, test := range []struct {
		name string
		a, b string
		want bool
		why  string
	}{
		{"identical", detected, detected, true, ""},
		{"unquoted against quoted", "/opt/af/claude --dangerously-skip-permissions", detected, true,
			"the #5487 pair: same argv spelled without quotes"},
		{"double against single quotes", `"/opt/af/claude" --dangerously-skip-permissions`, detected, true, ""},
		{"quoted flag against bare flag", `/opt/af/claude '--dangerously-skip-permissions'`, detected, true, ""},
		{"extra whitespace", "  /opt/af/claude   --dangerously-skip-permissions  ", detected, true,
			"spacing between words is not part of the argv"},

		{"different path", "/opt/other/claude --dangerously-skip-permissions", detected, false, ""},
		{"extra argument", "/opt/af/claude --dangerously-skip-permissions --settings /x", detected, false,
			"an added word is a different command even though every shared word matches"},
		{"missing argument", "/opt/af/claude", detected, false, ""},
		{"reordered", "--dangerously-skip-permissions /opt/af/claude", detected, false,
			"position is part of the word list"},
		{"exec on one side", "exec /opt/af/claude --dangerously-skip-permissions", detected, false,
			"exec is a word; argv equality does not unwrap it (review note: strict)"},
		{"bare name against path", "claude --dangerously-skip-permissions", detected, false,
			"PATH resolution is deliberately not part of the comparison"},

		{"substitution on one side", "/opt/af/claude $(echo x)", detected, false,
			"non-literal is not equal, whichever side carries it"},
		{"identical substitution on both", "claude $(id)", "claude $(id)", false,
			"byte-equal is not argv-equal: a non-literal word list is not equal to itself"},
		{"identical assignment on both", "HOME=/tmp claude", "HOME=/tmp claude", false,
			"same fail-closed rule for assignment prefixes"},
		{"expansion inside quotes", `"/opt/af/claude" "$(id)"`, detected, false, ""},
		{"ansi-c quoting never decodes", `claude $'x'`, "claude x", false,
			"the literal word is $x, not x — the variant-conservative parse cannot launder a decoded value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, LiteralCommandsEqual(test.a, test.b), test.why)
			require.Equal(t, test.want, LiteralCommandsEqual(test.b, test.a),
				"equality is symmetric: "+test.why)
		})
	}
}
