// Package probe runs a short, af-authored, non-interactive invocation of an
// agent's own CLI under a registered account's home, and reads a verdict off
// the result: is this account serving requests right now?
//
// It exists for the automatic account-handoff engine (epic #6776, design §3)
// as one of three evidence classes — the only one that spends provider quota
// (one minimal completion per run), so it runs on demand only: wall
// confirmation, candidate admission, stale-rollover refutation, each cached
// per account for a TTL. Nothing in this package self-activates: without a
// caller there is no probe, which is what "off by default" means here.
//
// THE TRUST BOUNDARY. A probe executes the agent CLI under an account's
// credential home, unattended — a tighter grant than a pane's, because no
// operator is watching. The boundary's job is argv/env/cwd provenance,
// credential-state proof, and lifecycle control:
//
//   - argv is a fixed literal slice. No account name, path, or caller input
//     ever reaches it; the per-agent spec is a package-level constant. The
//     codex spec is a least-privilege invocation: user config and execpolicy
//     rules ignored, ephemeral rollout, read-only sandbox, approval never,
//     MCP/hooks/notify/plugins/memory/tool surfaces disabled where the CLI
//     offers a fixed literal.
//   - env is an allowlist, not a filtered daemon environment. The child sees
//     only the names a probe invocation needs (PATH, HOME, locale, TLS roots)
//     through sessionenv's account math — never AF_DAEMON_*, GH_TOKEN,
//     SSH_AUTH_SOCK, proxies, TMUX, or CODEX_SQLITE_HOME, which would
//     redirect the account's databases to another home.
//   - the credential gate runs before exec: auth.lock held means a live copy
//     is mid-refresh; an access-token exp inside the refresh lead plus the
//     timeout means a run could BECOME the second refresher that kills every
//     sandbox copy of the credential. Unproven states refuse with
//     ClassAuthWindow — the probe never runs.
//   - cwd is a private af-owned directory, <af-home>/probe/<run-id>, created
//     0700 — never /tmp (world-writable instruction-file injection into a
//     credential home) and never a session worktree (repo-controlled files
//     must not execute under credentials). An ancestor chain containing
//     .git, .codex, or an AGENTS.md variant is refused outright: the CLI
//     loads repository instructions up to the enclosing .git, and an af home
//     inside a work tree would feed that repo's files to a credentialed run.
//     It also keeps probe rollouts out of the cwd-filtered
//     conversation-capture path: their session_meta cwd is this dir, which
//     never equals a session working directory.
//   - accounts resolve ONLY through agentaccount.Selected — never config
//     text, never a symlinked path — and the resolved dir must sit under
//     <af-home>/accounts/<agent>/. The default login (empty or unregistered
//     name) is refused before any exec: ambient has no credential directory
//     af can scope. Single-flight and cache identity is the resolved home's
//     filesystem object (dev:ino), not the caller's spelling — two names for
//     one directory share one flight.
//   - output is size-capped (the tail, where a wall message lands), treated
//     as data for the wall scan, and never logged or carried on a verdict.
//   - a hard timeout interrupts the whole process GROUP — SIGINT first so
//     truncate-then-write credential saves can finish, SIGKILL after the
//     grace bound — and the sweep runs after every exec, so no descendant
//     keeps spending quota after the run is counted done. The cap slot
//     releases only when the group is empty (or has resisted SIGKILL past a
//     bounded wait — disclosed, not hidden). The deadline is checked before
//     any positive read: timeout is Unknown, unconditionally, never Walled.
//   - executions are single-flight per account home, capped globally, and
//     always on a worker goroutine — never the caller's (poll) goroutine.
//   - platforms without a process-group primitive (Windows) refuse every
//     probe: the lifecycle guarantee is a precondition, not an option.
//
// The result type is deliberately tiny: Kind, an optional reset time, a
// coarse class for Unknown, the model the verdict was earned under, and a
// timestamp. It is evidence, never an entitlement — the TTL is the whole
// freshness claim.
package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/afhome"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// Kind is the three-way answer a probe can honestly give.
type Kind int

const (
	// KindUnknown covers every outcome that is neither proven healthy nor a
	// recognized wall: refusals, timeouts, start failures, and unparseable
	// output. Unparseable is never walled — the absence of evidence is not
	// evidence of a wall.
	KindUnknown Kind = iota
	// KindHealthy means the account completed the probe turn: the CLI wrote
	// the demanded token to the last-message file. Positive evidence.
	KindHealthy
	// KindWalled means the run showed the provider's recognizable
	// account-wide usage-limit message. ResetsAt carries the parsed reset
	// time when one is present. A per-model quota is not a wall — it is
	// Unknown of class model-limit.
	KindWalled
)

