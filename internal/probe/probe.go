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
// credential home — the same grant a pane receives, minus tmux — so the
// boundary's job is argv/env/cwd provenance, not secrecy:
//
//   - argv is a fixed literal slice. No account name, path, or caller input
//     ever reaches it; the per-agent spec is a package-level constant.
//   - env is built ONLY through sessionenv's account math —
//     FilterForCommand over a scrubbed base, then ApplyAccountEnvironment —
//     the same door the daemon-spawned editor uses (vscodeAccountEnvironment,
//     #4311). No ad-hoc os.Setenv, no VAR=value command string: a probe has
//     no command string at all, so the smuggling surface
//     ValidateAccountEnvironmentCommand governs is empty by construction.
//   - cwd is a private af-owned directory, <af-home>/probe/<run-id>, created
//     0700 — never /tmp (world-writable instruction-file injection into a
//     credential home) and never a session worktree (repo-controlled files
//     must not execute under credentials). It also keeps probe rollouts out
//     of the cwd-filtered conversation-capture path: their session_meta cwd
//     is this dir, which never equals a session working directory.
//   - accounts resolve ONLY through agentaccount.Selected — never config
//     text, never a symlinked path — and the resolved dir must sit under
//     <af-home>/accounts/<agent>/. The default login (empty or unregistered
//     name) is refused before any exec: ambient has no credential directory
//     af can scope.
//   - output is size-capped (the tail, where a wall message lands), treated
//     as data for the wall scan, and never logged or carried on a verdict.
//   - a hard timeout kills the child (and WaitDelay bounds the post-kill
//     wait on inherited pipes); the deadline is checked before any positive
//     read — timeout is Unknown, unconditionally, never Walled.
//   - executions are single-flight per account, capped globally, and always
//     on a worker goroutine — never the caller's (poll) goroutine.
//
// The result type is deliberately tiny: Kind, an optional reset time, a
// coarse class for Unknown, and a timestamp. It is evidence, never an
// entitlement — the TTL is the whole freshness claim.
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
	// KindWalled means the run showed the provider's recognizable usage-limit
	// message. ResetsAt carries the parsed reset time when one is present.
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
	// ResetsAt is the parsed reset time for a Walled verdict, zero when the
	// wall message named none or could not be parsed.
	ResetsAt time.Time
	// Class is the coarse Unknown classification — one of the Class*
	// constants, stable so event streams can enumerate it.
	Class string
	// At is when the probe completed, the timestamp a TTL is measured from.
	At time.Time
}

// Unknown classifications. They are declared strings, not errors, because the
// verdict IS the contract: the engine logs the class, it does not branch on
// error identity.
const (
	// ClassRefused covers a probe the boundary declined to run: the default
	// login, an invalid or unregistered name, a resolved home outside the
	// account root, or a sessionenv refusal.
	ClassRefused = "refused"
	// ClassBusy means the in-flight bound was already met; the request was
	// dropped rather than queueing past it.
	ClassBusy = "busy"
	// ClassSetup means the private probe directory could not be verified or
	// the per-run directory could not be created.
	ClassSetup = "setup"
	// ClassTimeout means the hard deadline killed the child. A timeout is
	// Unknown, never Walled.
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
	// daemon's own, minus the names this package always scrubs. Nil resolves
	// to os.Environ() at construction.
	BaseEnv []string
	// TTL is how long a completed verdict is served from cache before the
	// probe may run again. Zero disables caching (every Request execs); the
	// design's default is 10m, applied by the config layer.
	TTL time.Duration
	// Timeout is the hard kill bound per run — design: ~90s. Zero uses
	// defaultTimeout.
	Timeout time.Duration
	// MaxConcurrent caps simultaneous execs across all accounts — design: a
	// small global cap of 1–2. Zero uses defaultMaxConcurrent.
	MaxConcurrent int
	// MaxInFlight bounds total in-flight calls (queued plus running). Beyond
	// it a Request is refused busy instead of parking another goroutine
	// against a saturated executor. Zero uses defaultMaxInFlight.
	MaxInFlight int
	// Now is the clock for cache ages, reset-time resolution and verdict
	// stamps. Nil uses time.Now; tests inject a fixed clock.
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
	// waitDelay bounds the post-exit wait on inherited output pipes: a
	// descendant holding stdout open cannot stall Run past this once the
	// child is dead or the deadline has fired.
	waitDelay = 5 * time.Second
)

