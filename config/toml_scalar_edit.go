package config

import (
	"regexp"
	"strings"
)

// The line-level surgical editors behind `af config set` and `af config unset`:
// they find one key's line in config.toml and change (or remove) only that
// line's bytes, so the comments, blank lines and key ordering of a file the
// README tells users to hand-edit all survive an edit. The expression-level
// helpers they lean on live in toml_surgical.go; the promise a scanner must not
// break, and the guard that catches it when one does, live in rewrite_guard.go.

// setTOMLScalar returns content with [section] leaf set to encoded, changing only
// the target value's bytes. If the key exists its value (and only its value) is
// replaced, preserving any trailing inline comment. It recognizes both TOML
// spellings of a table entry — a leaf under a [section] header AND a top-level
// dotted key (section.leaf = …) — and edits whichever is present, so a
// hand-edited dotted-key file is never left with a duplicate. If the key is
// absent it is inserted with minimal disturbance — appended to the end of its
// section's content block, i.e. after the section's last non-blank line
// (comments included) and before any trailing blanks preceding the next section
// or EOF (#1687), or for a root key the pre-section block; if the section itself
// is absent a new [section] block is appended. section == "" targets the root
// block.
func setTOMLScalar(content, section, leaf, encoded string) string {
	newLine := leaf + " = " + encoded

	if strings.TrimSpace(content) == "" {
		if section == "" {
			return newLine + "\n"
		}
		return "[" + section + "]\n" + newLine + "\n"
	}

	hadTrailingNewline := strings.HasSuffix(content, "\n")
	ls := strings.Split(content, "\n")
	if hadTrailingNewline && len(ls) > 0 && ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}

	keyRe := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(leaf) + `\s*=\s*)(.*)$`)

	// TOML also lets a hand-editor write a table entry as a top-level dotted key
	// (program_overrides.claude = "…") instead of under a [program_overrides]
	// header. For a dynamic key we must recognize that form too, or we would
	// miss the existing key and append a duplicate — corrupting the file (a
	// valid config never has both forms, so at most one matches). dotted whitespace
	// around the '.' is allowed by TOML, so tolerate it.
	var dottedKeyRe *regexp.Regexp
	if section != "" {
		dottedKeyRe = regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(section) + `\s*\.\s*` + regexp.QuoteMeta(leaf) + `\s*=\s*)(.*)$`)
	}

	// A line that BEGINS inside an open multiline string is that string's
	// content, never syntax (#3662).
	stringContent := tomlStringContentLines(ls)

	curSection := ""
	firstHeaderIdx := -1
	targetHeaderIdx := -1
	// lastContentIdxInTarget tracks the last non-blank line of the target
	// section INCLUDING comment lines, so a missing key is appended at the END
	// of the section's content block (the documented contract). Tracking only
	// key=value lines (the pre-#1687 behavior) left it at -1 for a comment-only
	// section, which inserted the new key immediately after the [section] header
	// and ABOVE the section's comments (#1687). Blank lines never update it, so
	// trailing blanks before the next header / EOF are excluded and the insert
	// lands at the end of the content, not spilling past it.
	lastContentIdxInTarget := -1

	rebuild := func() string {
		out := strings.Join(ls, "\n")
		if hadTrailingNewline {
			out += "\n"
		}
		return out
	}

	for i, line := range ls {
		if stringContent[i] {
			// Content of somebody's multiline value: no header to open, no key
			// to replace. It is still content OF the section it sits in, though,
			// so an insert has to land after the whole value rather than in the
			// middle of it — which is why this updates the bookkeeping below
			// instead of skipping the line outright.
			if curSection == section && strings.TrimSpace(line) != "" {
				lastContentIdxInTarget = i
			}
			continue
		}
		if name, ok := tomlHeaderName(line); ok {
			if firstHeaderIdx == -1 {
				firstHeaderIdx = i
			}
			if name == section && targetHeaderIdx == -1 {
				targetHeaderIdx = i
			}
			curSection = name
			continue
		}
		// Top-level dotted form (section.leaf = …). Only valid at the root: the
		// same text under another header would name a different key.
		if dottedKeyRe != nil && curSection == "" {
			if m := dottedKeyRe.FindStringSubmatch(line); m != nil {
				if updated, ok := replaceTOMLAssignmentLines(ls, i, encoded); ok {
					ls = updated
					return rebuild()
				}
			}
			if tomlScalarLineMatches(line, section, leaf) {
				if updated, ok := replaceTOMLAssignmentLines(ls, i, encoded); ok {
					ls = updated
					return rebuild()
				}
			}
			if updated, ok := setTOMLInlineTableMember(line, section, leaf, encoded); ok {
				ls[i] = updated
				return rebuild()
			}
		}
		if curSection != section {
			continue
		}
		if m := keyRe.FindStringSubmatch(line); m != nil {
			if updated, ok := replaceTOMLAssignmentLines(ls, i, encoded); ok {
				ls = updated
				return rebuild()
			}
		}
		if tomlScalarLineMatches(line, "", leaf) {
			if updated, ok := replaceTOMLAssignmentLines(ls, i, encoded); ok {
				ls = updated
				return rebuild()
			}
		}
		if strings.TrimSpace(line) != "" {
			lastContentIdxInTarget = i
		}
	}

	// Key not found — insert.
	insertAt := func(idx int, s string) {
		ls = append(ls, "")
		copy(ls[idx+1:], ls[idx:])
		ls[idx] = s
	}

	switch {
	case section == "":
		switch {
		case lastContentIdxInTarget != -1:
			insertAt(lastContentIdxInTarget+1, newLine)
		case firstHeaderIdx != -1:
			insertAt(firstHeaderIdx, newLine)
		default:
			ls = append(ls, newLine)
		}
	case targetHeaderIdx == -1:
		// Section absent: append a fresh block, separated by one blank line.
		if len(ls) > 0 && ls[len(ls)-1] != "" {
			ls = append(ls, "")
		}
		ls = append(ls, "["+section+"]", newLine)
	default:
		if lastContentIdxInTarget != -1 {
			insertAt(lastContentIdxInTarget+1, newLine)
		} else {
			insertAt(targetHeaderIdx+1, newLine)
		}
	}
	return rebuild()
}

