package tmux

import (
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// resumeProgram derives a "resume the most recent session in cwd" variant of
// program for use when Restore() re-spawns a vanished tmux session (#386,
// #595). For agents without a resume-most-recent flag, programs that already
// include one, and unknown programs, returns program unchanged.
//
// Agent-specific rewrites preserve the original program
// string verbatim (modulo the inserted resume tokens) so user shell quoting,
// $VAR / ~ / glob metacharacters survive respawn unchanged (#640):
//
//   - claude: append --continue at the end. claude's resume flags are
//     position-independent, so appending preserves the original program
//     string verbatim (including any shell quoting on the executable
//     path — see #569).
//   - codex: splice " resume --last" into the original program string at
//     the byte offset right after the codex (or "codex exec") token.
//     Subcommand position matters for codex, so this can't be a tail
//     append; but a tokenize+rejoin round-trip would defensively quote
//     metachars in user flags (#640 regression of #596) — splicing the
//     original avoids that.
//   - aider: append --restore-chat-history at the end. Reads
//     .aider.chat.history.md from cwd if present; silently falls back to
//     a fresh chat if absent. Skipped if the user passed an explicit
//     --no-restore-chat-history opt-out.
//   - gemini: append --resume latest at the end. The "latest" keyword
//     resumes the most recent session in cwd and silently falls back
//     to a fresh session if none exists.
//   - amp: insert "threads continue --last" after any leading Amp global
//     options. Amp's resume path is a subcommand; explicit user subcommands
//     like "amp review" are left unchanged.
//   - opencode: append --continue at the end. opencode's TUI is its DEFAULT
//     command ("opencode [project]"), and --continue is a position-independent
//     boolean flag on it, so this is a tail append like claude/gemini/aider —
//     NOT a codex/amp-style subcommand splice. KNOWN, MEASURED WART: when no
//     opencode session exists for the cwd yet, --continue does fall back to a
//     fresh session (the same contract aider/gemini have above) but announces it
//     with a transient error overlay first — see opencodeResumePrecondition.
//
// The latest-session paths above either fall back to a fresh session when no
// prior session exists or are left unchanged when af cannot identify a safe
// provider resume command.
func resumeProgram(program string) string {
	tokens, ends := splitShellTokens(program)
	agentIdx, agent := findAgentToken(tokens)
	if agentIdx < 0 {
		return program
	}

	switch agent {
	case ProgramClaude:
		// Only scan tokens AFTER the agent token. Tokens before it belong to
		// a wrapper command (e.g. `ionice -c 3 claude`, `taskset -c 0-3
		// claude`) whose flags can collide with claude's resume flags and
		// false-positive the already-has-resume check (#742). Mirrors the
		// position-aware codex check below (#632).
		for _, tok := range tokens[agentIdx+1:] {
			if tok == "-c" || tok == "--continue" || tok == "-r" || tok == "--resume" ||
				strings.HasPrefix(tok, "--resume=") || strings.HasPrefix(tok, "-r=") ||
				isShortResumeWithAttachedValue(tok) {
				return program
			}
		}
		return program + " --continue"
	case ProgramCodex:
		// "resume" is a subcommand, not a flag, and codex only accepts it
		// immediately after the codex token (or after "exec"). Checking any
		// other position would false-positive on flag values like
		// `codex --profile resume` (#632).
		insertAt := agentIdx + 1
		if insertAt < len(tokens) && tokens[insertAt] == "exec" {
			insertAt++
		}
		if insertAt < len(tokens) && tokens[insertAt] == "resume" {
			return program
		}
		// Splice " resume --last" into the ORIGINAL program string at the
		// byte offset right after the codex (or "codex exec") token so
		// the user's quoting / $VAR / ~ / * / ? all pass through
		// untouched. A tokenize+rejoin round-trip would defensively
		// single-quote those metachars and break shell expansion on
		// respawn (#640).
		off := ends[insertAt-1]
		return program[:off] + " resume --last" + program[off:]
	case ProgramAider:
		// Only scan tokens after the agent token so wrapper-command flags
		// can't false-positive the already-has-resume check (#742).
		for _, tok := range tokens[agentIdx+1:] {
			if tok == "--restore-chat-history" || tok == "--no-restore-chat-history" {
				return program
			}
		}
		return program + " --restore-chat-history"
	case ProgramGemini:
		// Only scan tokens after the agent token so wrapper-command flags
		// (e.g. `ionice -c 3 gemini`) can't false-positive the
		// already-has-resume check (#742).
		for _, tok := range tokens[agentIdx+1:] {
			if tok == "--resume" || tok == "-r" ||
				strings.HasPrefix(tok, "--resume=") || strings.HasPrefix(tok, "-r=") ||
				isShortResumeWithAttachedValue(tok) {
				return program
			}
		}
		return program + " --resume latest"
	case ProgramAmp:
		insertAt, alreadyResume := ampResumeInsertIndex(tokens, agentIdx)
		if alreadyResume || insertAt < 0 {
			return program
		}
		off := ends[insertAt-1]
		return program[:off] + " threads continue --last" + program[off:]
	case ProgramOpencode:
		// Only scan tokens after the agent token so wrapper-command flags
		// (e.g. `ionice -c 3 opencode`, whose "-c" is ionice's class flag and
		// not opencode's --continue) can't false-positive the already-has-resume
		// check (#742).
		for _, tok := range tokens[agentIdx+1:] {
			if isOpencodeResumeFlag(tok) {
				return program
			}
		}
		return program + " --continue"
	}
	return program
}

// KNOWN LIMITATION — opencode resume precondition.
//
// af does NOT check that a session exists before asking opencode to continue one.
//
// Measured against 0.0.0-main-202604230742 with a 2x2 control — the error appears
// ONLY in (no prior session + --continue); (prior session + --continue) and (no
// prior session + no flag) are both clean:
//
//	opencode --continue, no session for this cwd
//	  → a validation error overlay ("sessionID": "dummy", code "invalid_format")
//	    is drawn ~3.5s-7.5s into boot, and opencode then runs a FRESH session.
//
// Why af ships this rather than gating on the precondition:
//
//   - The end state is USABLE, which is the bar. The overlay is NON-MODAL and does
//     not capture input: a prompt delivered while it is up still reaches the
//     composer, submits, and is answered (verified end-to-end). Falling back to a
//     fresh session when there is nothing to resume is exactly the contract aider
//     and gemini already have; opencode's fallback is merely NOISY rather than
//     silent, which is an opencode-side defect (it synthesizes a "dummy" sessionID
//     that then fails its own validation) and not a wrong flag choice by af.
//   - Checking is not cheap or stable. opencode keeps sessions in SQLite under its
//     data dir, not in a per-project path af could stat; `opencode session list`
//     answers but costs ~1.4s, which would land on EVERY restore, in a function
//     that is deliberately pure (no I/O, no subprocesses that can hang the
//     daemon's restore path). Deriving the db filename instead would couple af to
//     an undocumented internal naming scheme.
//   - Suppressing readiness until the overlay clears was considered and REJECTED:
//     it trades a cosmetic wart for a create failure. The overlay was observed
//     still present long after the ~8s self-clear once the pane had been
//     interacted with, so gating on it risks WaitForReady spinning its full 60s
//     timeout and failing the create outright.
//
// Left as a named limitation rather than a silent one. If opencode fixes the
// fallback, or exposes a cheap "has a session for this cwd" probe, this becomes
// deletable.

// isOpencodeResumeFlag reports whether tok already expresses a resume intent to
// opencode — either "continue the last session" (-c/--continue) or "continue
// this exact session" (-s/--session). Both forms are checked because appending
// --continue on top of an explicit --session would hand opencode two conflicting
// session selectors.
//
// "-s" is opencode's only "-s"-prefixed short flag (per `opencode --help` on
// 0.0.0-main-202604230742: -h, -v, -m, -c, -s), so an "-s"-prefixed token
// carrying an attached value ("-sses_091dbcc") is unambiguously a session
// selector — the same reasoning isShortResumeWithAttachedValue applies to "-r"
// for claude/gemini.
func isOpencodeResumeFlag(tok string) bool {
	switch {
	case tok == "-c" || tok == "--continue":
		return true
	case tok == "-s" || tok == "--session":
		return true
	case strings.HasPrefix(tok, "--session=") || strings.HasPrefix(tok, "-s="):
		return true
	case strings.HasPrefix(tok, "-s") && len(tok) > 2:
		return true
	}
	return false
}

// ResumeProgramWithConversationID derives a "resume this exact conversation"
// variant of program when the detected agent matches the recorded provider. It
// returns ok=false when the provider has no id-specific resume path here, when
// the command already carries an explicit resume/session flag, or when the
// recorded provider no longer matches the resolved command. Callers should fall
// back to Restore's latest-session resume behavior in those cases.
func ResumeProgramWithConversationID(program, recordedAgent, id string) (rewritten string, ok bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return program, false
	}
	tokens, ends := splitShellTokens(program)
	agentIdx, agent := findAgentToken(tokens)
	if agentIdx < 0 || agent == "" || recordedAgent != agent {
		return program, false
	}

	switch agent {
	case ProgramClaude:
		for _, tok := range tokens[agentIdx+1:] {
			if tok == "-c" || tok == "--continue" || tok == "-r" || tok == "--resume" ||
				strings.HasPrefix(tok, "--resume=") || strings.HasPrefix(tok, "-r=") ||
				isShortResumeWithAttachedValue(tok) ||
				tok == "--session-id" || strings.HasPrefix(tok, "--session-id=") {
				return program, false
			}
		}
		return program + " --resume " + shellQuoteArg(id), true
	case ProgramCodex:
		insertAt := agentIdx + 1
		if insertAt < len(tokens) && tokens[insertAt] == "exec" {
			insertAt++
		}
		if insertAt < len(tokens) && tokens[insertAt] == "resume" {
			return program, false
		}
		off := ends[insertAt-1]
		return program[:off] + " resume " + shellQuoteArg(id) + program[off:], true
	case ProgramAmp:
		insertAt, alreadyResume := ampResumeInsertIndex(tokens, agentIdx)
		if alreadyResume || insertAt < 0 {
			return program, false
		}
		off := ends[insertAt-1]
		return program[:off] + " threads continue " + shellQuoteArg(id) + program[off:], true
	}
	return program, false
}

