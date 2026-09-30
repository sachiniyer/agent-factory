package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// stubRebindCAS makes the daemon seam run the real compare-and-set rebind and
// answer a precondition refusal the way the HTTP client does, recording the
// expected (root, checkout) pair each request carried.
func stubRebindCAS(t *testing.T) *[][2]string {
	t.Helper()
	var expected [][2]string
	old := rebindProjectThroughDaemon
	rebindProjectThroughDaemon = func(projectID, expectedRoot, expectedCheckoutID, path string) (config.Project, error) {
		expected = append(expected, [2]string{expectedRoot, expectedCheckoutID})
		project, err := config.RebindProjectIfRoot(projectID, expectedRoot, expectedCheckoutID, path)
		var rebound *config.ProjectReboundError
		if errors.As(err, &rebound) {
			return config.Project{}, &apiclient.ProjectReboundError{Detail: err.Error()}
		}
		return project, err
	}
	t.Cleanup(func() { rebindProjectThroughDaemon = old })
	return &expected
}

// TestRebindConflictIsARefusalThatReArmsAgainstTheCurrentRoot pins #4822 on
// the TUI: another client rebinds the project after this picker read it. The
// picker's rebind carries the root it displayed, the daemon refuses it, and the
// picker treats that as a definitive refusal — open, re-armed, naming the root
// the registry holds now — whose next Enter expects that root and lands.
func TestRebindConflictIsARefusalThatReArmsAgainstTheCurrentRoot(t *testing.T) {
	h, id := activeRebindHome(t)
	displayed := h.projectPickerOverlay
	expected := stubRebindCAS(t)
	var recorded, recordedCheckout string
	projects, err := config.ListProjects()
	require.NoError(t, err)
	for _, p := range projects {
		if p.ID == id {
			recorded, recordedCheckout = p.Root, p.CheckoutID
		}
	}
	require.NotEmpty(t, recorded)
	require.NotEmpty(t, recordedCheckout)

	elsewhere := initTestGitRepo(t)
	moved, err := config.RebindProject(id, elsewhere) // another client, meanwhile
	require.NoError(t, err)
	mine := initTestGitRepo(t)

	_, cmd := h.Update(submitPickerRebind(t, h, mine)())
	require.NotNil(t, cmd, "the conflict refresh is dispatched off the event loop")
	h.Update(cmd())

	require.Equal(t, [][2]string{{recorded, recordedCheckout}}, *expected,
		"the rebind must carry the (root, checkout) pair the picker displayed")
	require.Same(t, displayed, h.projectPickerOverlay, "a conflict is a refusal: the picker stays open")
	assert.Equal(t, stateSwitchProject, h.state)
	assert.False(t, displayed.RebindPending(), "a definitive refusal re-arms the form")
	assert.Contains(t, displayed.Render(), "Rebound elsewhere", "the refusal says the project moved")
	assert.Empty(t, h.errBox.FullError(), "a conflict is not reported as an unknown outcome or a failure")
	root, ok := registeredProjectRoot(id)
	require.True(t, ok)
	assert.Equal(t, moved.Root, root, "the refused rebind wrote nothing")

	// Esc leaves the re-armed form for the list — which the conflict handler
	// rebuilt from the fresh registry read. The row must describe the rebound
	// registration, not the checkout the picker was opened on (#4888 review):
	// an Esc onto a stale row could select a path the registry dropped.
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEsc})
	row, ok := h.projectPickerOverlay.HighlightedProject()
	require.True(t, ok, "the rebuilt list still highlights a row")
	assert.Equal(t, moved.Root, row.Root, "the row's root is where the project is bound now")
	assert.Equal(t, config.RepoIDFromRoot(moved.Root), row.RepoID, "the row carries the rebound repo identity")
	assert.False(t, row.MissingPath, "the rebound checkout exists — the stale row claimed it did not")

	// Re-armed on that fresh row, Enter retries — expecting the root the
	// rebuilt row reports.
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(mine)})
	_, cmd = h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "the re-armed form submits again")
	h.Update(cmd())

	require.Len(t, *expected, 2)
	assert.Equal(t, [2]string{moved.Root, moved.CheckoutID}, (*expected)[1],
		"the retry expects the (root, checkout) pair the refresh found")
	root, ok = registeredProjectRoot(id)
	require.True(t, ok)
	assert.Equal(t, mine, root, "the retry, made against the current root, lands")
}

