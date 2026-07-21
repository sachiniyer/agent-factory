// Package shellsuggest builds the shell commands af PRINTS for a human to paste
// and run by hand ("kill it with: …", "restore it first (…)", "run: chmod +x …").
//
// It exists because those commands are built from values a user chose — session
// titles, filesystem paths, config values — and titles are not shell-safe by
// construction. A session's tmux handle is sanitized, but user-facing commands
// still address the raw title, so  deploy `id` ; echo hi  reaches these command
// suggestions intact. Interpolated raw, the printed command either fails or —
// worse — reads like one thing and does another. It is printed exactly when
// someone is already cleaning up a mess, which is when it most has to be right
// (#1978).
//
// # Why a seam rather than "remember to quote"
//
// Quoting correctly at each of ten call sites is a habit, and habits lose to the
// next contributor: the class was already understood and fixed at ONE site
// (daemon/manager_sessions.go, whose comment states the hazard exactly) while
// nine others kept printing raw values — one of them the very same
// `af sessions restore` suggestion. #1915's redaction guard was bypassed by
// adjacent sites twice for the same reason.
//
// So the suggestion API takes the command's PIECES, never a preformatted string.
// There is no parameter to smuggle an unquoted `fmt.Sprintf` through, and every
// piece is quoted on the way out. Doing it right is the only thing the seam lets
// you do, and it keeps the phrasing of what we tell users to run consistent.
//
// # Scope
//
// This is for commands we PRINT. Strings af itself feeds to a shell (ssh scripts,
// tmux program strings) are a different job with different helpers; consolidating
// those is #1529.
package shellsuggest

import "strings"

// shellSafeChars are the punctuation runes sh, bash and zsh all treat literally
// in the MIDDLE of an unquoted word, so a value made only of these (plus
// alphanumerics) needs no quoting and stays readable.
//
// Position matters: see startsExpandable — some of these are inert mid-word and
// expand at the START of one.
const shellSafeChars = "_@%+=:,./-"

// Arg renders s as exactly ONE shell argument, correct in **sh, bash and zsh**.
//
// Naming the shells is deliberate: "shell-safe" with no named target is a vibe,
// not a claim, and the next contributor inherits whichever shells the last one
// happened to think about. zsh is not optional here — it has been macOS's default
// login shell since Catalina, so it is the shell most readers of these
// suggestions are pasting into.
//
// Values that need no quoting pass through, so the common suggestion stays clean
// and readable (`af sessions restore captain`, not `af sessions restore 'captain'`)
// — readability matters for a string whose whole purpose is to be read and pasted.
// Anything else is single-quoted, with embedded single quotes escaped by the
// standard POSIX idiom ('\”), which makes every other metacharacter — space, ",
// $, backtick, ;, newline — literal in all three shells.
//
// Prefer Command: a bare Arg still leaves a caller assembling a command by hand.
// Arg is exported for the rare site that must interleave quoted values with
// literal shell syntax it controls.
func Arg(s string) string {
	if s == "" {
		return "''"
	}
	if !startsExpandable(s) && !strings.ContainsFunc(s, needsQuoting) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// startsExpandable reports whether s begins with a character that is inert in the
// middle of a word but EXPANDS at the start of one, so the passthrough must judge
// the first character more strictly than the rest.
//
//	'='  zsh equals-expansion: an unquoted word starting with '=' expands to the
//	     path of that command (the EQUALS option, on by default). `=foo:` becomes
//	     "foo: not found" and the command does not run. sh/bash leave it alone —
//	     so a bash-only test passes against this bug, which is why the test for it
//	     executes under zsh (#1978 review).
//
// This is not hypothetical for us: exactTarget() (session/tmux/cleanup.go) builds
// "=<name>:" precisely BECAUSE the '=' makes tmux match that session EXACTLY
// rather than prefix-matching a sibling (#1006). So the arguments most likely to
// start with '=' are exactly the ones where hitting the wrong target is the
// disaster the '=' was added to avoid.
//
// '~' (tilde expansion, sh/bash/zsh) needs no case here: it is absent from
// shellSafeChars, so the general rule already quotes it. '%' is a job spec only
// as an argument to a job-control builtin (kill/fg/bg), which we never suggest,
// and both shells leave it literal otherwise — verified, not assumed.
//
// A leading '-' is deliberately NOT handled: quoting does not stop a program
// parsing '-rf' as a flag (the shell passes the same bytes either way), that is a
// job for a "--" separator at the call site, and quoting every "-t"/"--init" we
// pass would make the common suggestion unreadable for no gain.
func startsExpandable(s string) bool {
	return s[0] == '='
}

func needsQuoting(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case strings.ContainsRune(shellSafeChars, r):
		return false
	default:
		return true
	}
}

// Command renders a command that is safe to paste, from its pieces: the program
// name and each argument, quoted individually.
//
// It deliberately takes pieces rather than a format string — that is the whole
// point of the seam. Pass the raw values; do NOT pre-format them:
//
//	shellsuggest.Command("tmux", "kill-session", "-t", sessionName)  // yes
//	shellsuggest.Command("tmux kill-session -t " + sessionName)      // defeats it
//
// The second compiles (it is a valid one-piece command) but quotes the whole
// string as a single argument, which is visibly wrong in the output — the failure
// is loud rather than silent.
//
// Built with a strings.Builder rather than the obvious
// make([]string, 0, len(args)+1) + strings.Join. That form tripped CodeQL's
// go/allocation-size-overflow (high) on `len(args)+1` inside make(). The alert
// was a FALSE POSITIVE — every call site passes a fixed literal arg list, so
// len(args) is a small compile-time constant, and reaching an overflow would
// need a []string of ~2^63 elements (~10^20 bytes of headers alone), which
// cannot be constructed. But the arithmetic bought nothing on a path that runs
// only while formatting an error message for a human, and code with no
// allocation-size expression at all beats code that argues with a scanner. Do
// not "optimise" this back into make()+Join: it re-raises a required-check
// failure to save an allocation nobody will ever measure (#1978).
func Command(name string, args ...string) string {
	var b strings.Builder
	b.WriteString(Arg(name))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(Arg(a))
	}
	return b.String()
}
