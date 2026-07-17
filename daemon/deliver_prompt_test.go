package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// promptRecorder collects every prompt that reaches a session's backend, across
// both the create path (StartAndSendPrompt) and the send path
// (SendPromptCommand), so a delivery test can prove no prompt was dropped and
// observe the order they landed in.
type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
}

func (r *promptRecorder) add(prompt string) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
}

func (r *promptRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.prompts))
	copy(out, r.prompts)
	return out
}

// recordingBackend is a ready fake backend that records the prompts sent to it.
type recordingBackend struct {
	readyFakeBackend
	rec *promptRecorder
}

func (b recordingBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.rec.add(prompt)
	return nil
}

// failingPromptBackend returns the low-level send error that should never leak
// once daemon delivery has rejected an undeliverable liveness state.
type failingPromptBackend struct {
	readyFakeBackend
	sent int
}

func (b *failingPromptBackend) SendPromptCommand(*session.Instance, string) error {
	b.sent++
	return fmt.Errorf("tmux error: session not found")
}

// slowRecordingKillBackend records prompts like recordingBackend and holds Kill
// inside the teardown window so a test can exercise delivery while the daemon's
// killsInFlight marker is set.
type slowRecordingKillBackend struct {
	recordingBackend
	killStarted chan struct{}
	killBlock   chan struct{}
}

func (b *slowRecordingKillBackend) Kill(inst *session.Instance) error {
	close(b.killStarted)
	<-b.killBlock
	return b.recordingBackend.Kill(inst)
}

// blockingSendKillBackend blocks SendPromptCommand and records when Kill
// reaches backend teardown. It proves SendPrompt holds the per-session op lock
// across the prompt write, so a kill that starts after the kill-in-flight
// pre-check cannot destroy the session mid-send.
type blockingSendKillBackend struct {
	readyFakeBackend
	rec *promptRecorder

	sendStarted chan struct{}
	releaseSend chan struct{}
	killStarted chan struct{}
	killBlock   chan struct{}
}

func (b *blockingSendKillBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	close(b.sendStarted)
	<-b.releaseSend
	b.rec.add(prompt)
	return nil
}

func (b *blockingSendKillBackend) Kill(inst *session.Instance) error {
	close(b.killStarted)
	<-b.killBlock
	return b.readyFakeBackend.Kill(inst)
}

// installRecordingBackend wires a backend factory whose Start completes
// immediately (so creates do not block) and that records delivered prompts.
func installRecordingBackend(t *testing.T) *promptRecorder {
	t.Helper()
	rec := &promptRecorder{}
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		return recordingBackend{readyFakeBackend{fake}, rec}, nil
	})
	t.Cleanup(restore)
	return rec
}

// TestDeliverPrompt_ConcurrentDeliveriesCreateOnceDeliverAll is the regression
// test for #865: several deliveries fired at the same missing target session
// must create that session exactly once and deliver EVERY prompt — the pre-fix
// path let the loser of the creation race surface "already reserved" and
// dropped its prompt entirely.
func TestDeliverPrompt_ConcurrentDeliveriesCreateOnceDeliverAll(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := installRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const n = 6
	prompts := make([]string, n)
	for i := range prompts {
		prompts[i] = fmt.Sprintf("prompt-%d", i)
	}

	var wg sync.WaitGroup
	statuses := make([]string, n)
	errs := make([]error, n)
	release := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-release // fire as close to simultaneously as the scheduler allows
			statuses[i], errs[i] = manager.DeliverPrompt(DeliverPromptRequest{
				Title:    "captain",
				RepoPath: repoPath,
				Program:  "claude",
				Prompt:   prompts[i],
			})
		}(i)
	}
	close(release)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("delivery %d returned an error (a dropped prompt — #865 regression): %v", i, e)
		}
	}

	// Exactly one session was created for the shared target.
	stored, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	if len(stored) != 1 || stored[0].Title != "captain" {
		t.Fatalf("expected exactly one persisted session titled captain, got %+v", stored)
	}

	// Exactly one delivery created it ("started"); the rest sent into it.
	started := 0
	for _, s := range statuses {
		switch s {
		case "started":
			started++
		case "sent":
		default:
			t.Fatalf("unexpected status %q", s)
		}
	}
	if started != 1 {
		t.Fatalf("expected exactly one create (started), got %d; statuses=%v", started, statuses)
	}

	// Every prompt was delivered — none dropped.
	got := rec.snapshot()
	if len(got) != n {
		t.Fatalf("expected %d delivered prompts, got %d: %v", n, len(got), got)
	}
	seen := make(map[string]bool, len(got))
	for _, p := range got {
		seen[p] = true
	}
	for _, want := range prompts {
		if !seen[want] {
			t.Fatalf("prompt %q was not delivered; delivered set: %v", want, got)
		}
	}
}

