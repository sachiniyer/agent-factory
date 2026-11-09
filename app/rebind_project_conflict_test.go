package app

import (
	"context"
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
	"github.com/sachiniyer/agent-factory/daemon"
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
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		fetched <- struct{}{}
		return nil, nil, errors.New("daemon snapshot unavailable")
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

	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return nil, nil, errors.New("daemon snapshot unavailable")
	}))

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd)
	h.Update(cmd())

	assert.False(t, displayed.RebindPending(), "the reply still re-arms the form on a failed fetch")
	displayed.SetWidth(200)
	assert.Contains(t, displayed.Render(), "Rebound elsewhere")

	// Esc to the list: the session-derived row survived, and the rebound
	// registration landed under the NEW repo — the merge splits on a RepoID
	// change (#4888 review), so the rebind target carries no borrowed counts
	// while the old repo keeps a session-only row with its own 4/1 tallies.
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEsc})
	row, ok := displayed.HighlightedProject()
	require.True(t, ok, "the rebuilt list still highlights a row")
	assert.Equal(t, id, row.RegistryID, "the cursor lands on the rebound record")
	assert.Equal(t, moved, row.Root, "the registry row still refreshes to where the project is bound now")
	assert.Equal(t, 0, row.SessionCount,
		"the moved registration must not inherit counts tallied against the old repo")
	var oldRepoRow *overlay.Project
	for i, r := range displayed.Projects() {
		if r.RegistryID == "" && r.Root == root {
			oldRepoRow = &displayed.Projects()[i]
		}
	}
	require.NotNil(t, oldRepoRow, "the old repo's sessions keep a session-only row")
	assert.Equal(t, 4, oldRepoRow.SessionCount, "the failed snapshot could not recount — the old repo keeps its last count")
	assert.Equal(t, 1, oldRepoRow.InPlaceCount)
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

	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(ctx context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		<-ctx.Done() // the daemon accepted and never answers; only the deadline frees the read
		return nil, nil, ctx.Err()
	}))

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

// TestRebindConflictWithADegradedRegistryKeepsRegistryRows pins the #4888
// review finding on the mirror-image failure: the conflict refresh's session
// snapshot LANDED but the registry read degraded. The fresh list carries live
// session rows and no registry records; a wholesale install would drop every
// registry-backed row behind a read that cannot prove absence — and leave the
// rebind form's target stale beside it. The merge preserves the picker's
// registry rows while live session truth replaces the session half.
func TestRebindConflictWithADegradedRegistryKeepsRegistryRows(t *testing.T) {
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
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay([]overlay.Project{
		{Name: "active", Root: root, RepoID: h.repoID, RegistryID: id, RegistryRoot: root, RegistryCheckoutID: recordedCheckout, MissingPath: true, SessionCount: 4, InPlaceCount: 1},
	}, h.repoRoot)
	h.projectPickerOverlay.SetMaxSize(80, 24)
	displayed := h.projectPickerOverlay

	moved := initTestGitRepo(t)
	_, err = config.RebindProject(id, moved) // another client, meanwhile
	require.NoError(t, err)
	stubRebindCAS(t)

	// The session half answers with a live session on an unrelated repo; the
	// registry read is degraded by a stray file under the test registry —
	// strict enumeration refuses it (#3297), which is exactly the read the
	// conflict refresh performs.
	liveRepo := initTestGitRepo(t)
	live := session.InstanceData{Title: "live", CreatedAt: time.Now()}
	live.Path = liveRepo
	live.Worktree.RepoPath = liveRepo
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return []session.InstanceData{live}, nil, nil
	}))
	dir, err := config.ProjectRegistryDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644))

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd)
	h.Update(cmd())

	assert.False(t, displayed.RebindPending(), "the reply still re-arms the form on a degraded registry")
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEsc})

	var regRow, liveRow *overlay.Project
	for i, r := range displayed.Projects() {
		if r.RegistryID == id {
			regRow = &displayed.Projects()[i]
		}
		if r.RegistryID == "" && r.RepoID == config.RepoIDFromRoot(liveRepo) {
			liveRow = &displayed.Projects()[i]
		}
	}
	require.NotNil(t, regRow,
		"a degraded registry read cannot prove absence — the picker's registry row survives")
	assert.Equal(t, root, regRow.RegistryRoot,
		"the preserved row keeps its last-known pair as the retry's expected value")
	assert.Equal(t, 0, regRow.SessionCount,
		"the landed session snapshot is the count authority — the repo's stale tally clears (#4888 review)")
	assert.Equal(t, 0, regRow.InPlaceCount)
	require.NotNil(t, liveRow,
		"the healthy session half still installs its live row")
	assert.Equal(t, 1, liveRow.SessionCount, "live counts come from the landed snapshot")
}

