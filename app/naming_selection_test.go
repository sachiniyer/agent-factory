package app

import (
	"errors"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// The naming row's title is a draft. While the create form is open it can spell
// an existing session's title for a keystroke, and nothing that decides the
// selection may read that as "the same session" (#7060). These tests drive the
// form the way a user does and take a frame (h.View) after every step, because
// the rail re-derives its rows — and commits whatever row its cursor lands on —
// at the next read, not at the keystroke.

// namingSelectionHome is a home with live, expandable sessions on the rail: no
// in-flight op, so whichever of them is the selection auto-expands its Agent tab
// row, exactly as the play-test sandbox's sessions do.
func namingSelectionHome(t *testing.T, titles ...string) (*home, []*session.Instance) {
	t.Helper()
	h := activeProjectHome(t)
	h.errBox.SetSize(200, 1)
	resizeHome(h, 80, 24)
	live := make([]*session.Instance, 0, len(titles))
	for _, title := range titles {
		inst := instanceWithFakeBackend(t, title)
		h.store.AddInstance(inst)
		live = append(live, inst)
	}
	// Every snapshot row here is a session the store already holds, so the
	// reconcile updates it in place. A build means the reconcile mistook a row
	// for a new or replaced session; refuse it rather than reattach real tmux.
	stubSnapshotBuilds(t, nil)
	return h, live
}

// stubSnapshotBuilds answers the reconcile's instance builder with the given
// prebuilt rows by ID and refuses any other build.
func stubSnapshotBuilds(t *testing.T, rows ...*session.Instance) {
	t.Helper()
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		for _, inst := range rows {
			if inst != nil && inst.ID == d.ID {
				return inst, nil
			}
		}
		return nil, fmt.Errorf("unexpected snapshot build of %q", d.Title)
	}))
}

// snapshotOf is the daemon's view of rows the store already holds: the same
// sessions (same IDs), so a reconcile updates them in place.
func snapshotOf(rows ...*session.Instance) []session.InstanceData {
	data := make([]session.InstanceData, 0, len(rows))
	for _, inst := range rows {
		d := inst.ToInstanceData()
		if d.Liveness != session.LiveArchived {
			d.Liveness = session.LiveRunning
		}
		data = append(data, d)
	}
	return data
}

// stubStartedSession answers the create with a fresh started session carrying
// the requested title, the shape startSessionThroughDaemon returns.
func stubStartedSession(t *testing.T) {
	t.Helper()
	t.Cleanup(SetSessionStarterForTest(func(_ *session.Instance, req sessionStartRequest) (*session.Instance, error) {
		started := newLoadingInstance(t, req.Title)
		started.SetBackend(session.NewFakeBackend())
		started.SetStartedForTest(true)
		return started, nil
	}))
}

// requireNamingRowSelected takes a frame and asserts both halves of the
// selection: the rail cursor's row and the store's display binding, which is
// what the workspace preview renders.
func requireNamingRowSelected(t *testing.T, h *home, naming *session.Instance, step string) {
	t.Helper()
	_ = h.View()
	got := h.sidebar.GetSelectedInstance()
	require.Samef(t, naming, got, "%s: the rail cursor left the naming row for %q", step, titleOf(got))
	bound := h.store.GetSelectedInstance()
	require.Samef(t, naming, bound, "%s: the workspace binding left the naming row for %q", step, titleOf(bound))
}

func titleOf(inst *session.Instance) string {
	if inst == nil {
		return "<nil>"
	}
	return inst.Title
}

