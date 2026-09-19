package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// A tilde prefix is resolved by /bin/sh from state the shell holds — HOME for
// `~`, PWD for `~+`, OLDPWD for `~-`, the passwd database for `~login` — and the
// result is never field-split or globbed (POSIX 2.6.1). So a word is a fixed
// path whose final component is its own literal text exactly when two things
// hold, and the walk proves each one separately:
//
//   - the prefix ends at a literal slash and names one of those directories
//     (tildePrefixNamesAPath). Without the slash the whole word is the
//     expansion — `PWD=unset; ~+ CODEX_HOME` runs the unset builtin under
//     bash — and the directory-stack forms (`~1/`, `~+1/`) follow pushd.
//   - the command never rebinds that state (tildeBindingNames). HOME, PWD and
//     OLDPWD arrive from the operator's environment and hold absolute paths;
//     once the command assigns one, `HOME=CODEX_HOME=; env ~/x codex` runs
//     `env CODEX_HOME=/x codex` under both dash and bash (Codex on #4466).
//
// With both proven, `~/../../usr/bin/env` is matched as env by its basename
// like any other path, and `nohup ~/bin/server` is an ordinary child command.
var tildeBindingNames = []string{"HOME", "OLDPWD", "PWD"}

// tildePrefixNamesAPath reports whether lit, a word's first literal part
// beginning with ~, carries a tilde prefix that ends at a slash inside lit and
// resolves to a directory the command cannot choose. Anything else — no slash,
// an escaped or unusual login name, a directory-stack index — is refused, which
// only costs spellings no program is launched through.
func tildePrefixNamesAPath(lit string) bool {
	prefix, _, found := strings.Cut(strings.TrimPrefix(lit, "~"), "/")
	if !found {
		return false
	}
	switch prefix {
	case "", "+", "-":
		return true
	}
	return tildeLoginName(prefix)
}

func tildeLoginName(name string) bool {
	for idx, r := range name {
		switch {
		case r == '_' || asciiLetter(r):
		case idx > 0 && (r >= '0' && r <= '9' || r == '.' || r == '-'):
		default:
			return false
		}
	}
	return name != ""
}

// withTildeBindingNames returns names plus HOME, PWD and OLDPWD when command
// contains a word that begins with ~, so the environment walk refuses every
// form that could rebind a tilde prefix — assignment, export, read, for,
// printf -v, nameref, ${HOME:=…} — through the same machinery that guards the
// account variables, including its fail-closed handling of unmodeled forms.
// A command with no tilde word keeps names unchanged: rebinding HOME is none of
// the account boundary's business until a tilde reads it.
func withTildeBindingNames(command string, names map[string]struct{}) map[string]struct{} {
	if !commandUsesTildePrefix(command) {
		return names
	}
	extended := make(map[string]struct{}, len(names)+len(tildeBindingNames))
	for name := range names {
		extended[name] = struct{}{}
	}
	for _, name := range tildeBindingNames {
		extended[name] = struct{}{}
	}
	return extended
}

func commandUsesTildePrefix(command string) bool {
	if !strings.Contains(command, "~") {
		return false
	}
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
		if err != nil {
			// The walk refuses what it cannot parse; answering "no tilde"
			// here would only matter for text it never admits.
			continue
		}
		found := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if word, ok := node.(*syntax.Word); ok && len(word.Parts) > 0 {
				if lit, ok := word.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "~") {
					found = true
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}
