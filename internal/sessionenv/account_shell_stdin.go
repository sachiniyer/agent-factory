package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// commandFeedsProvenShell reports whether command contains a proven
// startup-free shell (accountShellCommandWordsProven) together with any
// construct that can give a shell's standard input something other than the
// pane's terminal.
//
// Those argv forms are proven because an interactive shell reads code only
// from the person typing into the pane. `/bin/sh -i <./script`, a
// here-document, a pipe, or a coprocess replaces that terminal with text the
// command chose, and that text can export another CODEX_HOME and launch the
// agent — the same unproven code a `sh ./script` sibling is already refused for
// (Codex on #4474).
//
// Both halves are deliberately position-blind. Which statement a redirection
// reaches is not a local question — `f() { /bin/sh -i; }; f <script` and
// `exec <script; /bin/sh -i` feed a shell whose own call carries none — so a
// stdin-capable construct anywhere counts, and so does any argv suffix that
// spells a proven shell, including one a wrapper only carries as data. The
// cost is bounded: a command with no proven shell is never affected
// (`sort <data`, `git log | head`), and output redirections do not count
// unless they target descriptor 0, so `make >build.log; /bin/sh -i` stays
// admitted.
func commandFeedsProvenShell(command string) bool {
	if command == "" {
		return false
	}
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
		if err != nil {
			// commandMutatesAccountEnvironment has already refused anything
			// either variant cannot parse; never answer "no feed" about text
			// that was not read.
			return true
		}
		if fileHasProvenShell(file) && fileCanFeedStdin(file) {
			return true
		}
	}
	return false
}

func fileHasProvenShell(file *syntax.File) bool {
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok {
			for i := range call.Args {
				if accountShellCommandWordsProven(call.Args[i:]) {
					found = true
					break
				}
			}
		}
		return !found
	})
	return found
}

// fileCanFeedStdin is bounded by what can reach descriptor 0: an input
// redirection or here-document, an output-style redirection that names fd 0
// explicitly (`0>&3` duplicates a descriptor some earlier `3<script` opened for
// reading), a pipe, a coprocess, or an output process substitution, whose
// command reads what the outer command writes into it.
func fileCanFeedStdin(file *syntax.File) bool {
	feeds := false
	syntax.Walk(file, func(node syntax.Node) bool {
		// Returning false prunes only this node's children; later siblings are
		// still visited, so a found feed must not be overwritten by them.
		if !feeds {
			feeds = nodeCanFeedStdin(node)
		}
		return !feeds
	})
	return feeds
}

func nodeCanFeedStdin(node syntax.Node) bool {
	switch node := node.(type) {
	case *syntax.Redirect:
		return redirectCanFeedStdin(node)
	case *syntax.BinaryCmd:
		return node.Op == syntax.Pipe || node.Op == syntax.PipeAll
	case *syntax.Stmt:
		return node.Coprocess
	case *syntax.CoprocClause:
		return true
	case *syntax.ProcSubst:
		return node.Op == syntax.CmdOut
	default:
		return false
	}
}

func redirectCanFeedStdin(redirect *syntax.Redirect) bool {
	switch redirect.Op {
	case syntax.RdrIn, syntax.RdrInOut, syntax.DplIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		return true
	}
	// Every remaining operator opens or duplicates an output descriptor, which
	// is standard input only when the command names fd 0 (`0>&3`, `00>&3`).
	// A missing N means fd 1, and bash's {name}> allocates a fresh fd >= 10.
	if redirect.N == nil {
		return false
	}
	n := redirect.N.Value
	return n != "" && strings.Trim(n, "0") == ""
}
