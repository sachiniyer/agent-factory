package config

import (
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// tomlScalarLineMatches recognizes bare, quoted, and dotted TOML key spellings
// from the assignment's left-hand side. Looking only at a decoded line's value
// shape is unsafe: `ssh.host_key_verification = ...` and
// `ssh = { host_key_verification = ..., future = ... }` have the same shape,
// but deleting the latter line would clobber the unknown sibling.
func tomlScalarLineMatches(line, section, leaf string) bool {
	path, _, ok := tomlAssignmentPath(line)
	if !ok {
		return false
	}
	want := []string{leaf}
	if section != "" {
		want = []string{section, leaf}
	}
	if len(path) != len(want) {
		return false
	}
	for i := range want {
		if path[i] != want[i] {
			return false
		}
	}
	return true
}

func tomlAssignmentPath(line string) ([]string, int, bool) {
	equal := tomlAssignmentEqual(line)
	if equal < 0 {
		return nil, -1, false
	}
	lhs := strings.TrimSpace(line[:equal])
	if lhs == "" {
		return nil, -1, false
	}
	var shape map[string]any
	if err := toml.Unmarshal([]byte(lhs+" = true\n"), &shape); err != nil {
		return nil, -1, false
	}
	path := make([]string, 0, 2)
	var value any = shape
	for {
		table, nested := value.(map[string]any)
		if !nested {
			break
		}
		if len(table) != 1 {
			return nil, -1, false
		}
		for key, next := range table {
			path = append(path, key)
			value = next
		}
	}
	if len(path) == 0 {
		return nil, -1, false
	}
	return path, equal, true
}

func tomlAssignmentEqual(line string) int {
	inSingle, inDouble, escape := false, false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '=':
			return i
		}
	}
	return -1
}

// tomlStringContentLines marks, for every line of ls, whether that line BEGINS
// inside an open multiline string — that is, whether it is a string's CONTENT
// rather than TOML syntax.
//
// This is the whole of #3662. Every surgical scanner in this package decides
// what a line is by looking at that line alone: tomlHeaderName for `[section]`,
// tomlScalarLineMatches and the keyRe regexes for `key = value`. None of them
// tracked string state, so a line inside an open tomlMultilineLiteral or
// tomlMultilineBasic block — an ordinary thing to find in a shell script stored
// in on_archive_command — read as syntax and got edited. `af config set
// branch_prefix new/` rewrote the decoy line inside the string, left the real
// branch_prefix untouched, and exited 0.
//
// Callers scan this mask alongside their own loop rather than each carrying the
// state themselves, so a fifth scanner cannot be added with the blindness back
// in it.
func tomlStringContentLines(ls []string) []bool {
	content := make([]bool, len(ls))
	open := ""
	for i, line := range ls {
		content[i] = open != ""
		open = tomlMultilineCarry(line, open)
	}
	return content
}

// tomlMultilineCarry reports the multiline-string delimiter still open when line
// ends, given the one open when it began ("" outside every string). It reuses
// scanTrailingComment's open-delimiter carry — the state machine
// preservedTOMLAssignmentComments has driven line by line since #3455 — rather
// than growing a second string parser here.
//
// Only the two MULTILINE delimiters carry. A single-line "…" or '…' cannot span
// a newline in TOML, so an unterminated one is a syntax error, not state worth
// keeping: carrying it would make a scanner read the whole rest of the file as
// string content and miss the very key it was sent to find.
func tomlMultilineCarry(line, open string) string {
	_, _, stillOpen := scanTrailingComment(line, open)
	if len(stillOpen) != len(tomlMultilineBasic) {
		return ""
	}
	return stillOpen
}

func tomlHeaderName(line string) (string, bool) {
	var parser unstable.Parser
	parser.Reset([]byte(line))
	if !parser.NextExpression() {
		return "", false
	}
	expr := parser.Expression()
	if expr.Kind != unstable.Table && expr.Kind != unstable.ArrayTable {
		return "", false
	}
	parts, _ := tomlExpressionKey(expr)
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "."), true
}

func replaceTOMLAssignmentValue(line, encoded string) (string, bool) {
	_, equal, ok := tomlAssignmentPath(line)
	if !ok {
		return line, false
	}
	valueStart := equal + 1
	for valueStart < len(line) && (line[valueStart] == ' ' || line[valueStart] == '\t') {
		valueStart++
	}
	value, comment := splitTrailingComment(line[valueStart:])
	valueEnd := len(strings.TrimRight(value, " \t"))
	return line[:valueStart] + encoded + value[valueEnd:] + comment, true
}

