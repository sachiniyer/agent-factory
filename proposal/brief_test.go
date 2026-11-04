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