// TestRebindConflictWithAVanishedRecordStillRebuildsTheList pins the second
// #4888-review round on the conflict handler: the re-arm and the rebuilt list
// must come from the SAME registry read. When the record is deleted between
// the daemon's refusal and the TUI's re-read, the previous code skipped the
// rebuild entirely — the follow-up root read had nothing to name — leaving a
// row for a registration the registry dropped selectable. The rebuild must run
// regardless; the vanished record's row must be gone, and the form re-arms on
// the daemon's own refusal text.
func TestRebindConflictWithAVanishedRecordStillRebuildsTheList(t *testing.T) {
	h, id := activeRebindHome(t)
	displayed := h.projectPickerOverlay

	old := rebindProjectThroughDaemon
	rebindProjectThroughDaemon = func(projectID, _, _, path string) (config.Project, error) {
		// The daemon already computed its refusal; before this reply reaches
		// the TUI another client deletes the record outright.
		dir, err := config.ProjectRegistryDir()
		require.NoError(t, err)
		require.NoError(t, os.RemoveAll(filepath.Join(dir, projectID)))
		return config.Project{}, &apiclient.ProjectReboundError{Detail: "rebind project: project " + projectID + " was rebound elsewhere — refresh and retry"}
	}
	t.Cleanup(func() { rebindProjectThroughDaemon = old })

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd, "the conflict refresh is dispatched off the event loop")
	h.Update(cmd())

	require.Same(t, displayed, h.projectPickerOverlay, "a conflict is a refusal: the picker stays open")
	assert.False(t, displayed.RebindPending(), "a definitive refusal re-arms the form")
	// The daemon's full refusal is longer than the overlay's rendered line
	// width — raise the layout cap too, or SetWidth loses to the maxSize the
	// picker was opened with and the refusal is clipped before its tail.
	displayed.SetMaxSize(400, 60)
	displayed.SetWidth(200)
	assert.Contains(t, displayed.Render(), "rebound elsewhere", "the daemon's refusal text re-arms the form")

	// Esc back to the list: the rebuild ran even with the record gone, so no
	// row for the dropped registration can be highlighted or selected.
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEsc})
	if row, ok := displayed.HighlightedProject(); ok && row.RegistryID == id {
		t.Fatalf("a deleted record's stale row must not survive the conflict rebuild, got %+v", row)
	}
}

// TestRebindConflictRefreshRunsOffTheEventLoop pins the #4888 round-4 review
// finding on the conflict handler: the cross-repo snapshot fetch is a local
// API call with no response deadline, so run inside Update a stalled one
// freezes input and paint for the life of the stall — exactly while the
// refused rebind waits to re-arm. The refusal must hand the fetch to a tea.Cmd
// and keep the form inert until the reply lands.
func TestRebindConflictRefreshRunsOffTheEventLoop(t *testing.T) {
	h, id := activeRebindHome(t)
	displayed := h.projectPickerOverlay
	moved := initTestGitRepo(t)
	_, err := config.RebindProject(id, moved) // another client, meanwhile
	require.NoError(t, err)
	stubRebindCAS(t)

	fetched := make(chan struct{}, 1)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		fetched <- struct{}{}
		return nil, errors.New("daemon snapshot unavailable")
	}))

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())

	select {
	case <-fetched:
		t.Fatal("the conflict snapshot fetch must not run inside Update")
	default:
	}
	require.NotNil(t, cmd, "the conflict refresh is dispatched as a command")
	assert.True(t, displayed.RebindPending(), "the form stays inert while the refresh is in flight")

	h.Update(cmd())

	select {
	case <-fetched:
	default:
		t.Fatal("the dispatched command must perform the snapshot fetch")
	}
	assert.False(t, displayed.RebindPending(), "the landed reply re-arms the form")
	assert.Contains(t, displayed.Render(), "Rebound elsewhere")
}

