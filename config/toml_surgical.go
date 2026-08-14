package config

import (
	"strings"

	"github.com/pelletier/go-toml/v2"
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

func tomlHeaderName(line string) (string, bool) {
	const probe = "__af_config_header_probe__"
	var shape map[string]any
	if err := toml.Unmarshal([]byte(line+"\n"+probe+" = true\n"), &shape); err != nil {
		return "", false
	}
	for name, value := range shape {
		table, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if _, present := table[probe]; present {
			return name, true
		}
	}
	return "", false
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
	for i := start; i <= end; i++ {
		if !array && i != end {
			continue
		}
		segment := lines[i]
		if i == start {
			segment = segment[valueStart:]
		}
		_, comment := splitTrailingComment(segment)
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