// TestDeliverPrompt_SerializesDeliveryInOrder pins that two deliveries to the
// same target are serialized: the first creates the session and delivers its
// prompt, the second sends into it, and the prompts land in lock-acquisition
// order rather than interleaving.
func TestDeliverPrompt_SerializesDeliveryInOrder(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := &promptRecorder{}

	// One create happens (the winner); its Start blocks until we release it so
	// we can guarantee the first delivery holds the per-target lock before the
	// second is launched.
	backendCh := make(chan *session.FakeBackend, 1)
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		backendCh <- fake
		return recordingBackend{readyFakeBackend{fake}, rec}, nil
	})
	t.Cleanup(restore)

	repoPath := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	deliver := func(prompt string) (string, error) {
		return manager.DeliverPrompt(DeliverPromptRequest{
			Title:    "captain",
			RepoPath: repoPath,
			Program:  "claude",
			Prompt:   prompt,
		})
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := deliver("first"); err != nil {
			t.Errorf("first delivery: %v", err)
		}
	}()

	// Wait until the first delivery is inside CreateSession — at which point it
	// already holds the per-target lock — then let it proceed and launch the
	// second delivery, which must block on that lock.
	fake := <-backendCh
	<-fake.StartCalled()

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Give the first delivery a head start on the lock; even without this
		// the happens-before (first records its prompt before unlocking, second
		// can only send after locking) guarantees order, but launching after
		// StartCalled keeps the create-vs-send roles deterministic.
		if _, err := deliver("second"); err != nil {
			t.Errorf("second delivery: %v", err)
		}
	}()

	fake.CompleteStart()
	wg.Wait()

	got := rec.snapshot()
	want := []string{"first", "second"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("prompts delivered out of order or dropped: got %v, want %v", got, want)
	}
}

// TestDeliverPrompt_RefusesDeletingTarget pins that delivery into a session
// that is mid-teardown is surfaced as an error rather than silently dropped or
// delivered into a dying session (#847 must be respected).
func TestDeliverPrompt_RefusesDeletingTarget(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := installRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "init",
	}); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, "captain")]
	manager.mu.Unlock()
	if inst == nil {
		t.Fatal("expected the created session to be in the manager's instance map")
	}
	inst.SetStatusForTest(session.Deleting)

	before := len(rec.snapshot())
	_, err = manager.DeliverPrompt(DeliverPromptRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "during-delete",
	})
	if err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("expected a 'being deleted' error, got: %v", err)
	}
	if got := len(rec.snapshot()); got != before {
		t.Fatalf("prompt was delivered into a Deleting session: recorded count went %d -> %d", before, got)
	}
}

// TestDeliverPrompt_RefusesLostTargetBeforeTmuxSend is the #1432 regression:
// a target session whose daemon record is Lost must produce an actionable
// liveness error instead of attempting tmux delivery and leaking "session not
// found" to the RPC/API caller.
func TestDeliverPrompt_RefusesLostTargetBeforeTmuxSend(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &failingPromptBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	registerStarted(t, manager, repoID, repoPath, "captain", backend, true, session.Lost)

	_, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "during-outage",
	})
	if err == nil {
		t.Fatal("expected Lost target delivery to fail")
	}
	if !strings.Contains(err.Error(), `target session "captain" is Lost; prompt not delivered; recover it first`) {
		t.Fatalf("expected actionable Lost error, got: %v", err)
	}
	if strings.Contains(err.Error(), "tmux") || strings.Contains(err.Error(), "session not found") {
		t.Fatalf("Lost delivery must not leak raw tmux errors, got: %v", err)
	}
	if backend.sent != 0 {
		t.Fatalf("Lost delivery reached SendPromptCommand %d time(s); want 0", backend.sent)
	}
}

