package tmux

import (
	"path/filepath"
	"strings"
)

// resumeProgram derives a "resume the most recent session in cwd" variant of
// program for use when Restore() re-spawns a vanished tmux session (#386,
// #595). For agents without a resume-most-recent flag, programs that already
// include one, and unknown programs, returns program unchanged.
//
// Agent-specific rewrites — all four paths preserve the original program
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
//
// All four CLIs silently fall back to a fresh session when no prior session
// exists in cwd, so the rewrite is safe to apply unconditionally.
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
	}
	return program
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
		return program + " --resume " + id, true
	case ProgramCodex:
		insertAt := agentIdx + 1
		if insertAt < len(tokens) && tokens[insertAt] == "exec" {
			insertAt++
		}
		if insertAt < len(tokens) && tokens[insertAt] == "resume" {
			return program, false
		}
		off := ends[insertAt-1]
		return program[:off] + " resume " + id + program[off:], true
	}
	return program, false
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
	for i, tok := range tokens {
		base := strings.ToLower(filepath.Base(tok))
		for _, supported := range SupportedPrograms {
			if base == supported {
				return i, supported
			}
		}
	}
	return -1, ""
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
