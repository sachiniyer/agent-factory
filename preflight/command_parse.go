package preflight

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

func firstExecutable(words []string) (string, error) {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return "", nil
	}
	if !isEnvExecutable(words[0]) {
		return words[0], nil
	}

	// GNU env is parsed in one place. In particular, do not skip an unknown
	// option and guess that the following token is the executable: the same
	// command resolver is used to authorize a destructive handoff, so a guessed
	// preflight can stop the outgoing agent before discovering the target was not
	// runnable. Receipt routing and tmuxguard consume this parser as well.
	invocation, err := envcommand.Parse(words[1:], envcommand.Policy{AllowAssignments: true})
	if err != nil {
		return "", err
	}
	if invocation.CommandIndex < 0 {
		return "", nil
	}
	return words[1+invocation.CommandIndex], nil
}

func envWrapperExecutable(words []string) string {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	if len(words) > 0 && isEnvExecutable(words[0]) {
		return words[0]
	}
	return ""
}

func isEnvExecutable(word string) bool {
	return filepath.Base(config.ExpandTilde(word)) == "env"
}

func isAssignment(word string) bool {
	i := strings.IndexRune(word, '=')
	if i <= 0 {
		return false
	}
	for pos, r := range word[:i] {
		if pos == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
