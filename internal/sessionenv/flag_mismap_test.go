package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// CommandEndsOptionsTerminator and CommandHasControlOperator are the two shapes
// that misroute the flags injectSystemPrompt appends to the END of the resolved
// program string. The first (a `--` end-of-options terminator, anywhere after an
// optional `exec` prefix) is the SILENT failure surface the bug is about; the
// second (a shell control operator, including a statement terminator) is a loud
// misrouting. These tables pin them against the same mvdan.cc/sh parser the
// account boundary uses, so the warning and the refusal can never come to
// different conclusions about the same string (the same invariant
// CommandUsesExecSeparator keeps).

func TestCommandEndsOptionsTerminator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		{"bare agent has no terminator", "claude", false},
		{"agent with flags", "claude --model opus", false},
		{"trailing terminator", "claude --", true},
		{"trailing terminator after flags", "claude --model opus --", true},
		// A `--` ANYWHERE after the program demotes the appended flag to a
		// positional, not only the last word: `claude -- --resume` appends
		// `--plugin-dir` after `--resume`, but the `--` already made `--resume`
		// positional, so the appended flag is positional too.
		{"terminator in the middle demotes the appended flag", "claude -- --resume", true},
		{"terminator before a flag", "claude -- --resume --model opus", true},
		{"terminator as the only flag", "claude --model opus --resume", false},
		{"absolute path trailing terminator", "/usr/local/bin/claude --", true},
		{"exec prefix stripped, no terminator", "exec -- claude", false},
		{"exec prefix stripped, terminator remains", "exec -- claude --", true},
		{"exec prefix stripped, terminator in the middle", "exec -- claude -- --resume", true},
		{"double-quoted terminator is literal", `claude "--"`, true},
		{"single-quoted terminator is literal", "claude '--'", true},
		// Bash's dollar-quoted `$'--'` passes a literal `--` to the agent the
		// same way an unquoted `--` does, so the appended flag lands after the
		// end-of-options marker and the agent starts without it (#5167 review:
		// "Parse Bash-quoted `--` terminators"). `$'\n'` carries a real C-escape
		// and stays non-literal, so it is not a terminator.
		{"dollar-single-quoted terminator is literal", "claude $'--'", true},
		{"dollar-single-quoted escape is not a terminator", "claude $'\\n'", false},
		{"non-literal word is not a terminator", "claude --$FOO", false},
		{"non-literal flag value is not a terminator", "claude --model=--", false},
		{"redirect is ignored, terminator seen", "claude > /tmp/log --", true},
		{"stderr redirect ignored, terminator seen", "claude 2>/dev/null --", true},
		{"pipe is not a single call so not a terminator", "claude -- | tee", false},
		{"env-prefixed agent with terminator", "CLAUDE_CODE_USE_BEDROCK=1 claude --", true},
		{"env-prefixed agent with terminator in the middle", "CLAUDE_CODE_USE_BEDROCK=1 claude -- --resume", true},
		{"only terminator word", "--", true},
		{"empty string", "", false},

		// An `env` wrapper's `--` is env's end-of-options marker, not the command's:
		// `env -- claude` passes a trailing `--plugin-dir` to claude normally, so the
		// `--` is not the agent's terminator and must not warn. `env -- claude --`
		// still has claude's own trailing `--`, which is the agent's terminator. The
		// `--` env's VAR assignments come before is also env's (#5167 review:
		// "Distinguish wrapper option terminators from agent terminators").
		{"env wrapper terminator is not the agent's", "env -- claude", false},
		{"env wrapper -i and terminator", "env -i -- claude", false},
		{"env wrapper then agent's own terminator", "env -- claude --", true},
		{"env wrapper with VAR then agent's own terminator", "env VAR=1 claude --", true},
		{"env without terminator, agent's own terminator", "env VAR=1 claude -- --resume", true},

		// env can sit behind another argv-passthrough wrapper: `ionice -c 3 env --
		// claude` is one CallExpr whose first word is `ionice`, so the prior
		// words[0]-only check left env's `--` in the scan and warned even though
		// env passes the appended flag to claude normally. The wrapper prefix is
		// peeled first, so env's `--` is not mistaken for the agent's own
		// terminator; `ionice -c 3 env -- claude --` still flags claude's trailing
		// `--`. A wrapper's OWN `--` (nice's end-of-options) is also not the
		// agent's terminator (#5167 review: "Recognize env terminators behind
		// wrappers").
		{"ionice wrapper then env terminator", "ionice -c 3 env -- claude", false},
		{"nice wrapper then env terminator", "nice -n 5 env -- claude", false},
		{"nohup wrapper then env terminator", "nohup env -- claude", false},
		{"ionice wrapper then env then agent's own terminator", "ionice -c 3 env -- claude --", true},
		{"ionice wrapper then env then agent's own terminator mid-command", "ionice -c 3 env -- claude -- --resume", true},
		{"nice wrapper's own terminator is not the agent's", "nice -- claude", false},
		{"nice wrapper then agent's own terminator", "nice -- claude --", true},

		// env can sit in front of ANOTHER argv-passthrough wrapper: `env -- nice
		// -- claude` leaves `nice -- claude` once env's own `--` is consumed, and
		// nice's `--` is nice's own terminator (GNU nice passes the appended flag
		// through to claude), so the `--` is not the agent's and must not warn.
		// `env -- nice -- claude --` still has claude's own trailing `--`, which is
		// the agent's terminator. The peel continues past env so a wrapper chain
		// (env behind a wrapper, a wrapper behind env, nested env) is fully
		// unwrapped before the terminator scan (#5167 review: "Continue
		// unwrapping wrappers after `env`").
		{"env then nice wrapper's own terminator is not the agent's", "env -- nice -- claude", false},
		{"env then nice wrapper then agent's own terminator", "env -- nice -- claude --", true},
		{"env behind ionice then nice wrapper's own terminator", "ionice -c 3 env -- nice -- claude", false},
		{"nested env wrappers' terminators are not the agent's", "env -- env -- claude", false},
		{"nested env wrapper then agent's own terminator", "env -- env -- claude --", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandEndsOptionsTerminator(tc.command)
			require.Equalf(t, tc.want, got, "CommandEndsOptionsTerminator(%q)", tc.command)
		})
	}
}

