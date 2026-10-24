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

		{"quoted glob is literal", `claude '*'`, []string{"claude", "*"}, true,
			"a quoted * is the literal word *"},
		{"quoted question mark", `claude 'a?b'`, []string{"claude", "a?b"}, true, ""},
		{"quoted bracket class", `claude '[a-z]'`, []string{"claude", "[a-z]"}, true, ""},
		{"quoted tilde is literal", `claude '~'`, []string{"claude", "~"}, true,
			"a quoted ~ is the literal word ~"},
		{"mid-word tilde is literal", "claude a~", []string{"claude", "a~"}, true,
			"~ only expands at word start or after =/:, so a~ is plain text in every shell"},
		{"tilde after quoted text stays literal", "claude 'a'~", []string{"claude", "a~"}, true,
			"the ~ is not at word start"},
		{"quoted braces are literal", `claude '{a,b}'`, []string{"claude", "{a,b}"}, true, ""},
		{"braces without a separator", "claude {}", []string{"claude", "{}"}, true,
			"{} expands to nothing in bash but stays one literal-looking word; no , or .. means no expansion"},
		{"unclosed brace is literal", "claude a{b", []string{"claude", "a{b"}, true,
			"an unmatched { is literal text in every shell"},
		{"quoted comma inside braces", `claude a{','}b`, []string{"claude", "a{,}b"}, true,
			"brace expansion needs an UNQUOTED comma — a{','}b is the literal word a{,}b in bash too"},
		{"quoted backslash is literal", `claude 'a\ b'`, []string{"claude", `a\ b`}, true,
			"a backslash inside quotes is literal text"},

		{"command substitution", "claude $(id)", nil, false,
			"the text on disk is not the word that runs"},
		{"command substitution inside quotes", `claude "$(id)"`, nil, false,
			"double quotes still expand"},
		{"backtick substitution", "claude `id`", nil, false, ""},
		{"parameter expansion", "claude $HOME", nil, false, ""},
		{"unquoted glob star", "claude *", nil, false,
			"a bare * globs to whatever the cwd contains — the written word is not the argv (#5487 review)"},
		{"unquoted question mark", "claude a?b", nil, false, ""},
		{"unquoted bracket class", "claude [a-z]", nil, false, ""},
		{"glob adjacent to quote", `claude a*b'c'`, nil, false,
			"the unquoted part still expands"},
		{"leading tilde", "claude ~", nil, false,
			"a word-leading ~ expands to a home directory"},
		{"tilde user prefix", "claude ~u/x", nil, false, ""},
		{"tilde slash prefix", "claude ~/x", nil, false, ""},
		{"tilde after equals", "claude a=~", nil, false,
			"bash expands the tilde in an assignment-form argument even in arg position"},
		{"tilde after colon", "claude a=b:~", nil, false,
			"assignment-form :~ is ambiguous across shells; refuse it rather than guess"},
		{"brace expansion comma", "claude {a,b}", nil, false,
			"{a,b} expands to two words before quote removal"},
		{"brace expansion range", "claude {1..3}", nil, false, ""},
		{"unquoted comma inside braces", `claude a{'x',y}b`, nil, false,
			"the comma is unquoted so the brace expands even though a part is quoted"},
		{"unquoted escape", `claude a\ b`, nil, false,
			"an unquoted backslash rewrites the word text at exec — the raw bytes are not the word"},
		{"escaped glob char", `claude \*`, nil, false,
			`the escape makes runtime * while the raw text is \* — unprovable, refuse`},
		{"escaped dollar", `claude \$x`, nil, false, ""},
		{"escaped backslash", `claude a\\b`, nil, false, ""},
		{"ansi-c quoting", `claude $'x'`, nil, false,
			"$'..' is literal $x to a POSIX sh but decoded x to bash — ambiguous, refuse"},
		{"localized dollar quoting", `claude $"x"`, nil, false,
			"same ambiguity for $\"..\""},
		{"dollar-quote mid-word", `claude a$'x'b`, nil, false, ""},
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
			"$'..' is literal $x to POSIX sh but decoded x to bash — refuse the ambiguous form"},

		{"unquoted glob against quoted", "claude *", "claude '*'", false,
			"THE REVIEW BUG: the quoted word is literal * but the bare one globs — not the same argv"},
		{"unquoted tilde against quoted", "claude ~", "claude '~'", false,
			"word-leading ~ expands to a home directory; the quoted one is literal ~"},
		{"identical unquoted glob", "claude *", "claude *", false,
			"byte-equal is not argv-equal for a word list the parse cannot prove"},
		{"unquoted braces against quoted", "claude {a,b}", "claude '{a,b}'", false,
			"{a,b} expands to two words; '{a,b}' is one literal word"},
		{"escaped space against quoted", `claude a\ b`, "claude 'a b'", false,
			"the raw text a\\ b is not the runtime word a b — refuse rather than unescape"},
		{"tilde-equals against quoted", "claude a=~", "claude 'a=~'", false, ""},
		{"mid-word tilde against quoted", "claude a~", "claude 'a~'", true,
			"a~ is literal text in every shell, so both spellings give the same word"},
		{"quoted-comma braces against quoted", `claude a{','}b`, "claude 'a{,}b'", true,
			"the quoted comma cannot expand; both spell the literal word a{,}b"},
		{"ansi-c against dollar literal", `claude $'x'`, "claude '$x'", false,
			"ambiguous across shells: $'x' decodes under bash but the quoted word is always $x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, LiteralCommandsEqual(test.a, test.b), test.why)
			require.Equal(t, test.want, LiteralCommandsEqual(test.b, test.a),
				"equality is symmetric: "+test.why)
		})
	}
}