func (k Kind) String() string {
	switch k {
	case KindHealthy:
		return "healthy"
	case KindWalled:
		return "walled"
	default:
		return "unknown"
	}
}

// Verdict is the whole answer a probe produces. It carries no output text by
// construction — captured bytes are scan input, never evidence to relay.
type Verdict struct {
	Kind Kind
	// ResetsAt is the parsed reset time for a Walled or model-limit
	// verdict, zero when the message named none or could not be parsed.
	ResetsAt time.Time
	// Class is the coarse Unknown classification — one of the Class*
	// constants, stable so event streams can enumerate it.
	Class string
	// Model is the model the verdict was earned under — the spec's pinned
	// literal, recorded on EVERY verdict including refusals. The handoff
	// engine trusts a verdict only for the model its session will run
	// (epic #6776 R9); a verdict that could not say which model ran would
	// be evidence about nothing.
	Model string
	// At is when the probe completed, the timestamp a TTL is measured from.
	At time.Time
}

// Unknown classifications. They are declared strings, not errors, because the
// verdict IS the contract: the engine logs the class, it does not branch on
// error identity.
const (
	// ClassRefused covers a probe the boundary declined to run: the default
	// login, an invalid or unregistered name, a resolved home outside the
	// account root, a sessionenv refusal, or repository instruction sources
	// above the private cwd.
	ClassRefused = "refused"
	// ClassUnsupported means the platform cannot deliver the probe's
	// lifecycle guarantee (no process-group kill on Windows); every probe
	// refuses there rather than risk a runaway descendant under credentials.
	ClassUnsupported = "unsupported"
	// ClassAuthWindow means the account's credential state could not be
	// proven safe to read: an auth.lock held by a live refresher, an
	// access-token exp inside the refresh window, or auth metadata af
	// cannot parse. The probe never execs — running it could make the
	// probe a second refresher and kill every sandbox copy of the
	// credential.
	ClassAuthWindow = "auth-window"
	// ClassModelLimit means the run showed a per-model usage limit — the
	// account still serves other models, so it is Unknown evidence scoped
	// to Verdict.Model, never a wall for the whole account.
	ClassModelLimit = "model-limit"
	// ClassBusy means the in-flight bound was already met; the request was
	// dropped rather than queueing past it.
	ClassBusy = "busy"
	// ClassSetup means the private probe directory could not be verified or
	// the per-run directory could not be created.
	ClassSetup = "setup"
	// ClassTimeout means the hard deadline interrupted the process group.
	// A timeout is Unknown, never Walled.
	ClassTimeout = "timeout"
	// ClassStart means the executable never ran (not found, not executable).
	ClassStart = "start"
	// ClassExit means the process ran and exited nonzero without showing a
	// recognizable wall message.
	ClassExit = "exit"
	// ClassUnexpected means the run completed but produced neither the
	// success token nor a recognizable wall — shape drift, a reworded
	// banner, or a compliant model that answered something else.
	ClassUnexpected = "unexpected"
)

// Options tunes a Runner. The zero value gives a working probe; the engine's
// config slice maps [account_handoff].probe_ttl onto TTL.
type Options struct {
	// BaseEnv is the environment the filter+scope math starts from — the
	// daemon's own, reduced to the probe allowlist at construction. Nil
	// resolves to os.Environ() at construction.
	BaseEnv []string
	// TTL is how long a completed verdict is served from cache before the
	// probe may run again. Zero disables caching (every Request execs); the
	// design's default is 10m, applied by the config layer.
	TTL time.Duration
	// Timeout is the hard interrupt bound per run — design: ~90s. Zero uses
	// defaultTimeout.
	Timeout time.Duration
	// MaxConcurrent caps simultaneous execs across all accounts — design: a
	// small global cap of 1–2. Zero uses defaultMaxConcurrent.
	MaxConcurrent int
	// MaxInFlight bounds total in-flight calls (queued plus running). Beyond
	// it a Request is refused busy instead of parking another goroutine
	// against a saturated executor. Zero uses defaultMaxInFlight.
	MaxInFlight int
	// Now is the clock for cache ages, reset-time resolution, credential
	// windows, and verdict stamps. Nil uses time.Now; tests inject a fixed
	// clock.
	Now func() time.Time
}