// TestRebindConflictRefreshCarriesSnapshotHealthToTheDeleteGate pins the #4888
// review finding on the conflict refresh: the rows it installs replace the
// counts the picker opened with, so the snapshot half's health must replace the
// picker's snapshot-degraded flag too — the flag the D (delete project) gate
// reads before a confirmation may claim "no live sessions". A picker opened on
// a healthy snapshot whose conflict-time fetch fails holds unconfirmed counts
// (the cross-repo split even adds a zero-count row), so the gate must close; a
// fetch that answered recounts every row, so a gate an earlier failure closed
// reopens.
func TestRebindConflictRefreshCarriesSnapshotHealthToTheDeleteGate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		openDegraded bool
		fetchErr     error
		want         bool
	}{
		{name: "a failed conflict-time fetch closes the gate", openDegraded: false, fetchErr: errors.New("daemon snapshot unavailable"), want: true},
		{name: "a fetch that answered reopens it", openDegraded: true, fetchErr: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, id := activeRebindHome(t)
			displayed := h.projectPickerOverlay
			displayed.SetSnapshotDegraded(tc.openDegraded)
			_, err := config.RebindProject(id, initTestGitRepo(t)) // another client, meanwhile
			require.NoError(t, err)
			stubRebindCAS(t)
			t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
				return nil, nil, tc.fetchErr
			}))

			_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
			require.NotNil(t, cmd)
			h.Update(cmd())

			require.False(t, displayed.RebindPending(), "precondition: the conflict reply was applied")
			assert.Equal(t, tc.want, displayed.SnapshotDegraded(),
				"the delete gate must follow the health of the snapshot behind the counts now displayed")
		})
	}
}

// TestRebindConflictRefresh_FailedFetchRetainsPickerSkipMarkers pins the #4742
// review finding on the conflict-refresh error path: when Ctrl+P is already
// showing a skipped repo and the conflict-time all-repos request fails, the
// picker must NOT clear its known skip IDs and hints — the failed request
// arrives with a nil skip set yet cannot establish that the persistent skip
// was repaired, so clearing them would render the row ordinary while the
// sidebar still marks it hidden. The picker must keep the cached IDs/hints
// (mirroring updateOpenPickerSkipState and applySidebarProjects on their
// fetch-failure paths) while SnapshotDegraded gates the D path on the global
// fetch-failure refusal.
func TestRebindConflictRefresh_FailedFetchRetainsPickerSkipMarkers(t *testing.T) {
	h, id := activeRebindHome(t)
	displayed := h.projectPickerOverlay
	_, err := config.RebindProject(id, initTestGitRepo(t)) // another client, meanwhile
	require.NoError(t, err)
	stubRebindCAS(t)

	// The picker opened with alpha already marked skipped (the state a Ctrl+P
	// opened while the daemon was skipping alpha).
	displayed.SetSkippedRepoIDs([]string{h.repoID})
	displayed.SetSkippedRepoHints(map[string]string{h.repoID: "repair alpha"})
	require.True(t, displayed.IsRepoSkipped(h.repoID), "precondition: alpha is marked skipped")
	require.Equal(t, "repair alpha", displayed.SkippedRepoHint(h.repoID), "precondition: alpha carries its repair hint")

	// The conflict-time fetch fails. The failed request cannot prove the skip
	// was repaired, so the picker must retain the last-known skip markers
	// instead of clearing them; only SnapshotDegraded flips to gate D globally.
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return nil, nil, errors.New("daemon snapshot unavailable")
	}))

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd)
	h.Update(cmd())

	require.False(t, displayed.RebindPending(), "precondition: the conflict reply was applied")
	assert.True(t, displayed.SnapshotDegraded(),
		"a failed conflict-time fetch must set SnapshotDegraded so the D path falls back to the fetch-failure refusal")
	assert.True(t, displayed.IsRepoSkipped(h.repoID),
		"a failed conflict-time fetch must not clear a previously skipped repo's marker")
	assert.Equal(t, "repair alpha", displayed.SkippedRepoHint(h.repoID),
		"a failed conflict-time fetch must not clear a previously skipped repo's repair hint")
}

