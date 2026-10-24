package sessionenv

import (
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// unterminatedHeredocRe matches a here-document redirect (`<<` or `<<-`,
// optionally followed by whitespace) whose delimiter word is the final content
// of the value, as in `claude <<EOF` or `claude <<- EOF`. The closing delimiter
// line is missing, so this only appears on the parse-error path; the bare
// `<<`/`<<-` suffix (no delimiter yet) is matched separately above.
var unterminatedHeredocRe = regexp.MustCompile(`<<-?\s*\S+$`)

// stripTrailingLineContinuation removes trailing backslash-newline (shell line
// continuation) runs from command so a suffix check sees the operator the
// continuation was hiding. `claude |\<newline>` joins to `claude |` in the shell,
// so the incomplete `|` is what the value ends in once the continuation is gone;
// without the strip a bare suffix check sees the newline, not the `|`, and misses
// the misroute (injectSystemPrompt appends `--plugin-dir` to the joined value,
// completing the pipe and routing the flag to its empty right side). An even run
// of backslashes escapes the newline's escape (the last backslash escapes the
// second-to-last), leaving a real terminator that is NOT a continuation, so it is
// left in place. Whitespace after the newline is trimmed first so a `\<newline>`
// at the very end is found (#5167 review: "Handle incomplete operators followed
// by a continued newline"). A value can end in MORE than one continued physical
// line, such as `claude |\<newline>\<newline>`, which the shell joins to `claude |`;
// the strip is therefore run in a loop so every trailing continuation is removed
// before the suffix check runs, not just the final one (#5167 review: "Strip
// every trailing line continuation before checking operators").
func stripTrailingLineContinuation(command string) string {
	for {
		trimmed := strings.TrimRight(command, " \t")
		if !strings.HasSuffix(trimmed, "\n") {
			return command
		}
		body := trimmed[:len(trimmed)-1]
		backs := 0
		for len(body) > 0 && body[len(body)-1] == '\\' {
			backs++
			body = body[:len(body)-1]
		}
		if backs%2 == 0 {
			return command
		}
		// An odd backslash run inside a `#` shell comment cannot continue the
		// line — the comment ends at the newline regardless of backslashes —
		// so the trailing newline is a real statement terminator and is left
		// in place for the suffix check (#5167 review: "Do not treat comment
		// backslashes as line continuations").
		if trailingBackslashRunInComment(command, len(trimmed)-1, backs) {
			return command
		}
		next := strings.TrimRight(body, " \t")
		if next == command {
			return command
		}
		command = next
	}
}

// trailingBackslashRunInComment reports whether the odd run of `backs` trailing
// backslashes before the final newline of command sits inside a `#` shell
// comment, where a backslash cannot continue the line. newlineEnd is the index
// of that final newline within command. The POSIX parser used elsewhere joins
// a backslash-newline inside a comment as a continuation, so the raw-text scan
// re-parses with comments kept and treats the backslash run as in-comment when
// a `#` on the same line precedes it; the shell itself ends the comment at the
// newline, so the appended flag starts a new command and the value misroutes.
func trailingBackslashRunInComment(command string, newlineEnd, backs int) bool {
	if backs <= 0 || newlineEnd < 0 || newlineEnd >= len(command) || command[newlineEnd] != '\n' {
		return false
	}
	runStart := newlineEnd - backs
	if runStart < 0 {
		return false
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX), syntax.KeepComments(true)).Parse(strings.NewReader(command), "")
	if err != nil || file == nil {
		return false
	}
	for _, s := range file.Stmts {
		for _, c := range s.Comments {
			hash := int(c.Hash.Offset())
			if hash <= runStart && strings.IndexByte(command[hash:runStart], '\n') < 0 {
				return true
			}
		}
	}
	return false
}

// stripTrailingLineComment removes a trailing `#` shell comment from command so
// a suffix check sees the operator the comment was hiding. A `#` starts a comment
// only at the start of a word (preceded by whitespace or a shell metacharacter,
// or at the start of the line) and not inside quotes, so a `#` inside a quoted
// word (`claude "a#b" |`) is left alone. The comment runs to the end of its line,
// and the caller has already stripped line continuations, so trailing newlines and
// any comment line after the operator's line are removed in a loop until the last
// line has no leading comment: `claude | # note\n# more` reduces to `claude |` the
// same way `claude | # note` does. Only the last line's comment is stripped on each
// pass because a comment earlier in the value ends at its own newline and the
// operator after it is already visible to the suffix check
// (#5167 review: "Strip trailing comments before checking incomplete operators").
func stripTrailingLineComment(command string) string {
	for {
		trimmed := strings.TrimRight(command, " \t\r\n")
		if trimmed == "" {
			return command
		}
		lineStart := strings.LastIndex(trimmed, "\n") + 1
		line := trimmed[lineStart:]
		commentStart := trailingCommentStart(line)
		if commentStart < 0 {
			return command
		}
		next := trimmed[:lineStart+commentStart]
		if next == command {
			return command
		}
		command = next
	}
}