// TestNamingRowKeepsSelectionThroughAnExistingTitle is the #7060 repro: type a
// prefix of an existing title, then the exact title, then a free suffix, and
// submit. The exact match used to auto-expand the EXISTING same-title session
// (the rail matched "is this the selection?" by title), its tab row shifted every
// row below it, and the cursor — which keeps its flat index across a rebuild —
// landed on the unrelated row above the naming row and committed it. Nothing
// moved it back, so the create completed unselected and its pane never opened.
func TestNamingRowKeepsSelectionThroughAnExistingTitle(t *testing.T) {
	h, live := namingSelectionHome(t, "alpha", "gamma", "delta", "epsilon")
	stubStartedSession(t)

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NotNil(t, naming)
	requireNamingRowSelected(t, h, naming, "form opened")

	typeRunes(t, h, "alph")
	requireNamingRowSelected(t, h, naming, "prefix of an existing title")

	typeRunes(t, h, "a")
	require.Equal(t, "alpha", naming.Title)
	requireNamingRowSelected(t, h, naming, "exact match of an existing title")

	// A snapshot poll landing on the exact match re-pins the selection it
	// captured. The naming row is not in the snapshot; the re-pin must not
	// resolve its draft title to the session that owns that title.
	h.reconcileSnapshot(snapshotOf(live...))
	requireNamingRowSelected(t, h, naming, "snapshot reconcile during the exact match")

	// Submitting the exact match is refused as a conflict and the form stays.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state, "a duplicate title is refused, not submitted")
	require.Contains(t, h.errBox.String(), "conflicts with existing session")
	requireNamingRowSelected(t, h, naming, "after the conflict refusal")

	typeRunes(t, h, "x")
	requireNamingRowSelected(t, h, naming, "free suffix past the existing title")

	require.True(t, submitNaming(t, h), "the create must be submitted")
	created := h.sidebar.GetSelectedInstance()
	require.NotNil(t, created, "a successful create must land the selection on the new session")
	require.Equal(t, "alphax", created.Title)
	require.NotSame(t, naming, created, "the started session replaces the placeholder row")
	require.Same(t, created, h.store.GetSelectedInstance())
	require.Equal(t, 1, h.store.NumOpenPanes(), "an empty workspace opens the new session's pane")
	require.Same(t, created, h.store.OpenPanes()[0].Instance())
	require.Equal(t, stateHelp, h.state, "the user still watching sees the start help, as on a clean create")
}

// TestNamingRowSurvivesASnapshotWhileSpellingAnArchivedTitle isolates the
// reconcile's re-pin from the rail's expansion: an archived row never expands in
// the live tree, so only the snapshot path can move the cursor. It re-resolved
// the captured selection by title, found the archived session first, and pulled
// the cursor into the Archived folder.
func TestNamingRowSurvivesASnapshotWhileSpellingAnArchivedTitle(t *testing.T) {
	h, live := namingSelectionHome(t, "alpha", "beta", "gamma")
	beta := live[1]
	beta.SetStatusForTest(session.Archived)
	require.True(t, beta.ShownArchived(), "precondition: beta sits in the Archived folder")
	// The user has the Archived folder open, so its rows are cursor targets.
	h.sidebar.ClickHeaderKind(ui.SectionArchived)

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NotNil(t, naming)

	typeRunes(t, h, "beta")
	requireNamingRowSelected(t, h, naming, "exact match of an archived title")

	h.reconcileSnapshot(snapshotOf(live...))
	requireNamingRowSelected(t, h, naming, "snapshot reconcile during the exact match")
}

// TestRestoredDraftKeepsSelectionBesideTheSessionItCollidesWith is the retry
// after a daemon-side conflict: another client created "delta" while this
// create was in flight, the daemon refused it, and the form reopened on the
// draft. The next snapshot carries the other client's "delta". That session must
// reach the rail — the reconcile used to match it to the naming row by title and
// skip it — and the naming row must keep the selection beside it.
func TestRestoredDraftKeepsSelectionBesideTheSessionItCollidesWith(t *testing.T) {
	h, live := namingSelectionHome(t, "alpha", "gamma", "epsilon")

	// The submitted placeholder: newLoadingInstance already holds OpCreating.
	placeholder := newLoadingInstance(t, "delta")
	placeholder.Path = h.repoRoot
	h.store.AddInstance(placeholder)
	h.sidebar.SelectInstance(placeholder)

	req := sessionStartRequest{Title: "delta", RepoPath: h.repoRoot}
	_, _ = h.Update(instanceStartedMsg{
		instance: placeholder, draft: &req,
		err: errors.New(`a session titled "delta" already exists`),
	})
	require.Equal(t, stateNew, h.state, "the refused draft reopens the form")
	naming := h.namingInstance
	require.NotNil(t, naming)
	require.Equal(t, "delta", naming.Title)
	requireNamingRowSelected(t, h, naming, "restored draft")

	other := instanceWithFakeBackend(t, "delta")
	stubSnapshotBuilds(t, other)
	h.reconcileSnapshot(snapshotOf(append(append([]*session.Instance(nil), live...), other)...))

	var found bool
	for _, inst := range h.store.GetInstances() {
		if inst != naming && inst.Title == "delta" {
			found = true
		}
	}
	require.True(t, found, "the other client's delta must reach the rail beside the naming row")
	requireNamingRowSelected(t, h, naming, "snapshot adds the colliding session")
}