// tomlAssignmentEnd returns the final line occupied by the assignment that
// starts at start. Asking the TOML parser for the shortest valid prefix keeps
// this syntax-aware for multiline arrays and strings without inventing a
// second bracket/string parser here.
func tomlAssignmentEnd(lines []string, start int) int {
	for end := start; end < len(lines); end++ {
		var shape map[string]any
		if err := toml.Unmarshal([]byte(strings.Join(lines[start:end+1], "\n")+"\n"), &shape); err == nil {
			return end
		}
	}
	return start
}

func preservedTOMLAssignmentComments(lines []string, start, end int) []string {
	_, equal, ok := tomlAssignmentPath(lines[start])
	if !ok {
		return nil
	}
	valueStart := equal + 1
	for valueStart < len(lines[start]) && (lines[start][valueStart] == ' ' || lines[start][valueStart] == '\t') {
		valueStart++
	}
	array := strings.HasPrefix(strings.TrimSpace(lines[start][valueStart:]), "[")
	comments := make([]string, 0)
	// The string state carries from line to line. A multiline """ or ''' opened
	// on one line is still open on the next, so its closing delimiter has to read
	// as a CLOSE; reading it as a fresh open is what silently dropped the comment
	// after it (#3455). That is why EVERY line is scanned, including the ones
	// whose comments are not collected — skipping one would lose the state.
	open := ""
	for i := start; i <= end; i++ {
		segment := lines[i]
		if i == start {
			segment = segment[valueStart:]
		}
		var comment string
		_, comment, open = scanTrailingComment(segment, open)
		if !array && i != end {
			continue
		}
		if comment == "" {
			continue
		}
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
		comments = append(comments, indent+strings.TrimLeft(comment, " \t"))
	}
	return comments
}

func replaceTOMLAssignmentLines(lines []string, start int, encoded string) ([]string, bool) {
	_, equal, ok := tomlAssignmentPath(lines[start])
	if !ok {
		return lines, false
	}
	end := tomlAssignmentEnd(lines, start)
	if end == start {
		updated, replaced := replaceTOMLAssignmentValue(lines[start], encoded)
		if replaced {
			lines[start] = updated
		}
		return lines, replaced
	}
	valueStart := equal + 1
	for valueStart < len(lines[start]) && (lines[start][valueStart] == ' ' || lines[start][valueStart] == '\t') {
		valueStart++
	}
	replacement := lines[start][:valueStart] + encoded
	replacements := []string{replacement}
	// Comments inside a multiline value are not values. Keep them as
	// standalone comments after the compact replacement so a targeted edit never
	// silently erases operator notes.
	replacements = append(replacements, preservedTOMLAssignmentComments(lines, start, end)...)
	replacements = append(replacements, lines[end+1:]...)
	lines = append(lines[:start], replacements...)
	return lines, true
}

type tomlInlineMember struct {
	start     int
	end       int
	trimStart int
	trimEnd   int
}

func tomlInlineTableBody(line, section string) (start, end int, ok bool) {
	path, equal, parsed := tomlAssignmentPath(line)
	if !parsed || len(path) != 1 || path[0] != section {
		return 0, 0, false
	}
	valueStart := equal + 1
	for valueStart < len(line) && (line[valueStart] == ' ' || line[valueStart] == '\t') {
		valueStart++
	}
	value, _ := splitTrailingComment(line[valueStart:])
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return 0, 0, false
	}
	open := strings.Index(line[valueStart:], "{") + valueStart
	close := valueStart + strings.LastIndex(value, "}")
	return open + 1, close, true
}

func tomlInlineMembers(body string) []tomlInlineMember {
	starts := []int{0}
	commas := make([]int, 0)
	inSingle, inDouble, escape := false, false, false
	braceDepth, bracketDepth := 0, 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '{':
			braceDepth++
		case c == '}':
			braceDepth--
		case c == '[':
			bracketDepth++
		case c == ']':
			bracketDepth--
		case c == ',' && braceDepth == 0 && bracketDepth == 0:
			commas = append(commas, i)
			starts = append(starts, i+1)
		}
	}

	members := make([]tomlInlineMember, 0, len(starts))
	for i, start := range starts {
		end := len(body)
		if i < len(commas) {
			end = commas[i]
		}
		trimStart := start
		for trimStart < end && (body[trimStart] == ' ' || body[trimStart] == '\t') {
			trimStart++
		}
		trimEnd := end
		for trimEnd > trimStart && (body[trimEnd-1] == ' ' || body[trimEnd-1] == '\t') {
			trimEnd--
		}
		if trimStart < trimEnd {
			members = append(members, tomlInlineMember{start: start, end: end, trimStart: trimStart, trimEnd: trimEnd})
		}
	}
	return members
}