func shellQuoteArg(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			strings.ContainsRune("_@%+=:,./-", r))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

// ClaudeProgramWithSessionID appends Claude Code's explicit conversation id flag
// to a first-launch program string. It preserves the original program verbatim
// except for the appended tokens and refuses to inject when the Claude command
// already carries a resume/continue/session-id flag.
func ClaudeProgramWithSessionID(program, sessionID string) (string, bool) {
	if strings.TrimSpace(sessionID) == "" {
		return program, false
	}
	tokens, _ := splitShellTokens(program)
	agentIdx, agent := findAgentToken(tokens)
	if agent != ProgramClaude {
		return program, false
	}
	for _, tok := range tokens[agentIdx+1:] {
		if tok == "-c" || tok == "--continue" || tok == "-r" || tok == "--resume" ||
			strings.HasPrefix(tok, "--resume=") || strings.HasPrefix(tok, "-r=") ||
			isShortResumeWithAttachedValue(tok) ||
			tok == "--session-id" || strings.HasPrefix(tok, "--session-id=") {
			return program, false
		}
	}
	return program + " --session-id " + sessionID, true
}

// DetectAgentFromCommand returns the canonical agent name (one of
// SupportedPrograms) that a resolved command string will actually run, or ""
// when no agent token is present — e.g. a program_overrides entry that points
// an agent name at a plain shell or an arbitrary tool (#1116, #1131).
//
// Every agent-specific spawn/restore behavior (flag injection, readiness
// heuristics, trust-prompt handling) must key off THIS — what will actually
// run — never off the config-name enum an instance was created with: the two
// diverge exactly when program_overrides redirects an agent name, and keying
// off the name injects flags the real program rejects (it exits instantly and
// the spawn surfaces as an opaque timeout).
//
// The scan mirrors resumeProgram's: every shell token is checked, so wrapper
// prefixes like `ionice -c 3 claude` still match (#742), and a token counts
// only when filepath.Base equals a SupportedPrograms entry verbatim — a path
// like /opt/claude-wrapper/run never matches on substring.
func DetectAgentFromCommand(command string) string {
	tokens, _ := splitShellTokens(command)
	_, agent := findAgentToken(tokens)
	return agent
}