func TestCommandHasControlOperator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		// Single simple calls — no control operator.
		{"bare agent", "claude", false},
		{"agent with flags", "claude --model opus", false},
		{"absolute path", "/usr/local/bin/claude", false},
		{"env-prefixed agent", "CLAUDE_CODE_USE_BEDROCK=1 claude", false},
		{"exec prefix", "exec claude", false},
		{"exec separator", "exec -- claude", false},

		// Redirections do NOT misroute the flag (the agent still receives it),
		// so a single call with redirects is not a control operator.
		{"stdout redirect", "claude > /tmp/log", false},
		{"stderr redirect", "claude 2>/dev/null", false},
		{"stdin redirect", "claude < /tmp/in", false},
		{"redirect then trailing terminator", "claude > /tmp/log --", false},

		// Negation is not a control operator in the shell sense and does not
		// misroute the flag.
		{"negation", "! claude", false},

		// A single simple call terminated by a statement separator (`;` or a
		// trailing newline) still misroutes an appended flag: `claude;` becomes
		// `claude; --plugin-dir …` and the flag runs as its own command. A `;` is
		// exposed on stmt.Semicolon (a `&`/`|&` is already caught above as
		// Background/Coprocess); a trailing newline is the other statement
		// terminator the parser folds into a single Stmt without a Semicolon, so
		// it is detected on the raw value.
		{"trailing semicolon", "claude;", true},
		{"trailing semicolon after flags", "claude --model opus;", true},
		{"trailing newline", "claude\n", true},
		{"trailing newline with flags", "claude --model opus\n", true},
		{"trailing whitespace then newline", "claude \t\n ", true},

		// A backslash-newline is a shell line continuation, not a statement
		// terminator: `claude \\\n` keeps an appended flag on the same command,
		// so it is not a control operator (#5167 review: "Respect escaped
		// trailing newlines"). An even run of backslashes escapes the newline's
		// escape (the last backslash escapes the second-to-last), leaving a real
		// terminator; an odd run is a continuation.
		{"escaped trailing newline is a continuation", "claude \\\n", false},
		{"escaped trailing newline with flags", "claude --model opus \\\n", false},
		{"odd run of backslashes is a continuation", "claude \\\\\\\n", false},
		{"even run of backslashes is a terminator", "claude \\\\\n", true},

		// Actual control operators / compound constructs — misroute the flag.
		{"pipe", "claude | tee /tmp/log", true},
		{"and", "claude foo && claude bar", true},
		{"or", "claude foo || claude bar", true},
		{"semicolon", "claude; echo hi", true},
		{"background", "claude &", true},
		{"background with second command", "claude & echo hi", true},
		{"subshell", "(claude)", true},
		{"pipe with trailing terminator", "claude -- | tee", true},
		{"exec separator then pipe", "exec -- claude | tee", true},

		// A trailing operator that appending a word completes is a misroute, not a
		// silent parse error: `claude |` fails to parse, but injectSystemPrompt
		// appends `--plugin-dir`, so `claude | --plugin-dir …` is valid shell and
		// the flag runs as the right side of the pipe rather than as a flag to
		// claude. The same applies to an incomplete `&&`/`||` and an incomplete
		// redirection (`>`, `<`, `>>`): appending supplies the missing second
		// command or the redirect target (#5167 review: "Warn when appending
		// repairs an incomplete shell operator"). A trailing `&` is a valid
		// background operator, not a parse error, and is handled above (stmt.Background).
		{"trailing pipe is a parse error appending completes", "claude |", true},
		{"trailing and is a parse error appending completes", "claude &&", true},
		{"trailing or is a parse error appending completes", "claude ||", true},
		{"trailing redirect out is a parse error appending completes", "claude >", true},
		{"trailing redirect in is a parse error appending completes", "claude <", true},
		{"trailing append redirect is a parse error appending completes", "claude >>", true},
		// `|&` is Bash's pipe-both-stdout-and-stderr operator; it ends with `&`
		// so the trailing-`|` case does not catch it, but appending completes it
		// the same way and routes the flag to the right side of the pipe
		// (#5167 review: "Detect trailing `|&` pipelines before appending flags").
		{"trailing pipe-both is a parse error appending completes", "claude |&", true},

		// A trailing operator followed by a line continuation (`\<newline>`) is a
		// parse error the shell joins to the appended flag: `claude |\<newline>`
		// joins to `claude |`, and injectSystemPrompt appends `--plugin-dir`, so
		// `claude |\<newline> --plugin-dir` is a valid pipe whose right side is the
		// flag rather than an argument to claude. The same applies to an incomplete
		// `&&`/`||` and an incomplete redirection (`>`, `<`, `>>`) behind a
		// continuation; an even run of backslashes is a real terminator, not a
		// continuation, so `claude \\\\` (a literal backslash arg) is unaffected
		// (#5167 review: "Handle incomplete operators followed by a continued
		// newline").
		{"trailing pipe then line continuation", "claude |\\\n", true},
		{"trailing and then line continuation", "claude &&\\\n", true},
		{"trailing or then line continuation", "claude ||\\\n", true},
		{"trailing redirect out then line continuation", "claude >\\\n", true},
		{"trailing redirect in then line continuation", "claude <\\\n", true},
		{"trailing append redirect then line continuation", "claude >>\\\n", true},

		// A value ending in more than one continued physical line joins to the
		// operator the continuations were hiding: `claude |\<newline>\<newline>`
		// joins to `claude |`, and injectSystemPrompt appends `--plugin-dir`, so
		// `claude |\<newline>\<newline> --plugin-dir` is a valid pipe whose right
		// side is the flag rather than an argument to claude. Every trailing
		// continuation is stripped before the suffix check, not only the final one
		// (#5167 review: "Strip every trailing line continuation before checking
		// operators").
		{"trailing pipe then two line continuations", "claude |\\\n\\\n", true},
		{"trailing and then two line continuations", "claude &&\\\n\\\n", true},
		{"trailing or then two line continuations", "claude ||\\\n\\\n", true},
		{"trailing redirect then two line continuations", "claude >\\\n\\\n", true},

		// The shell permits an unescaped newline after an incomplete
		// list/pipeline operator and completes it with the appended command, so
		// `claude &&\<newline>` (including spaces before that newline) becomes
		// `claude && --plugin-dir …` and the flag runs as the second command
		// rather than as an argument to claude. The trailing newline (with any
		// surrounding whitespace) is trimmed before the suffix check so the
		// operator, not the newline, is what the check sees (#5167 review:
		// "Recognize incomplete operators ending with a newline").
		{"trailing and then newline", "claude &&\n", true},
		{"trailing or then newline", "claude ||\n", true},
		{"trailing pipe then newline", "claude |\n", true},
		{"trailing and then spaces before newline", "claude &&  \n", true},
		{"trailing pipe then spaces before newline", "claude | \n", true},

		// A trailing `#` comment on the operator's line hides the operator from
		// the raw suffix check, but appending the flag completes the operator
		// (the comment ends at the newline), so the operator is still stripped of
		// the comment and the misroute is flagged: `claude | # note\n --plugin-dir
		// …` runs the flag as the right side of the pipe while claude starts
		// without the plugin. A `#` inside a quoted word is not a comment, so
		// `claude "a#b" |` is unaffected (#5167 review: "Strip trailing comments
		// before checking incomplete operators").
		{"trailing pipe then comment", "claude | # note\n", true},
		{"trailing and then comment", "claude && # note\n", true},
		{"trailing pipe then comment with no newline", "claude | # note", true},
		{"trailing pipe then comment line then another comment line", "claude | # note\n# more\n", true},
		{"trailing pipe with quoted hash is not a comment", "claude \"a#b\" |", true},

		// An unterminated here-document (`<<`/`<<-`) is a parse error whose
		// appended flag supplies the delimiter word, so the flag becomes the
		// delimiter (and the rest of the line its body) rather than a flag to the
		// agent: `claude <<- --plugin-dir <path>` uses `--plugin-dir` as the
		// here-document delimiter and claude starts without the injected option
		// (#5167 review: "Flag an unterminated `<<-` here-document").
		{"unterminated dash heredoc", "claude <<-", true},
		{"unterminated heredoc", "claude <<", true},

		// A named but unclosed here-document reaches the parse-error path with
		// the delimiter word as the suffix rather than a bare `<<`: the closing
		// delimiter line is missing, so injectSystemPrompt's appended flag is
		// consumed as here-document input and the agent starts without it
		// (#5167 review: "Recognize named unterminated here-documents").
		{"unterminated named heredoc", "claude <<EOF\n", true},
		{"unterminated named dash heredoc", "claude <<-EOF\n", true},
		{"unterminated named heredoc with space", "claude << EOF\n", true},
		{"unterminated named dash heredoc with space", "claude <<- EOF\n", true},

		// A parse error that appending does not complete (an unbalanced quote, an
		// open subshell) still fails loudly at launch and is not a misroute.
		{"unbalanced quote is a parse error appending does not complete", "claude '", false},
		{"unbalanced subshell is a parse error appending does not complete", "claude (", false},
		{"empty string", "", false},

		// A single simple call ending in an odd run of bare backslashes (no
		// trailing newline) escapes the leading space injection concatenates, so
		// `claude \` becomes `claude \ --plugin-dir …` and the `\ ` joins the
		// injected flag into one literal word ` --plugin-dir` rather than the
		// `--plugin-dir` option, so claude starts without the plugin. An even run
		// leaves a literal backslash arg and the injected space splits the flag
		// into its own word, so it is not flagged; trailing whitespace after the
		// backslash already separates the flag, so it is not flagged either
		// (#5167 review: "Flag a trailing escaped append separator").
		{"trailing odd backslash escapes the injected separator", "claude \\", true},
		{"trailing odd backslash after flags escapes the separator", "claude --model opus \\", true},
		{"trailing even backslash is a literal arg, not a separator", "claude \\\\", false},
		{"trailing odd run of three backslashes escapes the separator", "claude \\\\\\", true},
		{"trailing backslash then whitespace does not escape the separator", "claude \\ ", false},
		{"trailing backslash then newline is a continuation", "claude \\\n", false},
		// A backslash before a newline inside a `#` comment cannot continue the
		// line — the comment ends at the newline — so the newline is a real
		// statement terminator and the appended flag starts a new command
		// (#5167 review: "Do not treat comment backslashes as line continuations").
		{"trailing backslash in a comment is not a continuation", "claude # note \\\n", true},
		{"trailing backslash in a comment with flags is not a continuation", "claude --model opus # x \\\n", true},
		{"even backslash run in a comment is a real terminator", "claude # note \\\\\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandHasControlOperator(tc.command, "")
			require.Equalf(t, tc.want, got, "CommandHasControlOperator(%q)", tc.command)
		})
	}
}