const (
	dirName              = "probe"
	dirMode              = 0o700
	runDirPattern        = "run-"
	defaultTimeout       = 90 * time.Second
	defaultMaxConcurrent = 2
	defaultMaxInFlight   = 8
	outputCap            = 32 << 10 // captured tail, bytes
	messageFileCap       = 64 << 10 // success-token file read bound, bytes
	// waitDelay is the graceful-interrupt window: the deadline SIGINTs the
	// process group, then os/exec waits this long for a clean exit before
	// killing the direct child, after which sweepProcGroup settles the
	// group. It doubles as the bound on the post-exit wait for inherited
	// output pipes.
	waitDelay = 5 * time.Second
)

// probeEnvAllowlist is the COMPLETE set of environment names a probe may
// inherit. Everything else is dropped before sessionenv's account math runs
// — an unattended credential execution gets the environment it needs, not
// the daemon's: no AF_* session or daemon identity, no GH_TOKEN, no
// SSH_AUTH_SOCK, no proxy variables, no TMUX, no CODEX_SQLITE_HOME (which
// would point the account's sqlite databases at another home), and no
// OPENAI_*/CODEX_* ambient credentials (sessionenv's math removes and
// re-pins identity either way). TMPDIR is absent on purpose: the run pins it
// to the private per-run directory after the account math.
var probeEnvAllowlist = map[string]struct{}{
	"PATH":                    {},
	"HOME":                    {},
	"LANG":                    {},
	"LC_ALL":                  {},
	"LC_CTYPE":                {},
	"LC_MESSAGES":             {},
	"TZ":                      {},
	"TERM":                    {},
	"SHELL":                   {},
	"USER":                    {},
	"LOGNAME":                 {},
	"SSL_CERT_FILE":           {},
	"SSL_CERT_DIR":            {},
	"NODE_EXTRA_CA_CERTS":     {},
	"SYSTEMROOT":              {},
	"SYSTEMDRIVE":             {},
	"WINDIR":                  {},
	"COMSPEC":                 {},
	"PATHEXT":                 {},
	"APPDATA":                 {},
	"LOCALAPPDATA":            {},
	"HOMEDRIVE":               {},
	"HOMEPATH":                {},
	"__CF_USER_TEXT_ENCODING": {},
}

// Runner owns the machinery every agent probe shares: the private cwd root,
// the filtered+scoped environment base, the single-flight map, the global
// exec cap and the verdict cache.
type Runner struct {
	home        string
	probeDir    string
	exec        cmd.Executor
	baseEnv     []string
	ttl         time.Duration
	timeout     time.Duration
	maxInFlight int
	sem         chan struct{}
	now         func() time.Time

	mu      sync.Mutex
	flights map[string]*flight
	cache   map[string]Verdict
}

// flight is one in-flight probe call. Requests that join it share the
// verdict through their own buffered channels.
type flight struct {
	waiters []chan Verdict
}

// NewRunner creates the shared machinery and the private probe directory
// under home. It refuses a relative or empty home, a nil executor, and a
// probe dir that is not a real directory — the boundary is created correctly
// or not at all.
func NewRunner(home string, executor cmd.Executor, opts Options) (*Runner, error) {
	if strings.TrimSpace(home) == "" || !filepath.IsAbs(home) {
		return nil, fmt.Errorf("probe runner requires an absolute agent-factory home, got %q", home)
	}
	if executor == nil {
		return nil, errors.New("probe runner requires a cmd.Executor")
	}
	base := opts.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	allowlisted := make([]string, 0, len(probeEnvAllowlist))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, keep := probeEnvAllowlist[name]; keep {
			allowlisted = append(allowlisted, kv)
		}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	maxInFlight := opts.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = defaultMaxInFlight
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	r := &Runner{
		home:        home,
		probeDir:    filepath.Join(home, dirName),
		exec:        executor,
		baseEnv:     allowlisted,
		ttl:         opts.TTL,
		timeout:     timeout,
		maxInFlight: maxInFlight,
		sem:         make(chan struct{}, maxConcurrent),
		now:         now,
		flights:     make(map[string]*flight),
		cache:       make(map[string]Verdict),
	}
	if err := ensurePrivateDir(r.probeDir); err != nil {
		return nil, err
	}
	return r, nil
}

