package tmuxguard

import "strings"

// inspectSed accepts GNU sed's runtime-enforced sandbox mode, or a closed
// subset of literal scripts whose grammar is understood below. Script files,
// dynamic scripts, unknown options, and unknown commands fail closed.
func inspectSed(args []shellWord) string {
	if hasEffectiveSedSandbox(args) {
		return ""
	}

	scripts, ok := literalSedScripts(args)
	if !ok || len(scripts) == 0 {
		return unknownShellReason
	}
	for _, script := range scripts {
		if !safeSedScript(script) {
			return unknownShellReason
		}
	}
	return ""
}

func hasEffectiveSedSandbox(args []shellWord) bool {
	for i := 0; i < len(args); i++ {
		if !args[i].resolved {
			return false
		}
		arg := args[i].literal
		switch {
		case arg == "--":
			return false
		case arg == "--sandbox":
			return true
		case arg == "-e" || arg == "-f" || arg == "--expression" || arg == "--file":
			// GNU sed applies sandboxing while parsing each script source. A
			// later --sandbox cannot retroactively constrain an earlier script.
			return false
		case strings.HasPrefix(arg, "--expression=") || strings.HasPrefix(arg, "--file="):
			return false
		case arg == "-l" || arg == "--line-length":
			if i+1 >= len(args) || !args[i+1].resolved {
				return false
			}
			i++
		case strings.HasPrefix(arg, "--line-length="):
		case arg == "--debug" || arg == "--follow-symlinks" || arg == "--null-data" ||
			arg == "--posix" || arg == "--quiet" || arg == "--regexp-extended" ||
			arg == "--separate" || arg == "--silent" || arg == "--unbuffered":
		case strings.HasPrefix(arg, "--"):
			return false
		case strings.HasPrefix(arg, "-") && arg != "-":
			scriptBearing, consumesNext, ok := sedShortOptionBeforeSandbox(arg[1:])
			if !ok || scriptBearing {
				return false
			}
			if consumesNext {
				if i+1 >= len(args) || !args[i+1].resolved {
					return false
				}
				i++
			}
		default:
			// Requiring --sandbox in the literal option prefix avoids relying
			// on GNU option permutation or inherited POSIXLY_CORRECT state.
			return false
		}
	}
	return false
}

func sedShortOptionBeforeSandbox(flags string) (bool, bool, bool) {
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case 'E', 'n', 'r', 's', 'u', 'z':
		case 'e', 'f':
			return true, false, true
		case 'l':
			return false, i+1 == len(flags), true
		case 'i':
			return false, false, true
		default:
			return false, false, false
		}
	}
	return false, false, true
}

func literalSedScripts(args []shellWord) ([]string, bool) {
	var scripts []string
	hasExpression := false
	optionsDone := false
	for i := 0; i < len(args); i++ {
		if !args[i].resolved {
			if optionsDone {
				continue
			}
			return nil, false
		}
		arg := args[i].literal
		if optionsDone {
			continue
		}
		switch {
		case arg == "--":
			optionsDone = true
		case arg == "-e" || arg == "--expression":
			if i+1 >= len(args) || !args[i+1].resolved {
				return nil, false
			}
			i++
			scripts = append(scripts, args[i].literal)
			hasExpression = true
		case strings.HasPrefix(arg, "--expression="):
			scripts = append(scripts, strings.TrimPrefix(arg, "--expression="))
			hasExpression = true
		case arg == "-f" || arg == "--file" || strings.HasPrefix(arg, "--file="):
			return nil, false
		case arg == "-l" || arg == "--line-length":
			if i+1 >= len(args) || !args[i+1].resolved {
				return nil, false
			}
			i++
		case strings.HasPrefix(arg, "--line-length="):
		case arg == "--debug" || arg == "--follow-symlinks" || arg == "--null-data" ||
			arg == "--posix" || arg == "--quiet" || arg == "--regexp-extended" ||
			arg == "--separate" || arg == "--silent" || arg == "--unbuffered":
		case arg == "--help" || arg == "--version":
			return []string{""}, len(args) == 1
		case strings.HasPrefix(arg, "--"):
			return nil, false
		case strings.HasPrefix(arg, "-") && arg != "-":
			var script string
			var consumed bool
			var parsedOK bool
			script, consumed, parsedOK = parseSedShortOptions(arg[1:], args, &i)
			if !parsedOK {
				return nil, false
			}
			if consumed {
				scripts = append(scripts, script)
				hasExpression = true
			}
		case !hasExpression:
			scripts = append(scripts, arg)
			hasExpression = true
		default:
			// Literal input paths are data. GNU option permutation is why a
			// dynamic path still requires an explicit -- above.
		}
	}
	return scripts, true
}

func parseSedShortOptions(flags string, args []shellWord, index *int) (string, bool, bool) {
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case 'E', 'n', 'r', 's', 'u', 'z':
		case 'e':
			if i+1 < len(flags) {
				return flags[i+1:], true, true
			}
			if *index+1 >= len(args) || !args[*index+1].resolved {
				return "", false, false
			}
			*index++
			return args[*index].literal, true, true
		case 'f':
			return "", false, false
		case 'i':
			return "", false, true // The rest is the optional backup suffix.
		case 'l':
			if i+1 < len(flags) {
				return "", false, allDecimal(flags[i+1:])
			}
			if *index+1 >= len(args) || !args[*index+1].resolved || !allDecimal(args[*index+1].literal) {
				return "", false, false
			}
			*index++
			return "", false, true
		default:
			return "", false, false
		}
	}
	return "", false, true
}