// TestRebindConflictRefresh_PreservesSessionOnlySkippedRows pins the #4742
// review finding on the conflict-rebuild path: when an open picker holds a
// session-only skipped project and a rebind of another registered project
// receives a "rebound elsewhere" conflict, fresh (built from the conflict
// snapshot) cannot reconstruct the skipped project because its instances are
// absent. Installing only the skipped ID and hint, then replacing the row set
// wholesale via SetRebindConflict, would drop the hidden project — and later
// background refreshes would no longer have an old picker row to graft back.
// The picker must preserve skipped rows from projectPickerOverlay.Projects()
// before rebuilding the conflict list (mirrors preserveSkippedPickerRows for
// the background refresh).
func TestRebindConflictRefresh_PreservesSessionOnlySkippedRows(t *testing.T) {
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

	// gamma is a separate git repo whose ONLY presence is a live snapshot
	// session — not registered, not a root agent, not the active root — so the
	// row a healthy snapshot produces is session-derived and disappears once
	// the daemon skips gamma and drops its instance.
	gammaRoot := initTestGitRepo(t)
	gammaID := config.RepoIDFromRoot(gammaRoot)
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay([]overlay.Project{
		{Name: "active", Root: root, RepoID: h.repoID, RegistryID: id, RegistryRoot: root, RegistryCheckoutID: recordedCheckout, MissingPath: true, SessionCount: 4, InPlaceCount: 1},
		{Name: "gamma", Root: gammaRoot, RepoID: gammaID, SessionCount: 3},
	}, h.repoRoot)
	h.projectPickerOverlay.SetMaxSize(80, 24)
	// The picker already marks gamma skipped (the state a Ctrl+P opened while
	// the daemon was skipping gamma).
	h.projectPickerOverlay.SetSkippedRepoIDs([]string{gammaID})
	h.projectPickerOverlay.SetSkippedRepoHints(map[string]string{gammaID: "repair gamma"})
	displayed := h.projectPickerOverlay
	require.True(t, displayed.IsRepoSkipped(gammaID), "precondition: gamma is marked skipped in the picker")

	// Another client rebinds the active project, so this rebind conflicts.
	_, err = config.RebindProject(id, initTestGitRepo(t))
	require.NoError(t, err)
	stubRebindCAS(t)

	// The conflict refresh answers: alpha's session survives, gamma's does
	// not (the daemon skipped it), and gamma appears in the skip set.
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return []session.InstanceData{{Worktree: session.GitWorktreeData{RepoPath: root}}},
			[]daemon.SkippedRepo{{RepoID: gammaID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
			nil
	}))

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd)
	h.Update(cmd())
	require.False(t, displayed.RebindPending(), "precondition: the conflict reply was applied")

	// gamma's session-only row must survive the conflict rebuild marked
	// hidden, instead of vanishing before the skip marker is installed.
	byRoot := map[string]overlay.Project{}
	for _, p := range displayed.Projects() {
		byRoot[p.Root] = p
	}
	require.Contains(t, byRoot, gammaRoot,
		"a session-only repo the daemon skipped must survive a conflict rebuild instead of vanishing")
	assert.True(t, displayed.IsRepoSkipped(gammaID),
		"the preserved session-only row is marked skipped so it renders hidden and D is gated")
	assert.Equal(t, corruptedRepoRepairHint, displayed.SkippedRepoHint(gammaID),
		"the preserved row carries the daemon's reason-specific repair hint for the skip reason")
}

