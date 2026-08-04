package session

import "strings"

// Root-agent re-create context (#2629).
//
// Healing a root replaces its session record rather than re-spawning into it,
// and #2616 made that carry the recorded conversation across so the root
// normally comes back where it was. What it could not do is make the FALLBACK
// visible: a root that genuinely could not resume — the configured program runs
// a different agent, it pins its own resume flag, the provider no longer has
// that conversation, or there was never a durable id to carry — comes back
// Ready, with an identical rail row, and the only trace is a line in
// agent-factory.log. That invisibility was the sharpest point of #2616: eight
// silent re-creates over three and a half weeks, found because someone went
// looking.
//
// So the outcome is recorded ON the replacement record and rendered as a short
// note on its row, in every rail. It is one-shot: the daemon clears it the first
// time a client opens the session's pane, which is the moment the user would
// have found out anyway.

// RootRecreateContext is what a re-created root agent's conversation carry
// actually did, in the vocabulary a rail row can render. It is deliberately
// three-valued rather than a "started fresh" bool, because the middle case is
// real and af must not claim to know it: a root whose resolved command selects
// its own conversation (`codex resume --last`, a program that pins its own
// resume flag) records nothing, so whether continuity survived is genuinely
// unknown. Rendering that as "fresh context" would be a confident answer to a
// question nobody asked af.
//
// Persisted as a string so an unrecognized value written by a newer binary
// degrades to no note at all rather than to a wrong one (the rollforward
// precedent Liveness and TabKind follow).
type RootRecreateContext string

const (
	// RootRecreateContextNone is every ordinary session, and a re-created root
	// that came back on exactly the conversation it had. Nothing to report.
	RootRecreateContextNone RootRecreateContext = ""
	// RootRecreateContextFresh means the root is demonstrably NOT on its prior
	// conversation: it had none to carry, or the launch committed a different
	// one. Its context is gone, and the user's next prompt lands on an agent
	// with no memory of what it was doing.
	RootRecreateContextFresh RootRecreateContext = "fresh"
	// RootRecreateContextUnknown means the replacement recorded no conversation
	// at all, so af cannot say whether the agent continued or started over. The
	// user still needs to know not to assume continuity — which is the same
	// action the fresh case calls for, arrived at honestly.
	RootRecreateContextUnknown RootRecreateContext = "unknown"
)

// ClassifyRootRecreateContext decides what a root heal's conversation carry did,
// from the conversation the reaped record held, the one the replacement actually
// came up with, and the agent the replacement actually launched.
//
// It reads the conversation the new record CARRIES rather than what the create
// was asked to do: a create can be handed a conversation and still come up on a
// different one, and the record is the thing that will be resumed from next
// time.
//
// launchedAgent is what disambiguates the one genuinely ambiguous outcome. A
// replacement that recorded NO conversation reaches that state two ways, and
// they deserve opposite answers:
//
//   - The launch runs a DIFFERENT agent than the carried conversation belongs to
//     (a claude root re-created as codex, because the root program was repointed).
//     ResumeProgramWithConversationID refuses on the agent mismatch, and the
//     launch starts that agent's own new conversation — so the carried one is
//     provably not resumed, whether or not the new id is captured synchronously.
//     codex ids are discovered asynchronously, so this case ALWAYS arrives here
//     with no recorded conversation; reading it as "unknown" would hide the
//     documented agent-change fallback behind the one word that means af cannot
//     tell.
//   - The launch runs the SAME agent but recorded nothing, which happens only
//     when the resolved command pins its own conversation selection (`claude
//     --continue`, `codex resume --last`) and both the resume rewrite and the
//     fresh-id injection therefore decline. There the agent may well have
//     continued something; af genuinely cannot say.
//
// This is the single authority for that judgment — the daemon's log line and
// the note on the row both come from here — so the log and the rail can never
// disagree about whether a root resumed.
func ClassifyRootRecreateContext(carried AgentConversationData, created *AgentConversationData, launchedAgent string) RootRecreateContext {
	switch {
	case !carried.HasID():
		// Nothing was recorded to carry, so nothing was resumed. The root really is
		// on a fresh context; that the loss predates the outage does not soften it.
		return RootRecreateContextFresh
	case created != nil && created.Agent == carried.Agent && created.ID == carried.ID:
		return RootRecreateContextNone
	case created == nil && sameAgent(launchedAgent, carried.Agent):
		return RootRecreateContextUnknown
	default:
		return RootRecreateContextFresh
	}
}