// TestDeliverPrompt_RefusesArchivedTargetBeforeTmuxSend is the #1529
// regression: an archived target has no live tmux to deliver into, so the
// prompt must be rejected with an actionable "restore it first" error instead
// of falling through to a confusing raw backend error, and it must never reach
// SendPromptCommand.
func TestDeliverPrompt_RefusesArchivedTargetBeforeTmuxSend(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &failingPromptBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	registerStarted(t, manager, repoID, repoPath, "captain", backend, true, session.Archived)

	_, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "while-archived",
	})
	if err == nil {
		t.Fatal("expected Archived target delivery to fail")
	}
	want := `target session "captain" is Archived; prompt not delivered; restore it first (af sessions restore captain)`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected actionable Archived error, got: %v", err)
	}
	if strings.Contains(err.Error(), "tmux") || strings.Contains(err.Error(), "session not found") {
		t.Fatalf("Archived delivery must not leak raw tmux errors, got: %v", err)
	}
	if backend.sent != 0 {
		t.Fatalf("Archived delivery reached SendPromptCommand %d time(s); want 0", backend.sent)
	}
}

// TestPromptTargetLivenessError_ArchivedQuotesRestoreCommand pins that the
// suggested `af sessions restore <title>` command is shell-safe (#1529): a
// title with a space or shell metacharacter must be single-quoted so a
// copy-pasted command restores the intended session and cannot smuggle a second
// shell command. A plain title stays unquoted so the common case reads cleanly.
func TestPromptTargetLivenessError_ArchivedQuotesRestoreCommand(t *testing.T) {
	cases := []struct {
		name     string
		title    string
		wantCmd  string
		unwanted string // a raw, unquoted form that must NOT appear
	}{
		{"plain title unquoted", "captain", "af sessions restore captain", ""},
		{"space quoted", "my session", "af sessions restore 'my session'", "restore my session)"},
		{"semicolon quoted", "a;rm -rf ~", "af sessions restore 'a;rm -rf ~'", "restore a;rm -rf ~"},
		{"embedded quote escaped", "it's", `af sessions restore 'it'"'"'s'`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := promptTargetLivenessError(tc.title, session.LiveArchived)
			if err == nil {
				t.Fatalf("expected an Archived liveness error for %q", tc.title)
			}
			if !strings.Contains(err.Error(), tc.wantCmd) {
				t.Fatalf("expected restore command %q in error, got: %v", tc.wantCmd, err)
			}
			if tc.unwanted != "" && strings.Contains(err.Error(), tc.unwanted) {
				t.Fatalf("error must not contain the unquoted command %q (shell-injection risk), got: %v", tc.unwanted, err)
			}
		})
	}
}

func TestSendPrompt_RefusesLostAndDeadTargetsBeforeBackendSend(t *testing.T) {
	tests := []struct {
		name       string
		status     session.Status
		wantStatus string
	}{
		{name: "lost", status: session.Lost, wantStatus: "Lost"},
		{name: "dead", status: session.Dead, wantStatus: "Dead"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			backend := &failingPromptBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
			registerStarted(t, manager, repoID, repoPath, "captain", backend, true, tc.status)

			err := manager.SendPrompt(SendPromptRequest{
				Title:  "captain",
				RepoID: repoID,
				Prompt: "during-outage",
			})
			if err == nil {
				t.Fatalf("expected %s target send to fail", tc.wantStatus)
			}
			want := fmt.Sprintf(`target session "captain" is %s; prompt not delivered; recover it first`, tc.wantStatus)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("expected actionable %s error, got: %v", tc.wantStatus, err)
			}
			if strings.Contains(err.Error(), "tmux") || strings.Contains(err.Error(), "session not found") {
				t.Fatalf("%s send must not leak raw tmux errors, got: %v", tc.wantStatus, err)
			}
			if backend.sent != 0 {
				t.Fatalf("%s send reached SendPromptCommand %d time(s); want 0", tc.wantStatus, backend.sent)
			}
		})
	}
}

