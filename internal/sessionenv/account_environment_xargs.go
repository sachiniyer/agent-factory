package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// unwrapXargs removes a GNU `xargs` prefix so the command it execs is what
// gets inspected. Unlike the other modeled wrappers, xargs splices content
// this walk cannot see into the child's argv: input items are appended after
// the initial arguments, and -I/-i/--replace substitutes every marker
// occurrence with an input line. An env invocation whose operand region can
// receive either is unprovable, because env re-parses the substituted word —
// an item spelling NAME=value becomes an assignment even when the marker sat
// in env's command slot.
func unwrapXargs(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	run := memo.optionRun("xargs")
	substituting := false
	markerKnown := true
	marker := "{}"
	// substituting/markerKnown/marker keep the walk's long-standing reading,
	// which the env operand scan below relies on. The real binaries differ,
	// tracked separately so the xargs input checks follow them while every
	// existing refusal stands (#4977). A bare -i/--replace means the marker {}.
	// And GNU xargs 4.9 makes -I, -L/-l and -n mutually exclusive with the
	// last one winning ("ignoring previous --replace value"), except -n1,
	// while BSD xargs (macOS) keeps -I in force alongside them. So a later
	// -L/-l/-n sets replaceCancelled, meaning input may be appended, and both
	// readings are judged.
	replaceCancelled := false
	replaceMarker := "{}"
options:
	for len(words) > 0 {
		state := xargsLoopState{substituting: substituting, markerKnown: markerKnown, marker: marker,
			replaceCancelled: replaceCancelled, replaceMarker: replaceMarker}
		if cached, seen := run.visit(words, state); seen {
			return run.done(cached.words, cached.unsafe)
		}
		option, literal := literalShellWord(words[0])
		if !literal {
			return run.done(nil, true)
		}
		switch {
		case option == "--":
			words = words[1:]
			break options
		case !strings.HasPrefix(option, "-") || option == "-":
			break options
		case strings.HasPrefix(option, "--"):
			name, value, attached := strings.Cut(option[2:], "=")
			switch name {
			case "help", "version":
				if shadowedOperandTailMutates(words[1:], names, memo) {
					return run.done(nil, true)
				}
				return run.done(words[1:], false)
			case "null", "interactive", "no-run-if-empty", "open-tty", "verbose", "exit", "show-limits":
				if attached {
					return run.done(nil, true)
				}
				words = words[1:]
			case "eof", "max-lines":
				// Optional-argument long options take a value only via =.
				if name == "max-lines" {
					replaceCancelled = true
				}
				words = words[1:]
			case "replace":
				substituting = true
				replaceCancelled, replaceMarker = false, "{}"
				// An explicitly empty --replace= marker is treated like a bare
				// --replace and defaults to {}: an empty marker would make every
				// strings.Contains/HasPrefix scan in the xargs-input walk report
				// every word as carrying substituted input (#4980).
				if attached && value != "" {
					marker, replaceMarker = value, value
				}
				words = words[1:]
			case "arg-file", "delimiter", "max-args", "max-procs", "max-chars":
				arg := value
				if !attached {
					if len(words) < 2 {
						return run.done(nil, true)
					}
					// The argument value itself is inert to this analysis on
					// the real binary, but a shadowed xargs may exec it.
					if wrapperOperandTailMutates(words[1:], names, memo) {
						return run.done(nil, true)
					}
					arg, _ = literalShellWord(words[1])
					words = words[1:]
				}
				if name == "max-args" && xargsCountCancelsReplace(arg) {
					replaceCancelled = true
				}
				words = words[1:]
			case "process-slot-var":
				var arg string
				var argLiteral bool
				if attached {
					arg, argLiteral = value, true
				} else {
					if len(words) < 2 {
						return run.done(nil, true)
					}
					arg, argLiteral = literalShellWord(words[1])
					if wrapperOperandTailMutates(words[1:], names, memo) {
						return run.done(nil, true)
					}
					words = words[1:]
				}
				if !argLiteral || accountEnvironmentOperandDenied(arg, names) {
					return run.done(nil, true)
				}
				words = words[1:]
			default:
				return run.done(nil, true)
			}
		default:
			flags := option[1:]
			for idx := 0; idx < len(flags); idx++ {
				switch flags[idx] {
				case '0', 'o', 'p', 'r', 't', 'x':
				case 'e', 'l':
					// -e/-l take an optional attached argument; whatever
					// remains in this word is the value.
					if flags[idx] == 'l' {
						replaceCancelled = true
					}
					idx = len(flags)
				case 'i':
					substituting = true
					replaceCancelled, replaceMarker = false, "{}"
					if idx+1 < len(flags) {
						marker, replaceMarker = flags[idx+1:], flags[idx+1:]
					}
					idx = len(flags)
				case 'a', 'd', 'E', 'I', 'L', 'n', 'P', 's':
					var arg string
					var argLiteral bool
					if idx+1 < len(flags) {
						arg, argLiteral = flags[idx+1:], true
					} else {
						if len(words) < 2 {
							return run.done(nil, true)
						}
						arg, argLiteral = literalShellWord(words[1])
						if wrapperOperandTailMutates(words[1:], names, memo) {
							return run.done(nil, true)
						}
						words = words[1:]
					}
					switch flags[idx] {
					case 'I':
						substituting = true
						replaceCancelled, replaceMarker = false, "{}"
						if argLiteral {
							if arg != "" {
								marker, replaceMarker = arg, arg
							}
							// An explicitly empty -I "" marker is treated like a
							// bare -i/--replace and defaults to {}: an empty
							// marker would make every strings.Contains/HasPrefix
							// scan in the xargs-input walk report every word as
							// carrying substituted input (#4980).
						} else {
							markerKnown = false
							replaceMarker = arg
						}
					case 'L':
						replaceCancelled = true
					case 'n':
						if xargsCountCancelsReplace(arg) {
							replaceCancelled = true
						}
					}
					idx = len(flags)
				default:
					return run.done(nil, true)
				}
			}
			words = words[1:]
		}
	}
	if len(words) == 0 {
		// With no command operand xargs runs its default echo on each input
		// item — nothing here to unwrap.
		return run.done(nil, false)
	}
	if _, literal := literalShellWord(words[0]); !literal {
		return run.done(nil, true)
	}
	state := xargsLoopState{substituting: substituting, markerKnown: markerKnown, marker: marker}
	if xargsEnvOperandsFed(words, state, names, memo) {
		return run.done(nil, true)
	}
	if !substituting {
		return run.done(xargsChild(words, false, "", names, memo))
	}
	replace := xargsLoopState{substituting: true, markerKnown: markerKnown, marker: marker,
		replaceCancelled: replaceCancelled, replaceMarker: replaceMarker}
	return run.done(xargsReplaceChild(words, replace, names, memo))
}
