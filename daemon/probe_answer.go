package daemon

import "errors"

// ProbeAnswer is what a service manager said in reply to one question.
//
// It has exactly FOUR inhabitants, because a probe has exactly four outcomes.
// Three review rounds on #1920 each fixed one instance of squeezing them into
// fewer, and the class walked one step every time — swallow the error and report
// inactive; then a timeout test that never hung; then a timeout resolving to a
// negative anyway. The instances were symptoms of the type being too small to
// hold the truth, so this is the type:
//
//   - Yes                 — it answered: the thing is so.
//   - No                  — it answered: the thing is not so.
//   - NotFound            — it answered: there is no such unit. A DEFINITE fact,
//     and the only one that tells a user autostart was never installed (or that
//     the unit file exists but systemd never loaded it). Folding it into No or
//     Undetermined throws away the case where we genuinely know.
//   - Undetermined(cause) — we did not get an answer: it timed out, the bus was
//     down, the binary was missing, permission was denied. ALL of these.
//
// Three properties make the fourth state unignorable, which is the point:
//
//  1. The zero value is Undetermined. A field nobody managed to probe reads as
//     "we do not know", never as a negative.
//  2. There is no literal form. kind is unexported, so no caller can mint a No
//     out of an error; answers only come from the constructors below.
//  3. Reading requires Match, which has no default branch. Every outcome must be
//     handled at every call site — `log.Warn(err); return inactive` does not
//     compile — and adding a fifth outcome would break every call site loudly
//     instead of silently defaulting to a negative.
//
// Doctor is the worst possible place for a fabricated negative: it is the
// command a user runs BECAUSE something is already broken, so a made-up
// "inactive" arrives exactly when they are least able to check it, and sends
// them to fix a thing that is not broken.
type ProbeAnswer struct {
	kind  probeKind
	cause error
}

type probeKind uint8

const (
	// probeUndetermined is the zero value on purpose: see (1) above.
	probeUndetermined probeKind = iota
	probeYes
	probeNo
	probeNotFound
)

// AnswerYes records that the manager answered in the affirmative.
func AnswerYes() ProbeAnswer { return ProbeAnswer{kind: probeYes} }

// AnswerNo records that the manager answered in the negative. Reachable only
// from a probe that actually completed — see probeResult.Output.
func AnswerNo() ProbeAnswer { return ProbeAnswer{kind: probeNo} }

// AnswerNotFound records that the manager answered that no such unit exists.
func AnswerNotFound() ProbeAnswer { return ProbeAnswer{kind: probeNotFound} }

// Undetermined records that no answer was obtained, and why.
//
// The cause is mandatory. An undetermined answer with nothing to say about why
// is not reportable — "unknown" alone tells a user nothing they can act on — so
// a missing cause is backfilled rather than silently dropped.
func Undetermined(cause error) ProbeAnswer {
	if cause == nil {
		cause = errors.New("the service manager gave no answer and no reason")
	}
	return ProbeAnswer{kind: probeUndetermined, cause: cause}
}

// Match dispatches on the answer. There is deliberately no default: the
// undetermined branch is a parameter, so a caller cannot forget it and quietly
// treat "I could not ask" as "the answer is no".
func (a ProbeAnswer) Match(yes, no, notFound func(), undetermined func(cause error)) {
	switch a.kind {
	case probeYes:
		yes()
	case probeNo:
		no()
	case probeNotFound:
		notFound()
	default:
		undetermined(a.cause)
	}
}

// Cause is why the answer is undetermined; nil for every other outcome.
func (a ProbeAnswer) Cause() error { return a.cause }

// String renders the answer for a report line. Undetermined prints as
// "unknown" rather than as its cause: the cause is a reason, not a state, and
// putting it in the state slot is how a non-answer starts looking like one.
func (a ProbeAnswer) String() string {
	switch a.kind {
	case probeYes:
		return "yes"
	case probeNo:
		return "no"
	case probeNotFound:
		return "not-found"
	default:
		return "unknown"
	}
}