// agentProbeSpec is the per-agent recipe: a fixed literal argv, the model the
// run is pinned to, and the two recognition rules a verdict needs. argv[0]
// resolves through PATH at exec time, exactly as a bare agent name does for
// a pane launch.
type agentProbeSpec struct {
	agent       string
	model       string // pinned model literal; recorded on every verdict
	argv        []string
	messageFile string // relative file the CLI writes its final message to
	successText string // exact trimmed content proving a completed turn
	// wall recognizes the provider's usage-limit message in the captured
	// tail and parses its reset phrase when present. limitNone never claims
	// a wall — unparseable is Unknown, not Walled; a model-scoped limit is
	// reported apart, because it is evidence about the model, not the
	// account.
	wall func(output []byte, now time.Time) (resetsAt time.Time, verdict limitVerdict)
}

// agentProbes is the seam's roster: the agents af has a reviewed probe for.
// Codex is first; an agent absent here answers ok=false and relies on
// reading-only confirmation, never an improvised invocation.
var agentProbes = map[string]agentProbeSpec{
	"codex": codexProbeSpec,
}

// AgentProbe is one agent's probe bound to a Runner — the value
// AccountProbe hands the engine.
type AgentProbe struct {
	runner *Runner
	spec   agentProbeSpec
}

// AccountProbe is the per-agent seam: the probe af ships for agent, or
// ok=false when it has none.
func (r *Runner) AccountProbe(agent string) (*AgentProbe, bool) {
	spec, ok := agentProbes[agent]
	if !ok {
		return nil, false
	}
	return &AgentProbe{runner: r, spec: spec}, true
}

// Request asks for a verdict on account and returns a channel that delivers
// exactly one then closes. It never blocks: the exec — and its timeout —
// live on a worker goroutine, so a caller on the daemon's poll goroutine is
// never parked behind a provider round-trip.
//
// Concurrent requests for the same account HOME join one in-flight call: the
// flight and cache key is the resolved directory's filesystem identity, so
// two spellings of one credential home can never double a probe. A completed
// verdict inside its TTL is served from cache with no exec at all.
func (p *AgentProbe) Request(account string) <-chan Verdict {
	ch := make(chan Verdict, 1)
	r := p.runner
	account = strings.TrimSpace(account)

	scope, key, refused := p.resolve(account)
	if refused != "" {
		ch <- Verdict{Kind: KindUnknown, Class: refused, Model: p.spec.model, At: r.now()}
		close(ch)
		return ch
	}

	r.mu.Lock()
	if v, ok := r.cache[key]; ok && r.now().Sub(v.At) < r.ttl {
		r.mu.Unlock()
		ch <- v
		close(ch)
		return ch
	}
	if f, ok := r.flights[key]; ok {
		f.waiters = append(f.waiters, ch)
		r.mu.Unlock()
		return ch
	}
	if len(r.flights) >= r.maxInFlight {
		r.mu.Unlock()
		ch <- Verdict{Kind: KindUnknown, Class: ClassBusy, Model: p.spec.model, At: r.now()}
		close(ch)
		return ch
	}
	f := &flight{waiters: []chan Verdict{ch}}
	r.flights[key] = f
	r.mu.Unlock()

	go p.work(key, scope, f)
	return ch
}

// resolve turns a caller's account spelling into the credential home the
// verdict will actually exercise, and the single-flight/cache identity of
// that home. The key is the resolved directory's filesystem object —
// dev:ino where the platform offers one — so "work" and "Work" on a
// case-insensitive filesystem, or two path spellings of one inode, share one
// flight and one cached verdict. Resolution failures refuse before a flight
// exists: an unregistered name, the default login, a home outside the
// account root, a directory that vanished, or a platform that cannot
// guarantee the run's lifecycle.
func (p *AgentProbe) resolve(account string) (sessionenv.Account, string, string) {
	r := p.runner
	if !probeExecSupported {
		return sessionenv.Account{}, "", ClassUnsupported
	}
	if account == "" {
		return sessionenv.Account{}, "", ClassRefused
	}
	scope, err := agentaccount.Selected(r.home, p.spec.agent, account)
	if err != nil || strings.TrimSpace(scope.Dir) == "" {
		return sessionenv.Account{}, "", ClassRefused
	}
	// The resolved home must sit under <af-home>/accounts/<agent>/ — the
	// invariant agentaccount.Dir already guarantees, asserted here because
	// this boundary is exactly where a drifted derivation would cash out.
	if !pathutil.IsStrictlyInside(scope.Dir, filepath.Join(r.home, agentaccount.DirName, p.spec.agent)) {
		return sessionenv.Account{}, "", ClassRefused
	}
	info, err := os.Stat(scope.Dir)
	if err != nil || !info.IsDir() {
		return sessionenv.Account{}, "", ClassRefused
	}
	return scope, p.spec.agent + "\x00" + accountHomeKey(scope.Dir, info), ""
}