// findAgentToken returns the index and canonical name of the first token whose
// filepath.Base equals a SupportedPrograms entry, or (-1, "") when none does.
func findAgentToken(tokens []string) (int, string) {
	idx, agent, _ := findAgentTokenStrict(tokens)
	return idx, agent
}

// findAgentTokenStrict is findAgentToken with the parser failure preserved for
// callers that must explain why a command cannot be modeled. Detection-only
// callers deliberately collapse that failure to "no agent" and fail closed.
func findAgentTokenStrict(tokens []string) (int, string, error) {
	for i := 0; i < len(tokens); {
		tok := tokens[i]
		if _, _, assignment := shellAssignment(tok); assignment {
			i++
			continue
		}
		if strings.EqualFold(baseCommand(tok), "env") {
			invocation, err := envcommand.Parse(tokens[i+1:], envcommand.Policy{AllowAssignments: true})
			if err != nil {
				return -1, "", err
			}
			if invocation.CommandIndex < 0 {
				return -1, "", nil
			}
			i += 1 + invocation.CommandIndex
			continue
		}
		base := strings.ToLower(filepath.Base(tok))
		for _, supported := range SupportedPrograms {
			if base == supported {
				return i, supported, nil
			}
		}
		i++
	}
	return -1, "", nil
}