// deleteTOMLScalar removes the [section] leaf line from content, changing only
// that one line and preserving every other comment, blank line, and key. It is
// the inverse of setTOMLScalar and recognizes the same two spellings of a table
// entry — a leaf under a [section] header AND a top-level dotted key
// (section.leaf = …) — removing whichever is present. section == "" targets a
// root-block key. Returns the edited content and whether a line was removed; an
// absent key leaves content untouched and reports false. An emptied [section]
// header is left in place (a present-but-empty table resolves to no leaves).
func deleteTOMLScalar(content, section, leaf string) (string, bool) {
	if strings.TrimSpace(content) == "" {
		return content, false
	}

	hadTrailingNewline := strings.HasSuffix(content, "\n")
	ls := strings.Split(content, "\n")
	if hadTrailingNewline && len(ls) > 0 && ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}

	keyRe := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(leaf) + `\s*=\s*)(.*)$`)
	var dottedKeyRe *regexp.Regexp
	if section != "" {
		dottedKeyRe = regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(section) + `\s*\.\s*` + regexp.QuoteMeta(leaf) + `\s*=\s*)(.*)$`)
	}

	stringContent := tomlStringContentLines(ls)

	curSection := ""
	removeAt := -1
	removeThrough := -1
	rebuild := func() string {
		out := strings.Join(ls, "\n")
		if hadTrailingNewline && out != "" {
			out += "\n"
		}
		return out
	}
	for i, line := range ls {
		// Content of a multiline value is not a key to delete, and not a header
		// that changes which section the lines below it belong to (#3662).
		if stringContent[i] {
			continue
		}
		if name, ok := tomlHeaderName(line); ok {
			curSection = name
			continue
		}
		// Top-level dotted form (section.leaf = …), valid only at the root.
		if dottedKeyRe != nil && curSection == "" && (dottedKeyRe.MatchString(line) || tomlScalarLineMatches(line, section, leaf)) {
			removeAt = i
			removeThrough = tomlAssignmentEnd(ls, i)
			break
		}
		if curSection == "" && section != "" {
			if updated, ok := deleteTOMLInlineTableMember(line, section, leaf); ok {
				ls[i] = updated
				return rebuild(), true
			}
		}
		if curSection != section {
			continue
		}
		if keyRe.MatchString(line) || tomlScalarLineMatches(line, "", leaf) {
			removeAt = i
			removeThrough = tomlAssignmentEnd(ls, i)
			break
		}
	}
	if removeAt < 0 {
		return content, false
	}

	kept := preservedTOMLAssignmentComments(ls, removeAt, removeThrough)
	kept = append(kept, ls[removeThrough+1:]...)
	ls = append(ls[:removeAt], kept...)
	return rebuild(), true
}