// work runs the probe behind the global exec cap, then completes the flight:
// cache the verdict, drop the key, and wake every waiter.
func (p *AgentProbe) work(key string, scope sessionenv.Account, f *flight) {
	r := p.runner
	r.sem <- struct{}{}
	v := p.execProbe(scope)
	<-r.sem

	r.mu.Lock()
	delete(r.flights, key)
	// Verdicts a run actually produced cache for the TTL — the freshness
	// bound the caller already accepted. A refusal, a setup failure, or an
	// unproven credential window is a condition, not an outcome: it is cheap
	// to re-evaluate and must not be frozen behind the TTL (an account
	// registered, a lock released, or a token refreshed mid-TTL is probeable
	// at once).
	if v.Class != ClassRefused && v.Class != ClassUnsupported &&
		v.Class != ClassSetup && v.Class != ClassAuthWindow {
		r.cache[key] = v
	}
	for _, ch := range f.waiters {
		ch <- v
		close(ch)
	}
	r.mu.Unlock()
}

// execProbe is the one bounded, synchronous run. Everything it can produce is
// a Verdict: refusals and infrastructure failures are Unknown of a class,
// never a Go error a caller could drop. The verdict stamps at completion —
// the TTL measures how fresh the evidence IS, so At belongs to the moment the
// run produced it, not the moment the request arrived — and it always
// records the model it was earned under.
func (p *AgentProbe) execProbe(scope sessionenv.Account) (v Verdict) {
	r := p.runner
	v = Verdict{Kind: KindUnknown}
	defer func() {
		v.At = r.now()
		v.Model = p.spec.model
	}()

	// The credential gate is FIRST: before env, before a private cwd,
	// before the executable is ever invoked. If the account's refresh state
	// cannot be proven safe — a held auth.lock, an exp inside the refresh
	// window, metadata af cannot read — the verdict is Unknown and nothing
	// ran. A probe that refreshes is not a probe; it is the second
	// refresher the design exists to prevent.
	release, ok := authProbeable(scope.Dir, r.now(), r.timeout)
	if !ok {
		v.Class = ClassAuthWindow
		return v
	}
	defer release()

	// The private cwd root is re-verified every run: a probe dir swapped
	// for a symlink after construction is refused where it would be used.
	if err := usablePrivateDir(r.probeDir); err != nil {
		v.Class = ClassSetup
		return v
	}
	// No repository content may reach a credentialed run: the CLI discovers
	// AGENTS.md variants and .codex/skills walking ancestors up to the
	// enclosing .git, so any such ancestor above the private root — an af
	// home inside a work tree is the shape that matters — refuses. The
	// run dir itself is fresh-made and can hold nothing.
	if bad := instructionSourceAbove(r.probeDir); bad != "" {
		v.Class = ClassRefused
		return v
	}
	runDir, err := os.MkdirTemp(r.probeDir, runDirPattern)
	if err != nil {
		v.Class = ClassSetup
		return v
	}
	defer func() { _ = os.RemoveAll(runDir) }()

	// Env: the account math over the allowlisted base, and nothing else.
	// The command argument is empty as a statement of fact — the probe is
	// exec'd directly from argv with no shell, so there is no command
	// string for an assignment to hide in. What the call still contributes
	// is the work the boundary exists for: injection of this account's
	// credential root, subtraction of every competing identity, refusal of
	// an active cloud mode, and the shell-startup strip. TMPDIR pins agent
	// scratch into the private run dir — one literal, appended after the
	// math, never an identity name.
	env, err := sessionenv.ApplyAccountEnvironment(
		sessionenv.FilterForCommand(r.baseEnv, p.spec.agent, "", nil), "", scope)
	if err != nil {
		v.Class = ClassRefused
		return v
	}
	env = append(env, "TMPDIR="+runDir)

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	c := exec.CommandContext(ctx, p.spec.argv[0], p.spec.argv[1:]...)
	c.Dir = runDir
	c.Env = env
	// The run owns a process group: SIGINT to the group at the deadline (a
	// mid-refresh SIGKILL can tear a truncate-then-write credential save),
	// os/exec's WaitDelay as the grace window plus the bound on inherited
	// pipes, then sweepProcGroup kills whatever remains. The cap slot is
	// still held — it releases only after the sweep below returns.
	setupProcGroup(c)
	c.Cancel = cancelViaGroup(c)
	c.WaitDelay = waitDelay
	tail := newTailWriter(outputCap)
	c.Stdout = tail
	c.Stderr = tail
	runErr := r.exec.Run(c)
	sweepProcGroup(c)

	// A run the deadline had to interrupt proves nothing — not the wall its
	// captured tail happens to show, and not the turn a last-write file
	// claims. Timeout is Unknown unconditionally: that ordering is the
	// contract this boundary exists to keep.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		v.Class = ClassTimeout
		return v
	}
	// The success token is a FILE, not output text: the CLI writes its
	// final message to the af-named path under the private cwd, so a
	// completed turn is proven structurally rather than scraped from prose.
	// The file settles the question even on a nonzero exit — a turn that
	// completed before a late crash still proves the account answers.
	if msg, ok := readMessageFile(runDir, p.spec.messageFile); ok && msg == p.spec.successText {
		v.Kind = KindHealthy
		return v
	}
	resetsAt, lv := p.spec.wall(tail.Bytes(), r.now())
	if lv != limitNone {
		v.ResetsAt = resetsAt
		if lv == limitAccountWall {
			v.Kind = KindWalled
		} else {
			v.Class = ClassModelLimit
		}
		return v
	}
	switch {
	case runErr != nil:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			v.Class = ClassExit
		} else {
			v.Class = ClassStart
		}
	default:
		v.Class = ClassUnexpected
	}
	return v
}

