package daemon

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
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
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
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
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
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
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
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
	inst.SetStatus(session.Deleting)

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

// TestWaitForTargetSession_ReturnsWhenSessionAppears covers the cross-process
// fallback's wait: a session that materializes after a brief delay is picked up
// rather than timing out, while a Deleting one is surfaced as an error.
func TestWaitForTargetSession_ReturnsWhenSessionAppears(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
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
		if _, err := manager.CreateSession(CreateSessionRequest{
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
		{"reserved", fmt.Errorf("session with title %q is already reserved", "captain"), true},
		{"exists", fmt.Errorf("session with title %q already exists", "captain"), true},
		{"branch collision", fmt.Errorf("session titled %q conflicts with existing session %q: both sanitize to the same git branch %q", "A B", "a-b", "af-a-b"), false},
		{"nil", nil, false},
		{"unrelated", fmt.Errorf("git is not installed"), false},
	}
	for _, tc := range cases {
		if got := isConcurrentCreateErr(tc.err); got != tc.want {
			t.Errorf("%s: isConcurrentCreateErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}
