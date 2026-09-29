package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func arrayReadMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	options := true
	for len(words) > 0 {
		value, literal := literalShellWord(words[0])
		if !literal {
			return true
		}
		words = words[1:]
		if options {
			switch value {
			case "--":
				options = false
				continue
			case "-t":
				continue
			case "-d":
				if len(words) == 0 {
					return true
				}
				if _, literal := literalShellWord(words[0]); !literal {
					return true
				}
				words = words[1:]
				continue
			}
			if strings.HasPrefix(value, "-") {
				// The remaining options accept arithmetic expressions or callbacks.
				// Either can assign an identity indirectly, so unsupported option
				// forms fail closed.
				return true
			}
		}
		if len(words) != 0 {
			return true
		}
		return accountEnvironmentOperandDenied(value, names)
	}
	return false
}