// TestSendPrompt_RefusesKillInFlightTarget is the #1473 regression test for
// the direct SendPrompt RPC/API path. KillSession marks killsInFlight but does
// not set OpKilling on the instance, so SendPrompt must check the daemon kill
// marker instead of writing into a session that is already being torn down.
func TestSendPrompt_RefusesKillInFlightTarget(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := &promptRecorder{}
	backend := &slowRecordingKillBackend{
		killStarted: make(chan struct{}),
		killBlock:   make(chan struct{}),
	}
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		backend.recordingBackend = recordingBackend{readyFakeBackend{fake}, rec}
		return backend, nil
	})
	t.Cleanup(restore)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "init",
	}); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	killDone := make(chan error, 1)
	go func() {
		_, kerr := manager.KillSession(KillSessionRequest{Title: "captain", RepoID: repo.ID})
		killDone <- kerr
	}()
	select {
	case <-backend.killStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("KillSession never reached the backend teardown")
	}

	var releaseOnce sync.Once
	releaseKill := func() {
		close(backend.killBlock)
		select {
		case err := <-killDone:
			if err != nil {
				t.Errorf("KillSession: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("KillSession did not complete after the teardown was released")
		}
	}
	defer releaseOnce.Do(releaseKill)

	before := len(rec.snapshot())
	err = manager.SendPrompt(SendPromptRequest{
		Title:  "captain",
		RepoID: repo.ID,
		Prompt: "during-kill",
	})
	if err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("expected a 'being deleted' error during KillSession teardown, got: %v", err)
	}
	if got := len(rec.snapshot()); got != before {
		t.Fatalf("SendPrompt delivered into a kill-in-flight session: recorded count went %d -> %d", before, got)
	}

	releaseOnce.Do(releaseKill)
}

func TestSendPrompt_DeliversBeforeLaterKillStartsTeardown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := &promptRecorder{}
	backend := &blockingSendKillBackend{
		rec:         rec,
		sendStarted: make(chan struct{}),
		releaseSend: make(chan struct{}),
		killStarted: make(chan struct{}),
		killBlock:   make(chan struct{}),
	}
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		backend.readyFakeBackend = readyFakeBackend{fake}
		return backend, nil
	})
	t.Cleanup(restore)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- manager.SendPrompt(SendPromptRequest{
			Title:  "captain",
			RepoID: repo.ID,
			Prompt: "during-send",
		})
	}()
	select {
	case <-backend.sendStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("SendPrompt never reached backend send")
	}

	killDone := make(chan error, 1)
	go func() {
		_, kerr := manager.KillSession(KillSessionRequest{Title: "captain", RepoID: repo.ID})
		killDone <- kerr
	}()
	key := daemonInstanceKey(repo.ID, "captain")
	deadline := time.After(5 * time.Second)
	for {
		manager.mu.Lock()
		_, killing := manager.killsInFlight[key]
		manager.mu.Unlock()
		if killing {
			break
		}
		select {
		case <-deadline:
			t.Fatal("KillSession never registered killsInFlight")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-backend.killStarted:
		t.Fatal("KillSession reached backend teardown before SendPromptCommand returned")
	default:
	}

	close(backend.releaseSend)
	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("SendPrompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendPrompt did not return after backend send was released")
	}
	if got := rec.snapshot(); len(got) != 1 || got[0] != "during-send" {
		t.Fatalf("prompt was not delivered before kill teardown: got %v", got)
	}

	select {
	case <-backend.killStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("KillSession did not reach backend teardown after SendPrompt returned")
	}
	close(backend.killBlock)
	select {
	case err := <-killDone:
		if err != nil {
			t.Fatalf("KillSession: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("KillSession did not complete after teardown was released")
	}
}

// TestDeliverPrompt_RefusesKillInFlightTarget pins the daemon-initiated teardown
// path: KillSession marks killsInFlight but does not set OpKilling, so
// DeliverPrompt must consult the manager's in-flight kill marker and reject
// instead of reporting success for a prompt that may be lost mid-kill (#1333).
func TestDeliverPrompt_RefusesKillInFlightTarget(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := &promptRecorder{}
	backend := &slowRecordingKillBackend{
		killStarted: make(chan struct{}),
		killBlock:   make(chan struct{}),
	}
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		backend.recordingBackend = recordingBackend{readyFakeBackend{fake}, rec}
		return backend, nil
	})
	t.Cleanup(restore)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "init",
	}); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	killDone := make(chan error, 1)
	go func() {
		_, kerr := manager.KillSession(KillSessionRequest{Title: "captain", RepoID: repo.ID})
		killDone <- kerr
	}()
	select {
	case <-backend.killStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("KillSession never reached the backend teardown")
	}

	var releaseOnce sync.Once
	releaseKill := func() {
		close(backend.killBlock)
		select {
		case err := <-killDone:
			if err != nil {
				t.Errorf("KillSession: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("KillSession did not complete after the teardown was released")
		}
	}
	defer releaseOnce.Do(releaseKill)

	before := len(rec.snapshot())
	_, err = manager.DeliverPrompt(DeliverPromptRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "during-kill",
	})
	if err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("expected a 'being deleted' error during KillSession teardown, got: %v", err)
	}
	if got := len(rec.snapshot()); got != before {
		t.Fatalf("prompt was delivered into a kill-in-flight session: recorded count went %d -> %d", before, got)
	}

	releaseOnce.Do(releaseKill)
}

