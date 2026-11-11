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
// resolves exactly as before. A repository is remembered once resolved; a
// failed repository probe is retried by the next caller. A project config is
// remembered once ANSWERED — resolved, or answered with an error such as an
// invalid value — so each read of the batch gets the answer the first read got
// rather than asking git again with a load's single attempt (#7088 review);
// only a config read that went unanswered (config.ResolveWentUnanswered) is
// retried. The shared *RepoContext and *ResolvedConfig are read-only on the
// Start path.
//
// ResolveConfigForRepo's durable observation (the in-repo config hash marker) is
// idempotent — it records only when the content hash changed — so the first
// resolution in the batch records what N calls in a row would have.
type RestoreContext struct {
	repoFromPath  func(string) (*config.RepoContext, error)
	resolveConfig func(*config.RepoContext) (*config.ResolvedConfig, error)
	// attempts bounds how many unanswered project-config reads one resolve
	// through this context makes before reporting it unknown (#7077); zero is
	// the launch default. A load asks once: whoever retries the load — the
	// daemon's deferred-row probe, the lost-restore loop — owns the patience,
	// and a load runs under locks that must not pay a launch's retry budget.
	attempts int

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
	answered bool
	resolved *config.ResolvedConfig
	err      error
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

// newLoadRestoreContext is the context a single load runs in when its caller
// brought none: every config read of that one Start shares the first answer,
// so a sibling tab's read can never miss after the agent's read succeeded and
// tear down an agent the load only reattached (#7077 review), and an
// unanswered read is asked once.
func newLoadRestoreContext() *RestoreContext {
	rc := NewRestoreContext()
	rc.attempts = 1
	return rc
}

// loadContext is the context a load reads through: its caller's, else its own.
func loadContext(rc *RestoreContext) *RestoreContext {
	if rc != nil {
		return rc
	}
	return newLoadRestoreContext()
}

// PrimeRestoreContext resolves path's repository and project config with a
// launch's full retry budget and returns a context that answers both from
// memory. A caller that must not pay the reads where it materializes the row —
// the daemon's refresh runs under its manager lock — primes one off that lock
// and hands it to FromInstanceDataWithContext, so the load reads no config
// at all. The error is resolveProjectConfig's: nil for a readable or absent
// config, a *ProjectConfigUnknownError when every attempt went unanswered.
func PrimeRestoreContext(path string) (*RestoreContext, error) {
	rc := NewRestoreContext()
	_, err := resolveProjectConfig(path, projectConfigSource{
		repoFromPath: rc.RepoFromPath, resolveRepo: rc.ResolveConfigForRepo, attempts: launchConfigResolveAttempts,
	})
	// The load that consumes it answers from memory; should it ever miss, it
	// asks once, as any load does.
	rc.attempts = 1
	return rc, err
}

// ResolvedConfigFor is path's project config answered through this context —
// from memory once primed. (nil, nil) is a path with no project config, as for
// resolveProjectConfig; a context primed without an error answers without I/O.
func (c *RestoreContext) ResolvedConfigFor(path string) (*config.ResolvedConfig, error) {
	src := projectConfigSource{repoFromPath: c.RepoFromPath, resolveRepo: c.ResolveConfigForRepo}
	if c != nil {
		src.attempts = c.attempts
	}
	return resolveProjectConfig(path, src)
}

// ReleaseConfigSnapshot ends the create's use of InstanceOptions.ConfigSnapshot:
// later config reads for this session resolve afresh rather than from the
// snapshot the create was primed with.
func (i *Instance) ReleaseConfigSnapshot() {
	i.restoreCtx.Store(nil)
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
	if m.answered {
		return m.resolved, m.err
	}
	resolved, err := c.resolveConfig(repo)
	if err == nil || !config.ResolveWentUnanswered(err) {
		m.answered, m.resolved, m.err = true, resolved, err
	}
	return resolved, err
}

// restoreContext is the batch context set on this instance for the duration of
// its restore, or nil.
func (i *Instance) restoreContext() *RestoreContext {
	return i.restoreCtx.Load()
}