// TestRebindConflictRefresh_RetainsSkipSetWhenNewerPollApplied pins the #4742
// review finding on the conflict-rebuild path: the off-loop conflict fetch can
// overlap a regular background poll. A newer poll that began skipping a repo
// AFTER the conflict fetch opened must not have its Hidden gate cleared by the
// stale conflict reply landing later. The conflict handler detects the newer
// poll (crossRepoSnapshotSeq — bumped at each fetch's START — advanced past the
// value this conflict captured at its open) and retains the sidebar's current
// skip set and hints instead of replacing them.
func TestRebindConflictRefresh_RetainsSkipSetWhenNewerPollApplied(t *testing.T) {
	h, id := activeRebindHome(t)
	root, ok := registeredProjectRoot(id)
	require.True(t, ok)

	// alpha is a repo the daemon began skipping AFTER the conflict fetch
	// opened. The sidebar's newer poll already marked it skipped and retained
	// the repair hint; the picker already carries that marker.
	const alphaID = "alpha-skipped-id"
	h.sidebarProjectsSkipped = map[string]bool{alphaID: true}
	h.sidebarProjectsSkippedHints = map[string]string{alphaID: "repair alpha"}
	h.crossRepoSnapshotSeq = 5
	displayed := h.projectPickerOverlay
	displayed.SetSkippedRepoIDs([]string{alphaID})
	displayed.SetSkippedRepoHints(map[string]string{alphaID: "repair alpha"})
	require.True(t, displayed.IsRepoSkipped(alphaID), "precondition: alpha marked skipped")
	require.Equal(t, "repair alpha", displayed.SkippedRepoHint(alphaID), "precondition: alpha carries its repair hint")

	// Another client rebinds the active project, so this rebind conflicts.
	_, err := config.RebindProject(id, initTestGitRepo(t))
	require.NoError(t, err)
	stubRebindCAS(t)

	// The conflict refresh bumps the seq at its START (5 -> 6) and captures 6,
	// so it ordered AFTER every fetch that started before it. Its reply's skip
	// set does NOT include alpha (it was not skipped yet at fetch-open time). A
	// newer regular poll that started AFTER this conflict then bumps the seq to
	// 7 while the conflict fetch is still in flight; the conflict reply lands
	// after that, so 7 > 6 marks it stale.
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		h.crossRepoSnapshotSeq = 7
		return []session.InstanceData{{Worktree: session.GitWorktreeData{RepoPath: root}}},
			nil, // stale reply: alpha not yet skipped at fetch-open time
			nil
	}))

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd)
	h.Update(cmd())

	require.False(t, displayed.RebindPending(), "precondition: the conflict reply was applied")
	// The stale reply must not clear alpha's hidden marker the newer poll set.
	assert.True(t, h.sidebarProjectsSkipped[alphaID],
		"a stale conflict reply must not replace the newer poll's skip set")
	assert.Equal(t, "repair alpha", h.sidebarProjectsSkippedHints[alphaID],
		"a stale conflict reply must not replace the newer poll's repair hint")
	assert.True(t, displayed.IsRepoSkipped(alphaID),
		"the picker must retain the newer poll's skip marker for alpha")
	assert.Equal(t, "repair alpha", displayed.SkippedRepoHint(alphaID),
		"the picker must retain the newer poll's repair hint for alpha")
}