// TestWaitForTargetSession_ReturnsWhenSessionAppears covers the cross-process
// fallback's wait: a session that materializes after a brief delay is picked up
// rather than timing out, while a Deleting one is surfaced as an error.
func TestWaitForTargetSession_ReturnsWhenSessionAppears(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
			Title:    "captain",
			RepoPath: repoPath,
			Program:  "claude",
			Prompt:   "init",
		}); err != nil {
			t.Errorf("background create: %v", err)
		}
	}()

	if err := manager.waitForTargetSession(repo.ID, "captain"); err != nil {
		t.Fatalf("waitForTargetSession should have seen the session appear: %v", err)
	}
}

// TestIsConcurrentCreateErr pins which CreateSession failures DeliverPrompt
// treats as a retryable creation race (wait-then-send) versus a hard error.
func TestIsConcurrentCreateErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"reserved", fmt.Errorf("session with title %q is already reserved: %w", "captain", errConcurrentCreate), true},
		{"exists", fmt.Errorf("session with title %q already exists: %w", "captain", errConcurrentCreate), true},
		{"branch collision", fmt.Errorf("session titled %q conflicts with existing session %q: both sanitize to the same git branch %q", "A B", "a-b", "af-a-b"), false},
		// #916: a tmux orphan is terminal, not a retryable concurrent-create —
		// even though its message used to contain the "already exists" substring.
		{"tmux orphan", fmt.Errorf("conflicting tmux session %q is already running; no agent-factory session owns it. Clean it up with: tmux kill-session -t %s", "captain", "af-abc_captain"), false},
		// A plain "already exists" string with no sentinel must not be treated as
		// retryable: classification keys off errConcurrentCreate, not the text.
		{"unwrapped exists", fmt.Errorf("session with title %q already exists", "captain"), false},
		{"nil", nil, false},
		{"unrelated", fmt.Errorf("git is not installed"), false},
	}
	for _, tc := range cases {
		if got := isConcurrentCreateErr(tc.err); got != tc.want {
			t.Errorf("%s: isConcurrentCreateErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDeliverPrompt_RootTargetWaitsForRecreationThenSends is the #1223
// regression: a watch/monitor delivery to the daemon-managed root agent whose
// tmux is momentarily absent (being re-materialized by the ensure loop) must
// NOT fall through to auto-create — which the reserved-name guard rejects,
// dropping the event with a misleading "pick another name" error. It must wait
// for root to come back and then send into it, mirroring the concurrent-create
// retry.
func TestDeliverPrompt_RootTargetWaitsForRecreationThenSends(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := installRecordingBackend(t)
	repoPath := setupControlRepo(t)
	// The repo is opted into a root agent, so the ensure loop owns "root".
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Root's tmux is momentarily absent. A short while into the delivery's wait,
	// the ensure loop re-materializes it in place (allowReserved, as the daemon's
	// own ensure path does). The delivery must then send into it.
	go func() {
		time.Sleep(50 * time.Millisecond)
		if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
			Title:         session.RootSessionTitle,
			RepoPath:      repoPath,
			Program:       "claude",
			InPlace:       true,
			allowReserved: true,
		}); err != nil {
			t.Errorf("background root (re-)create: %v", err)
		}
	}()

	status, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:    session.RootSessionTitle,
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "monitor-event",
	})
	if err != nil {
		t.Fatalf("delivery to a momentarily-absent root must defer-then-send, not error: %v", err)
	}
	if status != "sent" {
		t.Fatalf("expected status \"sent\" (delivered into the re-materialized root), got %q", status)
	}

	// The event landed in the root session, not dropped.
	got := rec.snapshot()
	if len(got) != 1 || got[0] != "monitor-event" {
		t.Fatalf("expected the monitor event delivered into root, got %v", got)
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatalf("root instance should be registered after recreation")
	}
}

