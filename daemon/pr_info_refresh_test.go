package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// TestRefreshPRInfos_DiscoversWithoutATUI is the #3232 regression: PR
// discovery must be daemon-owned, so a session used only through the web or CLI
// still gains its PR projection without opening the TUI.
func TestRefreshPRInfos_DiscoversWithoutATUI(t *testing.T) {
	const title = "web-only"
	manager, repo := tabEventSession(t, title)
	inst, _, _, err := manager.findSession(title, repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}

	var fetchedRepo, fetchedBranch string
	manager.prInfoFetcher = func(_ context.Context, repoPath, branch string) (*git.PRInfo, error) {
		fetchedRepo, fetchedBranch = repoPath, branch
		return &git.PRInfo{
			Number: 3232,
			Title:  "daemon-owned PR discovery",
			URL:    "https://example.test/pull/3232",
			State:  "OPEN",
		}, nil
	}

	eventID, events := manager.events.subscribe()
	defer manager.events.unsubscribe(eventID)
	if err := manager.refreshPRInfos(context.Background()); err != nil {
		t.Fatalf("refreshPRInfos: %v", err)
	}
	if fetchedRepo == "" || fetchedBranch != title+"-branch" {
		t.Fatalf("PR lookup target = (%q, %q), want a repo and branch %q",
			fetchedRepo, fetchedBranch, title+"-branch")
	}

	got := inst.GetPRInfo()
	if got == nil || got.Number != 3232 || got.Branch != title+"-branch" {
		t.Fatalf("daemon-refreshed PR info = %+v", got)
	}
	event := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	if event.PRInfo.Number != 3232 || event.PRInfo.Branch != title+"-branch" {
		t.Fatalf("session.updated PR info = %+v", event.PRInfo)
	}
}

func TestRefreshPRInfos_DebouncesFreshSession(t *testing.T) {
	manager, _ := tabEventSession(t, "debounced")
	calls := 0
	manager.prInfoFetcher = func(context.Context, string, string) (*git.PRInfo, error) {
		calls++
		return &git.PRInfo{Number: 7, State: "OPEN"}, nil
	}

	if err := manager.refreshPRInfos(context.Background()); err != nil {
		t.Fatalf("first refreshPRInfos: %v", err)
	}
	if err := manager.refreshPRInfos(context.Background()); err != nil {
		t.Fatalf("second refreshPRInfos: %v", err)
	}
	if calls != 1 {
		t.Fatalf("PR fetch calls = %d, want 1 within the stale window", calls)
	}
}

func TestRefreshPRInfos_RefreshesEveryLocalSession(t *testing.T) {
	manager, repo := tabEventSession(t, "alpha")
	alpha, _, _, err := manager.findSession("alpha", repo.ID)
	if err != nil {
		t.Fatalf("findSession alpha: %v", err)
	}
	repoPath, _ := alpha.FetchPRInfoSnapshot()
	beta := startedLocalTabInstance(t, manager, repo.ID, repoPath, "beta", "af_beta_agent")
	if err := manager.SaveInstances(); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	var mu sync.Mutex
	fetched := map[string]bool{}
	manager.prInfoFetcher = func(_ context.Context, _ string, branch string) (*git.PRInfo, error) {
		mu.Lock()
		fetched[branch] = true
		mu.Unlock()
		return &git.PRInfo{Number: len(branch), State: "OPEN"}, nil
	}
	if err := manager.refreshPRInfos(context.Background()); err != nil {
		t.Fatalf("refreshPRInfos: %v", err)
	}

	mu.Lock()
	gotAlpha, gotBeta := fetched["alpha-branch"], fetched["beta-branch"]
	mu.Unlock()
	if !gotAlpha || !gotBeta {
		t.Fatalf("fetched branches = %v, want both local sessions", fetched)
	}
	if alpha.GetPRInfo() == nil || beta.GetPRInfo() == nil {
		t.Fatalf("PR info missing after fleet refresh: alpha=%+v beta=%+v", alpha.GetPRInfo(), beta.GetPRInfo())
	}
}

