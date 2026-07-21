package preflight

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/config"
)

func shellExecutable(words []string) string {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	if len(words) > 0 {
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
