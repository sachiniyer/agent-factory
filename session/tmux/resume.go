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
	if len(tokens) == 0 {
		return program
	}

	agentIdx := -1
	var agent string
	for i, tok := range tokens {
		base := strings.ToLower(filepath.Base(tok))
		for _, supported := range SupportedPrograms {
			if base == supported {
				agentIdx = i
				agent = supported
				break
			}
		}
		if agentIdx >= 0 {
			break
		}
	}
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