// instructionSourceAbove walks dir's ancestor chain to the filesystem root
// and returns the first repository-controlled entry a credentialed agent
// could ingest: .git (the discovery boundary the CLI stops at — its presence
// means ancestors below it are a repo), an AGENTS.md variant (the file the
// CLI loads), or .codex (project skills/config). An ancestor that cannot be
// verified — a stat error other than absence — counts as a source: the
// property is "established clean," not "nothing found." A clean chain
// returns "".
func instructionSourceAbove(dir string) string {
	for d := filepath.Clean(dir); ; {
		for _, name := range []string{".git", "AGENTS.md", "Agents.md", ".codex"} {
			path := filepath.Join(d, name)
			if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
				return path
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// readMessageFile reads the success-token file, bounded, and returns its
// trimmed content. Absence, size overflow, or a read error is ok=false —
// never evidence either way.
func readMessageFile(dir, name string) (string, bool) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, messageFileCap))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// ensurePrivateDir creates dir 0700 and verifies it is a real directory —
// construction-time provisioning, mirroring the discipline Register uses for
// the accounts root.
func ensurePrivateDir(dir string) error {
	if err := afhome.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create probe directory %s: %w", dir, err)
	}
	if err := usablePrivateDir(dir); err != nil {
		return err
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("secure probe directory %s: %w", dir, err)
	}
	return nil
}

// usablePrivateDir refuses anything but a real directory: a symlinked probe
// dir would land af's private cwd — and the rollout cwd records — somewhere
// chosen by whoever made the link.
func usablePrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect probe directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("probe directory %s is a symlink; the private cwd must be a real directory", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("probe path %s exists and is not a directory", dir)
	}
	return nil
}

// tailWriter is an io.Writer that keeps only the last cap bytes. A probe's
// output is capped because it is diagnostic input, not data to retain — and
// because an account wall is reported on the LAST line of the CLI's output:
// a flooded head can push a wall banner out of the capture, but cannot push
// it out of the provider's own verdict.
type tailWriter struct {
	buf []byte
	cap int
}

func newTailWriter(cap int) *tailWriter {
	return &tailWriter{cap: cap}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	if len(p) >= w.cap {
		w.buf = append(w.buf[:0], p[len(p)-w.cap:]...)
		return len(p), nil
	}
	need := len(w.buf) + len(p) - w.cap
	if need > 0 {
		copy(w.buf, w.buf[need:])
		w.buf = w.buf[:len(w.buf)-need]
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *tailWriter) Bytes() []byte { return w.buf }