// TestCommandHasControlOperatorAgentLast pins the compound-command refinement
// (#5167 review: "Do not flag compound commands whose final command is the
// agent"): a compound (|, &&, ||) appends the flag to the END of the value, which
// is the rightmost command, so the flag is misrouted only when that command is not
// the detected agent. `true && claude` routes the flag to claude (the last command)
// and is not a misroute, so it is not flagged with the agent passed; `claude | tee`
// and `claude && true` route it to a command that is not the agent and stay flagged.
// The agent is the first agent token the config loader detects, so a compound whose
// last command is that agent (`tee | claude`, `true && exec claude`) is not a
// misroute, while one whose last command is not the agent is. An empty agent keeps
// the prior behavior of flagging every compound.
func TestCommandHasControlOperatorAgentLast(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		agent   string
		want    bool
	}{
		// The agent is the last command of the compound, so the appended flag
		// reaches it — not a misroute.
		{"and with agent last", "true && claude", "claude", false},
		{"or with agent last", "true || claude", "claude", false},
		{"pipe with agent last", "tee | claude", "claude", false},
		{"and with agent last after flags", "true && claude --resume", "claude", false},
		{"and with agent last behind exec", "true && exec claude", "claude", false},
		{"and with agent last behind env", "true && env claude", "claude", false},
		{"and with agent last behind env --", "true && env -- claude", "claude", false},
		// The agent is the first command but not the last, so the flag is
		// routed to a non-agent command — a misroute.
		{"and with agent first", "claude && true", "claude", true},
		{"or with agent first", "claude || true", "claude", true},
		{"pipe with agent first", "claude | tee /tmp/log", "claude", true},
		// A chain whose rightmost command is itself a compound keeps the
		// rightmost command: `a | b && c` routes the flag to c, so the agent
		// is the last command only if c is.
		{"pipe then and with agent last", "tee | true && claude", "claude", false},
		{"pipe then and with agent first", "claude | tee && true", "claude", true},
		// An agent name with a path form is detected by its base name.
		{"and with absolute-path agent last", "true && /usr/local/bin/claude", "claude", false},
		{"and with absolute-path agent first", "/usr/local/bin/claude && true", "claude", true},
		// An empty agent keeps the prior behavior: every compound is flagged,
		// even when the last command would be the agent.
		{"empty agent flags and with agent last", "true && claude", "", true},
		{"empty agent flags pipe with agent last", "tee | claude", "", true},
		// A compound whose rightmost command is not a simple call (a subshell)
		// is conservatively flagged: the flag after the subshell is not the
		// agent invocation.
		{"and with subshell last", "claude && (claude)", "claude", true},
		// The tail-agent exemption only holds when the appended flag reaches the
		// agent. A `--` end-of-options terminator inside the final call demotes
		// the appended flag to a positional, so `true && claude --` becomes
		// `true && claude -- --plugin-dir …` and claude starts without the plugin
		// even though the agent is the last command (#5167 review: "Inspect
		// terminal `--` inside compound commands").
		{"and with agent last and trailing terminator", "true && claude --", "claude", true},
		{"or with agent last and trailing terminator", "true || claude --", "claude", true},
		{"pipe with agent last and trailing terminator", "tee | claude --", "claude", true},
		{"and with agent last and mid-command terminator", "true && claude -- --resume", "claude", true},
		{"and with agent last and terminator behind exec", "true && exec claude --", "claude", true},
		{"and with agent last and terminator behind env", "true && env -- claude --", "claude", true},
		{"and with agent last and env terminator only is not the agent's", "true && env -- claude", "claude", false},
		// A compound statement terminated by `;` or a trailing newline still
		// misroutes an appended flag even when the last command is the agent:
		// `true && claude;` becomes `true && claude; --plugin-dir …` and the flag
		// runs as its own command. The binary branch returns before the
		// Semicolon/trailing-newline checks, so a terminated compound would
		// otherwise be exempted (#5167 review: "Check terminators before
		// accepting a terminal compound agent").
		{"and with agent last and trailing semicolon", "true && claude;", "claude", true},
		{"or with agent last and trailing semicolon", "true || claude;", "claude", true},
		{"pipe with agent last and trailing semicolon", "tee | claude;", "claude", true},
		{"and with agent last and trailing semicolon after flags", "true && claude --model opus;", "claude", true},
		{"and with agent last and trailing newline", "true && claude\n", "claude", true},
		{"pipe with agent last and trailing newline", "tee | claude\n", "claude", true},
		{"and with agent last and trailing newline after flags", "true && claude --resume\n", "claude", true},
		// A compound whose tail is the agent still misroutes the flag when a `#`
		// shell comment follows that tail on the last line: the appended flag
		// lands inside the comment. CommandHasTrailingComment only inspects a
		// single simple call, so a binary command's tail comment would otherwise
		// be exempted by the tail-agent check and go unwarned (#5167 review:
		// "Scan tail comments in terminal compounds").
		{"and with agent last and trailing comment", "true && claude # note", "claude", true},
		{"pipe with agent last and trailing comment", "tee | claude # note", "claude", true},
		{"and with agent last and comment behind env", "true && env -- claude # note", "claude", true},
		// A trailing newline after the comment starts a new statement that
		// carries the flag, which is already a control operator; the comment is
		// not the misroute on its own.
		{"and with agent last and comment then trailing newline", "true && claude # note\n", "claude", true},
		// A compound whose tail is the agent still misroutes the flag when that
		// tail has a here-document redirect whose body is the last content on the
		// last line: the appended flag lands inside the here-document (or breaks
		// the closing delimiter). CommandHasHeredoc only inspects a single simple
		// call, so a binary command's tail heredoc would otherwise be exempted by
		// the tail-agent check and go unwarned (#5167 review: "Scan tail heredocs
		// in terminal compounds").
		{"and with agent last and trailing heredoc", "true && claude <<EOF\nprompt\nEOF", "claude", true},
		{"pipe with agent last and trailing heredoc", "tee | claude <<EOF\nprompt\nEOF", "claude", true},
		// A trailing newline after the closing delimiter starts a new statement
		// that carries the flag, which is already a control operator; the heredoc
		// is not the misroute on its own.
		{"and with agent last and heredoc then trailing newline", "true && claude <<EOF\nprompt\nEOF\n", "claude", true},
		// The tail-agent exemption only holds when the tail is the ONLY
		// selected-agent invocation. injectSystemPrompt selects the FIRST agent
		// DetectAgentFromCommand finds, so a compound that invokes the agent
		// earlier AND at the tail leaves the earlier (normally interactive)
		// invocation without the flag: `claude && claude` becomes
		// `claude && claude --plugin-dir …`, the first claude starts without the
		// plugin, and the tail-name match would otherwise hide it. The exemption
		// requires the left of the binary to be agent-free (#5167 review: "Warn
		// when an earlier matching agent misses the appended flag").
		{"and with agent twice", "claude && claude", "claude", true},
		{"or with agent twice", "claude || claude", "claude", true},
		{"pipe with agent twice", "claude | claude", "claude", true},
		{"and with agent twice with flags", "claude --resume && claude", "claude", true},
		{"pipe then and with agent twice", "claude | true && claude", "claude", true},
		{"and with agent earlier behind exec", "exec claude && claude", "claude", true},
		{"and with agent earlier behind env", "env claude && claude", "claude", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandHasControlOperator(tc.command, tc.agent)
			require.Equalf(t, tc.want, got, "CommandHasControlOperator(%q, %q)", tc.command, tc.agent)
		})
	}
}

