package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func unwrapNohup(words []*syntax.Word) ([]*syntax.Word, bool) {
	if len(words) > 0 && wordEquals(words[0], "--") {
		words = words[1:]
	}
	if len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal || strings.HasPrefix(option, "-") {
			return nil, true
		}
	}
	return words, false
}

func unwrapNice(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			return words[1:], false
		case option == "-n" || option == "--adjustment":
			if len(words) < 2 {
				return nil, true
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return nil, true
			}
			words = words[2:]
		case strings.HasPrefix(option, "-n") || strings.HasPrefix(option, "--adjustment="):
			words = words[1:]
		case strings.HasPrefix(option, "-") && len(option) > 1:
			// Traditional nice accepts a bare numeric adjustment such as -10.
			if strings.Trim(option[1:], "0123456789") != "" {
				return nil, true
			}
			words = words[1:]
		default:
			return words, false
		}
	}
	return nil, false
}

func unwrapTimeout(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			words = words[1:]
			if len(words) < 2 {
				return nil, false
			}
			return words[1:], false
		case option == "-k" || option == "--kill-after" || option == "-s" || option == "--signal":
			if len(words) < 2 {
				return nil, true
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return nil, true
			}
			words = words[2:]
		case option == "--foreground" || option == "--preserve-status" || option == "-v" || option == "--verbose":
			words = words[1:]
		case strings.HasPrefix(option, "--kill-after=") || strings.HasPrefix(option, "--signal="):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return nil, true
		default:
			if len(words) < 2 {
				return nil, false
			}
			return words[1:], false
		}
	}
	return nil, false
}

func unwrapSetsid(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch option {
		case "--":
			return words[1:], false
		case "-h", "--help", "-V", "--version":
			return nil, false
		case "-c", "--ctty", "-f", "--fork", "-w", "--wait":
			words = words[1:]
		default:
			if strings.HasPrefix(option, "-") && len(option) > 1 {
				for _, flag := range option[1:] {
					if flag != 'c' && flag != 'f' && flag != 'w' {
						return nil, true
					}
				}
				words = words[1:]
				continue
			}
			return words, false
		}
	}
	return nil, false
}

func waitMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	sawResultTarget := false
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			// Before an explicit -p, an expanded leading operand can become -p
			// at runtime. After one, it is the customary expanded job spec.
			return !sawResultTarget
		}
		if option == "--" || option == "-" || !strings.HasPrefix(option, "-") {
			return false
		}
		flags := option[1:]
		consumed := 1
		for idx, flag := range flags {
			switch flag {
			case 'f', 'n':
			case 'p':
				// Bash documents -p varname as a separate operand. Refuse
				// attached or dynamic spellings whose assignment target cannot
				// be proven, and keep scanning because repeated -p options use
				// the last target.
				if idx != len(flags)-1 || len(words) < 2 {
					return true
				}
				target, literal := literalShellWord(words[1])
				if !literal || accountEnvironmentOperandDenied(target, names) {
					return true
				}
				sawResultTarget = true
				consumed = 2
			default:
				return true
			}
		}
		words = words[consumed:]
	}
	return false
}

func letMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	for _, word := range words {
		expression, literal := literalShellWord(word)
		if !literal || accountSubscriptInArithmetic(expression, names) {
			return true
		}
		parsed, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Arithmetic(strings.NewReader(expression))
		if err != nil {
			return true
		}
		mutates := false
		syntax.Walk(parsed, func(node syntax.Node) bool {
			if nodeMutatesAccountEnvironment(node, names) {
				mutates = true
				return false
			}
			return true
		})
		if mutates {
			return true
		}
	}
	return false
}

func accountSubscriptInArithmetic(expression string, names map[string]struct{}) bool {
	for name := range names {
		for offset := 0; offset < len(expression); {
			index := strings.Index(expression[offset:], name+"[")
			if index < 0 {
				break
			}
			index += offset
			if index == 0 || !isShellNameByte(expression[index-1]) {
				return true
			}
			offset = index + len(name)
		}
	}
	return false
}

func isShellNameByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

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