// TestDeliverPrompt_RootTargetAbsentSurfacesAccurateError pins the accurate-
// error half of #1223: when the root does not come back within the wait bound,
// the delivery surfaces a "being recreated" error rather than the misleading
// reserved-name / "pick another name" one, and no event is delivered.
func TestDeliverPrompt_RootTargetAbsentSurfacesAccurateError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := installRecordingBackend(t)
	repoPath := setupControlRepo(t)

	// Bound the wait tightly so the timeout path is exercised fast, not the real
	// 30s. Restore after the test.
	origWait := targetDeliverWait
	targetDeliverWait = 150 * time.Millisecond
	t.Cleanup(func() { targetDeliverWait = origWait })

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Root never comes back during this delivery (no ensure/create fires).
	_, err = manager.DeliverPrompt(DeliverPromptRequest{
		Title:    session.RootSessionTitle,
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "monitor-event",
	})
	if err == nil {
		t.Fatal("expected an error when root does not return within the wait bound")
	}
	if !strings.Contains(err.Error(), "being recreated") {
		t.Fatalf("expected an accurate \"being recreated\" error, got: %v", err)
	}
	if strings.Contains(err.Error(), "reserved") || strings.Contains(err.Error(), "pick another name") {
		t.Fatalf("must NOT surface the misleading reserved-name error for a root target, got: %v", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("no event should be delivered on the timeout path, got %v", got)
	}
}

// TestDeliverPrompt_ReservedRootUnconfiguredStillRejected pins that the #1223
// special-case is narrow: a delivery to the reserved "root" title on a repo
// that is NOT opted into root_agents still gets the reserved-name error — the
// ensure loop will never materialize a root there, so waiting would be pointless
// and the actionable "add it to root_agents" guidance must remain.
func TestDeliverPrompt_ReservedRootUnconfiguredStillRejected(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installRecordingBackend(t)
	repoPath := setupControlRepo(t)

	// No root_agents entry for this repo.
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	start := time.Now()
	_, err = manager.DeliverPrompt(DeliverPromptRequest{
		Title:    session.RootSessionTitle,
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "monitor-event",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the reserved-name error for an unconfigured root target")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected the reserved-name error, got: %v", err)
	}
	// It must fail fast (the create path), not wait out the root recreation bound.
	if elapsed > 5*time.Second {
		t.Errorf("unconfigured reserved-root delivery took %v; must fail fast, not wait for a root that will never come", elapsed)
	}
}