// TOML's two multiline string delimiters. They are scanned as three-byte UNITS
// rather than as three individual quotes, which is the whole of #3455: a
// per-quote toggle reads a triple quote as open-close-open and leaves the
// scanner believing it is inside a string, so a '#' after the delimiter never
// registers as a comment.
const (
	tomlMultilineBasic   = `"""`
	tomlMultilineLiteral = `'''`
)

// splitTrailingComment separates a TOML value from a trailing inline comment,
// tracking string state so a '#' inside a string is not mistaken for a comment.
// It returns the value part and the comment part (including the whitespace that
// preceded the '#'), so the comment can be reattached byte-for-byte.
//
// The scan begins OUTSIDE every string, which is right for a line that opens
// whatever strings it contains: a single-line assignment's value, or an inline
// table. The closing line of a MULTILINE assignment is the other case — the
// string it ends was opened on an earlier line — and that one needs
// scanTrailingComment with the delimiter still open.
func splitTrailingComment(rest string) (value, comment string) {
	value, comment, _ = scanTrailingComment(rest, "")
	return value, comment
}

// scanTrailingComment splits one line of a TOML value, resuming inside the
// string that `open` closes ("" when the line begins outside every string), and
// reports the delimiter still open when the line ends so the caller can carry
// the state to the next line.
//
// Carrying that state is what makes the closing delimiter of a multiline string
// read as the CLOSE it is rather than the open of a new string. It also covers
// the same defect one level down, where a multiline string is an ELEMENT of a
// multiline array and holds its state open across the element boundary.
func scanTrailingComment(rest, open string) (value, comment, stillOpen string) {
	for i := 0; i < len(rest); {
		if open != "" {
			// Basic strings process backslash escapes, so an escaped quote is
			// content and cannot close anything. Literal strings process none
			// at all, which is exactly what lets a lone backslash sit inside
			// one.
			if open[0] == '"' && rest[i] == '\\' {
				i += 2
				continue
			}
			if !strings.HasPrefix(rest[i:], open) {
				i++
				continue
			}
			i += len(open)
			if len(open) == 3 {
				// TOML lets a multiline string's content end with one or two of
				// its own quote characters, so the delimiter is the LAST three
				// of the run: `"""a""""` is the string `a"`. Consuming the whole
				// run keeps a stray quote from opening a phantom string that
				// would swallow the comment after it.
				for i < len(rest) && rest[i] == open[0] {
					i++
				}
			}
			open = ""
			continue
		}
		switch {
		case strings.HasPrefix(rest[i:], tomlMultilineBasic):
			open = tomlMultilineBasic
			i += len(tomlMultilineBasic)
		case strings.HasPrefix(rest[i:], tomlMultilineLiteral):
			open = tomlMultilineLiteral
			i += len(tomlMultilineLiteral)
		case rest[i] == '"':
			open = `"`
			i++
		case rest[i] == '\'':
			open = `'`
			i++
		case rest[i] == '#':
			j := i
			for j > 0 && (rest[j-1] == ' ' || rest[j-1] == '\t') {
				j--
			}
			return rest[:j], rest[j:], ""
		default:
			i++
		}
	}
	return rest, "", open
}