// severity orders the outcomes by how much they claim was lost, so a pending
// notice can be escalated across a second heal but never quietly downgraded.
// None is "nothing to report", Unknown is "af cannot tell", Fresh is "provably
// gone" — and a proven loss must outrank an unproven one.
func (c RootRecreateContext) severity() int {
	switch c {
	case RootRecreateContextFresh:
		return 2
	case RootRecreateContextUnknown:
		return 1
	default:
		// Includes any value this binary does not know. It renders nothing, so it
		// must not be able to outrank a notice this binary would actually show.
		return 0
	}
}

// moreSevereRecreateContext returns whichever of two outcomes claims more loss.
func moreSevereRecreateContext(a, b RootRecreateContext) RootRecreateContext {
	if b.severity() > a.severity() {
		return b
	}
	return a
}

// sameAgent reports whether the launch came up on the provider the carried
// conversation belongs to. An UNKNOWN launched agent ("" — no tmux binding, or a
// command no matcher recognizes) is deliberately not treated as a match: the
// unknown-continuity verdict has to be earned by positively identifying the same
// provider, not fall out of a question that could not be answered.
func sameAgent(launchedAgent, carriedAgent string) bool {
	launched := strings.TrimSpace(launchedAgent)
	return launched != "" && launched == strings.TrimSpace(carriedAgent)
}

// Note returns the short note a rail row renders for this outcome, or "" when
// there is nothing to say. Sentence case, static, no animation — the copy rules
// every user-facing surface follows. It lives here, next to the values, so the
// TUI rail, the web rail, and `af sessions get` cannot render three different
// words for one fact; an unrecognized value renders nothing.
func (c RootRecreateContext) Note() string {
	switch c {
	case RootRecreateContextFresh:
		return "fresh context"
	case RootRecreateContextUnknown:
		return "context unknown"
	default:
		return ""
	}
}

// RootRecreateContext reports the note-worthy outcome of this session's
// re-create, if it was one and nobody has looked at it yet.
func (i *Instance) RootRecreateContext() RootRecreateContext {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.rootRecreateContext
}

// AcknowledgeRootRecreateContext clears the one-shot marker and reports whether
// it was set, so the caller persists and announces exactly once. Seeing the
// session's pane IS the acknowledgement: the note exists to tell a user
// something they would otherwise learn only by reading the log, and once they
// are looking at the agent it has done its job.
//
// It clears only a value THIS binary actually renders (Note() != ""). A record
// written by a newer daemon can carry an outcome this version does not know: it
// renders nothing for it, deliberately, and a value nobody was ever shown has
// not been acknowledged by anybody. Clearing it would let an older binary erase
// roll-forward state on a stream open — silently destroying the notice a newer
// daemon was going to display. Leaving it is the safe direction: the binary that
// understands it is the one that gets to clear it.
func (i *Instance) AcknowledgeRootRecreateContext() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.rootRecreateContext.Note() == "" {
		return false
	}
	i.rootRecreateContext = RootRecreateContextNone
	return true
}

// ReconcileRootRecreateContext mirrors the daemon's authoritative value onto a
// client's projection of this row, reporting whether anything changed.
//
// It is applied unconditionally in BOTH directions, unlike a monotonic marker
// like the kill tombstone: the note appears when the daemon heals a root and
// disappears when any client acknowledges it, and a projection that could only
// adopt it would leave the note burned onto every other open rail forever.
func (i *Instance) ReconcileRootRecreateContext(ctx RootRecreateContext) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.rootRecreateContext == ctx {
		return false
	}
	i.rootRecreateContext = ctx
	return true
}

