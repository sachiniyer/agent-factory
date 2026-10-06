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

		// A parse error that appending does not complete (an unbalanced quote, an
		// open subshell) still fails loudly at launch and is not a misroute.
		{"unbalanced quote is a parse error appending does not complete", "claude '", false},
		{"unbalanced subshell is a parse error appending does not complete", "claude (", false},
		{"empty string", "", false},
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
		{"empty string", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandInvokesAgentViaInterpreter(tc.command)
			require.Equalf(t, tc.want, got, "CommandInvokesAgentViaInterpreter(%q)", tc.command)
		})
	}
}
