package proposal

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PushMode is the push setting of a Docs tab.
type PushMode string

const (
	PushAuto   PushMode = "auto"
	PushManual PushMode = "manual"
)

// ParsePushMode validates a push mode. An empty mode is auto, because a tab
// with no stored mode pushes automatically.
func ParsePushMode(mode string) (PushMode, error) {
	switch PushMode(mode) {
	case PushAuto, "":
		return PushAuto, nil
	case PushManual:
		return PushManual, nil
	}
	return "", fmt.Errorf("invalid push mode %q: use auto or manual", mode)
}

// PushState is the state the Docs tab shows in its push chip.
type PushState string

const (
	PushIdle         PushState = "idle"
	PushCountdown    PushState = "countdown"
	PushWaitingAgent PushState = "waiting_agent"
	PushHeld         PushState = "held"
	PushSent         PushState = "sent"
)

const (
	// PushQuiet is how long after the last human save an auto push waits.
	PushQuiet = 10 * time.Second
	// PushUnconfirmedTimeout bounds how long a delivery that af could not
	// confirm blocks the next push when the agent never shows as busy.
	PushUnconfirmedTimeout = 60 * time.Second
	// PushPatchLimit is the largest patch text a push prompt carries.
	PushPatchLimit = 8 << 10
)

// PushInput is what DecidePush needs to know about one session.
type PushInput struct {
	Mode PushMode
	// LastSave is the last human save that no push has sent yet; zero when
	// there is none.
	LastSave time.Time
	Held     bool
	SendNow  bool
	// Ready means the agent is idle and waits for input.
	Ready bool
	// Attached means a TUI is attached full-screen to the session, so the user
	// may be typing in its pane. It blocks an auto push, not Send now.
	Attached bool
	Unsent   int
	// Sending means a delivery is in flight.
	Sending bool
	// UnconfirmedAt is when a delivery that af could not confirm returned;
	// zero when the last delivery is confirmed.
	UnconfirmedAt time.Time
	// NotBefore delays a retry after a delivery that did not happen.
	NotBefore time.Time
	// Sent means the last push was delivered and no human save came after it.
	Sent bool
}

// PushDecision is the result of DecidePush.
type PushDecision struct {
	State PushState
	// DueAt is when an auto push is due; zero when none is.
	DueAt time.Time
	// Send means the daemon sends the unsent edits now.
	Send bool
}

// DecidePush applies the push rules (docs/dev/proposals/proposal-mode/api.md)
// to one session at time now.
func DecidePush(in PushInput, now time.Time) PushDecision {
	if in.Unsent <= 0 {
		if in.Sent {
			return PushDecision{State: PushSent}
		}
		return PushDecision{State: PushIdle}
	}
	var due time.Time
	if in.Mode != PushManual && !in.LastSave.IsZero() {
		due = in.LastSave.Add(PushQuiet)
	}
	unconfirmed := !in.UnconfirmedAt.IsZero() && now.Sub(in.UnconfirmedAt) < PushUnconfirmedTimeout
	blocked := in.Sending || unconfirmed || !in.Ready || now.Before(in.NotBefore)
	if in.SendNow {
		if blocked {
			return PushDecision{State: PushWaitingAgent, DueAt: due}
		}
		return PushDecision{State: PushWaitingAgent, DueAt: due, Send: true}
	}
	if in.Held {
		return PushDecision{State: PushHeld}
	}
	if due.IsZero() {
		return PushDecision{State: PushIdle}
	}
	if now.Before(due) {
		return PushDecision{State: PushCountdown, DueAt: due}
	}
	if blocked || in.Attached {
		return PushDecision{State: PushWaitingAgent, DueAt: due}
	}
	return PushDecision{State: PushWaitingAgent, DueAt: due, Send: true}
}

const pushInstruction = "Apply these edits to your understanding, answer new [!question] replies, do the [!todo] callouts the user added, then run af proposal checkpoint."

// editorOrder is the order editors are named in, with their names as the
// receiving agent sees them. Changes never holds the receiver's own commits.
var editorOrder = []struct {
	author Author
	name   string
}{
	{AuthorHuman, "the user"},
	{AuthorAgent, "another agent"},
	{AuthorExternal, "an editor outside af"},
}

func editorName(a Author) string {
	for _, e := range editorOrder {
		if e.author == a {
			return e.name
		}
	}
	return "an editor outside af"
}

// PushPrompt is the prompt a push sends: who changed which files, the patches,
// and the instruction line. When more than one kind of editor made the edits,
// each patch names its editor. When the patches are larger than PushPatchLimit
// it names the files only, and tells the agent to read them, because the
// cursor has already moved past these edits.
func PushPrompt(slug, path string, edits []Edit) string {
	seen := map[string]bool{}
	by := map[string]bool{}
	var files []string
	for _, e := range edits {
		by[editorName(e.Author)] = true
		for _, f := range e.Files {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	var names []string
	for _, e := range editorOrder {
		if by[e.name] {
			names = append(names, e.name)
		}
	}
	var patches strings.Builder
	for _, e := range edits {
		if e.Patch == "" {
			continue
		}
		if len(names) > 1 {
			fmt.Fprintf(&patches, "[edit by %s]\n", editorName(e.Author))
		}
		patches.WriteString(e.Patch)
		if !strings.HasSuffix(e.Patch, "\n") {
			patches.WriteString("\n")
		}
	}
	sort.Strings(files)
	var b strings.Builder
	fmt.Fprintf(&b, "Proposal %s: %s edited %s.\n", slug, joinNames(names), strings.Join(files, ", "))
	if patches.Len() > PushPatchLimit {
		fmt.Fprintf(&b, "The diff is too large to send. Read those files in %s; af proposal changes will not show these edits again.\n", path)
	} else {
		b.WriteString(patches.String())
	}
	b.WriteString(pushInstruction)
	return b.String()
}

func joinNames(names []string) string {
	if len(names) <= 1 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
