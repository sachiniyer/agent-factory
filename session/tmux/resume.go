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
// Agent-specific rewrites:
//
//   - claude: append --continue at the end. claude's resume flags are
//     position-independent, so appending preserves the original program
//     string verbatim (including any shell quoting on the executable
//     path — see #569).
//   - codex: insert "resume --last" immediately after the codex token,
//     or after "exec" if it follows codex. Subcommand position matters
//     for codex, so this can't be a tail append.
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
	tokens := splitShellTokens(program)
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
		for _, tok := range tokens {
			if tok == "-c" || tok == "--continue" || tok == "-r" || tok == "--resume" ||
				strings.HasPrefix(tok, "--resume=") || strings.HasPrefix(tok, "-r=") {
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
		newTokens := make([]string, 0, len(tokens)+2)
		newTokens = append(newTokens, tokens[:insertAt]...)
		newTokens = append(newTokens, "resume", "--last")
		newTokens = append(newTokens, tokens[insertAt:]...)
		return shellJoinTokens(newTokens)
	case ProgramAider:
		for _, tok := range tokens {
			if tok == "--restore-chat-history" || tok == "--no-restore-chat-history" {
				return program
			}
		}
		return program + " --restore-chat-history"
	case ProgramGemini:
		for _, tok := range tokens {
			if tok == "--resume" || tok == "-r" ||
				strings.HasPrefix(tok, "--resume=") || strings.HasPrefix(tok, "-r=") {
				return program
			}
		}
		return program + " --resume latest"
	}
	return program
}

// splitShellTokens tokenizes a shell-style command string, respecting single
// quotes (no escapes), double quotes (with \" and \\ escapes), and backslash
// escapes outside quotes. Adjacent runs concatenate into a single token (e.g.
// 'foo'bar -> "foobar"). Unclosed quotes consume to end of input. Mirrors the
// session.splitShell helper used by injectSystemPrompt — kept private here
// to avoid an import cycle (session/tmux is imported by session).
func splitShellTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	inToken := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case ' ', '\t':
			if inToken {
				tokens = append(tokens, cur.String())
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
	}
	return tokens
}

// shellJoinTokens joins tokens with spaces, single-quoting any token that
// would otherwise be reinterpreted by the shell. Used to rebuild a program
// string after token-level surgery in resumeProgram.
func shellJoinTokens(tokens []string) string {
	var b strings.Builder
	for i, tok := range tokens {
		if i > 0 {
			b.WriteByte(' ')
		}
		if needsShellQuote(tok) {
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(tok, "'", `'\''`))
			b.WriteByte('\'')
		} else {
			b.WriteString(tok)
		}
	}
	return b.String()
}

func needsShellQuote(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\'', '"', '\\',
			'$', '`', '*', '?', '[', ']',
			'(', ')', '{', '}', '|', '&',
			';', '<', '>', '#', '~', '!':
			return true
		}
	}
	return false
}
