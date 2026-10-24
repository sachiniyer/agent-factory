package sessionenv

import "slices"

// LiteralCommandArgv reduces a command string to its argv only when it is one
// simple call of literal words: no assignment prefix, no expansion, no
// redirection, no operator, no second command. It shares singleSimpleCall and
// literalCommandArgs with the account launch proof, so every caller parses
// command strings through the same rules rather than owning a second copy.
//
// Each returned word is its literal TEXT: quotes are removed by the parser and
// nothing is expanded, so two spellings of one argv — an unquoted, single- or
// double-quoted path among them — produce the same slice. Escaped characters
// keep their escape text (`a\ b` parses to the literal `a\ b`), which errs
// toward INEQUALITY, never toward widening.
//
// A command that is not a plain literal word list is unanswerable rather than
// approximated: it returns false and the caller must fail closed on its own
// terms.
func LiteralCommandArgv(command string) ([]string, bool) {
	call, ok := singleSimpleCall(command)
	if !ok || len(call.Assigns) > 0 {
		return nil, false
	}
	return literalCommandArgs(call.Args)
}

// LiteralCommandsEqual reports whether two command strings run the same argv:
// the same literal words in the same order, with nothing resolved or
// normalized — a bare `claude` does not match `/abs/path/claude`, and flag
// order is not canonicalized. Quoting a word differently is not a difference;
// an added, removed, reordered, or non-literal word is. A command that is not
// a plain literal word list is not equal to anything — including itself — so
// a trust decision fails closed and a drift check falls back to comparing
// bytes.
func LiteralCommandsEqual(a, b string) bool {
	aArgv, ok := LiteralCommandArgv(a)
	if !ok {
		return false
	}
	bArgv, ok := LiteralCommandArgv(b)
	if !ok {
		return false
	}
	return slices.Equal(aArgv, bArgv)
}