// TestCommandHasControlOperatorMultiStatement pins the multi-statement tail-agent
// exemption (#5167 review: "Exempt terminal agents in statement sequences"): a
// `;`- or newline-separated sequence appends the flag to the END of the value,
// which is the LAST statement, so the flag is misrouted only when the last
// statement is not the agent or the flag cannot reach it (a `--` in the last
// call, a trailing comment, a heredoc, a terminator after it, or a trailing odd
// backslash run). `true; claude` routes the flag to claude and is not a misroute,
// while `claude; echo hi` routes it to echo and is. The single-call predicates
// (CommandEndsOptionsTerminator, CommandHasTrailingComment, CommandHasHeredoc)
// only inspect a one-statement value, so the last statement is evaluated
// comprehensively here: a `--`/comment/heredoc on the last statement of a
// multi-statement value would otherwise go unwarned. An empty agent keeps the
// prior behavior of flagging every multi-statement.
func TestCommandHasControlOperatorMultiStatement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		agent   string
		want    bool
	}{
		// The agent is the last statement, so the appended flag reaches it — not a
		// misroute.
		{"semicolon with agent last", "true; claude", "claude", false},
		{"newline with agent last", "true\nclaude", "claude", false},
		{"semicolon with agent last after flags", "true; claude --resume", "claude", false},
		{"semicolon with agent last behind exec", "true; exec claude", "claude", false},
		{"semicolon with agent last behind env", "true; env -- claude", "claude", false},
		{"two statements then agent last", "echo one; echo two; claude", "claude", false},
		// The agent is the first statement but not the last, so the flag is routed
		// to a non-agent command — a misroute.
		{"semicolon with agent first", "claude; echo hi", "claude", true},
		{"newline with agent first", "claude\necho hi", "claude", true},
		// A backgrounded statement before the agent still routes the flag to the
		// last (agent) statement.
		{"background then agent last", "echo hi & claude", "claude", false},
		// The last statement is a compound whose tail is the agent, so the flag
		// reaches it through the compound — not a misroute.
		{"semicolon then compound with agent last", "echo one; true && claude", "claude", false},
		{"semicolon then pipe with agent last", "echo one; tee | claude", "claude", false},
		// A `--` in the last call demotes the appended flag even when the last
		// statement is the agent: the single-call predicate does not see a
		// multi-statement value, so the tail `--` is checked here.
		{"semicolon with agent last and trailing terminator", "true; claude --", "claude", true},
		{"semicolon with agent last and mid-command terminator", "true; claude -- --resume", "claude", true},
		// A trailing comment on the last statement swallows the appended flag.
		{"semicolon with agent last and trailing comment", "true; claude # note", "claude", true},
		// A heredoc on the last statement swallows the appended flag.
		{"semicolon with agent last and trailing heredoc", "true; claude <<EOF\nprompt\nEOF", "claude", true},
		// A terminator after the last agent still misroutes: `true; claude;`
		// becomes `true; claude; --plugin-dir …` and the flag runs as its own
		// command.
		{"semicolon with agent last and trailing semicolon", "true; claude;", "claude", true},
		{"newline with agent last and trailing newline", "true; claude\n", "claude", true},
		// A trailing odd backslash on the last statement escapes the injected
		// separator, so the flag does not reach the agent.
		{"semicolon with agent last and trailing backslash", "true; claude \\", "claude", true},
		// The last statement is not a simple call (a subshell), so the flag is
		// routed to a command that is not the agent — a misroute.
		{"semicolon with subshell last", "claude; (claude)", "claude", true},
		// The tail-agent exemption holds only when the tail is the ONLY
		// selected-agent invocation. injectSystemPrompt selects the FIRST
		// agent DetectAgentFromCommand finds, so a statement list that invokes
		// the agent earlier AND at the tail leaves the earlier (normally
		// interactive) invocation without the flag: `claude; claude` becomes
		// `claude; claude --plugin-dir …`, the first claude starts without the
		// plugin, and the tail-name match would otherwise hide it. The earlier
		// invocation that selects the agent is itself a misroute (#5167 review:
		// "Warn when an earlier statement also invokes the agent").
		{"semicolon with agent twice", "claude; claude", "claude", true},
		{"newline with agent twice", "claude\nclaude", "claude", true},
		{"semicolon with agent twice and flags", "claude --resume; claude", "claude", true},
		{"semicolon with agent earlier behind exec", "exec claude; claude", "claude", true},
		{"semicolon with agent earlier behind env", "env claude; claude", "claude", true},
		{"two agents then non-agent last still warns on earlier", "claude; claude; echo hi", "claude", true},
		// An earlier non-agent statement does not select the agent, so a tail
		// agent still routes the flag correctly — not a misroute (covered above
		// by "two statements then agent last").
		// An empty agent keeps the prior behavior: every multi-statement is
		// flagged, even when the last statement would be the agent.
		{"empty agent flags semicolon with agent last", "true; claude", "", true},
		{"empty agent flags newline with agent last", "true\nclaude", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandHasControlOperator(tc.command, tc.agent)
			require.Equalf(t, tc.want, got, "CommandHasControlOperator(%q, %q)", tc.command, tc.agent)
		})
	}
}

