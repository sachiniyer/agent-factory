package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file implements `af config set` (#1192). It writes a single scalar key
// into the global config.toml with three deliberate properties:
//
//  1. Comment/ordering-preserving: it does a SURGICAL in-place edit of the file
//     text (see setTOMLScalar) rather than re-marshaling the Config struct.
//     toml.Marshal regenerates the file and would strip the comments, blank
//     lines, and key ordering of a file the README tells users to hand-edit —
//     an external-user footgun. Only the target value's bytes change.
//  2. Allowlisted: only a curated set of safe, documented scalar keys is
//     settable (settableKeySpecs). Unknown or structural keys (root_agents, the
//     [keys] rebinds) are rejected with the settable list — they stay
//     hand-edited.
//  3. Validated with the loader's own rules BEFORE writing: the same
//     ValidateProgramEnum / enum / range checks the loader applies, plus a final
//     parseConfigTOML gate on the edited bytes, so `config set` can never write
//     a config that then fails to load.
//
// config.toml is user-hand-editable state read by the daemon and TUI at
// startup (not daemon-exclusively-owned like instances.json), so the write is a
// file write guarded by WithFileLock (mirroring the pre-daemon tasks path) — not
// a daemon RPC. Changes apply exactly as a hand-edit does: on the next af /
// daemon start (SetResult.RequiresRestart is always true).

// cfgValueKind is the scalar type a settable key accepts.
type cfgValueKind int

const (
	cfgString cfgValueKind = iota
	cfgInt
	cfgBool
)

// settableKeySpec describes one settable key (or one dynamic family such as
// program_overrides.<name>).
type settableKeySpec struct {
	kind cfgValueKind
	// section is the TOML table the key lives under ("" = the root, pre-section
	// block).
	section string
	// dynamic marks a family whose leaf is user-supplied (program_overrides.<x>,
	// limit_patterns.<x>); the registry key is then the section/prefix name.
	dynamic bool
	// validate runs the loader's own validation on the parsed value before the
	// write, returning the loader's error verbatim where possible. leaf is the
	// sub-key for a dynamic family (the program name), else the key itself.
	validate func(leaf, value string) error
}

// settableKeySpecs is the allowlist. Scalars + the two simple string maps only;
// root_agents (nested table) and [keys] (array-capable rebinds) are
// intentionally excluded from v1 and stay hand-edited.
var settableKeySpecs = map[string]settableKeySpec{
	"default_program": {kind: cfgString, validate: func(_, v string) error {
		return ValidateProgramEnum("default_program", "default_program", v, "")
	}},
	"auto_yes":             {kind: cfgBool},
	"auto_update":          {kind: cfgBool},
	"daemon_poll_interval": {kind: cfgInt, validate: func(_, v string) error { return requirePositiveInt("daemon_poll_interval", v) }},
	"log_max_size_mb":      {kind: cfgInt, validate: func(_, v string) error { return requirePositiveInt("log_max_size_mb", v) }},
	"log_max_backups":      {kind: cfgInt, validate: func(_, v string) error { return requireNonNegativeInt("log_max_backups", v) }},
	"branch_prefix":        {kind: cfgString},
	"worktree_root": {kind: cfgString, validate: func(_, v string) error {
		if !validateWorktreeRootValue(v) {
			return fmt.Errorf("worktree_root must be one of [%s, %s], got %q", WorktreeRootSubdirectory, WorktreeRootSibling, v)
		}
		return nil
	}},
	"detach_keys": {kind: cfgString, validate: func(_, v string) error {
		if _, err := ParseDetachKey(v); err != nil {
			return fmt.Errorf("detach_keys: %w", err)
		}
		return nil
	}},
	"update_channel": {kind: cfgString, validate: func(_, v string) error {
		if v != UpdateChannelStable && v != UpdateChannelPreview {
			return fmt.Errorf("update_channel must be one of [%s, %s], got %q", UpdateChannelStable, UpdateChannelPreview, v)
		}
		return nil
	}},
	"program_overrides": {kind: cfgString, section: "program_overrides", dynamic: true, validate: func(leaf, v string) error {
		return ValidateProgramEnum("program_overrides key", "program_overrides key", leaf, v)
	}},
	"limit_patterns": {kind: cfgString, section: "limit_patterns", dynamic: true, validate: func(leaf, v string) error {
		if err := ValidateProgramEnum("limit_patterns key", "limit_patterns key", leaf, v); err != nil {
			return err
		}
		if _, err := regexp.Compile(v); err != nil {
			return fmt.Errorf("limit_patterns.%s is not a valid regular expression: %w", leaf, err)
		}
		return nil
	}},
}

func requirePositiveInt(name, v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("%s must be an integer, got %q", name, v)
	}
	if n <= 0 {
		return fmt.Errorf("%s must be a positive integer, got %d", name, n)
	}
	return nil
}