// TestRebindConflictWithAFailedSnapshotKeepsSessionRows pins the other half of
// the same finding: when that off-loop fetch fails, the rebuilt list carries
// the registry-side union only — installed wholesale it would drop every
// session-derived row and zero the counts the daemon still runs. The merge
// must keep session-derived rows and re-attach each surviving registry row's
// last-known counts, while still refreshing the record that re-arms.
func TestRebindConflictWithAFailedSnapshotKeepsSessionRows(t *testing.T) {
	h, id := activeRebindHome(t)
	root, ok := registeredProjectRoot(id)
	require.True(t, ok)
	var recordedCheckout string
	projects, err := config.ListProjects()
	require.NoError(t, err)
	for _, p := range projects {
		if p.ID == id {
			recordedCheckout = p.CheckoutID
		}
	}
	require.NotEmpty(t, recordedCheckout)
	sessionOnly := initTestGitRepo(t)
	// The picker lists the registered row beside one that only live sessions
	// produce — the row a failed snapshot cannot re-derive.
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay([]overlay.Project{
		{Name: "active", Root: root, RepoID: h.repoID, RegistryID: id, RegistryRoot: root, RegistryCheckoutID: recordedCheckout, MissingPath: true, SessionCount: 4, InPlaceCount: 1},
		{Name: "sessonly", Root: sessionOnly, RepoID: config.RepoIDFromRoot(sessionOnly), SessionCount: 3},
	}, h.repoRoot)
	h.projectPickerOverlay.SetMaxSize(80, 24)
	displayed := h.projectPickerOverlay

	moved := initTestGitRepo(t)
	_, err = config.RebindProject(id, moved) // another client, meanwhile
	require.NoError(t, err)
	stubRebindCAS(t)

	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return nil, errors.New("daemon snapshot unavailable")
	}))

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd)
	h.Update(cmd())

	assert.False(t, displayed.RebindPending(), "the reply still re-arms the form on a failed fetch")
	displayed.SetWidth(200)
	assert.Contains(t, displayed.Render(), "Rebound elsewhere")

	// Esc to the list: the session-derived row survived, and the registry row
	// refreshed to the rebound root with its last-known counts carried over.
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEsc})
	row, ok := displayed.HighlightedProject()
	require.True(t, ok, "the rebuilt list still highlights a row")
	assert.Equal(t, id, row.RegistryID, "the cursor lands on the rebound record")
	assert.Equal(t, moved, row.Root, "the registry row still refreshes to where the project is bound now")
	assert.Equal(t, 4, row.SessionCount, "the failed snapshot could not recount — the row keeps its last count")
	assert.Equal(t, 1, row.InPlaceCount)
	assert.Contains(t, displayed.Render(), "sessonly",
		"a session-derived row must survive the failed snapshot refresh")
}

// TestRebindConflictRefreshTimeoutReArmsTheForm pins the #4888 round-6 review
// finding: the conflict refresh's cross-repo fetch can accept the connection
// and never answer, and a fetch with no deadline leaves rebindPending — and
// every key the inert form consumes, Esc included — stuck for the life of the
// stall. The fetch is bounded; at the deadline the reply lands as a failed
// fetch and the form re-arms on the daemon's refusal.
func TestRebindConflictRefreshTimeoutReArmsTheForm(t *testing.T) {
	h, id := activeRebindHome(t)
	displayed := h.projectPickerOverlay
	moved := initTestGitRepo(t)
	_, err := config.RebindProject(id, moved) // another client, meanwhile
	require.NoError(t, err)
	stubRebindCAS(t)

	old := rebindConflictRefreshTimeout
	rebindConflictRefreshTimeout = 50 * time.Millisecond
	t.Cleanup(func() { rebindConflictRefreshTimeout = old })

	release := make(chan struct{})
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		<-release // never answered: the fetch hangs for the life of the test
		return nil, nil
	}))
	t.Cleanup(func() { close(release) })

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd, "the conflict refresh is dispatched off the event loop")
	assert.True(t, displayed.RebindPending(), "the form stays inert while the refresh is in flight")

	// Run the refresh beside a test-level deadline: without the bound the cmd
	// never returns, and that must read as an assertion failure — a hung test
	// would reproduce the finding but prove nothing about where it ends.
	replied := make(chan tea.Msg, 1)
	go func() { replied <- cmd() }()
	select {
	case snapshotMsg := <-replied:
		h.Update(snapshotMsg) // the ~50ms bound elapsed; the reply lands as a failed fetch
	case <-time.After(5 * time.Second):
		t.Fatal("the conflict refresh must be bounded — the fetch never answered")
	}

	assert.False(t, displayed.RebindPending(),
		"a refresh that never answers must still re-arm the form at the deadline")
	displayed.SetMaxSize(400, 60)
	displayed.SetWidth(200)
	assert.Contains(t, displayed.Render(), "Rebound elsewhere",
		"the re-armed form names the daemon's refusal")
}
