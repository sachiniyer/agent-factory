package proposal

import (
	"time"
)

// Delivery is what the daemon learned when it sent a push prompt.
type Delivery int

const (
	// DeliveryConfirmed means the agent's pane shows the prompt submitted.
	DeliveryConfirmed Delivery = iota
	// DeliveryUnconfirmed means the prompt may have landed, but af could not
	// see it land.
	DeliveryUnconfirmed
	// DeliveryFailed means the prompt provably did not reach the agent.
	DeliveryFailed
)

// PushTarget is what the daemon knows about one session on a poll tick or a
// push route.
type PushTarget struct {
	RepoID    string
	SessionID string
	// Modes maps each slug the session has a Docs tab for to that tab's push
	// mode.
	Modes map[string]PushMode
	// Ready means the session's liveness is Ready.
	Ready bool
	// Attached means a TUI is attached full-screen to the session.
	Attached bool
}

// PushStatus is the payload of a proposal.push event, and the reply of the
// push routes.
type PushStatus struct {
	SessionID string    `json:"session_id"`
	Slug      string    `json:"slug"`
	Mode      PushMode  `json:"mode"`
	State     PushState `json:"state"`
	DueAt     *string   `json:"due_at"`
	// DueInMs is the time from when the daemon built this status to DueAt.
	// Clients count down from it, so a skewed client clock does not matter.
	DueInMs *int64 `json:"due_in_ms"`
	Unsent  int    `json:"unsent"`
}

// pushUnsentMaxAge is how long a cached unsent count is used when no commit
// through the daemon marked it stale.
const pushUnsentMaxAge = 30 * time.Second

type pushEntry struct {
	repoID, slug  string
	mode          PushMode
	hasTab        bool
	ready         bool
	attached      bool
	lastSave      time.Time
	held          bool
	sendNow       bool
	sending       bool
	sent          bool
	unconfirmedAt time.Time
	notBefore     time.Time
	unsent        int
	unsentAt      time.Time
	last          *PushStatus
}

func (e *pushEntry) input() PushInput {
	in := PushInput{
		Mode: e.mode, LastSave: e.lastSave, Held: e.held, SendNow: e.sendNow,
		Ready: e.ready, Attached: e.attached, Unsent: e.unsent, Sending: e.sending,
		UnconfirmedAt: e.unconfirmedAt, NotBefore: e.notBefore, Sent: e.sent,
	}
	if !e.hasTab {
		in.LastSave = time.Time{}
	}
	return in
}

func (e *pushEntry) apply(t PushTarget) {
	mode, ok := t.Modes[e.slug]
	if !ok || mode == "" {
		mode = PushAuto
	}
	e.mode, e.hasTab = mode, ok
	e.ready, e.attached = t.Ready, t.Attached
	if !t.Ready && !e.sending {
		e.unconfirmedAt = time.Time{}
	}
}

func (s *Service) status(sessionID string, e *pushEntry, now time.Time) PushStatus {
	d := DecidePush(e.input(), now)
	st := PushStatus{SessionID: sessionID, Slug: e.slug, Mode: e.mode, State: d.State, Unsent: e.unsent}
	if !d.DueAt.IsZero() {
		due := d.DueAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")
		st.DueAt = &due
		in := max(d.DueAt.Sub(now).Milliseconds(), 0)
		st.DueInMs = &in
	}
	return st
}

// sameStatus ignores DueInMs, which changes with every call.
func sameStatus(a *PushStatus, b PushStatus) bool {
	if a == nil || a.SessionID != b.SessionID || a.Slug != b.Slug || a.Mode != b.Mode || a.State != b.State || a.Unsent != b.Unsent {
		return false
	}
	if (a.DueAt == nil) != (b.DueAt == nil) {
		return false
	}
	return a.DueAt == nil || *a.DueAt == *b.DueAt
}

// changedLocked returns the status of the entry, and whether it differs from
// the last status published for it.
func (s *Service) changedLocked(sessionID string, e *pushEntry, now time.Time) (PushStatus, bool) {
	st := s.status(sessionID, e, now)
	if sameStatus(e.last, st) {
		return st, false
	}
	e.last = &st
	return st, true
}

