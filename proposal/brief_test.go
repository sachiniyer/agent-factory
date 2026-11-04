package proposal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The web asks the daemon to brief the agent itself: start and attach carry a
// deliver flag that pastes Rules(slug, path) through the same prompt path a
// push uses, once per attach, and only into a pane that can take it
// (#6755). The CLI passes false and keeps printing the rules instead.

// promptList snapshots the prompts the fake host recorded.
func promptList(h *fakeHost) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.prompts...)
}

// paneReady marks sessionID in the host able to take a prompt: a live agent
// that idles at its prompt and no client holding the pane.
func paneReady(h *fakeHost, sessionID string) {
	s := h.sessions[sessionID]
	s.Ready, s.Paused, s.NoAgent = true, false, false
	h.sessions[sessionID] = s
}

func paneBusy(h *fakeHost, sessionID string) {
	s := h.sessions[sessionID]
	s.Ready = false
	h.sessions[sessionID] = s
}

func paneHeld(h *fakeHost, sessionID string) {
	s := h.sessions[sessionID]
	s.Ready, s.Paused = true, true
	h.sessions[sessionID] = s
}

func paneDead(h *fakeHost, sessionID string) {
	s := h.sessions[sessionID]
	s.Ready, s.NoAgent = false, true
	h.sessions[sessionID] = s
}

func briefTick(svc *Service, sessionID string, ready, attached bool) {
	svc.PushTick([]PushTarget{{RepoID: testRepoID, SessionID: sessionID, Ready: ready, Attached: attached}})
}

func TestServiceBriefOnWebStart(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)

	info, err := svc.Start(sessA, "p", []string{"goals"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "delivered" {
		t.Fatalf("rules_delivery = %q (rules_error %q), want delivered", info.RulesDelivery, info.RulesError)
	}
	prompts := promptList(host)
	if len(prompts) != 1 {
		t.Fatalf("%d prompts, want the contract delivered once", len(prompts))
	}
	for _, want := range []string{"proposal p", info.Path, "af proposal changes", "af proposal checkpoint", "[!question]", "[!todo]", "[!decision]"} {
		if !strings.Contains(prompts[0], want) {
			t.Fatalf("the contract prompt misses %q:\n%s", want, prompts[0])
		}
	}
}

func TestServiceBriefRepeatAttachDoesNotResend(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)

	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	// Attaching again to the same proposal — the web's resync, a retry, a
	// double click — must not paste the contract a second time.
	again, err := svc.Attach(sessA, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if again.RulesDelivery != "delivered" {
		t.Fatalf("repeat attach: rules_delivery = %q, want delivered", again.RulesDelivery)
	}
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("repeat attach left %d prompts, want 1", n)
	}
	// A later poll tick must not redeliver it either.
	briefTick(svc, sessA, true, false)
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("the tick redelivered the contract: %d prompts", n)
	}
}

