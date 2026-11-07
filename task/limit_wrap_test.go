package task

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// This file holds the wrapped-banner evidence-stability test, split out of
// limit_test.go to keep that file under the structural-health line limit
// (#1145). It pins the #6765 re-read identity for wrapped banners.

// TestClaudeResetWrappedBannerEvidenceStability pins the #6765 re-read identity
// for wrapped banners: re-reading an unchanged wrapped banner must reproduce the
// same evidence phrase, so the poll's same-phrase dedupe keeps the first (nearest
// to the wall) reset rather than re-parsing against a later clock. The evidence
// is the whitespace-normalized reset clause — deterministic for the same banner.
func TestClaudeResetWrappedBannerEvidenceStability(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)
	det := NewLimitDetector(nil)

	wrapped := "⎿  Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York)."
	// Two reads of the same banner at different clocks must produce the same
	// evidence (the phrase is the re-read's identity) but may produce different
	// reset times (a time-only phrase re-resolves against the later clock).
	_, reset1, _, ev1 := det.Check(wrapped, tmux.ProgramClaude, now)
	_, reset2, _, ev2 := det.Check(wrapped, tmux.ProgramClaude, now.Add(time.Hour))
	if ev1 != ev2 {
		t.Fatalf("an unchanged wrapped banner must reproduce identical evidence: %q != %q", ev1, ev2)
	}
	if ev1 == "" {
		t.Fatalf("a wrapped banner with a parseable reset must carry evidence, got empty")
	}
	if !strings.Contains(ev1, "America/New_York") {
		t.Fatalf("evidence must contain the parenthesized timezone (the fix's whole point): %q", ev1)
	}
	if strings.Contains(ev1, "\n") {
		t.Fatalf("evidence must be whitespace-normalized (no raw newlines): %q", ev1)
	}
	// The reset time for a time-only phrase re-resolves against the later clock,
	// so the two reads differ — but the evidence (identity) is unchanged.
	if reset1.Equal(reset2) {
		// 2pm is after both 10:00 and 11:00, so both resolve to today 2pm —
		// they CAN be equal. Assert only that the evidence is stable, which
		// is the #6765 contract. (A phrase past the later clock would differ.)
	}

	// Trailing transcript/prompt rows after a clause that carries no trailing
	// period must NOT be swallowed into the capture: the evidence is the reset
	// clause alone, so it stays stable across re-reads even as the rows below
	// the banner change. The clause here has no terminating period (the shipped
	// banner form), and two trailing rows follow it in the same pane capture.
	trailing := "⎿  Claude usage limit reached. Your limit will reset at 2pm (America/New_York)\n" +
		"some transcript row\nanother changing row"
	_, _, _, evT := det.Check(trailing, tmux.ProgramClaude, now)
	if want := "reset at 2pm (America/New_York)"; evT != want {
		t.Fatalf("trailing rows must not be swallowed into the evidence: got %q, want %q", evT, want)
	}

	// A wrap between complete words ("reset\nat") is byte-identical to a
	// mid-token wrap at the newline, so the normalizer must not collapse it to
	// "resetat": the evidence must match the unwrapped render ("reset at …")
	// so a pane resize between reads does not change the identity phrase and
	// bypass the same-wall dedupe in SetLimitReachedAtEpoch.
	unwrappedAt := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrappedAt := "Claude usage limit reached. Your limit will reset\nat 2pm (America/New_York)."
	_, _, _, evUnwrappedAt := det.Check(unwrappedAt, tmux.ProgramClaude, now)
	_, _, _, evWrappedAt := det.Check(wrappedAt, tmux.ProgramClaude, now)
	if evUnwrappedAt != evWrappedAt {
		t.Fatalf("a word-boundary wrap must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedAt, evWrappedAt)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedAt != want {
		t.Fatalf("wrapped word-boundary evidence = %q, want %q", evWrappedAt, want)
	}

	// A wrap that pushes the trailing period onto its own row — "2pm
	// (America/New_York)\n." — must produce the SAME evidence as the
	// unwrapped "2pm (America/New_York)." form: the period is sentence
	// punctuation, not a new word, so the wrap newline is dropped (not
	// spaced to "(…) ."). A divergent evidence would bypass the same-wall
	// dedupe in SetLimitReachedAtEpoch after a pane resize.
	unwrappedPeriod := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrappedPeriod := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)\n."
	_, _, _, evUnwrappedPeriod := det.Check(unwrappedPeriod, tmux.ProgramClaude, now)
	_, _, _, evWrappedPeriod := det.Check(wrappedPeriod, tmux.ProgramClaude, now)
	if evUnwrappedPeriod != evWrappedPeriod {
		t.Fatalf("a wrapped trailing period must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedPeriod, evWrappedPeriod)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedPeriod != want {
		t.Fatalf("wrapped trailing-period evidence = %q, want %q", evWrappedPeriod, want)
	}

	// A wrap inside the [ap]m meridiem suffix — "2p\nm" — must produce the
	// SAME evidence as the unwrapped "2pm" form: the suffix letters rejoin
	// (not space to "2p m"), keeping the identity stable across resizes.
	wrappedMeridiem := "Claude usage limit reached. Your limit will reset at 2p\nm (America/New_York)."
	_, _, _, evWrappedMeridiem := det.Check(wrappedMeridiem, tmux.ProgramClaude, now)
	if evWrappedMeridiem != evUnwrappedPeriod {
		t.Fatalf("a wrapped meridiem suffix must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedPeriod, evWrappedMeridiem)
	}

	// A wrap before the date comma — "Nov 6\n, 2027 9am" — must produce the
	// SAME evidence as the unwrapped "Nov 6, 2027 9am" form: the comma
	// immediately follows the day (not "Nov 6 ,"), keeping the identity
	// stable and the explicit year matched.
	unwrappedComma := "Claude usage limit reached. Your limit will reset at Nov 6, 2027 9am (America/New_York)."
	wrappedComma := "Claude usage limit reached. Your limit will reset at Nov 6\n, 2027 9am (America/New_York)."
	_, _, _, evUnwrappedComma := det.Check(unwrappedComma, tmux.ProgramClaude, now)
	_, _, _, evWrappedComma := det.Check(wrappedComma, tmux.ProgramClaude, now)
	if evUnwrappedComma != evWrappedComma {
		t.Fatalf("a wrap before the date comma must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedComma, evWrappedComma)
	}

	// A wrap that pushes BOTH the timezone and the trailing period onto
	// their own rows — "2pm\n(America/New_York)\n." — must produce the
	// SAME evidence as the unwrapped "2pm (America/New_York)." form: the
	// paren and the period are both sentence punctuation, not new words,
	// so the wrap newlines are dropped (not spaced to "(…) ."). A
	// divergent evidence would bypass the same-wall dedupe in
	// SetLimitReachedAtEpoch after a pane resize.
	unwrappedTzPeriod := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrappedTzPeriod := "Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York)\n."
	_, _, _, evUnwrappedTzPeriod := det.Check(unwrappedTzPeriod, tmux.ProgramClaude, now)
	_, _, _, evWrappedTzPeriod := det.Check(wrappedTzPeriod, tmux.ProgramClaude, now)
	if evUnwrappedTzPeriod != evWrappedTzPeriod {
		t.Fatalf("a wrapped tz+period must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedTzPeriod, evWrappedTzPeriod)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedTzPeriod != want {
		t.Fatalf("wrapped tz+period evidence = %q, want %q", evWrappedTzPeriod, want)
	}

	// A wrap inside the "reset" keyword itself — "re\nset at 2pm (…)." —
	// must produce the SAME evidence as the unwrapped "reset at 2pm (…)."
	// form: the keyword letters rejoin (not space to "re set"), keeping
	// the identity stable across pane resizes.
	wrappedKeyword := "Claude usage limit reached. Your limit will re\nset at 2pm (America/New_York)."
	_, _, _, evWrappedKeyword := det.Check(wrappedKeyword, tmux.ProgramClaude, now)
	if evWrappedKeyword != evUnwrappedTzPeriod {
		t.Fatalf("a wrapped keyword must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedTzPeriod, evWrappedKeyword)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedKeyword != want {
		t.Fatalf("wrapped keyword evidence = %q, want %q", evWrappedKeyword, want)
	}

	// A wrap inside a weekday name — "Th\nursday at 9am (…)." — must
	// produce the SAME evidence as the unwrapped "Thursday at 9am (…)."
	// form: the weekday letters rejoin (not space to "Th ursday"),
	// keeping the identity stable across pane resizes.
	unwrappedWeekday := "Claude usage limit reached. Your limit will reset Thursday at 9am (America/New_York)."
	wrappedWeekday := "Claude usage limit reached. Your limit will reset Th\nursday at 9am (America/New_York)."
	_, _, _, evUnwrappedWeekday := det.Check(unwrappedWeekday, tmux.ProgramClaude, now)
	_, _, _, evWrappedWeekday := det.Check(wrappedWeekday, tmux.ProgramClaude, now)
	if evUnwrappedWeekday != evWrappedWeekday {
		t.Fatalf("a wrapped weekday must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedWeekday, evWrappedWeekday)
	}
	if want := "reset Thursday at 9am (America/New_York)."; evWrappedWeekday != want {
		t.Fatalf("wrapped weekday evidence = %q, want %q", evWrappedWeekday, want)
	}

	// A wrap between the clock's final digit and its [ap]m meridiem suffix —
	// "2:30\npm" — must produce the SAME evidence as the unwrapped "2:30pm"
	// form: the suffix rejoining the digits (not spacing to "2:30 pm")
	// keeps the identity stable across pane resizes.
	unwrappedMeridiemDigits := "Claude usage limit reached. Your limit will reset at 2:30pm (America/New_York)."
	wrappedMeridiemDigits := "Claude usage limit reached. Your limit will reset at 2:30\npm (America/New_York)."
	_, _, _, evUnwrappedMeridiemDigits := det.Check(unwrappedMeridiemDigits, tmux.ProgramClaude, now)
	_, _, _, evWrappedMeridiemDigits := det.Check(wrappedMeridiemDigits, tmux.ProgramClaude, now)
	if evUnwrappedMeridiemDigits != evWrappedMeridiemDigits {
		t.Fatalf("a wrapped meridiem-after-digits must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedMeridiemDigits, evWrappedMeridiemDigits)
	}
	if want := "reset at 2:30pm (America/New_York)."; evWrappedMeridiemDigits != want {
		t.Fatalf("wrapped meridiem-after-digits evidence = %q, want %q", evWrappedMeridiemDigits, want)
	}

	// A wrap inside an ordinal date suffix — "Nov 12t\nh, 2027 9am" — must
	// produce the SAME evidence as the unwrapped "Nov 12th, 2027 9am" form:
	// the suffix letters rejoin (not space to "12t h") so monthDay keeps the
	// explicit year and the identity stays stable across pane resizes.
	unwrappedOrdinal := "Claude usage limit reached. Your limit will reset at Nov 12th, 2027 9am (America/New_York)."
	wrappedOrdinal := "Claude usage limit reached. Your limit will reset at Nov 12t\nh, 2027 9am (America/New_York)."
	_, _, _, evUnwrappedOrdinal := det.Check(unwrappedOrdinal, tmux.ProgramClaude, now)
	_, _, _, evWrappedOrdinal := det.Check(wrappedOrdinal, tmux.ProgramClaude, now)
	if evUnwrappedOrdinal != evWrappedOrdinal {
		t.Fatalf("a wrapped ordinal suffix must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedOrdinal, evWrappedOrdinal)
	}
	if want := "reset at Nov 12th, 2027 9am (America/New_York)."; evWrappedOrdinal != want {
		t.Fatalf("wrapped ordinal-suffix evidence = %q, want %q", evWrappedOrdinal, want)
	}

	// A wrap between a complete weekday name and the following word —
	// "Monday\nat" — must NOT rejoin to "Mondayat": the joined token is a
	// weekday-shaped prefix but not a complete weekday name, so the wrap
	// stays a space and the evidence matches the unwrapped "Monday at"
	// render, keeping the identity stable across pane resizes.
	unwrappedWeekdayAt := "Claude usage limit reached. Your limit will reset Monday at 9am (America/New_York)."
	wrappedWeekdayAt := "Claude usage limit reached. Your limit will reset Monday\nat 9am (America/New_York)."
	_, _, _, evUnwrappedWeekdayAt := det.Check(unwrappedWeekdayAt, tmux.ProgramClaude, now)
	_, _, _, evWrappedWeekdayAt := det.Check(wrappedWeekdayAt, tmux.ProgramClaude, now)
	if evUnwrappedWeekdayAt != evWrappedWeekdayAt {
		t.Fatalf("a wrap after a complete weekday must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedWeekdayAt, evWrappedWeekdayAt)
	}
	if want := "reset Monday at 9am (America/New_York)."; evWrappedWeekdayAt != want {
		t.Fatalf("wrapped weekday+word evidence = %q, want %q", evWrappedWeekdayAt, want)
	}

	// A month token split across MORE than two rows — "November" splits as
	// "N\no\nvember" — must produce the SAME evidence as the unwrapped
	// "November 6, 2027 9am" form: each 2-letter fragment rejoins (it is a
	// strict prefix of a month abbreviation) so the next wrap sees the
	// longer fragment and rejoins in turn, keeping the identity stable
	// across pane resizes (rather than spacing to "N o vember", which
	// monthDay misses).
	unwrappedMultiRowMonth := "Claude usage limit reached. Your limit will reset at November 6, 2027 9am (America/New_York)."
	wrappedMultiRowMonth := "Claude usage limit reached. Your limit will reset at N\no\nvember 6, 2027 9am (America/New_York)."
	_, _, _, evUnwrappedMultiRowMonth := det.Check(unwrappedMultiRowMonth, tmux.ProgramClaude, now)
	_, _, _, evWrappedMultiRowMonth := det.Check(wrappedMultiRowMonth, tmux.ProgramClaude, now)
	if evUnwrappedMultiRowMonth != evWrappedMultiRowMonth {
		t.Fatalf("a multi-row month split must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedMultiRowMonth, evWrappedMultiRowMonth)
	}
	if want := "reset at November 6, 2027 9am (America/New_York)."; evWrappedMultiRowMonth != want {
		t.Fatalf("multi-row month evidence = %q, want %q", evWrappedMultiRowMonth, want)
	}

	// The production gutter+indent layout: a wrap inside the tz paren that
	// carries the 5-space continuation indent — "(America/\n     New_York)"
	// — must produce the SAME evidence as the unwrapped "(America/New_York)"
	// form: the indent is stripped on rejoin (not spaced to
	// "(America/      New_York)"), keeping the identity stable and the zone
	// acceptable to time.LoadLocation across pane resizes.
	unwrappedIndent := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrappedIndent := "⎿  Claude usage limit reached. Your limit will reset at 2pm (America/\n     New_York)."
	_, _, _, evUnwrappedIndent := det.Check(unwrappedIndent, tmux.ProgramClaude, now)
	_, _, _, evWrappedIndent := det.Check(wrappedIndent, tmux.ProgramClaude, now)
	if evUnwrappedIndent != evWrappedIndent {
		t.Fatalf("a wrapped tz paren with gutter indent must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedIndent, evWrappedIndent)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedIndent != want {
		t.Fatalf("wrapped tz-paren-indent evidence = %q, want %q", evWrappedIndent, want)
	}

	// A wrap inside the "at" preposition — "a\nt" — must produce the SAME
	// evidence as the unwrapped "reset at 2pm (…)." form: the preposition
	// letters rejoin (not space to "reset a t 2pm"), keeping the identity
	// stable across pane resizes.
	wrappedAtPrep := "Claude usage limit reached. Your limit will reset a\nt 2pm (America/New_York)."
	_, _, _, evWrappedAtPrep := det.Check(wrappedAtPrep, tmux.ProgramClaude, now)
	if evWrappedAtPrep != evUnwrappedIndent {
		t.Fatalf("a wrapped at preposition must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedIndent, evWrappedAtPrep)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedAtPrep != want {
		t.Fatalf("wrapped at-preposition evidence = %q, want %q", evWrappedAtPrep, want)
	}

	// A wrap between a day digit and its ordinal suffix — "Nov 12\nth" —
	// must produce the SAME evidence as the unwrapped "Nov 12th, 2027 9am"
	// form: the suffix rejoins the day (not space to "Nov 12 th") so
	// monthDay keeps the explicit year and the identity stays stable.
	wrappedDigitOrdinal := "Claude usage limit reached. Your limit will reset at Nov 12\nth, 2027 9am (America/New_York)."
	_, _, _, evWrappedDigitOrdinal := det.Check(wrappedDigitOrdinal, tmux.ProgramClaude, now)
	if evWrappedDigitOrdinal != evUnwrappedOrdinal {
		t.Fatalf("a wrapped digit+ordinal suffix must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedOrdinal, evWrappedDigitOrdinal)
	}
	if want := "reset at Nov 12th, 2027 9am (America/New_York)."; evWrappedDigitOrdinal != want {
		t.Fatalf("wrapped digit-ordinal evidence = %q, want %q", evWrappedDigitOrdinal, want)
	}

	// A wrap after a complete ordinal suffix and before the comma —
	// "Nov 12th\n, 2027" — must produce the SAME evidence as the unwrapped
	// "Nov 12th, 2027 9am" form: the comma rejoins the suffix (not space to
	// "Nov 12th , 2027") so monthDay keeps the explicit year.
	wrappedOrdinalComma := "Claude usage limit reached. Your limit will reset at Nov 12th\n, 2027 9am (America/New_York)."
	_, _, _, evWrappedOrdinalComma := det.Check(wrappedOrdinalComma, tmux.ProgramClaude, now)
	if evWrappedOrdinalComma != evUnwrappedOrdinal {
		t.Fatalf("a wrapped ordinal-suffix+comma must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedOrdinal, evWrappedOrdinalComma)
	}
	if want := "reset at Nov 12th, 2027 9am (America/New_York)."; evWrappedOrdinalComma != want {
		t.Fatalf("wrapped ordinal-suffix-comma evidence = %q, want %q", evWrappedOrdinalComma, want)
	}

	// A styled abbreviated date whose ANSI color resets between the
	// month's period and the wrap — "Nov.\x1b[39m\n6" — must produce the
	// SAME evidence as the unwrapped "Nov. 6, 2027 9am" form: the SGR
	// bytes are stripped by the normalizer (not left between "Nov." and
	// the day) so monthDay matches the date and the identity stays stable
	// across pane resizes.
	unwrappedStyledMonth := "Claude usage limit reached. Your limit will reset at Nov. 6, 2027 9am (America/New_York)."
	wrappedStyledMonth := "Claude usage limit reached. Your limit will reset at Nov.\x1b[39m\n6, 2027 9am (America/New_York)."
	_, _, _, evUnwrappedStyledMonth := det.Check(unwrappedStyledMonth, tmux.ProgramClaude, now)
	_, _, _, evWrappedStyledMonth := det.Check(wrappedStyledMonth, tmux.ProgramClaude, now)
	if evUnwrappedStyledMonth != evWrappedStyledMonth {
		t.Fatalf("a styled abbreviated-month wrap must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedStyledMonth, evWrappedStyledMonth)
	}
	if want := "reset at Nov. 6, 2027 9am (America/New_York)."; evWrappedStyledMonth != want {
		t.Fatalf("styled abbreviated-month evidence = %q, want %q", evWrappedStyledMonth, want)
	}
}
