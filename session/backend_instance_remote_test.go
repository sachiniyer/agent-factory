package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Instance delegation ---

func TestInstanceDelegatesStartToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)
	assert.True(t, i.Started())

	b.closePTY(i.Title)
}

func TestInstanceDelegatesKillToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-kill",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)

	err = i.Kill()
	require.NoError(t, err)
	assert.False(t, i.Started())
}

func TestInstanceDelegatesPreviewToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-preview",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	content, err := i.Preview()
	require.NoError(t, err)
	assert.Contains(t, content, "attached to delegate-preview")

	b.closePTY(i.Title)
}

func TestInstanceRepoNameErrorsForRemote(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{
		Title:   "remote-inst",
		backend: b,
		started: true,
	}
	_, err := i.RepoName()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remote")
}

func TestInstanceGetWorktreePathEmptyForRemote(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{
		Title:   "remote-inst",
		backend: b,
		started: true,
	}
	assert.Equal(t, "", i.GetWorktreePath())
}

// --- list_cmd variations ---

func TestHookBackendIsAliveWithBadJSON(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `echo "not json"`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{Title: "test", backend: b}
	assert.False(t, b.IsAlive(i))
}

func TestHookBackendIsAliveWithStoppedSession(t *testing.T) {
	dir := t.TempDir()
	slug := Slugify("test-session")
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "`+slug+`", "status": "stopped"}]'`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{Title: "test-session", backend: b}
	// status=stopped means not alive
	assert.False(t, b.IsAlive(i))
}

func TestHookBackendIsAliveWithMultipleSessions(t *testing.T) {
	dir := t.TempDir()
	slugA := Slugify("session-a")
	slugB := Slugify("session-b")
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "`+slugA+`", "status": "stopped"}, {"name": "`+slugB+`", "status": "running"}]'`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}

	iA := &Instance{Title: "session-a", backend: b}
	iB := &Instance{Title: "session-b", backend: b}

	assert.False(t, b.IsAlive(iA))
	assert.True(t, b.IsAlive(iB))
}

// --- slugify shape ---

func TestSlugifyNoHashSuffix(t *testing.T) {
	cases := map[string]string{
		"hello":             "hello",
		"Hello World":       "hello-world",
		"My App!":           "my-app",
		"  spaced  ":        "spaced",
		"CAPS":              "caps",
		"already-a-slug":    "already-a-slug",
		"af-test":           "af-test",
		"some/name:thing@1": "somenamething1",
	}
	for title, want := range cases {
		assert.Equal(t, want, Slugify(title), "Slugify(%q)", title)
	}
}

func TestSlugifyDeterministic(t *testing.T) {
	title := "some-session-title"
	assert.Equal(t, Slugify(title), Slugify(title))
}

func TestSlugifyNonEmpty(t *testing.T) {
	// Even pathological inputs should produce a non-empty slug.
	for _, title := range []string{"!!!", "   ", ""} {
		s := Slugify(title)
		assert.NotEmpty(t, s, "Slugify(%q) should not be empty", title)
	}
}

func TestSlugifyCollisionsReduce(t *testing.T) {
	collisions := [][2]string{
		{"my_app", "myapp"},
		{"My App!", "my-app"},
		{"HELLO", "hello"},
	}
	for _, p := range collisions {
		assert.Equal(t, Slugify(p[0]), Slugify(p[1]))
	}
}

func TestFindSlugCollision(t *testing.T) {
	mk := func(title string) *Instance { return &Instance{Title: title} }
	existing := []*Instance{mk("myapp"), mk("other"), nil}

	assert.Equal(t, "myapp", FindSlugCollision("my_app", existing))
	assert.Equal(t, "myapp", FindSlugCollision("MyApp", existing))
	assert.Equal(t, "", FindSlugCollision("fresh-title", existing))
	assert.Equal(t, "", FindSlugCollision("myapp", existing))
	assert.Equal(t, "", FindSlugCollision("", existing))
}

func TestRemoteHookNamePrefersRemoteMetaName(t *testing.T) {
	assert.Equal(t, "box-af-test", RemoteHookName("af-test", map[string]interface{}{"name": "box-af-test"}))
	assert.Equal(t, "af-test", RemoteHookName("af-test", nil))
}