func inlineMemberIndex(body, leaf string) ([]tomlInlineMember, int) {
	members := tomlInlineMembers(body)
	for i, member := range members {
		path, _, ok := tomlAssignmentPath(body[member.trimStart:member.trimEnd])
		if ok && len(path) == 1 && path[0] == leaf {
			return members, i
		}
	}
	return members, -1
}

func setTOMLInlineTableMember(line, section, leaf, encoded string) (string, bool) {
	start, end, ok := tomlInlineTableBody(line, section)
	if !ok {
		return line, false
	}
	body := line[start:end]
	members, target := inlineMemberIndex(body, leaf)
	if target >= 0 {
		member := members[target]
		updated, replaced := replaceTOMLAssignmentValue(body[member.start:member.end], encoded)
		if !replaced {
			return line, false
		}
		body = body[:member.start] + updated + body[member.end:]
		return line[:start] + body + line[end:], true
	}
	trailing := len(strings.TrimRight(body, " \t"))
	separator := ""
	if strings.TrimSpace(body) != "" {
		separator = ", "
	}
	body = body[:trailing] + separator + leaf + " = " + encoded + body[trailing:]
	return line[:start] + body + line[end:], true
}

func deleteTOMLInlineTableMember(line, section, leaf string) (string, bool) {
	start, end, ok := tomlInlineTableBody(line, section)
	if !ok {
		return line, false
	}
	body := line[start:end]
	members, target := inlineMemberIndex(body, leaf)
	if target < 0 {
		return line, false
	}
	member := members[target]
	switch {
	case len(members) == 1:
		body = body[:member.trimStart] + body[member.trimEnd:]
	case target < len(members)-1:
		body = body[:member.trimStart] + body[members[target+1].trimStart:]
	default:
		body = body[:members[target-1].trimEnd] + body[member.trimEnd:]
	}
	return line[:start] + body + line[end:], true
}

// tomlRootScalarRawValue returns the value text of a root-block `leaf = …`
// assignment exactly as it was written — quote style, spacing and all — so a
// key relocated by `af config migrate` carries its own bytes to the new
// spelling instead of a re-encoded lookalike. The diff then shows a moved line
// rather than a moved-and-restyled one, and nothing has to trust the round
// trip.
//
// It reports false for an absent key and for a value spread over more than one
// line (an array, typically): relocating those raw bytes would mean re-indenting
// them under a new header, so the migration re-encodes the decoded value there
// instead. section is always the root block, because a flat legacy alias can
// only ever live there.
func tomlRootScalarRawValue(content, leaf string) (string, bool) {
	ls := strings.Split(content, "\n")
	stringContent := tomlStringContentLines(ls)
	curSection := ""
	for i, line := range ls {
		if stringContent[i] {
			continue
		}
		if name, ok := tomlHeaderName(line); ok {
			curSection = name
			continue
		}
		if curSection != "" || !tomlScalarLineMatches(line, "", leaf) {
			continue
		}
		if tomlAssignmentEnd(ls, i) != i {
			return "", false
		}
		equal := tomlAssignmentEqual(line)
		if equal < 0 {
			return "", false
		}
		value, _ := splitTrailingComment(line[equal+1:])
		if value = strings.TrimSpace(value); value == "" {
			return "", false
		}
		return value, true
	}
	return "", false
}

// tomlRootDottedTable reports whether the root block already defines section
// through a dotted key (`network.future_option = 'x'`) rather than a [section]
// header.
//
// TOML forbids re-opening such a table with a header — "table network already
// exists as defined by a dotted key" — so a leaf being moved into that section
// has to join it in the same dotted form. Without this check the rewrite
// produces a file that will not parse, which the migration's pre-write gate
// catches, but only by refusing a perfectly valid config with an internal
// error (#3624 review).
func tomlRootDottedTable(content, section string) bool {
	ls := strings.Split(content, "\n")
	stringContent := tomlStringContentLines(ls)
	curSection := ""
	for i, line := range ls {
		if stringContent[i] {
			continue
		}
		if name, ok := tomlHeaderName(line); ok {
			curSection = name
			continue
		}
		if curSection != "" {
			continue
		}
		if path, _, ok := tomlAssignmentPath(line); ok && len(path) > 1 && path[0] == section {
			return true
		}
	}
	return false
}