// The two predicates are mutually exclusive on the shapes the warning pairs them
// on: a terminator requires a single simple call (a `--` word the parser would
// otherwise route into a compound), while a control operator requires NOT a
// single simple call (a `|`/`&&`/subshell) or a single call terminated by a `;`
// or newline (which has no `--` word). The one exception is the exec-prefix
// family, where the exec builtin is a call even with the separator — and that is
// already covered by CommandUsesExecSeparator. warnLaunchFlagMismap's switch
// takes the first matching kind, so even a value that trips both (a single call
// with a `--` AND a `;`, like `claude --;`) emits one warning, not two. This
// pins the invariant on the shapes the two predicates are paired on, so a future
// edit cannot accidentally make both fire on the same plain value.
func TestEndsOptionsTerminatorAndControlOperatorAreMutuallyExclusive(t *testing.T) {
	for _, command := range []string{
		"claude",
		"claude --model opus",
		"claude --",
		"claude --model opus --",
		"/usr/local/bin/claude --",
		"claude | tee /tmp/log",
		"claude foo && claude bar",
		"claude; echo hi",
		"claude &",
	} {
		trailing := CommandEndsOptionsTerminator(command)
		control := CommandHasControlOperator(command, "")
		require.Falsef(t, trailing && control,
			"%q matched both predicates; they are meant to be mutually exclusive", command)
	}
}