// trailingCommentStart returns the byte index of the `#` that starts a comment on
// line, or -1 if the line has no comment. A `#` starts a comment only at the start
// of a word: at the beginning of the line, or preceded by whitespace or a shell
// operator metacharacter, and never inside quotes. A `#` inside a quoted word
// (`claude "a#b" |`) is part of the word and is not a comment.
func trailingCommentStart(line string) int {
	inSingle := false
	inDouble := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '#':
			if i == 0 || isShellWordSeparator(line[i-1]) {
				return i
			}
		}
	}
	return -1
}

// isShellWordSeparator reports whether b is a byte that ends a shell word, so a
// following `#` begins a comment. Whitespace and the operator metacharacters
// (`|`, `&`, `;`, `<`, `>`, `(`, `)`) delimit words; a `#` after any of them or
// at the start of the line starts a comment, while a `#` inside a word
// (`claude#note`) does not.
func isShellWordSeparator(b byte) bool {
	switch b {
	case ' ', '\t', '|', '&', ';', '<', '>', '(', ')':
		return true
	}
	return false
}

// endsWithIncompleteOperator reports whether command ends with a shell operator
// (`|`, `&&`, `||`, `>`, `<`, `>>`) that makes it a parse error on its own but
// that appending a word completes — so the flag injectSystemPrompt appends to the
// END of the value supplies the missing right side of a pipe (`|`/`||`), the
// second command of an `&&`, or the target of a redirection (`>`, `<`, `>>`),
// and the flag runs as that rather than as an argument to the agent. A trailing
// `&` is a valid background operator (handled by stmt.Background above), not a
// parse error, so it is not in this set; a quoted operator (`claude "a|"`) is not
// a parse error and parses as a single call with a literal word.
func endsWithIncompleteOperator(command string) bool {
	command = stripTrailingLineContinuation(command)
	// A trailing `#` comment on the operator's line hides the operator from the
	// suffix check: `claude | # note` parses as an incomplete pipe, so this is
	// the parse-error path, but the trim above leaves `# note` as the suffix and
	// none of the operator cases match. Appending the flag completes the pipe —
	// the comment ends at the newline and `claude | # note\n --plugin-dir …`
	// runs the flag as the right side of the pipe — so the operator is stripped
	// of the trailing comment first to inspect the final non-comment shell token
	// (#5167 review: "Strip trailing comments before checking incomplete
	// operators").
	command = stripTrailingLineComment(command)
	// The shell permits an unescaped newline after an incomplete list/pipeline
	// operator (`claude &&\<newline>`, including spaces before that newline) and
	// completes it with the appended command, so the trailing newline is trimmed
	// (with any whitespace around it) before the suffix check — otherwise the
	// suffix sees the newline rather than the `&&`/`||`/`|` and the misroute goes
	// unwarned (#5167 review: "Recognize incomplete operators ending with a
	// newline"). This is only reached on the parse-error path, so a trailing
	// newline here always accompanies an incomplete operator.
	trimmed := strings.TrimRight(command, " \t\r\n")
	if trimmed == "" {
		return false
	}
	switch {
	case strings.HasSuffix(trimmed, "|&"):
		// `|&` is Bash's pipe-both-stdout-and-stderr operator, incomplete on its
		// own: appending supplies the right side of the pipe, which receives the
		// injected flag while the agent starts without it. It ends with `&`, so
		// the trailing-`|` case below does not catch it (#5167 review: "Detect
		// trailing `|&` pipelines before appending flags").
		return true
	case strings.HasSuffix(trimmed, "|"):
		// `|` is an incomplete pipe and `||` an incomplete or: appending supplies
		// the right side / the second command.
		return true
	case strings.HasSuffix(trimmed, "&&"):
		// `&&` is an incomplete and: appending supplies the second command.
		return true
	case strings.HasSuffix(trimmed, "<<-"), strings.HasSuffix(trimmed, "<<"):
		// A bare `<<`/`<<-` is an unterminated here-document: appending supplies
		// the delimiter word, so the flag becomes the delimiter (and the rest of
		// the line its body) rather than a flag to the agent
		// (#5167 review: "Flag an unterminated `<<-` here-document").
		return true
	case unterminatedHeredocRe.MatchString(trimmed):
		// A named but unclosed here-document, such as `claude <<EOF` (the
		// closing delimiter line is missing), also reaches this parse-error
		// path, but the suffix is the delimiter word rather than a bare `<<`.
		// Appending supplies the next line of the body, so the flag is read as
		// here-document input and the agent starts without it
		// (#5167 review: "Recognize named unterminated here-documents").
		return true
	case strings.HasSuffix(trimmed, ">"), strings.HasSuffix(trimmed, "<"):
		// A bare `>`/`<`/`>>` is an incomplete redirection: appending supplies the
		// target, so the flag becomes the redirect target rather than a flag.
		return true
	}
	return false
}