func (s *Service) entryLocked(sessionID, repoID, slug string) *pushEntry {
	e := s.push[sessionID]
	if e == nil || e.repoID != repoID || e.slug != slug {
		e = &pushEntry{repoID: repoID, slug: slug, mode: PushAuto}
		s.push[sessionID] = e
	}
	return e
}

func (s *Service) refreshUnsentLocked(store *Store, sessionID string, e *pushEntry, now time.Time) {
	if !e.unsentAt.IsZero() && now.Sub(e.unsentAt) < pushUnsentMaxAge {
		return
	}
	slug, n, err := store.Unseen(sessionID)
	if err != nil || slug != e.slug {
		n = 0
	}
	e.unsent, e.unsentAt = n, now
}

func (s *Service) publishPush(statuses []PushStatus) {
	for _, st := range statuses {
		s.host.PublishPush(st)
	}
}

// PushTick runs the push rules for every session the daemon knows. The poll
// loop calls it on each tick. A send runs in the background, so a slow
// delivery does not hold the poll loop.
func (s *Service) PushTick(targets []PushTarget) {
	now := s.now()
	var publish []PushStatus
	var sends []string
	s.mu.Lock()
	live := map[string]bool{}
	for _, t := range targets {
		if t.SessionID == "" {
			continue
		}
		store, err := Open(t.RepoID)
		if err != nil || !store.initialized() {
			continue
		}
		slug := s.attachmentsLocked(store, t.RepoID)[t.SessionID]
		if slug == "" {
			continue
		}
		live[t.SessionID] = true
		e := s.entryLocked(t.SessionID, t.RepoID, slug)
		e.apply(t)
		s.refreshUnsentLocked(store, t.SessionID, e, now)
		if DecidePush(e.input(), now).Send {
			e.sending = true
			sends = append(sends, t.SessionID)
		}
		if st, changed := s.changedLocked(t.SessionID, e, now); changed {
			publish = append(publish, st)
		}
	}
	for id, e := range s.push {
		if !live[id] && !e.sending {
			delete(s.push, id)
		}
	}
	s.mu.Unlock()
	s.publishPush(publish)
	for _, id := range sends {
		s.spawn(func() { _ = s.runPush(id) })
	}
}

// runPush sends the session its unseen edits. The caller has set sending on
// the session's entry.
func (s *Service) runPush(sessionID string) error {
	s.mu.Lock()
	var savedAt time.Time
	if e := s.push[sessionID]; e != nil {
		savedAt = e.lastSave
		e.lastSave, e.sendNow = time.Time{}, false
	}
	s.mu.Unlock()

	delivery, sendErr := DeliveryFailed, error(nil)
	empty := false
	r, err := s.sessionTarget(sessionID)
	if err == nil {
		var res *Changes
		if res, err = r.store.Changes(r.session.ID); err == nil {
			if len(res.Edits) == 0 {
				empty = true
			} else {
				prompt := PushPrompt(res.Slug, r.store.SlugPath(res.Slug), res.Edits)
				delivery, sendErr = s.host.SendPrompt(r.session.ID, prompt)
				if delivery == DeliveryFailed {
					if rerr := r.store.RewindCursor(r.session.ID, res.From, res.To); rerr != nil && sendErr == nil {
						sendErr = rerr
					}
				}
			}
		}
	}
	if err != nil {
		sendErr = err
	}

	now := s.now()
	s.mu.Lock()
	e := s.push[sessionID]
	var publish []PushStatus
	if e != nil {
		e.sending = false
		e.unsentAt = time.Time{}
		switch {
		case empty:
		case err == nil && delivery == DeliveryConfirmed:
			e.sent, e.held, e.unconfirmedAt = true, false, time.Time{}
		case err == nil && delivery == DeliveryUnconfirmed:
			e.sent, e.held, e.unconfirmedAt = true, false, now
		default:
			if e.lastSave.IsZero() {
				e.lastSave = savedAt
			}
			e.notBefore = now.Add(PushQuiet)
		}
		if r != nil {
			s.refreshUnsentLocked(r.store, sessionID, e, now)
		}
		if st, changed := s.changedLocked(sessionID, e, now); changed {
			publish = append(publish, st)
		}
	}
	s.mu.Unlock()
	s.publishPush(publish)
	return sendErr
}