// ampResumeInsertIndex returns the token index before which Amp's resume
// subcommand should be inserted. Amp global options may appear before the
// subcommand ("amp --no-ide threads continue --last"), while an explicit user
// subcommand ("amp review", "amp threads list") should not be rewritten.
func ampResumeInsertIndex(tokens []string, agentIdx int) (insertAt int, alreadyResume bool) {
	i := agentIdx + 1
	for i < len(tokens) {
		tok := tokens[i]
		switch {
		case tok == "--":
			return -1, false
		case tok == "-x" || strings.HasPrefix(tok, "-x") || tok == "--execute" || strings.HasPrefix(tok, "--execute="):
			return -1, false
		case tok == "-h" || tok == "--help" || tok == "-V" || tok == "--version" || tok == "-v":
			return -1, false
		case ampFlagHasAttachedValue(tok):
			i++
		case ampFlagTakesValue(tok):
			i++
			if i < len(tokens) {
				i++
			}
		case tok == "--plugin-ready-timeout":
			i++
			if i < len(tokens) && !strings.HasPrefix(tokens[i], "-") {
				i++
			}
		case ampKnownBooleanFlag(tok):
			i++
		case strings.HasPrefix(tok, "-"):
			i = ampConsumeUnknownFlag(tokens, i)
		default:
			goto command
		}
	}
	return i, false

command:
	switch tokens[i] {
	case "last", "l":
		return i, true
	case "threads", "thread", "t":
		if i+1 < len(tokens) && (tokens[i+1] == "continue" || tokens[i+1] == "c") {
			return i, true
		}
		return -1, false
	default:
		return -1, false
	}
}

func ampFlagTakesValue(tok string) bool {
	switch tok {
	case "--visibility", "--settings-file", "--log-level", "--log-file",
		"--mcp-config", "-m", "--mode", "--effort", "-l", "--label":
		return true
	default:
		return false
	}
}

