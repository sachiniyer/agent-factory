package session

import (
	"crypto/rand"
	"fmt"
	"path/filepath"
	"time"
)

// Options for creating a new instance
type InstanceOptions struct {
	// Title is the title of the instance.
	Title string
	// Path is the path to the workspace.
	Path string
	// Program is the program to run in the instance (e.g. "claude", "aider --model ollama_chat/gemma3:1b")
	Program string
	// If AutoYes is true, then
	AutoYes bool
	// ForceRemote forces the instance to use the remote hook backend,
	// even if the repo config would default to local.
	ForceRemote bool
	// InPlace attaches the session to the repo's existing working tree at its
	// current branch (`af sessions create --here`) instead of creating a new
	// git worktree+branch. The worktree is marked external so kill/cleanup
	// never removes the user's tree or branch. Local backend only.
	InPlace bool
}

// backendFactory constructs the Backend used by a new Instance. It is a
// package-level variable (not a hard-coded branch) so tests can inject a
// FakeBackend through SetBackendFactoryForTest without touching production
// code paths. Defaults to the real local/remote branching.
var backendFactory = defaultBackendFactory

func defaultBackendFactory(opts InstanceOptions, absPath string) (Backend, error) {
	if opts.ForceRemote {
		hook, err := loadHookBackendForPath(absPath)
		if err != nil {
			return nil, fmt.Errorf("remote hooks not configured for this repo: %w", err)
		}
		return hook, nil
	}
	return &LocalBackend{}, nil
}

// SetBackendFactoryForTest replaces the backend factory with f and returns a
// restore function. Intended for use in tests that need to swap in a
// FakeBackend so NewInstance-driven creation flows stay on the hot path.
func SetBackendFactoryForTest(f func(opts InstanceOptions, absPath string) (Backend, error)) func() {
	prev := backendFactory
	backendFactory = f
	return func() { backendFactory = prev }
}

// newSessionID mints a random RFC-4122 v4 UUID for an instance's stable identity
// (#1195). It is a package var so tests can inject deterministic IDs. crypto/rand
// is the entropy source; on the (near-impossible) read failure it falls back to a
// timestamp-derived value so session creation never blocks on entropy — still
// unique per call in practice, and the reconcile's title+CreatedAt fallback covers
// any theoretical collision.
var newSessionID = func() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func NewInstance(opts InstanceOptions) (*Instance, error) {
	t := time.Now()

	// An in-place session runs in the repo's local working tree; a remote
	// session has no local worktree at all — the two are contradictory.
	if opts.InPlace && opts.ForceRemote {
		return nil, fmt.Errorf("remote sessions cannot run in-place in the local repo working tree")
	}

	// Convert path to absolute
	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	backend, err := backendFactory(opts, absPath)
	if err != nil {
		return nil, err
	}

	return &Instance{
		ID:        newSessionID(),
		Title:     opts.Title,
		liveness:  LiveReady,
		Path:      absPath,
		Program:   opts.Program,
		Height:    0,
		Width:     0,
		CreatedAt: t,
		UpdatedAt: t,
		AutoYes:   opts.AutoYes,
		inPlace:   opts.InPlace,
		backend:   backend,
	}, nil
}
