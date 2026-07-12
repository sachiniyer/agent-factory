package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- Remote instance shape ---
//
// #1592 Phase 4 PR7: the remote-hook backend is a provision-and-expose runtime
// whose agent operations delegate to the instance's remote AgentServer (driven
// over wss:// to a user-provisioned `af agent-server`). Its end-to-end lifecycle
// is exercised by the mock-hook round-trip in integration/remote_roundtrip_test.go
// (a real af agent-server behind a mock launch_cmd), not by script-based unit
// stubs. The unit-level contract that remains here is the daemon-side shape: a
// remote instance has no local worktree.

func TestInstanceRepoNameErrorsForRemote(t *testing.T) {
	i := &Instance{
		Title:   "remote-inst",
		backend: &HookBackend{},
		started: true,
	}
	_, err := i.RepoName()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remote")
}

func TestInstanceGetWorktreePathEmptyForRemote(t *testing.T) {
	i := &Instance{
		Title:   "remote-inst",
		backend: &HookBackend{},
		started: true,
	}
	assert.Equal(t, "", i.GetWorktreePath())
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