// TestCommandHasHeredoc pins the here-document shape (#5167 review: "Handle
// here-document delimiters before appending flags"): a `<<`/`<<-` redirect makes
// the appended flag land inside the here-document body (or break the closing
// delimiter), so the agent does not receive it. A trailing newline after the
// closing delimiter is already a control operator, so this fires only when the
// heredoc body is the last content on the last line.
func TestCommandHasHeredoc(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		{"heredoc", "claude <<EOF\nprompt\nEOF", true},
		{"heredoc with dash", "claude <<-EOF\n\tprompt\nEOF", true},
		{"heredoc empty body", "claude <<EOF\nEOF", true},
		// A trailing newline after the closing delimiter starts a new statement that
		// carries the flag, which the control-operator predicate already warns about;
		// the heredoc predicate does not fire (the flag is not in the body).
		{"heredoc with trailing newline", "claude <<EOF\nprompt\nEOF\n", false},
		// An ordinary input/output redirection does not misroute the flag (the agent
		// still receives it), so it is not a heredoc.
		{"stdout redirect", "claude > /tmp/log", false},
		{"stdin redirect", "claude < /tmp/in", false},
		{"bare agent", "claude", false},
		{"pipe", "claude | tee /tmp/log", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandHasHeredoc(tc.command)
			require.Equalf(t, tc.want, got, "CommandHasHeredoc(%q)", tc.command)
		})
	}
}