func requireNonNegativeInt(name, v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("%s must be an integer, got %q", name, v)
	}
	if n < 0 {
		return fmt.Errorf("%s must be a non-negative integer, got %d", name, n)
	}
	return nil
}

// SettableKeys returns the sorted, human-facing list of keys `config set`
// accepts; dynamic families are rendered as prefix.<name>.
func SettableKeys() []string {
	out := make([]string, 0, len(settableKeySpecs))
	for k, s := range settableKeySpecs {
		if s.dynamic {
			out = append(out, k+".<name>")
		} else {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// SetResult reports a successful `config set`.
type SetResult struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Path  string `json:"path"`
	// RequiresRestart is always true: config.toml is read at startup, so a
	// change applies to af and the daemon on their next start, exactly like a
	// hand-edit.
	RequiresRestart bool `json:"requires_restart"`
}

// resolveSettable maps a user key ("default_program" or "program_overrides.claude")
// to its spec, section, and leaf. ok is false for anything not on the allowlist.
func resolveSettable(key string) (section, leaf string, spec settableKeySpec, ok bool) {
	if s, found := settableKeySpecs[key]; found && !s.dynamic {
		return "", key, s, true
	}
	if i := strings.IndexByte(key, '.'); i > 0 {
		prefix, rest := key[:i], key[i+1:]
		if s, found := settableKeySpecs[prefix]; found && s.dynamic && rest != "" && !strings.Contains(rest, ".") {
			return prefix, rest, s, true
		}
	}
	return "", "", settableKeySpec{}, false
}

// SetGlobalConfigValue validates key+rawValue against the settable-key allowlist
// and the loader's validators, then surgically writes the value into the global
// config.toml under a file lock, preserving all comments and ordering. It
// guarantees the written file still loads. Returns an actionable error for an
// unknown key, a wrong-typed or invalid value, or an I/O failure.
func SetGlobalConfigValue(key, rawValue string) (*SetResult, error) {
	section, leaf, spec, ok := resolveSettable(key)
	if !ok {
		return nil, fmt.Errorf("%q is not a settable config key. Settable keys: %s. "+
			"Structural keys (root_agents, [keys] rebinds) are edited directly in config.toml",
			key, strings.Join(SettableKeys(), ", "))
	}

	canonical, encoded, err := canonicalizeScalar(spec.kind, rawValue)
	if err != nil {
		return nil, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	if spec.validate != nil {
		if err := spec.validate(leaf, canonical); err != nil {
			return nil, err
		}
	}

	// Ensure config.toml exists (migrating a legacy config.json if needed) and
	// that the current config actually loads, so a later parse failure is
	// unambiguously our edit's fault, not a pre-existing broken file.
	if _, err := LoadConfig(); err != nil {
		return nil, fmt.Errorf("refusing to write: the current config does not load: %w", err)
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	prettyPath := prettyHomePath(tomlPath)

	var result *SetResult
	writeErr := WithFileLock(tomlPath, func() error {
		current, err := os.ReadFile(tomlPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to read %s: %w", prettyPath, err)
		}
		updated := setTOMLScalar(string(current), section, leaf, encoded)
		updated = setTOMLScalar(updated, "", SchemaVersionField, strconv.Itoa(GlobalConfigSchemaVersion))

		// Final gate: the edited bytes must parse and validate exactly as the
		// loader would, so `config set` can never leave an unloadable config.
		if _, err := parseConfigTOML([]byte(updated), prettyPath); err != nil {
			return fmt.Errorf("internal error: edited config would not load (no changes written): %w", err)
		}
		if err := AtomicWriteFile(tomlPath, []byte(updated), 0644); err != nil {
			return err
		}
		result = &SetResult{Key: key, Value: canonical, Path: tomlPath, RequiresRestart: true}
		return nil
	})
	if writeErr != nil {
		return nil, writeErr
	}
	return result, nil
}

// canonicalizeScalar parses rawValue per kind and returns both the canonical
// string form (for echo/validation) and its TOML encoding (for the file).
func canonicalizeScalar(kind cfgValueKind, raw string) (canonical, encoded string, err error) {
	switch kind {
	case cfgBool:
		b, perr := strconv.ParseBool(strings.TrimSpace(raw))
		if perr != nil {
			return "", "", fmt.Errorf("expected a boolean (true/false), got %q", raw)
		}
		s := strconv.FormatBool(b)
		return s, s, nil
	case cfgInt:
		n, perr := strconv.Atoi(strings.TrimSpace(raw))
		if perr != nil {
			return "", "", fmt.Errorf("expected an integer, got %q", raw)
		}
		s := strconv.Itoa(n)
		return s, s, nil
	default:
		return raw, encodeTOMLString(raw), nil
	}
}

// encodeTOMLString renders s as a TOML string, preferring a literal single-quoted
// string (matching go-toml's output style and leaving backslashes in paths
// untouched). It falls back to a basic double-quoted string only when s contains
// a single quote or a newline, which a literal string cannot represent.
func encodeTOMLString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

var (
	tomlHeaderRe = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(#.*)?$`)
)

// setTOMLScalar returns content with [section] leaf set to encoded, changing only
// the target value's bytes. If the key exists its value (and only its value) is
// replaced, preserving any trailing inline comment. It recognizes both TOML
// spellings of a table entry — a leaf under a [section] header AND a top-level
// dotted key (section.leaf = …) — and edits whichever is present, so a
// hand-edited dotted-key file is never left with a duplicate. If the key is
// absent it is inserted with minimal disturbance — appended to the end of its
// section's block (or, for a root key, the pre-section block); if the section
// itself is absent a new [section] block is appended. section == "" targets the
// root block.
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

	curSection := ""
	firstHeaderIdx := -1
	targetHeaderIdx := -1
	lastLineIdxInTarget := -1

	rebuild := func() string {
		out := strings.Join(ls, "\n")
		if hadTrailingNewline {
			out += "\n"
		}
		return out
	}

	for i, line := range ls {
		if m := tomlHeaderRe.FindStringSubmatch(line); m != nil {
			if firstHeaderIdx == -1 {
				firstHeaderIdx = i
			}
			name := strings.TrimSpace(m[1])
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
				_, comment := splitTrailingComment(m[2])
				ls[i] = m[1] + encoded + comment
				return rebuild()
			}
		}
		if curSection != section {
			continue
		}
		if m := keyRe.FindStringSubmatch(line); m != nil {
			_, comment := splitTrailingComment(m[2])
			ls[i] = m[1] + encoded + comment
			return rebuild()
		}
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			lastLineIdxInTarget = i
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
		case lastLineIdxInTarget != -1:
			insertAt(lastLineIdxInTarget+1, newLine)
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
		if lastLineIdxInTarget != -1 {
			insertAt(lastLineIdxInTarget+1, newLine)
		} else {
			insertAt(targetHeaderIdx+1, newLine)
		}
	}
	return rebuild()
}

// splitTrailingComment separates a TOML value from a trailing inline comment,
// tracking quote state so a '#' inside a string is not mistaken for a comment.
// It returns the value part and the comment part (including the whitespace that
// preceded the '#'), so the comment can be reattached byte-for-byte.
func splitTrailingComment(rest string) (value, comment string) {
	inSingle, inDouble := false, false
	escape := false
	for i := 0; i < len(rest); i++ {
		c := rest[i]
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
				escape = false
			}
		case c == '\'':
			inSingle = true
			escape = false
		case c == '"':
			inDouble = true
			escape = false
		case c == '#':
			j := i
			for j > 0 && (rest[j-1] == ' ' || rest[j-1] == '\t') {
				j--
			}
			return rest[:j], rest[j:]
		}
	}
	return rest, ""
}