// scrubbedEnvNames are removed from the base environment before the account
// math runs, for the same reasons daemon/vscode_account_env.go scrubs them:
// the three tmux ancestry markers (AF_SESSION/AF_HOME/AF_SESSION_GEN — the
// names tmux.EnvMarker* in session/tmux/envmarker.go pin; duplicating the
// strings rather than importing a pane package for constants) keep a daemon
// that was itself launched inside an af pane from stamping a session's
// identity onto a probe process, where doctor's process attribution would
// then own it. NO_BROWSER/BROWSER belong to af's account-login flow; a probe
// is not a login pane and must not inherit its suppressors.
var scrubbedEnvNames = []string{
	"AF_SESSION", "AF_HOME", "AF_SESSION_GEN", "NO_BROWSER", "BROWSER",
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
	scrubbed := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		drop := false
		for _, denied := range scrubbedEnvNames {
			if name == denied {
				drop = true
				break
			}
		}
		if !drop {
			scrubbed = append(scrubbed, kv)
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
		baseEnv:     scrubbed,
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

// agentProbeSpec is the per-agent recipe: a fixed literal argv and the two
// recognition rules a verdict needs. argv[0] resolves through PATH at exec
// time, exactly as a bare agent name does for a pane launch.
type agentProbeSpec struct {
	agent       string
	argv        []string
	messageFile string // relative file the CLI writes its final message to
	successText string // exact trimmed content proving a completed turn
	// wall recognizes the provider's usage-limit message in the captured
	// tail and parses its reset phrase when present. ok=false never claims
	// a wall — unparseable is Unknown, not Walled.
	wall func(output []byte, now time.Time) (resetsAt time.Time, ok bool)
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
// Concurrent requests for the same account join one in-flight call; a
// completed verdict inside its TTL is served from cache with no exec at all.
func (p *AgentProbe) Request(account string) <-chan Verdict {
	ch := make(chan Verdict, 1)
	r := p.runner
	account = strings.TrimSpace(account)
	key := p.spec.agent + "\x00" + account

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
		ch <- Verdict{Kind: KindUnknown, Class: ClassBusy, At: r.now()}
		close(ch)
		return ch
	}
	f := &flight{waiters: []chan Verdict{ch}}
	r.flights[key] = f
	r.mu.Unlock()

	go p.work(key, account, f)
	return ch
}

// work runs the probe behind the global exec cap, then completes the flight:
// cache the verdict, drop the key, and wake every waiter.
func (p *AgentProbe) work(key, account string, f *flight) {
	r := p.runner
	r.sem <- struct{}{}
	v := p.execProbe(account)
	<-r.sem

	r.mu.Lock()
	delete(r.flights, key)
	// Verdicts a run actually produced cache for the TTL — the freshness
	// bound the caller already accepted. A refusal or setup failure is a
	// condition, not an outcome: it is cheap to re-evaluate and must not be
	// frozen behind the TTL (an account registered mid-TTL is probeable at
	// once).
	if v.Class != ClassRefused && v.Class != ClassSetup {
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
// run produced it, not the moment the request arrived.
func (p *AgentProbe) execProbe(account string) (v Verdict) {
	r := p.runner
	v = Verdict{Kind: KindUnknown}
	defer func() { v.At = r.now() }()

	if account == "" {
		v.Class = ClassRefused
		return v
	}
	scope, err := agentaccount.Selected(r.home, p.spec.agent, account)
	if err != nil || strings.TrimSpace(scope.Dir) == "" {
		v.Class = ClassRefused
		return v
	}
	// The resolved home must sit under <af-home>/accounts/<agent>/ — the
	// invariant agentaccount.Dir already guarantees, asserted here because
	// this boundary is exactly where a drifted derivation would cash out.
	if !pathutil.IsStrictlyInside(scope.Dir, filepath.Join(r.home, agentaccount.DirName, p.spec.agent)) {
		v.Class = ClassRefused
		return v
	}

	// Env: the account math and nothing else. The command argument is empty
	// as a statement of fact — the probe is exec'd directly from argv with
	// no shell, so there is no command string for an assignment to hide in.
	// What the call still contributes is the work the boundary exists for:
	// injection of this account's credential root, subtraction of every
	// competing identity, refusal of an active cloud mode, and the
	// shell-startup strip.
	env, err := sessionenv.ApplyAccountEnvironment(
		sessionenv.FilterForCommand(r.baseEnv, p.spec.agent, "", nil), "", scope)
	if err != nil {
		v.Class = ClassRefused
		return v
	}

	// The private cwd is re-verified every run: a probe dir swapped for a
	// symlink after construction is refused where it would be used.
	if err := usablePrivateDir(r.probeDir); err != nil {
		v.Class = ClassSetup
		return v
	}
	runDir, err := os.MkdirTemp(r.probeDir, runDirPattern)
	if err != nil {
		v.Class = ClassSetup
		return v
	}
	defer func() { _ = os.RemoveAll(runDir) }()

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	c := exec.CommandContext(ctx, p.spec.argv[0], p.spec.argv[1:]...)
	c.Dir = runDir
	c.Env = env
	// CommandContext kills the direct child at the deadline, but Run can then
	// still hang on the output-copy goroutine if a descendant inherited the
	// pipes — with no WaitDelay the hard timeout would not bound the wait at
	// all and stuck runs could exhaust the global cap. The bound is on af's
	// side of the wait; the descendant is the CLI's own problem.
	c.WaitDelay = waitDelay
	tail := newTailWriter(outputCap)
	c.Stdout = tail
	c.Stderr = tail
	runErr := r.exec.Run(c)

	// A run the deadline had to kill proves nothing — not the wall its
	// captured tail happens to show, and not the turn a last-write file
	// claims. Timeout is Unknown unconditionally: that ordering is the
	// contract this boundary exists to keep.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		v.Class = ClassTimeout
		return v
	}
	// The success token is a FILE, not output text: the CLI writes its final
	// message to the af-named path under the private cwd, so a completed
	// turn is proven structurally rather than scraped from prose. The file
	// settles the question even on a nonzero exit — a turn that completed
	// before a late crash still proves the account answers.
	if msg, ok := readMessageFile(runDir, p.spec.messageFile); ok && msg == p.spec.successText {
		v.Kind = KindHealthy
		return v
	}
	if resetsAt, walled := p.spec.wall(tail.Bytes(), r.now()); walled {
		v.Kind = KindWalled
		v.ResetsAt = resetsAt
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
// the tail is the end that matters, since a wall message is written at the
// run's death. stdout and stderr share one writer, which os/exec maps onto a
// single pipe so the interleave order is preserved.
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
	if excess := len(w.buf) + len(p) - w.cap; excess > 0 {
		w.buf = append(w.buf[:0], w.buf[excess:]...)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *tailWriter) Bytes() []byte {
	return w.buf
}
