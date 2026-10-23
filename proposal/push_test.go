package proposal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var pushT0 = time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC)

func TestDecidePush(t *testing.T) {
	save := pushT0
	base := PushInput{Mode: PushAuto, LastSave: save, Ready: true, Unsent: 1}
	cases := []struct {
		name  string
		edit  func(*PushInput)
		at    time.Duration
		state PushState
		send  bool
		due   bool
	}{
		{"waits 10 s after the save", nil, 9 * time.Second, PushCountdown, false, true},
		{"sends 10 s after the save", nil, PushQuiet, PushWaitingAgent, true, true},
		{"waits for a busy agent", func(in *PushInput) { in.Ready = false }, time.Minute, PushWaitingAgent, false, true},
		{"waits while a TUI is attached", func(in *PushInput) { in.Attached = true }, time.Minute, PushWaitingAgent, false, true},
		{"held", func(in *PushInput) { in.Held = true }, time.Minute, PushHeld, false, false},
		{"nothing unsent", func(in *PushInput) { in.Unsent = 0 }, time.Minute, PushIdle, false, false},
		{"sent and nothing unsent", func(in *PushInput) { in.Unsent, in.Sent = 0, true }, time.Minute, PushSent, false, false},
		{"no human save", func(in *PushInput) { in.LastSave = time.Time{} }, time.Minute, PushIdle, false, false},
		{"manual mode", func(in *PushInput) { in.Mode = PushManual }, time.Minute, PushIdle, false, false},
		{"manual send now", func(in *PushInput) { in.Mode, in.SendNow = PushManual, true }, 0, PushWaitingAgent, true, false},
		{"send now in the countdown", func(in *PushInput) { in.SendNow = true }, time.Second, PushWaitingAgent, true, true},
		{"send now overrides a hold", func(in *PushInput) { in.SendNow, in.Held = true, true }, 0, PushWaitingAgent, true, true},
		{"send now waits for a busy agent", func(in *PushInput) { in.SendNow, in.Ready = true, false }, 0, PushWaitingAgent, false, true},
		{"send now ignores an attached TUI", func(in *PushInput) { in.SendNow, in.Attached = true, true }, 0, PushWaitingAgent, true, true},
		{"a delivery in flight blocks", func(in *PushInput) { in.Sending = true }, time.Minute, PushWaitingAgent, false, true},
		{"an unconfirmed delivery blocks", func(in *PushInput) { in.UnconfirmedAt = save.Add(30 * time.Second) }, time.Minute, PushWaitingAgent, false, true},
		{"an unconfirmed delivery blocks send now", func(in *PushInput) { in.SendNow, in.UnconfirmedAt = true, save }, 30 * time.Second, PushWaitingAgent, false, true},
		{"an unconfirmed delivery times out", func(in *PushInput) { in.UnconfirmedAt = save }, PushUnconfirmedTimeout, PushWaitingAgent, true, true},
		{"a failed delivery retries later", func(in *PushInput) { in.NotBefore = save.Add(20 * time.Second) }, 15 * time.Second, PushWaitingAgent, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			if tc.edit != nil {
				tc.edit(&in)
			}
			got := DecidePush(in, save.Add(tc.at))
			if got.State != tc.state || got.Send != tc.send || got.DueAt.IsZero() == tc.due {
				t.Fatalf("got %+v, want state %s send %v due %v", got, tc.state, tc.send, tc.due)
			}
			if tc.due && !got.DueAt.Equal(save.Add(PushQuiet)) {
				t.Fatalf("due at %v, want %v", got.DueAt, save.Add(PushQuiet))
			}
		})
	}
}

