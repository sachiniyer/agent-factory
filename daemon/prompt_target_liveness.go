package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
)

func promptTargetLivenessError(title string, liveness session.Liveness) error {
	switch liveness {
	case session.LiveLost:
		return fmt.Errorf("target session %q is Lost; prompt not delivered; recover it first", title)
	case session.LiveDead:
		return fmt.Errorf("target session %q is Dead; prompt not delivered; recover it first", title)
	case session.LiveArchived:
		// Archived sessions have no live tmux to deliver into (#1529): without
		// this case the prompt falls through to a confusing backend error. Point
		// at the off-ramp, mirroring the TUI's interactiveGuard message. The
		// restore command embeds the title, so it goes through shellsuggest — a title
		// with spaces or shell metacharacters must not turn a copy-pasted
		// `af sessions restore ...` into the wrong target or a second command, and a
		// title beginning with "-" must not be parsed as a flag by af itself (#3432).
		return fmt.Errorf("target session %q is Archived; prompt not delivered; restore it first (%s)", title, shellsuggest.PositionalCommand("af", []string{"sessions", "restore"}, title))
	}
	return nil
}

// taskPromptTargetLivenessError extends the ordinary target guard only for an
// automated task delivery. LiveLimitReached is intentionally allowed by the
// manual helper above: typing into a limit or credits picker can be the only way
// for an operator to recover the lane. A scheduler has no such intent, and its
// keystrokes can cancel an agent's own timed auto-continue.
func taskPromptTargetLivenessError(title string, liveness session.Liveness, taskOrigin bool) error {
	if taskOrigin && liveness == session.LiveLimitReached {
		return fmt.Errorf("%w: target session %q is at a usage limit; prompt not delivered", errTargetLimitReached, title)
	}
	return promptTargetLivenessError(title, liveness)
}
