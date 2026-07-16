package daemon

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The daemon half of the stable-tab-id addressing contract (#1738, completed in
// #1779): once a client addresses a tab by its STABLE id, no daemon path may fall
// back to a POSITIONAL tab. A stale or unknown id is refused — reported gone —
// because the tab that now sits at the old ordinal is a DIFFERENT tab, and
// serving it is the precise misroute the stable id was introduced to prevent.
//
// These cover the two entry points that took an id and could silently degrade to
// an ordinal: the Preview control RPC and the WS PTY stream's tab binding.

// TestPreview_RefusesStaleTabID is the regression test for the ordinal fallback
// (#1779): with [agent, a, b], closing `a` shifts `b` into a's old ordinal. A
// Preview still addressed by a's id must report GONE — not capture b, which is
// what leaving the request's stale ordinal in place used to do.
func TestPreview_RefusesStaleTabID(t *testing.T) {
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

	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	a, err := inst.AddProcessTab("a", "")
	if err != nil {
		t.Fatalf("AddProcessTab a: %v", err)
	}
	b, err := inst.AddProcessTab("b", "")
	if err != nil {
		t.Fatalf("AddProcessTab b: %v", err)
	}

	// Close a (ordinal 1); b shifts down into ordinal 1.
	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: "a"}); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	if idx, ok := inst.TabIndexByID(b.ID); !ok || idx != 1 {
		t.Fatalf("tab b must have shifted to ordinal 1, got idx=%d ok=%v", idx, ok)
	}

	cs := &controlServer{manager: manager}
	// The client still holds a's id AND its captured ordinal (1) — which now names b.
	// The id is the authority: a is gone, so the capture is refused.
	var resp PreviewResponse
	if err := cs.Preview(PreviewRequest{Title: title, RepoID: repo.ID, TabID: a.ID, Tab: 1}, &resp); err != nil {
		t.Fatalf("Preview with a stale tab id must not error, it must report gone: %v", err)
	}
	if !resp.Gone {
		t.Fatal("Preview addressed by a CLOSED tab's id must report gone, not fall back to the ordinal")
	}
	if resp.Content != "" {
		t.Fatalf("a refused Preview must capture nothing, got %q — it previewed the tab at the stale ordinal", resp.Content)
	}

	// An id that was never minted is likewise gone rather than a positional capture.
	resp = PreviewResponse{}
	if err := cs.Preview(PreviewRequest{Title: title, RepoID: repo.ID, TabID: "never-minted", Tab: 0}, &resp); err != nil {
		t.Fatalf("Preview with an unknown tab id must not error: %v", err)
	}
	if !resp.Gone {
		t.Fatal("Preview addressed by an UNKNOWN tab id must report gone, not capture tab 0")
	}
}

// TestPreview_NoTabIDStillUsesOrdinal pins the legacy path the refusal must not
// break: a client that supplies NO tab id (pre-#1738) has only ever addressed
// tabs positionally, so its ?tab= ordinal is still honored. The refusal keys on a
// non-empty-but-unresolvable id, not on "no id".
func TestPreview_NoTabIDStillUsesOrdinal(t *testing.T) {
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

	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	if _, err := inst.AddProcessTab("a", ""); err != nil {
		t.Fatalf("AddProcessTab: %v", err)
	}

	cs := &controlServer{manager: manager}
	var resp PreviewResponse
	if err := cs.Preview(PreviewRequest{Title: title, RepoID: repo.ID, Tab: 1}, &resp); err != nil {
		t.Fatalf("Preview without a tab id must still capture by ordinal: %v", err)
	}
	if resp.Gone {
		t.Fatal("a legacy no-tab_id Preview must capture by ordinal, not report gone")
	}
}

// TestBindTab_RefusesStaleID: the WS stream's tab binding refuses a stale/unknown
// stable id up front (404), rather than binding whatever tab now holds the old
// ordinal. This is the subscribe-path half of the same guarantee.
func TestBindTab_RefusesStaleID(t *testing.T) {
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

	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	a, err := inst.AddProcessTab("a", "")
	if err != nil {
		t.Fatalf("AddProcessTab a: %v", err)
	}
	b, err := inst.AddProcessTab("b", "")
	if err != nil {
		t.Fatalf("AddProcessTab b: %v", err)
	}
	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: "a"}); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}

	cs := &controlServer{manager: manager}
	as := inst.AgentServer()

	// bindTab never resolves an id up front — the refusal surfaces at the operation,
	// which is the point where the id is resolved atomically.
	binding, err := cs.bindTab(as, inst, a.ID, 1)
	if err != nil {
		t.Fatalf("bindTab: %v", err)
	}
	if _, serr := binding.subscribe(0); serr == nil {
		t.Fatal("subscribing by a CLOSED tab's id must be refused, not bound to the tab at its old ordinal")
	} else if !errors.Is(serr, session.ErrTabGone) {
		t.Fatalf("a stale tab id must be refused with ErrTabGone, got %v", serr)
	} else if got := httpStatusForTab(serr); got != 404 {
		t.Fatalf("a stale tab id must map to HTTP 404, got %d", got)
	}
	// The WRITE half is refused too — a keystroke must never reach the tab that
	// inherited the closed tab's ordinal.
	if werr := binding.input([]byte("x")); !errors.Is(werr, session.ErrTabGone) {
		t.Fatalf("input to a stale tab id must be refused with ErrTabGone, got %v", werr)
	}

	// The surviving tab's own id still binds and subscribes.
	binding, err = cs.bindTab(as, inst, b.ID, 0)
	if err != nil {
		t.Fatalf("a live tab's stable id must bind: %v", err)
	}
	sub, err := binding.subscribe(0)
	if err != nil {
		t.Fatalf("a live tab's stable id must subscribe: %v", err)
	}
	_ = sub.Close()
}