func safeSedScript(script string) bool {
	for i := 0; ; {
		skipSedSeparators(script, &i)
		if i >= len(script) {
			return true
		}
		if script[i] == '#' {
			skipSedLine(script, &i)
			continue
		}
		if !skipSedAddresses(script, &i) {
			return false
		}
		skipSedSpaces(script, &i)
		if i < len(script) && script[i] == '!' {
			i++
			skipSedSpaces(script, &i)
		}
		if i >= len(script) {
			return false
		}
		command := script[i]
		i++
		switch command {
		case 'e':
			return false
		case '{', '}':
			continue
		case '#':
			skipSedLine(script, &i)
		case '=', 'd', 'D', 'F', 'g', 'G', 'h', 'H', 'l', 'n', 'N', 'p', 'P', 'x', 'z':
			if !sedCommandEnds(script, &i) {
				return false
			}
		case 'q', 'Q':
			skipSedSpaces(script, &i)
			for i < len(script) && script[i] >= '0' && script[i] <= '9' {
				i++
			}
			if !sedCommandEnds(script, &i) {
				return false
			}
		case ':', 'b', 't', 'T', 'v':
			skipSedToCommandEnd(script, &i)
		case 'a', 'c', 'i', 'r', 'R', 'w', 'W':
			skipSedLine(script, &i)
		case 's':
			if !safeSedSubstitution(script, &i) {
				return false
			}
		case 'y':
			if !skipSedDelimitedPair(script, &i) || !sedCommandEnds(script, &i) {
				return false
			}
		default:
			return false
		}
	}
}

func skipSedAddresses(script string, index *int) bool {
	skipSedSpaces(script, index)
	if !skipOneSedAddress(script, index) {
		return true
	}
	skipSedSpaces(script, index)
	if *index < len(script) && script[*index] == ',' {
		*index++
		skipSedSpaces(script, index)
		if !skipOneSedAddress(script, index) {
			return false
		}
	}
	return true
}

func skipOneSedAddress(script string, index *int) bool {
	if *index >= len(script) {
		return false
	}
	switch script[*index] {
	case '$':
		*index++
		return true
	case '/':
		return skipSedDelimited(script, index, '/')
	case '\\':
		*index++
		if *index >= len(script) {
			return false
		}
		delimiter := script[*index]
		*index++
		return skipSedDelimitedBody(script, index, delimiter)
	}
	if script[*index] < '0' || script[*index] > '9' {
		return false
	}
	for *index < len(script) && script[*index] >= '0' && script[*index] <= '9' {
		*index++
	}
	if *index < len(script) && script[*index] == '~' {
		*index++
		start := *index
		for *index < len(script) && script[*index] >= '0' && script[*index] <= '9' {
			*index++
		}
		return *index > start
	}
	return true
}

func safeSedSubstitution(script string, index *int) bool {
	if !skipSedDelimitedPair(script, index) {
		return false
	}
	for *index < len(script) && script[*index] != ';' && script[*index] != '\n' {
		flag := script[*index]
		*index++
		switch flag {
		case 'e':
			return false
		case ' ', '\t', 'g', 'i', 'I', 'm', 'M', 'p':
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		case 'w':
			skipSedLine(script, index)
			return true
		default:
			return false
		}
	}
	return true
}

func skipSedDelimitedPair(script string, index *int) bool {
	if *index >= len(script) || script[*index] == '\\' || script[*index] == '\n' {
		return false
	}
	delimiter := script[*index]
	*index++
	return skipSedDelimitedBody(script, index, delimiter) && skipSedDelimitedBody(script, index, delimiter)
}

func skipSedDelimited(script string, index *int, delimiter byte) bool {
	if *index >= len(script) || script[*index] != delimiter {
		return false
	}
	*index++
	return skipSedDelimitedBody(script, index, delimiter)
}

func skipSedDelimitedBody(script string, index *int, delimiter byte) bool {
	for *index < len(script) {
		char := script[*index]
		*index++
		if char == '\\' {
			if *index >= len(script) {
				return false
			}
			*index++
			continue
		}
		if char == delimiter {
			return true
		}
		if char == '\n' {
			return false
		}
	}
	return false
}

func sedCommandEnds(script string, index *int) bool {
	skipSedSpaces(script, index)
	return *index >= len(script) || script[*index] == ';' || script[*index] == '\n'
}

func skipSedToCommandEnd(script string, index *int) {
	for *index < len(script) && script[*index] != ';' && script[*index] != '\n' {
		*index++
	}
}

func skipSedLine(script string, index *int) {
	for *index < len(script) && script[*index] != '\n' {
		*index++
	}
}

func skipSedSeparators(script string, index *int) {
	for *index < len(script) && (script[*index] == ' ' || script[*index] == '\t' ||
		script[*index] == '\r' || script[*index] == '\n' || script[*index] == ';') {
		*index++
	}
}

func skipSedSpaces(script string, index *int) {
	for *index < len(script) && (script[*index] == ' ' || script[*index] == '\t' || script[*index] == '\r') {
		*index++
	}
}

func allDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