// TestDeliverPrompt_DeletedProjectRootStillRejected pins #1835: a project
// deleted at runtime suppresses its root agent for the rest of the daemon's
// life, so a delivery to the reserved "root" title must be treated exactly like
// the unconfigured case above — fail fast with the reserved-name error. The
// in-memory root_agents config is immutable and still lists the deleted repo, so
// consulting it alone would wait out the recreation bound for a root the ensure
// loop will never bring back, then blame the delay on a recreation that is not
// happening.
func TestDeliverPrompt_DeletedProjectRootStillRejected(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installRecordingBackend(t)
	repoPath := setupControlRepo(t)

	// Bound the wait well under the real 30s: a regression waits it out in full,
	// so this is what separates a fast rejection from a pointless wait.
	origWait := targetDeliverWait
	targetDeliverWait = 2 * time.Second
	t.Cleanup(func() { targetDeliverWait = origWait })

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()
	root := findRootInstance(t, manager, repoPath)
	if root == nil {
		t.Fatal("root should materialize before the project is deleted")
	}
	// The fake backend skips Provision, which is what attaches the in-place
	// (external) worktree in production. Restore it so DeleteProject routes the
	// root down its real tear-down path instead of trying to archive it.
	gw, _, err := sessiongit.NewGitWorktreeInPlace(repoPath)
	if err != nil {
		t.Fatalf("NewGitWorktreeInPlace: %v", err)
	}
	root.SetGitWorktreeForTest(gw)

	if _, err := manager.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if findRootInstance(t, manager, repoPath) != nil {
		t.Fatal("the deleted project's root should be gone before the delivery under test")
	}

	start := time.Now()
	_, err = manager.DeliverPrompt(DeliverPromptRequest{
		Title:    session.RootSessionTitle,
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "monitor-event",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the reserved-name error for a deleted project's root target")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected the reserved-name error, got: %v", err)
	}
	// The root is permanently suppressed, not coming back — saying otherwise
	// sends the user looking for a recreation that will never happen.
	if strings.Contains(err.Error(), "being recreated") {
		t.Fatalf("a deleted project's root is not being recreated; error must not claim it is, got: %v", err)
	}
	if elapsed > targetDeliverWait/2 {
		t.Errorf("deleted-project root delivery took %v; must fail fast, not wait out the %v recreation bound", elapsed, targetDeliverWait)
	}
}

// TestDeliverPrompt_TmuxOrphanReturnsImmediatelyWithError pins the #916 fix: a
// tmux session with no daemon/disk record is a terminal conflict, not a
// retryable concurrent-create. DeliverPrompt must fail fast with an actionable
// message instead of waiting out waitForTargetSession's 30s timeout.
func TestDeliverPrompt_TmuxOrphanReturnsImmediatelyWithError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux; skipped in -short")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// #1056: private tmux server so the raw orphan session below dies with
	// the test even when the kill-session cleanup fails.
	testguard.IsolateTmux(t)
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Unique throwaway title so we never collide with a real af session, and
	// derive the tmux name the SAME way the app does rather than hardcoding a
	// format string (the app's scheme is not the issue's `af_<hash>_<title>`).
	const program = "claude"
	orphanTitle := fmt.Sprintf("orphan-916-%d", time.Now().UnixNano())
	orphan := tmux.NewTmuxSessionForRepo(orphanTitle, repo.Root, program)
	tmuxName := orphan.SanitizedName()

	if out, err := exec.Command("tmux", "new-session", "-d", "-s", tmuxName).CombinedOutput(); err != nil {
		t.Skipf("cannot create tmux session (no usable tmux server?): %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", tmuxName).Run()
	})

	// The orphan must exist in tmux but carry no daemon/disk record.
	if !tmux.NewTmuxSessionForRepo(orphanTitle, repo.Root, program).DoesSessionExist() {
		t.Fatal("orphan tmux session should exist after creation")
	}
	if exists, _, _, err := manager.targetSessionState(repo.ID, orphanTitle); err != nil {
		t.Fatalf("targetSessionState: %v", err)
	} else if exists {
		t.Fatal("orphan title should NOT exist in daemon state")
	}

	start := time.Now()
	_, err = manager.DeliverPrompt(DeliverPromptRequest{
		Title:    orphanTitle,
		RepoPath: repoPath,
		Program:  program,
		Prompt:   "test",
	})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("DeliverPrompt took %v; expected immediate return for a tmux orphan, not a wait-out of the %v concurrent-create timeout", elapsed, targetDeliverWait)
	}
	if err == nil {
		t.Fatal("expected a tmux-conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "tmux session") {
		t.Errorf("expected error to mention the tmux conflict, got: %v", err)
	}
}