// TestCommandInvokesAgentViaInterpreter pins the interpreter `-c` shape
// (e.g. `sh -c 'claude'`): the agent is the script, and the flag
// injectSystemPrompt appends to the END of the value lands after the script as a
// positional to the interpreter, not as an argument to the agent. The terminator,
// control-operator, and comment predicates do not flag this single CallExpr, so
// this predicate is what surfaces it (#5167 review: "Warn when an agent is
// invoked through `sh -c`").
func TestCommandInvokesAgentViaInterpreter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		{"sh -c script", "sh -c 'claude'", true},
		{"bash -c script", "bash -c 'claude'", true},
		{"dash -c script", "dash -c 'claude'", true},
		{"zsh -c script", "zsh -c 'claude'", true},
		{"exec sh -c script", "exec sh -c 'claude'", true},
		{"sh -c with args before the flag", "sh -c 'claude --model opus'", true},
		// A combined short-option cluster such as `bash -ic 'claude'` also carries
		// the `-c` flag (bash --help lists `-ilrsD or -c command`): the shell
		// runs the next word as a script, so the appended flag is a positional to
		// the interpreter, not an argument to the agent. `-ic`, `-ci`, and `-lc`
		// all flag it; a cluster without `c` (`-il`) does not (#5167 review:
		// "Detect combined shell -c options").
		{"bash combined -ic", "bash -ic 'claude'", true},
		{"bash combined -ci", "bash -ci 'claude'", true},
		{"bash combined -ilc", "bash -ilc 'claude'", true},
		{"bash combined -sc", "bash -sc 'claude'", true},
		{"dash combined -c", "dash -c 'claude'", true},
		{"bash combined without c", "bash -il 'claude'", false},
		// `-e` (errexit) is an argument-free shell option for bash and dash, so a
		// cluster such as `bash -ec 'claude'` is also a `-c` invocation: the
		// appended flag is a positional to the shell, not an argument to the agent
		// inside the script (#5167 review: "Recognize `-e` in shell `-c` option
		// clusters"). `-ec` and `-ce` both flag it; a cluster without `c` (`-ei`)
		// does not.
		{"bash combined -ec", "bash -ec 'claude'", true},
		{"bash combined -ce", "bash -ce 'claude'", true},
		{"bash combined -eic", "bash -eic 'claude'", true},
		{"bash combined -ei", "bash -ei 'claude'", false},
		// `-u` (nounset) is an argument-free shell option for bash and dash, so a
		// cluster such as `bash -euc 'claude'` is also a `-c` invocation: the
		// appended flag is a positional to the shell, not an argument to the agent
		// inside the script (#5167 review: "Include `-u` in shell `-c` option
		// clusters"). `-euc`, `-cue`, and `-ue` all flag it; a cluster without `c`
		// (`-eu`) does not.
		{"bash combined -euc", "bash -euc 'claude'", true},
		{"bash combined -cue", "bash -cue 'claude'", true},
		{"bash combined -uc", "bash -uc 'claude'", true},
		{"bash combined -eu", "bash -eu 'claude'", false},
		// `-x` (xtrace) is an argument-free shell option for bash and dash, so a
		// cluster such as `bash -xc 'claude'` is also a `-c` invocation: the
		// appended flag is a positional to the shell, not an argument to the agent
		// inside the script (#5167 review: "Recognize additional shell flags before
		// `-c`"). `-xc`, `-cx`, and `-xuc` all flag it; a cluster without `c`
		// (`-xe`) does not.
		{"bash combined -xc", "bash -xc 'claude'", true},
		{"bash combined -cx", "bash -cx 'claude'", true},
		{"bash combined -xuc", "bash -xuc 'claude'", true},
		{"bash combined -uxc", "bash -uxc 'claude'", true},
		{"bash combined -xe", "bash -xe 'claude'", false},
		// A flag that takes an argument before `c` would swallow a following `c`
		// as its operand rather than the script flag; bash's `-O shopt_option`
		// takes the next word, so `-Oc` is not the `-c` shape (conservative: do
		// not claim the script flag after an unknown flag).
		{"bash -Oc is not a -c cluster", "bash -Oc 'claude'", false},
		// A plain `sh claude` (no `-c`) runs claude as a script FILE, not the
		// `-c` shape this predicate is about; it is not flagged here.
		{"sh without -c", "sh claude", false},
		// A non-shell command with `-c` as one of its own flags is not an
		// interpreter wrapper: claude has no `-c` flag shape that takes a script,
		// so the predicate must not fire.
		{"claude with -c flag", "claude -c foo", false},
		{"bare agent", "claude", false},
		{"agent with flags", "claude --model opus", false},
		{"env wrapper", "env -- claude", false},
		// A wrapper (env, or an argv-passthrough wrapper in front of env) in
		// front of the interpreter reaches Claude detection, but a words[0]-only
		// check saw the wrapper and returned false, so the misroute went
		// unwarned: `env sh -c 'claude'` appends `--plugin-dir` as a positional to
		// sh, not to the claude inside the script. The wrapper prefix is peeled so
		// the interpreter — not the wrapper — is what the shell check runs on
		// (#5167 review: "Unwrap interpreter wrappers before checking `-c`").
		{"env then sh -c script", "env sh -c 'claude'", true},
		{"env -- then sh -c script", "env -- sh -c 'claude'", true},
		{"env with VAR then sh -c script", "env VAR=1 sh -c 'claude'", true},
		// env in front of a non-shell command is not an interpreter wrapper: the
		// `--plugin-dir` appended after `env claude` reaches claude normally.
		{"env without a shell is not an interpreter", "env claude", false},
		// A script that forwards its positionals to the command it invokes
		// carries af's appended flag — a positional to the interpreter —
		// through to the agent, so `sh -c 'exec "$@"' sh claude` becomes
		// `sh -c 'exec "$@"' sh claude --plugin-dir …` and claude receives
		// `--plugin-dir`; this is not a misroute and must not warn (#5167
		// review: "Avoid warning for forwarding `sh -c` wrappers"). Only
		// `"$@"`/`$@` preserve the appended flag as separate argv entries;
		// quoted `"$*"` collapses the positionals into one program name and
		// unquoted `$*` is IFS-dependent, so `$*` is NOT forwarding and stays
		// warned (#5167 review: "Do not treat quoted `$*` as argv forwarding");
		// a script that names the agent without forwarding still keeps the
		// flag from it.
		{"sh -c exec argv pass-through", `sh -c 'exec "$@"' sh claude`, false},
		{"bash -c exec argv pass-through", `bash -c 'exec "$@"' bash claude`, false},
		{"sh -c unquoted argv pass-through", `sh -c 'exec $@' sh claude`, false},
		{"sh -c quoted star does not forward", `sh -c 'exec "$*"' sh claude`, true},
		{"sh -c unquoted star does not forward", `sh -c 'exec $*' sh claude`, true},
		{"sh -c pass-through with agent in script", `sh -c 'claude "$@"' sh claude`, false},
		// A script that names the agent itself with `"$@"` as an argument
		// (`claude "$@"`) invokes the agent directly, so the `$0` operand is
		// irrelevant: even with the agent as `$0` (`sh -c 'claude "$@"' claude`),
		// `"$@"` carries the appended flag to the hard-coded `claude` and the
		// wrapper is safe. The placeholder check only applies when the script
		// runs the positionals as the command (`exec "$@"`), where `$0` is the
		// only place the agent can live (#5167 review: "Do not warn for
		// hard-coded `$@` forwarding without `$0`").
		{"sh -c hard-coded agent argv without $0 pass-through", `sh -c 'claude "$@"' claude`, false},
		{"env sh -c exec argv pass-through", `env sh -c 'exec "$@"' sh claude`, false},
		{"bash -ic exec argv pass-through", `bash -ic 'exec "$@"' bash claude`, false},
		{"sh -c names agent without forwarding still warns", `sh -c 'claude'`, true},
		{"sh -c exec without argv still warns", `sh -c 'exec claude'`, true},
		// A `$@` that is consumed by an earlier statement and not by the
		// command that detection selects does not forward the appended flag:
		// `echo "$@"; claude` lets `echo` consume the positionals and the
		// hard-coded `claude` start without the plugin, so a bare substring test
		// would suppress a real misroute. The exemption only covers a `$@` in the
		// last statement of the script, the one the appended positional must
		// reach (#5167 review: "Only exempt scripts that actually forward
		// positional argv").
		{"sh -c echo argv then agent warns", `sh -c 'echo "$@"; claude'`, true},
		{"sh -c echo argv then exec argv last pass-through", `sh -c 'echo "$@"; exec "$@"'`, false},
		{"sh -c exec argv last pass-through", `sh -c 'echo hi; exec "$@"'`, false},
		// A `$@` that is only consumed by a non-agent command (`echo "$@"`)
		// prints the positionals and exits without launching the agent, so the
		// exemption does not apply and the wrapper still warns (#5167 review:
		// "Verify `$@` actually launches the forwarded command").
		{"sh -c echo argv does not forward", `sh -c 'echo "$@"' placeholder claude`, true},
		// A forwarding wrapper that puts the agent in the `$0` position has no
		// placeholder before it: `exec "$@"` then runs the appended flag rather
		// than the agent, so the wrapper still warns. A placeholder before the
		// agent (`sh -c 'exec "$@"' sh claude`) is the safe pass-through shape
		// (#5167 review: "Require a `$0` placeholder before exempting forwarding
		// shells").
		{"sh -c forwarding without $0 placeholder warns", `sh -c 'exec "$@"' claude`, true},
		{"sh -c forwarding with $0 placeholder pass-through", `sh -c 'exec "$@"' sh claude`, false},
		// `-v` (verbose) is an argument-free Bash shell option, so a cluster
		// such as `bash -vc 'claude'` is also a `-c` invocation: the appended flag
		// is a positional to the shell, not an argument to the agent inside the
		// script (#5167 review: "Scan all argument-free shell flags before
		// `-c`").
		{"bash combined -vc", "bash -vc 'claude'", true},
		{"bash combined -cv", "bash -cv 'claude'", true},
		{"bash combined -vuc", "bash -vuc 'claude'", true},
		{"bash combined -vxn", "bash -vxn 'claude'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandInvokesAgentViaInterpreter(tc.command, "claude")
			require.Equalf(t, tc.want, got, "CommandInvokesAgentViaInterpreter(%q)", tc.command)
		})
	}
}