func TestRefreshPRInfos_SkipsRemoteSession(t *testing.T) {
	manager, repo := tabEventSession(t, "remote")
	inst, _, _, err := manager.findSession("remote", repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	inst.SetBackend(remoteTypeBackend{FakeBackend: session.NewFakeBackend()})
	calls := 0
	manager.prInfoFetcher = func(context.Context, string, string) (*git.PRInfo, error) {
		calls++
		return nil, nil
	}
	if err := manager.refreshPRInfos(context.Background()); err != nil {
		t.Fatalf("refreshPRInfos: %v", err)
	}
	if calls != 0 {
		t.Fatalf("remote PR fetch calls = %d, want 0", calls)
	}
}

func TestRefreshPRInfos_FetchErrorPreservesCachedInfo(t *testing.T) {
	const title = "cached"
	manager, repo := tabEventSession(t, title)
	inst, _, _, err := manager.findSession(title, repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	want := &git.PRInfo{Number: 8, State: "OPEN", Branch: title + "-branch"}
	inst.SetPRInfo(want)
	manager.prInfoStaleAfter = 0
	manager.prInfoFetcher = func(context.Context, string, string) (*git.PRInfo, error) {
		return nil, errors.New("GitHub unavailable")
	}

	if err := manager.refreshPRInfos(context.Background()); err != nil {
		t.Fatalf("refreshPRInfos: %v", err)
	}
	if got := inst.GetPRInfo(); got != want {
		t.Fatalf("cached PR info = %+v, want preserved %+v", got, want)
	}
}

func TestRefreshPRInfos_ClearsPRThatNoLongerExists(t *testing.T) {
	const title = "closed"
	manager, repo := tabEventSession(t, title)
	inst, _, _, err := manager.findSession(title, repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	if err := manager.SetPRInfo(SetPRInfoRequest{
		ID: inst.ID,
		PRInfo: session.PRInfoData{
			Number: 13, State: "CLOSED", Branch: title + "-branch",
		},
	}); err != nil {
		t.Fatalf("seed SetPRInfo: %v", err)
	}
	manager.prInfoStaleAfter = 0
	manager.prInfoFetcher = func(context.Context, string, string) (*git.PRInfo, error) {
		return nil, nil
	}

	if err := manager.refreshPRInfos(context.Background()); err != nil {
		t.Fatalf("refreshPRInfos: %v", err)
	}
	if got := inst.GetPRInfo(); got != nil {
		t.Fatalf("PR info after no-PR lookup = %+v, want nil", got)
	}
	if got := snapshotTabs(t, manager, repo.ID, title).PRInfo; got.Number != 0 {
		t.Fatalf("cleared PR remained in snapshot: %+v", got)
	}
}

func TestRefreshPRInfos_DropsResultAfterBranchChanges(t *testing.T) {
	const title = "branch-change"
	manager, repo := tabEventSession(t, title)
	inst, _, _, err := manager.findSession(title, repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	repoPath, _ := inst.FetchPRInfoSnapshot()
	started := make(chan struct{})
	release := make(chan struct{})
	manager.prInfoFetcher = func(context.Context, string, string) (*git.PRInfo, error) {
		close(started)
		<-release
		return &git.PRInfo{Number: 10, State: "OPEN"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- manager.refreshPRInfos(context.Background()) }()
	<-started

	worktree, err := git.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "replacement-worktree"), title,
		"replacement-branch", "", false, true,
	)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	inst.SetGitWorktreeForTest(worktree)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("refreshPRInfos: %v", err)
	}
	if got := inst.GetPRInfo(); got != nil {
		t.Fatalf("stale branch PR info was applied: %+v", got)
	}
}

func TestRefreshPRInfos_DropsResultAfterSameTitleReplacement(t *testing.T) {
	const title = "replacement"
	manager, repo := tabEventSession(t, title)
	original, _, _, err := manager.findSession(title, repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	repoPath, _ := original.FetchPRInfoSnapshot()
	started := make(chan struct{})
	release := make(chan struct{})
	manager.prInfoFetcher = func(context.Context, string, string) (*git.PRInfo, error) {
		close(started)
		<-release
		return &git.PRInfo{Number: 11, State: "OPEN"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- manager.refreshPRInfos(context.Background()) }()
	<-started

	replacement := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_replacement_2_agent")
	if replacement.ID == original.ID {
		t.Fatal("replacement unexpectedly reused the original stable id")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("refreshPRInfos: %v", err)
	}
	if got := replacement.GetPRInfo(); got != nil {
		t.Fatalf("old session's PR info landed on its replacement: %+v", got)
	}
}

func TestRefreshPRInfos_DropsResultAfterArchive(t *testing.T) {
	manager, repo := tabEventSession(t, "archived")
	inst, _, _, err := manager.findSession("archived", repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	manager.prInfoFetcher = func(context.Context, string, string) (*git.PRInfo, error) {
		close(started)
		<-release
		return &git.PRInfo{Number: 12, State: "OPEN"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- manager.refreshPRInfos(context.Background()) }()
	<-started
	inst.SetStatusForTest(session.Archived)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("refreshPRInfos: %v", err)
	}
	if got := inst.GetPRInfo(); got != nil {
		t.Fatalf("PR info was applied after archive: %+v", got)
	}
}

func TestPRInfoRefreshLoop_CancelsFetchOnShutdown(t *testing.T) {
	manager, _ := tabEventSession(t, "shutdown")
	started := make(chan struct{})
	manager.prInfoFetcher = func(ctx context.Context, _, _ string) (*git.PRInfo, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	startPRInfoRefreshLoop(manager, stop, &wg)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("PR refresh loop did not run its first pass")
	}
	close(stop)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("PR refresh loop did not join after shutdown cancellation")
	}
}
