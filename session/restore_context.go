package session

import (
	"sync"

	"github.com/sachiniyer/agent-factory/config"
)

// A RestoreContext shares the repository and config resolution of ONE batch of
// session restores. Instance.Start resolves its project several times (the tmux
// session's repo identity, the session-environment pass-through, the launch
// program), and every resolution runs git rev-parse subprocesses and re-reads the
// config files; restoring N sessions of one project therefore repeated the same
// answer ~4N times at launch (#6446). Resolving it once per project path and
// handing the result to each restore removes the repeats.
//
// The context is explicit — carried on the Instance for the duration of its
// restore — rather than process-global, so nothing outside a batch that opted in
// ever sees a shared result. Every method is nil-safe: a nil *RestoreContext
// resolves exactly as before. Only successes are remembered; a failed resolution
// is retried by the next caller, as it always was. The shared *RepoContext and
// *ResolvedConfig are read-only on the Start path.
//
// ResolveConfigForRepo's durable observation (the in-repo config hash marker) is
// idempotent — it records only when the content hash changed — so the first
// resolution in the batch records what N calls in a row would have.
type RestoreContext struct {
	repoFromPath  func(string) (*config.RepoContext, error)
	resolveConfig func(*config.RepoContext) (*config.ResolvedConfig, error)

	mu    sync.Mutex
	repos map[string]*repoMemo
	cfgs  map[string]*configMemo
}

type repoMemo struct {
	mu   sync.Mutex
	repo *config.RepoContext
}

type configMemo struct {
	mu       sync.Mutex
	resolved *config.ResolvedConfig
}

// NewRestoreContext returns a context that resolves through the real config
// package, once per project.
func NewRestoreContext() *RestoreContext {
	return &RestoreContext{
		repoFromPath:  config.RepoFromPath,
		resolveConfig: config.ResolveConfigForRepo,
		repos:         map[string]*repoMemo{},
		cfgs:          map[string]*configMemo{},
	}
}

// RepoFromPath is config.RepoFromPath, answered once per path for the batch.
// Concurrent callers for the same path wait for the first rather than each
// spawning their own git probes.
func (c *RestoreContext) RepoFromPath(path string) (*config.RepoContext, error) {
	if c == nil {
		return config.RepoFromPath(path)
	}
	c.mu.Lock()
	m, ok := c.repos[path]
	if !ok {
		m = &repoMemo{}
		c.repos[path] = m
	}
	c.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.repo != nil {
		return m.repo, nil
	}
	repo, err := c.repoFromPath(path)
	if err == nil {
		m.repo = repo
	}
	return repo, err
}

// ResolveConfigForRepo is config.ResolveConfigForRepo, answered once per
// (identity root, workspace root) for the batch.
func (c *RestoreContext) ResolveConfigForRepo(repo *config.RepoContext) (*config.ResolvedConfig, error) {
	if c == nil || repo == nil {
		return config.ResolveConfigForRepo(repo)
	}
	key := repo.IdentityPath() + "\x00" + repo.WorkspacePath()
	c.mu.Lock()
	m, ok := c.cfgs[key]
	if !ok {
		m = &configMemo{}
		c.cfgs[key] = m
	}
	c.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resolved != nil {
		return m.resolved, nil
	}
	resolved, err := c.resolveConfig(repo)
	if err == nil {
		m.resolved = resolved
	}
	return resolved, err
}

// restoreContext is the batch context set on this instance for the duration of
// its restore, or nil.
func (i *Instance) restoreContext() *RestoreContext {
	return i.restoreCtx.Load()
}
