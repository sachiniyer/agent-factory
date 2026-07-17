// Package shellsuggest builds the shell commands af PRINTS for a human to paste
// and run by hand ("kill it with: …", "restore it first (…)", "run: chmod +x …").
//
// It exists because those commands are built from values a user chose — session
// titles, filesystem paths, config values — and titles are not shell-safe by
// construction: toTmuxName (session/tmux/session.go) rewrites only the characters
// TMUX needs, and its own comment notes that other punctuation is "preserved
// verbatim". So a session titled  deploy `id` ; echo hi  reaches these commands
// intact. Interpolated raw, the printed command either fails or — worse — reads
// like one thing and does another. It is printed exactly when someone is already
// cleaning up a mess, which is when it most has to be right (#1978).
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

// shellSafeChars are the punctuation runes a POSIX shell treats literally in an
// unquoted word, so a value made only of these (plus alphanumerics) needs no
// quoting and stays readable.
const shellSafeChars = "_@%+=:,./-"

// Arg renders s as exactly ONE shell argument.
//
// Values that need no quoting pass through, so the common suggestion stays clean
// and readable (`af sessions restore captain`, not `af sessions restore 'captain'`)
// — readability matters for a string whose whole purpose is to be read and pasted.
// Anything else is single-quoted, with embedded single quotes escaped by the
// standard POSIX idiom ('\”), which makes every other metacharacter — space, ",
// $, backtick, ;, newline — literal.
//
// Prefer Command: a bare Arg still leaves a caller assembling a command by hand.
// Arg is exported for the rare site that must interleave quoted values with
// literal shell syntax it controls.
func Arg(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsFunc(s, needsQuoting) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