func TestListRemoteHookInstanceDataImportsRunningSessions(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "remote-one", "status": "running", "host": "h1"}, {"name": "remote-two", "status": "stopped"}]'`)

	now := time.Now()
	data, err := ListRemoteHookInstanceData("/repo/root", config.RemoteHooks{ListCmd: listCmd}, now)
	require.NoError(t, err)
	require.Len(t, data, 1)
	assert.Equal(t, "remote-one", data[0].Title)
	assert.Equal(t, "/repo/root", data[0].Path)
	assert.Equal(t, "remote-one", data[0].Branch)
	assert.Equal(t, Running, data[0].Status)
	assert.Equal(t, "remote", data[0].BackendType)
	assert.Equal(t, "remote-one", data[0].RemoteMeta["name"])
	assert.Equal(t, "h1", data[0].RemoteMeta["host"])
}

// TestListRemoteHookInstanceDataIgnoresStderrDiagnostics covers the common
// pattern of a list_cmd script that writes progress to stderr while emitting
// JSON on stdout (e.g. an ssh-backed lister that logs "connecting…"). The
// captured stderr must not corrupt the JSON we parse. See #561.
func TestListRemoteHookInstanceDataIgnoresStderrDiagnostics(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `
echo "connecting to remote host..." >&2
echo "fetched 1 session" >&2
echo '[{"name": "remote-one", "status": "running", "host": "h1"}]'
`)

	now := time.Now()
	data, err := ListRemoteHookInstanceData("/repo/root", config.RemoteHooks{ListCmd: listCmd}, now)
	require.NoError(t, err)
	require.Len(t, data, 1)
	assert.Equal(t, "remote-one", data[0].Title)
	assert.Equal(t, "remote-one", data[0].RemoteMeta["name"])
	assert.Equal(t, "h1", data[0].RemoteMeta["host"])
}

// TestListRemoteHookInstanceDataSurfacesStderrOnFailure verifies that when
// list_cmd exits non-zero, the returned error includes the captured stderr
// so the warning surfaced at app/sync.go and daemon/control.go is actually
// diagnostic. Before #561 the error was just "list_cmd failed: exit status 1".
func TestListRemoteHookInstanceDataSurfacesStderrOnFailure(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `
echo "ssh: could not resolve hostname remote.example.com" >&2
exit 1
`)

	_, err := ListRemoteHookInstanceData("/repo/root", config.RemoteHooks{ListCmd: listCmd}, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list_cmd failed")
	assert.Contains(t, err.Error(), "ssh: could not resolve hostname remote.example.com",
		"error must surface captured stderr so users can debug list_cmd failures (#561)")
}

// TestListRemoteHookInstanceDataListCmdHangs is the regression test for #692:
// ListRemoteHookInstanceData runs the user-supplied list_cmd at TUI startup
// inside the daemon handler that the TUI blocks on over RPC (with no
// client-side call deadline). A hanging list_cmd (e.g. SSH to a wedged host)
// previously had no timeout here, so startup blocked for the full duration of
// the script. The startup import path must abort within restoreAliveTimeout
// plus a small tolerance for WaitDelay and scheduling slack.
func TestListRemoteHookInstanceDataListCmdHangs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout-bound test in short mode")
	}

	dir := t.TempDir()
	// Sleep well past restoreAliveTimeout so the timeout, not the script,
	// is what ends the call.
	listCmd := writeScript(t, dir, "list.sh", `sleep 30; echo '[]'`)

	start := time.Now()
	_, err := ListRemoteHookInstanceData("/repo/root", config.RemoteHooks{ListCmd: listCmd}, time.Now())
	elapsed := time.Since(start)

	require.Error(t, err, "startup import must error when list_cmd hangs past timeout")
	// restoreAliveTimeout is 2s; allow a buffer for WaitDelay (500ms) plus
	// scheduling slack. The key bound is that startup must NOT block anywhere
	// near the script's 30s sleep — that was the #692 hang.
	assert.Less(t, elapsed, restoreAliveTimeout+2*time.Second,
		"startup import must return within restoreAliveTimeout+tolerance when list_cmd hangs (got %v)", elapsed)
}