func TestParsePushMode(t *testing.T) {
	for in, want := range map[string]PushMode{"": PushAuto, "auto": PushAuto, "manual": PushManual} {
		if got, err := ParsePushMode(in); err != nil || got != want {
			t.Fatalf("ParsePushMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParsePushMode("Auto"); err == nil {
		t.Fatal("want an error for Auto")
	}
}

func TestPushPrompt(t *testing.T) {
	edits := []Edit{
		{Author: AuthorHuman, Files: []string{"goals.md"}, Patch: "@@ -1 +1 @@\n-a\n+b\n"},
		{Author: AuthorHuman, Files: []string{"api.md", "goals.md"}, Patch: "@@ -2 +2 @@\n-c\n+d"},
	}
	got := PushPrompt("auth", "/p/auth", edits)
	want := "Proposal auth: the user edited api.md, goals.md.\n" +
		"@@ -1 +1 @@\n-a\n+b\n@@ -2 +2 @@\n-c\n+d\n" + pushInstruction
	if got != want {
		t.Fatalf("prompt:\n%s\nwant:\n%s", got, want)
	}
}

func TestPushPromptLargeDiffNamesFilesOnly(t *testing.T) {
	edits := []Edit{{Author: AuthorHuman, Files: []string{"api.md"}, Patch: strings.Repeat("+line\n", PushPatchLimit/6+1)}}
	got := PushPrompt("auth", "/p/auth", edits)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", len(lines), got)
	}
	if lines[0] != "Proposal auth: the user edited api.md." || !strings.Contains(lines[1], "Read those files in /p/auth") || lines[2] != pushInstruction {
		t.Fatalf("prompt:\n%s", got)
	}
}

func TestPushPromptAttributesEachEdit(t *testing.T) {
	edits := []Edit{
		{Author: AuthorHuman, Files: []string{"goals.md"}, Patch: "+b\n"},
		{Author: AuthorAgent, Session: "other", Files: []string{"api.md"}, Patch: "+> [!todo] drop the cache\n"},
	}
	got := PushPrompt("auth", "/p/auth", edits)
	want := "Proposal auth: the user and another agent edited api.md, goals.md.\n" +
		"[edit by the user]\n+b\n" +
		"[edit by another agent]\n+> [!todo] drop the cache\n" + pushInstruction
	if got != want {
		t.Fatalf("prompt:\n%s\nwant:\n%s", got, want)
	}
}

type pushRig struct {
	t     *testing.T
	svc   *Service
	host  *fakeHost
	clock time.Time
	base  string
	ready bool
	mode  PushMode
}

func newPushRig(t *testing.T) *pushRig {
	t.Helper()
	svc, host, _ := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	rig := &pushRig{t: t, svc: svc, host: host, clock: pushT0, ready: true, mode: PushAuto}
	svc.now = func() time.Time { return rig.clock }
	svc.spawn = func(f func()) { f() }
	rig.base = readFor(t, svc, "goals").Commit
	rig.tick()
	return rig
}

func (r *pushRig) target() PushTarget {
	return PushTarget{RepoID: testRepoID, SessionID: sessA, Modes: map[string]PushMode{"p": r.mode}, Ready: r.ready}
}

func (r *pushRig) save(text string) {
	r.t.Helper()
	res, err := r.svc.Save(Target{SessionID: sessA}, "p", "goals", text, r.base)
	if err != nil {
		r.t.Fatal(err)
	}
	r.base = res.Commit
}

func (r *pushRig) tick() { r.svc.PushTick([]PushTarget{r.target()}) }

func (r *pushRig) advance(d time.Duration) {
	r.clock = r.clock.Add(d)
	r.tick()
}

func (r *pushRig) state() PushStatus {
	r.t.Helper()
	st, err := r.svc.PushStatusFor(r.target(), "p")
	if err != nil || st == nil {
		r.t.Fatalf("status: %v, %v", st, err)
	}
	return *st
}

func (r *pushRig) prompts() []string {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	return append([]string(nil), r.host.prompts...)
}

func (r *pushRig) expect(state PushState, prompts int) {
	r.t.Helper()
	st := r.state()
	if st.State != state || len(r.prompts()) != prompts {
		r.t.Fatalf("state %s with %d prompts, want %s with %d (status %+v)", st.State, len(r.prompts()), state, prompts, st)
	}
}

func TestPushAutoSendsAfterTheQuietPeriod(t *testing.T) {
	r := newPushRig(t)
	r.expect(PushIdle, 0)
	r.save("human edit\n")
	st := r.state()
	if st.State != PushCountdown || st.Unsent != 1 || st.DueAt == nil || *st.DueAt != "2026-10-20T22:00:10.000Z" {
		t.Fatalf("after save: %+v", st)
	}
	if st.DueInMs == nil || *st.DueInMs != 10000 {
		t.Fatalf("due_in_ms after save: %v", st.DueInMs)
	}
	r.advance(9 * time.Second)
	r.expect(PushCountdown, 0)
	if st := r.state(); st.DueInMs == nil || *st.DueInMs != 1000 {
		t.Fatalf("due_in_ms 9 s after save: %v", st.DueInMs)
	}
	r.advance(time.Second)
	r.expect(PushSent, 1)
	if p := r.prompts()[0]; !strings.HasPrefix(p, "Proposal p: the user edited goals.md.\n") || !strings.Contains(p, "+human edit") {
		t.Fatalf("prompt:\n%s", p)
	}
	ch, err := r.svc.Changes(sessA)
	if err != nil || len(ch.Edits) != 0 {
		t.Fatalf("the push must move the cursor like a pull: %+v, %v", ch, err)
	}
	r.advance(time.Minute)
	r.expect(PushSent, 1)
}

func TestPushWaitsForAReadyAgent(t *testing.T) {
	r := newPushRig(t)
	r.ready = false
	r.save("edit\n")
	r.advance(PushQuiet + time.Second)
	r.expect(PushWaitingAgent, 0)
	if st := r.state(); st.DueInMs == nil || *st.DueInMs != 0 {
		t.Fatalf("due_in_ms past due: %v", st.DueInMs)
	}
	r.ready = true
	r.advance(time.Second)
	r.expect(PushSent, 1)
}

func TestPushHoldAndRelease(t *testing.T) {
	r := newPushRig(t)
	r.save("one\n")
	if st, err := r.svc.HoldPush(r.target()); err != nil || st.State != PushHeld {
		t.Fatalf("hold: %+v, %v", st, err)
	}
	r.advance(time.Minute)
	r.expect(PushHeld, 0)
	r.save("two\n")
	r.expect(PushCountdown, 0)
	r.advance(PushQuiet - time.Second)
	r.expect(PushCountdown, 0)
	r.advance(time.Second)
	r.expect(PushSent, 1)
}

func TestPushSendNowWhileBusyQueues(t *testing.T) {
	r := newPushRig(t)
	r.mode = PushManual
	r.save("edit\n")
	r.advance(time.Minute)
	r.expect(PushIdle, 0)
	r.ready = false
	st, err := r.svc.SendNow(r.target())
	if err != nil || st.State != PushWaitingAgent {
		t.Fatalf("send now while busy: %+v, %v", st, err)
	}
	r.advance(time.Second)
	r.expect(PushWaitingAgent, 0)
	r.ready = true
	r.advance(time.Second)
	r.expect(PushSent, 1)
}

func TestPushSendNowWhenReadySendsBeforeItReplies(t *testing.T) {
	r := newPushRig(t)
	r.save("edit\n")
	st, err := r.svc.SendNow(r.target())
	if err != nil || st.State != PushSent || st.Unsent != 0 || len(r.prompts()) != 1 {
		t.Fatalf("send now: %+v, %v, %d prompts", st, err, len(r.prompts()))
	}
	st, err = r.svc.SendNow(r.target())
	if err != nil || st.Unsent != 0 || len(r.prompts()) != 1 {
		t.Fatalf("send now with nothing unsent: %+v, %v", st, err)
	}
}

func TestPushUnconfirmedDeliveryBlocksTheNextPush(t *testing.T) {
	r := newPushRig(t)
	r.host.delivery = DeliveryUnconfirmed
	r.save("one\n")
	r.advance(PushQuiet)
	r.expect(PushSent, 1)
	r.host.delivery = DeliveryConfirmed
	r.save("two\n")
	r.advance(PushQuiet)
	r.expect(PushWaitingAgent, 1)
	if _, err := r.svc.SendNow(r.target()); err != nil {
		t.Fatal(err)
	}
	r.expect(PushWaitingAgent, 1)
	r.ready = false
	r.advance(time.Second)
	r.expect(PushWaitingAgent, 1)
	r.ready = true
	r.advance(time.Second)
	r.expect(PushSent, 2)
}

func TestPushUnconfirmedDeliveryTimesOut(t *testing.T) {
	r := newPushRig(t)
	r.host.delivery = DeliveryUnconfirmed
	r.save("one\n")
	r.advance(PushQuiet)
	r.save("two\n")
	r.advance(PushQuiet)
	r.expect(PushWaitingAgent, 1)
	r.advance(PushUnconfirmedTimeout)
	r.expect(PushSent, 2)
}

func TestPushFailedDeliveryRewindsTheCursor(t *testing.T) {
	r := newPushRig(t)
	r.host.delivery, r.host.sendErr = DeliveryFailed, errors.New("prompt not delivered")
	r.save("edit\n")
	r.advance(PushQuiet)
	st := r.state()
	if st.State != PushWaitingAgent || st.Unsent != 1 || len(r.prompts()) != 1 {
		t.Fatalf("after a failed delivery: %+v, %d prompts", st, len(r.prompts()))
	}
	r.advance(time.Second)
	r.expect(PushWaitingAgent, 1)
	r.host.delivery, r.host.sendErr = DeliveryConfirmed, nil
	r.advance(PushQuiet)
	r.expect(PushSent, 2)
	if !strings.Contains(r.prompts()[1], "+edit") {
		t.Fatalf("the retry must carry the same edit:\n%s", r.prompts()[1])
	}
}

func TestPushSendNowReportsAFailedDelivery(t *testing.T) {
	r := newPushRig(t)
	r.host.delivery, r.host.sendErr = DeliveryFailed, errors.New("prompt not delivered")
	r.save("edit\n")
	if _, err := r.svc.SendNow(r.target()); err == nil {
		t.Fatal("want the delivery error")
	}
	ch, err := r.svc.Changes(sessA)
	if err != nil || len(ch.Edits) != 1 {
		t.Fatalf("a failed send must leave the edit unseen: %+v, %v", ch, err)
	}
}

func TestPushPublishesOnlyChanges(t *testing.T) {
	r := newPushRig(t)
	r.host.mu.Lock()
	r.host.pushes = nil
	r.host.mu.Unlock()
	r.save("edit\n")
	for i := 0; i < 5; i++ {
		r.advance(time.Second)
	}
	r.host.mu.Lock()
	got := append([]PushStatus(nil), r.host.pushes...)
	r.host.mu.Unlock()
	if len(got) != 1 || got[0].State != PushCountdown || got[0].SessionID != sessA || got[0].Slug != "p" {
		t.Fatalf("pushes: %+v", got)
	}
	if got[0].DueInMs == nil || *got[0].DueInMs != 10000 {
		t.Fatalf("the event must carry due_in_ms from when it was built: %v", got[0].DueInMs)
	}
}

func TestPushModeAndDetach(t *testing.T) {
	r := newPushRig(t)
	r.mode = PushManual
	st, err := r.svc.SetPushMode(r.target(), PushManual)
	if err != nil || st.Mode != PushManual {
		t.Fatalf("set mode: %+v, %v", st, err)
	}
	r.save("edit\n")
	r.advance(time.Minute)
	r.expect(PushIdle, 0)
	if _, err := r.svc.Detach(sessA); err != nil {
		t.Fatal(err)
	}
	if st, err := r.svc.PushStatusFor(r.target(), "p"); err != nil || st != nil {
		t.Fatalf("detached status: %+v, %v", st, err)
	}
	if _, err := r.svc.SendNow(r.target()); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("send now when detached: %v", err)
	}
}

func TestPushNeedsADocsTabForAuto(t *testing.T) {
	r := newPushRig(t)
	r.save("edit\n")
	noTab := r.target()
	noTab.Modes = nil
	r.clock = r.clock.Add(time.Minute)
	r.svc.PushTick([]PushTarget{noTab})
	if n := len(r.prompts()); n != 0 {
		t.Fatalf("a session with no Docs tab got %d auto pushes", n)
	}
	if st, err := r.svc.SendNow(noTab); err != nil || st.State != PushSent {
		t.Fatalf("send now with no Docs tab: %+v, %v", st, err)
	}
}