// NoteRecreateContext classifies the launch this instance just completed and
// records the resulting one-shot marker.
//
// The CALLER decides that this create is a heal — only the daemon knows it just
// reaped a record, and it is the one fact the carried conversation cannot
// supply: a heal whose reaped root recorded no conversation carries nothing at
// all, and that is exactly the heal whose root comes back with no history. An
// ordinary create must never be asked, because starting fresh is what an
// ordinary create IS, and marking it would be noise on every new session.
//
// Called once the launch has settled, because the conversation committed to
// Tabs[0] is the answer: it is what the record will hold and what a future
// recovery resumes from. Calling it before the create's own persist is what
// keeps the marker in the SAME projection the create writes and publishes,
// rather than a second write racing the first client to render the row.
func (i *Instance) NoteRecreateContext() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.noteRecreateContextLocked()
}

// RefreshRecreateContext re-runs the classification against evidence that
// arrived AFTER the launch settled — the async provider capture — and reports
// whether the note changed.
//
// It exists because a root whose command pins its own conversation selection
// (`codex resume --last`) records nothing synchronously, so the launch can only
// answer "unknown". Minutes later the capture goroutine discovers the id the
// agent actually came up on, and if that is the carried conversation, af has
// PROVEN the continuity it could not see before. Leaving the row warning
// `context unknown` after that is a stale warning, and a stale warning is how a
// real one stops being read.
//
// Gated on a notice still being pending, which is what keeps this from
// resurrecting one the user already acknowledged: acknowledgement is final, and
// later evidence about a launch nobody is being warned about changes nothing.
// It cannot silently downgrade a notice inherited from an EARLIER heal either —
// noteRecreateContextLocked floors every result at that carried notice.
func (i *Instance) RefreshRecreateContext() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.rootRecreateContext == RootRecreateContextNone {
		return false
	}
	before := i.rootRecreateContext
	i.noteRecreateContextLocked()
	return i.rootRecreateContext != before
}

// noteRecreateContextLocked is the shared body. Caller holds i.mu for writing.
//
// The result is FLOORED at any notice inherited from an earlier heal. A root can
// lose its history and then have tmux die again before anyone opens the pane;
// the second heal's inputs describe the second replacement and answer a
// different question, so classifying it alone would let a clean second heal
// erase an unacknowledged warning about the first — putting the loss back out of
// sight, which is the whole bug. A pending notice is cleared by acknowledgement
// and by nothing else; a later heal can only make it worse.
func (i *Instance) noteRecreateContextLocked() {
	// A carried notice this binary cannot READ is kept verbatim. severity() has to
	// score an unrecognized value as 0 — it must never outrank a note this binary
	// would actually show — but that score would then let a locally-classified
	// outcome overwrite a newer daemon's pending state, destroying roll-forward
	// data nobody acknowledged. Claims that cannot be compared are not compared:
	// the binary that understands a value is the one that gets to replace it.
	//
	// The cost is real and deliberate: while running the older binary the rail
	// shows no note for that root, because it has no words for the value it is
	// holding. The heal outcome still reaches the application log, and the note
	// returns the moment the newer daemon is back.
	if i.carriedRecreateNotice != RootRecreateContextNone && i.carriedRecreateNotice.Note() == "" {
		i.rootRecreateContext = i.carriedRecreateNotice
		return
	}
	var created *AgentConversationData
	if len(i.Tabs) > 0 {
		created = conversationDataPtr(i.Tabs[0].Conversation)
	}
	// currentAgentNameLocked reads the LAUNCHED command off the bound tmux
	// session, so a root whose program was repointed to another agent is
	// identified as that agent — which is what separates the provable
	// agent-change fallback from a command that picked its own conversation.
	classified := ClassifyRootRecreateContext(i.carriedConversation, created, i.currentAgentNameLocked())
	i.rootRecreateContext = moreSevereRecreateContext(i.carriedRecreateNotice, classified)
}