func TestServiceBriefCLIStartAttachDeliverNothing(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)

	info, err := svc.Start(sessA, "p", []string{"goals"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "" || len(promptList(host)) != 0 {
		t.Fatalf("a CLI start sent the rules: %+v", info)
	}
	if _, err := svc.Attach(sessA, "p", false); err != nil {
		t.Fatal(err)
	}
	briefTick(svc, sessA, true, false)
	if n := len(promptList(host)); n != 0 {
		t.Fatalf("a CLI attach owes no delivery, got %d prompts", n)
	}
}

func TestServiceBriefAttachToAnotherProposalBriefsAgain(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)
	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(sessB, "q", []string{"goals"}, false); err != nil {
		t.Fatal(err)
	}
	info, err := svc.Attach(sessA, "q", true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "delivered" {
		t.Fatalf("attach to q: rules_delivery = %q", info.RulesDelivery)
	}
	prompts := promptList(host)
	if len(prompts) != 2 || !strings.Contains(prompts[1], "proposal q") {
		t.Fatalf("attaching a different proposal must brief it again: %v", prompts)
	}
}

func TestServiceBriefDetachThenAttachBriefsAgain(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)
	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Detach(sessA); err != nil {
		t.Fatal(err)
	}
	info, err := svc.Attach(sessA, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "delivered" {
		t.Fatalf("re-attach after detach: rules_delivery = %q", info.RulesDelivery)
	}
	if n := len(promptList(host)); n != 2 {
		t.Fatalf("a fresh attach after detach is a new briefing, got %d prompts", n)
	}
}

func TestServiceBriefWaitsForAnIdleAgent(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneBusy(host, sessA)

	info, err := svc.Start(sessA, "p", []string{"goals"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "held" {
		t.Fatalf("busy agent: rules_delivery = %q, want held", info.RulesDelivery)
	}
	if n := len(promptList(host)); n != 0 {
		t.Fatalf("the contract pasted into a busy pane: %d prompts", n)
	}
	briefTick(svc, sessA, false, false)
	if n := len(promptList(host)); n != 0 {
		t.Fatalf("the tick delivered while the agent was busy: %d prompts", n)
	}
	paneReady(host, sessA)
	briefTick(svc, sessA, true, false)
	prompts := promptList(host)
	if len(prompts) != 1 || !strings.Contains(prompts[0], "af proposal checkpoint") {
		t.Fatalf("the held contract did not land on the idle tick: %v", prompts)
	}
}

func TestServiceBriefNeverPastesOverTheUsersInput(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneHeld(host, sessA)

	info, err := svc.Start(sessA, "p", []string{"goals"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "held" {
		t.Fatalf("pane in use: rules_delivery = %q, want held", info.RulesDelivery)
	}
	// The lease stays held across ticks: the contract must wait.
	briefTick(svc, sessA, true, true)
	if n := len(promptList(host)); n != 0 {
		t.Fatalf("the contract pasted over an in-use pane: %d prompts", n)
	}
	paneReady(host, sessA)
	briefTick(svc, sessA, true, false)
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("the contract did not land once the pane was free: %d prompts", n)
	}
}

func TestServiceBriefSessionWithNoLiveAgent(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneDead(host, sessA)

	info, err := svc.Start(sessA, "p", []string{"goals"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "held" || info.RulesError == "" {
		t.Fatalf("a stopped session must not claim a briefing: %+v", info)
	}
	if n := len(promptList(host)); n != 0 {
		t.Fatalf("the contract was sent to a pane that does not exist: %d prompts", n)
	}
	// The debt survives: when the session is back and idle, the tick delivers.
	paneReady(host, sessA)
	briefTick(svc, sessA, true, false)
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("the owed contract did not land after restore: %d prompts", n)
	}
}

func TestServiceBriefRetriesAfterAFailedSend(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	clock := pushT0
	svc.now = func() time.Time { return clock }
	paneReady(host, sessA)
	host.delivery, host.sendErr = DeliveryFailed, errors.New("prompt not delivered")

	info, err := svc.Start(sessA, "p", []string{"goals"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "held" && info.RulesDelivery != "failed" {
		t.Fatalf("a send that did not land must not claim delivery: %+v", info)
	}
	host.delivery, host.sendErr = DeliveryConfirmed, nil
	// The debt is still owed but backs off like a failed push, so the same
	// tick does not re-paste.
	briefTick(svc, sessA, true, false)
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("no backoff after the failed send: %d prompts", n)
	}
	clock = clock.Add(PushQuiet + time.Second)
	briefTick(svc, sessA, true, false)
	prompts := promptList(host)
	if len(prompts) != 2 || !strings.Contains(prompts[1], "af proposal checkpoint") {
		t.Fatalf("the failed delivery was never retried: %v", prompts)
	}
}

func TestServiceBriefUnconfirmedIsDeliveredOnce(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)
	host.delivery = DeliveryUnconfirmed

	info, err := svc.Start(sessA, "p", []string{"goals"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "unconfirmed" {
		t.Fatalf("rules_delivery = %q, want unconfirmed", info.RulesDelivery)
	}
	// Unconfirmed may already have landed: it settles, never retries.
	briefTick(svc, sessA, true, false)
	briefTick(svc, sessA, true, false)
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("an unconfirmed delivery was resent: %d prompts", n)
	}
	// A repeat attach must not turn "maybe landed" into a second copy either.
	if _, err := svc.Attach(sessA, "p", true); err != nil {
		t.Fatal(err)
	}
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("a repeat attach resent the contract: %d prompts", n)
	}
}

func TestServiceBriefUnconfirmedHoldsEditsForTheQuietWindow(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	clock := pushT0
	svc.now = func() time.Time { return clock }
	paneReady(host, sessA)
	host.delivery = DeliveryUnconfirmed
	info, err := svc.Start(sessA, "p", []string{"goals"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "unconfirmed" {
		t.Fatalf("rules_delivery = %q, want unconfirmed", info.RulesDelivery)
	}
	host.delivery = DeliveryConfirmed
	// The contract may still sit unsubmitted in the pane; a human save now
	// must wait out the same quiet window an unconfirmed push would owe it —
	// and the session has no push entry yet, so the debt rides the briefing.
	read, err := svc.Read(Target{SessionID: sessA}, "p", "goals")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "user edit\n", read.Commit); err != nil {
		t.Fatal(err)
	}
	target := PushTarget{RepoID: testRepoID, SessionID: sessA, Modes: map[string]PushMode{"p": PushAuto}, Ready: true}
	clock = clock.Add(PushQuiet + time.Second)
	svc.PushTick([]PushTarget{target})
	if prompts := promptList(host); len(prompts) != 1 {
		t.Fatalf("the edit spliced into an unconfirmed contract paste: %v", prompts)
	}
	clock = clock.Add(PushUnconfirmedTimeout)
	svc.PushTick([]PushTarget{target})
	prompts := promptList(host)
	if len(prompts) != 2 || !strings.HasPrefix(prompts[1], "Proposal p: the user edited") {
		t.Fatalf("the edit did not follow once the pane was provably free: %v", prompts)
	}
}

func TestServiceBriefSurvivesAServiceRestart(t *testing.T) {
	svc, host, store := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneBusy(host, sessA)
	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	if n := len(promptList(host)); n != 0 {
		t.Fatalf("premise: nothing delivered yet, got %d prompts", n)
	}
	// A daemon restart drops the in-memory push entries. The owed contract is
	// durable: a fresh service over the same store must still deliver it.
	restarted := NewService(host)
	restarted.spawn = func(f func()) { f() }
	paneReady(host, sessA)
	restarted.PushTick([]PushTarget{{RepoID: testRepoID, SessionID: sessA, Ready: true}})
	prompts := promptList(host)
	if len(prompts) != 1 || !strings.Contains(prompts[0], "af proposal checkpoint") {
		t.Fatalf("the restart lost the owed contract: %v", prompts)
	}
	// And a restart does not resurrect a settled one.
	paneReady(host, sessA)
	again := NewService(host)
	again.spawn = func(f func()) { f() }
	again.PushTick([]PushTarget{{RepoID: testRepoID, SessionID: sessA, Ready: true}})
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("a delivered contract was re-sent after a restart: %d prompts", n)
	}
	_ = store
}

func TestServiceBriefAheadOfPendingEdits(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	clock := pushT0
	svc.now = func() time.Time { return clock }
	paneBusy(host, sessA)
	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	// The user edits while the contract is still owed; both are now pending.
	read, err := svc.Read(Target{SessionID: sessA}, "p", "goals")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "user edit\n", read.Commit); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(PushQuiet + time.Second)
	paneReady(host, sessA)
	target := PushTarget{RepoID: testRepoID, SessionID: sessA, Modes: map[string]PushMode{"p": PushAuto}, Ready: true}
	svc.PushTick([]PushTarget{target})
	prompts := promptList(host)
	if len(prompts) != 1 || !strings.Contains(prompts[0], "You are working on proposal p") {
		t.Fatalf("the contract must reach the agent before its edit prompts: %v", prompts)
	}
	// The edit push then follows on the next tick.
	svc.PushTick([]PushTarget{target})
	prompts = promptList(host)
	if len(prompts) != 2 || !strings.HasPrefix(prompts[1], "Proposal p: the user edited") {
		t.Fatalf("the pending edit push did not follow the contract: %v", prompts)
	}
}

func TestServiceBriefUnconfirmedWithAnErrorStillSettles(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)
	// The transport may have pasted but reports a problem: retrying could put
	// a second copy in front of the agent, so the debt settles anyway.
	host.delivery, host.sendErr = DeliveryUnconfirmed, errors.New("pane observation lost")

	info, err := svc.Start(sessA, "p", []string{"goals"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "unconfirmed" {
		t.Fatalf("rules_delivery = %q, want unconfirmed", info.RulesDelivery)
	}
	briefTick(svc, sessA, true, false)
	briefTick(svc, sessA, true, false)
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("an unconfirmed send with an error was resent: %d prompts", n)
	}
}

func TestServiceBriefSlugSwapThroughCLIReArms(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)
	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(sessB, "q", []string{"goals"}, false); err != nil {
		t.Fatal(err)
	}
	// A CLI attach moves the session to q without a briefing request; coming
	// back to p over the web is a fresh attach, not a resync.
	if _, err := svc.Attach(sessA, "q", false); err != nil {
		t.Fatal(err)
	}
	info, err := svc.Attach(sessA, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if info.RulesDelivery != "delivered" {
		t.Fatalf("re-attach after a swap: rules_delivery = %q, want delivered", info.RulesDelivery)
	}
	prompts := promptList(host)
	if len(prompts) != 2 || !strings.Contains(prompts[1], "proposal p") {
		t.Fatalf("re-attaching p after the swap must brief it again: %v", prompts)
	}
}

func TestServiceBriefSendNowSendsContractFirst(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneBusy(host, sessA)
	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	read, err := svc.Read(Target{SessionID: sessA}, "p", "goals")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "user edit\n", read.Commit); err != nil {
		t.Fatal(err)
	}
	paneReady(host, sessA)
	// The user clicks Send now before the next poll tick: the contract still
	// goes out first, and the queued edit follows it in the same request.
	target := PushTarget{RepoID: testRepoID, SessionID: sessA, Modes: map[string]PushMode{"p": PushAuto}, Ready: true}
	if _, err := svc.SendNow(target); err != nil {
		t.Fatal(err)
	}
	prompts := promptList(host)
	if len(prompts) != 2 ||
		!strings.Contains(prompts[0], "You are working on proposal p") ||
		!strings.HasPrefix(prompts[1], "Proposal p: the user edited") {
		t.Fatalf("Send now must send the contract before the edits: %v", prompts)
	}
}

func TestServiceBriefUnconfirmedRepeatsHonestlyOnReattach(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneReady(host, sessA)
	host.delivery = DeliveryUnconfirmed

	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	// The first send may not have landed: a resync attach must keep saying so
	// rather than upgrading the answer to "delivered" — while still never
	// sending a second copy.
	again, err := svc.Attach(sessA, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if again.RulesDelivery != "unconfirmed" {
		t.Fatalf("repeat attach after an unconfirmed send: rules_delivery = %q, want unconfirmed", again.RulesDelivery)
	}
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("a repeat attach resent the contract: %d prompts", n)
	}
}

func TestStoreAttachArmsTheDebtInItsOwnCommit(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.Create("p", []string{"goals"}, Committer{Author: AuthorHuman}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create("q", []string{"goals"}, Committer{Author: AuthorHuman}); err != nil {
		t.Fatal(err)
	}
	// The debt commits with the attachment, so the push machinery can never
	// see one without the other — and a later attach replaces both together,
	// never leaving a marker for a proposal the session no longer holds.
	if err := store.AttachOwing("p", sessA); err != nil {
		t.Fatal(err)
	}
	slug, state, err := store.Brief(sessA)
	if err != nil || slug != "p" || state != BriefOwed {
		t.Fatalf("attach owing = %q %q, %v; want p owed", slug, state, err)
	}
	if err := store.AttachOwing("q", sessA); err != nil {
		t.Fatal(err)
	}
	slug, state, err = store.Brief(sessA)
	if err != nil || slug != "q" || state != BriefOwed {
		t.Fatalf("the newer attach must carry its own debt: %q %q %v", slug, state, err)
	}
	// A same-slug resync keeps the armed marker exactly once.
	if err := store.AttachOwing("q", sessA); err != nil {
		t.Fatal(err)
	}
	slug, state, err = store.Brief(sessA)
	if err != nil || slug != "q" || state != BriefOwed {
		t.Fatalf("a resync must not re-arm or drop the debt: %q %q %v", slug, state, err)
	}
}

func TestServiceBriefQueuedSendNowOverridesTheHold(t *testing.T) {
	svc, host, _ := newTestService(t)
	svc.spawn = func(f func()) { f() }
	paneBusy(host, sessA)
	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	read, err := svc.Read(Target{SessionID: sessA}, "p", "goals")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "user edit\n", read.Commit); err != nil {
		t.Fatal(err)
	}
	// Send now while the agent is busy: the request queues, like DecidePush's
	// sendNow contract promises — including over a held pane.
	if _, err := svc.SendNow(PushTarget{RepoID: testRepoID, SessionID: sessA, Ready: false}); err != nil {
		t.Fatal(err)
	}
	// The agent idles but a TUI holds the pane. A queued Send now overrides
	// that hold for the edits — and the briefing queued ahead of them gets
	// the same override instead of stranding both.
	paneHeld(host, sessA)
	briefTick(svc, sessA, true, true)
	prompts := promptList(host)
	if len(prompts) != 1 || !strings.Contains(prompts[0], "You are working on proposal p") {
		t.Fatalf("the held pane stranded the queued send's contract: %v", prompts)
	}
	// The edits the user asked for follow on the next tick.
	briefTick(svc, sessA, true, true)
	prompts = promptList(host)
	if len(prompts) != 2 || !strings.HasPrefix(prompts[1], "Proposal p: the user edited") {
		t.Fatalf("the queued edits did not follow the contract: %v", prompts)
	}
}

func TestServiceBriefRechecksThePaneAtSendTime(t *testing.T) {
	svc, host, _ := newTestService(t)
	var spawned []func()
	svc.spawn = func(f func()) { spawned = append(spawned, f) }
	clock := pushT0
	svc.now = func() time.Time { return clock }
	paneBusy(host, sessA)
	if _, err := svc.Start(sessA, "p", []string{"goals"}, true); err != nil {
		t.Fatal(err)
	}
	// The agent idles and the tick picks the session — then a client takes
	// the pane before the spawned send runs.
	paneReady(host, sessA)
	briefTick(svc, sessA, true, false)
	if len(spawned) != 1 {
		t.Fatalf("the tick did not queue a briefing send: %d", len(spawned))
	}
	paneHeld(host, sessA)
	spawned[0]()
	if n := len(promptList(host)); n != 0 {
		t.Fatalf("the contract pasted into a pane a client took: %d prompts", n)
	}
	// The debt is still owed: freeing the pane lets the next tick deliver.
	clock = clock.Add(PushQuiet + time.Second)
	paneReady(host, sessA)
	briefTick(svc, sessA, true, false)
	if len(spawned) != 2 {
		t.Fatalf("the freeing tick did not retry the owed contract: %d sends", len(spawned))
	}
	spawned[1]()
	if n := len(promptList(host)); n != 1 {
		t.Fatalf("the refused send did not stay owed: %d prompts", n)
	}
}