func ampFlagHasAttachedValue(tok string) bool {
	for _, prefix := range []string{
		"--visibility=", "--settings-file=", "--log-level=", "--log-file=",
		"--mcp-config=", "--mode=", "--effort=", "--label=",
		"--plugin-ready-timeout=",
	} {
		if strings.HasPrefix(tok, prefix) {
			return true
		}
	}
	return strings.HasPrefix(tok, "-m") && len(tok) > 2 ||
		strings.HasPrefix(tok, "-l") && len(tok) > 2
}

func ampKnownBooleanFlag(tok string) bool {
	switch tok {
	case "--notifications", "--no-notifications",
		"--color", "--no-color",
		"--ide", "--no-ide",
		"--stream-json", "--stream-json-thinking", "--stream-json-input",
		"--no-archive-after-execute":
		return true
	default:
		return false
	}
}

func ampConsumeUnknownFlag(tokens []string, i int) int {
	tok := tokens[i]
	i++
	if strings.Contains(tok, "=") {
		return i
	}
	// Accepted forward-compat limitation: for an unknown Amp option, we cannot
	// know whether `amp --foo threads` means boolean --foo plus the `threads`
	// subcommand, or value-taking --foo whose value is "threads". All current
	// value-taking global options from `amp --help` are enumerated above; if
	// Amp ships another one, add it to ampFlagTakesValue so resume insertion
	// can consume its value unambiguously.
	if i < len(tokens) && !strings.HasPrefix(tokens[i], "-") && !ampKnownTopLevelCommand(tokens[i]) {
		i++
	}
	return i
}

func ampKnownTopLevelCommand(tok string) bool {
	switch tok {
	case "version", "logout", "login", "orb", "clone", "top",
		"last", "l",
		"threads", "thread", "t",
		"tools", "tool",
		"review",
		"skill", "skills",
		"permissions", "permission",
		"mcp", "config",
		"projects", "usage",
		"update", "up":
		return true
	default:
		return false
	}
}

// isShortResumeWithAttachedValue reports whether tok is the POSIX
// attached-value form of the short resume flag, e.g. "-r5" or "-rlatest"
// (#685). Both claude and gemini expose "-r" as their only "-r*" short flag
// (per their --help output), so any "-r"-prefixed token with a non-"="
// attached value is unambiguously a resume flag. The "=" forms ("-r" /
// "-r=VALUE") are matched separately by the callers, so they're excluded here.
// Not used for codex (resume is a subcommand) or aider (no "-r" resume flag).
func isShortResumeWithAttachedValue(tok string) bool {
	return strings.HasPrefix(tok, "-r") && len(tok) > 2 && tok[2] != '='
}

// splitShellTokens tokenizes a shell-style command string, respecting single
// quotes (no escapes), double quotes (with \" and \\ escapes), and backslash
// escapes outside quotes. Adjacent runs concatenate into a single token (e.g.
// 'foo'bar -> "foobar"). Unclosed quotes consume to end of input.
//
// Returns the tokens alongside ends[i], the byte offset in s immediately
// after token i ends (one past any closing quote). resumeProgram's codex
// rewrite uses these offsets to splice text into the original string
// without round-tripping through a join step that would defensively quote
// shell metacharacters (#640).
func splitShellTokens(s string) (tokens []string, ends []int) {
	var cur strings.Builder
	inToken := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case ' ', '\t':
			if inToken {
				tokens = append(tokens, cur.String())
				ends = append(ends, i)
				cur.Reset()
				inToken = false
			}
			i++
		case '\\':
			inToken = true
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i += 2
			} else {
				i++
			}
		case '\'':
			inToken = true
			i++
			for i < len(s) && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++
			}
		case '"':
			inToken = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					n := s[i+1]
					if n == '"' || n == '\\' {
						cur.WriteByte(n)
						i += 2
						continue
					}
				}
				cur.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++
			}
		default:
			inToken = true
			cur.WriteByte(c)
			i++
		}
	}
	if inToken {
		tokens = append(tokens, cur.String())
		ends = append(ends, i)
	}
	return tokens, ends
}