// TestBindTab_NoTabIDPinsOrdinal: a legacy client that supplies no ?tab_id= keeps
// the pre-#1738 positional behavior — the binding pins the ordinal it asked for
// and never consults the id map.
func TestBindTab_NoTabIDPinsOrdinal(t *testing.T) {
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

	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	if _, err := inst.AddProcessTab("a", ""); err != nil {
		t.Fatalf("AddProcessTab: %v", err)
	}

	cs := &controlServer{manager: manager}
	binding, err := cs.bindTab(inst.AgentServer(), inst, "", 1)
	if err != nil {
		t.Fatalf("a legacy no-tab_id bind must succeed: %v", err)
	}
	ord, ok := binding.(ordinalTabBinding)
	if !ok {
		t.Fatalf("no ?tab_id= must pin the ordinal binding, got %T", binding)
	}
	if ord.tab != 1 {
		t.Fatalf("the legacy binding must pin the requested ordinal 1, got %d", ord.tab)
	}
}

// TestBindTab_LiveIDBindsIDNativePlane: with a ?tab_id= against a runtime that has
// an id-native plane (the local agent-server), the binding must be the ID one —
// NOT an ordinal binding derived by resolving the id up front. Resolving to an
// ordinal here is what reopened the race (#1779): a close between the resolution
// and the subscribe shifts a different tab under the captured index.
func TestBindTab_LiveIDBindsIDNativePlane(t *testing.T) {
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

	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	b, err := inst.AddProcessTab("b", "")
	if err != nil {
		t.Fatalf("AddProcessTab: %v", err)
	}

	cs := &controlServer{manager: manager}
	binding, err := cs.bindTab(inst.AgentServer(), inst, b.ID, 0)
	if err != nil {
		t.Fatalf("bindTab: %v", err)
	}
	idb, ok := binding.(idTabBinding)
	if !ok {
		t.Fatalf("a ?tab_id= against an id-native runtime must bind by id, got %T — an ordinal binding reintroduces the resolve-then-subscribe race", binding)
	}
	if idb.tabID != b.ID {
		t.Fatalf("the binding must carry the stable id %q, got %q", b.ID, idb.tabID)
	}
}

// TestBindTab_IDAddressedNeverPinsAnOrdinal is the invariant behind the whole
// follow-up, asserted at the seam where it could regress: an id-addressed
// connection must NEVER end up on ordinalTabBinding, whatever the runtime shape.
// A runtime with an id-native plane binds by id; one without (the ordinal-shaped
// remote wire protocol) re-resolves per operation. Pinning an ordinal at bind time
// — which an earlier revision of this change did for the remote path — means a tab
// that shifts mid-connection sends later input to whatever now holds the old index
// (#1779).
func TestBindTab_IDAddressedNeverPinsAnOrdinal(t *testing.T) {
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

	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	b, err := inst.AddProcessTab("b", "")
	if err != nil {
		t.Fatalf("AddProcessTab: %v", err)
	}
	cs := &controlServer{manager: manager}

	// The id-native runtime.
	binding, err := cs.bindTab(inst.AgentServer(), inst, b.ID, 0)
	if err != nil {
		t.Fatalf("bindTab: %v", err)
	}
	if _, pinned := binding.(ordinalTabBinding); pinned {
		t.Fatal("an id-addressed connection must never pin a fixed ordinal")
	}

	// A runtime with NO id-native plane must re-resolve, not pin.
	binding, err = cs.bindTab(ordinalOnlyServer{}, inst, b.ID, 0)
	if err != nil {
		t.Fatalf("bindTab (ordinal-only runtime): %v", err)
	}
	if _, pinned := binding.(ordinalTabBinding); pinned {
		t.Fatal("an id-addressed connection to an ordinal-shaped runtime must re-resolve per op, not pin the bind-time ordinal")
	}
	rb, ok := binding.(resolvingTabBinding)
	if !ok {
		t.Fatalf("expected a re-resolving binding for an ordinal-only runtime, got %T", binding)
	}
	if rb.tabID != b.ID {
		t.Fatalf("the re-resolving binding must carry the stable id %q, got %q", b.ID, rb.tabID)
	}

	// It resolves the id to the tab's CURRENT ordinal on every op — so a shift is
	// followed, not misrouted — and refuses an id that names no live tab.
	if idx, rerr := rb.resolve(); rerr != nil || idx != 1 {
		t.Fatalf("re-resolve = (%d, %v), want (1, nil)", idx, rerr)
	}
	gone := resolvingTabBinding{as: ordinalOnlyServer{}, instance: inst, tabID: "never-minted"}
	if _, rerr := gone.resolve(); !errors.Is(rerr, session.ErrTabGone) {
		t.Fatalf("an unknown id must re-resolve to ErrTabGone, got %v", rerr)
	}
}

// ordinalOnlyServer is an AgentServer with NO id-native plane — the shape of the
// remote agent-server, whose wire protocol is ordinal-keyed. It exists to pin
// bindTab's runtime-shape branch; none of its methods are driven.
type ordinalOnlyServer struct{ session.AgentServer }