// noteHumanSave records a human save of slug for every session attached to it,
// releases their holds, and restarts their wait.
func (s *Service) noteHumanSave(r *resolved, slug string) {
	ids, err := s.attachedTo(r, slug)
	if err != nil {
		return
	}
	now := s.now()
	var publish []PushStatus
	s.mu.Lock()
	for _, id := range ids {
		e := s.entryLocked(id, r.repoID, slug)
		e.lastSave, e.held, e.sent, e.notBefore = now, false, false, time.Time{}
		e.unsentAt = time.Time{}
		s.refreshUnsentLocked(r.store, id, e, now)
		if !e.hasTab {
			continue
		}
		if st, changed := s.changedLocked(id, e, now); changed {
			publish = append(publish, st)
		}
	}
	s.mu.Unlock()
	s.publishPush(publish)
}

// markUnsentStale makes the next tick count the unsent edits again for each
// session of the project that pushes slug, or every slug when slug is "".
func (s *Service) markUnsentStale(repoID, slug, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.push {
		if (sessionID != "" && id == sessionID) || (sessionID == "" && e.repoID == repoID && (slug == "" || e.slug == slug)) {
			e.unsentAt = time.Time{}
		}
	}
}

// pushRoute resolves the session of a push route, applies the daemon's view of
// it, and runs fn on its entry under the lock. fn returns true to send now.
func (s *Service) pushRoute(t PushTarget, fn func(e *pushEntry) bool) (*PushStatus, error) {
	r, err := s.sessionTarget(t.SessionID)
	if err != nil {
		return nil, err
	}
	slug, err := r.store.Attached(r.session.ID)
	if err != nil {
		return nil, err
	}
	if slug == "" {
		return nil, s.notAttached(r)
	}
	id := r.session.ID
	now := s.now()
	s.mu.Lock()
	e := s.entryLocked(id, r.repoID, slug)
	e.apply(t)
	s.refreshUnsentLocked(r.store, id, e, now)
	send := fn(e) && DecidePush(e.input(), now).Send
	if send {
		e.sending = true
	}
	st, changed := s.changedLocked(id, e, now)
	s.mu.Unlock()
	if changed {
		s.publishPush([]PushStatus{st})
	}
	if !send {
		return &st, nil
	}
	sendErr := s.runPush(id)
	s.mu.Lock()
	if e := s.push[id]; e != nil {
		st = s.status(id, e, s.now())
	}
	s.mu.Unlock()
	return &st, sendErr
}

// PushStatusFor returns the push status of the session for slug, or nil when
// the session is not attached to slug.
func (s *Service) PushStatusFor(t PushTarget, slug string) (*PushStatus, error) {
	r, err := s.sessionTarget(t.SessionID)
	if err != nil {
		return nil, err
	}
	attached, err := r.store.Attached(r.session.ID)
	if err != nil || attached != slug {
		return nil, err
	}
	return s.pushRoute(t, func(*pushEntry) bool { return false })
}

// SetPushMode records the session's push mode. The daemon stores the mode on
// the session's Docs tabs before it calls this.
func (s *Service) SetPushMode(t PushTarget, mode PushMode) (*PushStatus, error) {
	return s.pushRoute(t, func(e *pushEntry) bool {
		e.mode = mode
		return false
	})
}

// HoldPush holds the session's pending push until the next human save.
func (s *Service) HoldPush(t PushTarget) (*PushStatus, error) {
	return s.pushRoute(t, func(e *pushEntry) bool {
		e.held, e.sendNow = true, false
		return false
	})
}

// SendNow sends the session's unsent edits at once when the agent is Ready,
// or on the first tick where it is.
func (s *Service) SendNow(t PushTarget) (*PushStatus, error) {
	return s.pushRoute(t, func(e *pushEntry) bool {
		e.held = false
		e.sendNow = e.unsent > 0
		return true
	})
}

// AttachedSlug returns the proposal the session is attached to, or
// ErrNotAttached.
func (s *Service) AttachedSlug(sessionID string) (string, error) {
	r, err := s.sessionTarget(sessionID)
	if err != nil {
		return "", err
	}
	slug, err := r.store.Attached(r.session.ID)
	if err != nil {
		return "", err
	}
	if slug == "" {
		return "", s.notAttached(r)
	}
	return slug, nil
}
