package probe

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// fakeExec implements cmd.Executor, capturing every command it is asked to run
// and behaving however the test's run hook dictates. Nothing in this package's
// tests ever spawns a real agent CLI: the probe's contract is proven against
// the exec.Cmd it BUILDS (argv, env, cwd, writer caps), not against a provider.
type recordedRun struct {
	args      []string
	env       []string
	dir       string
	waitDelay time.Duration
}

type fakeExec struct {
	mu       sync.Mutex
	calls    []recordedRun
	inFlight int
	maxSeen  int
	runFn    func(c *exec.Cmd) error
	release  chan struct{}
}

func (f *fakeExec) Run(c *exec.Cmd) error {
	f.mu.Lock()
	f.calls = append(f.calls, recordedRun{
		args:      append([]string(nil), c.Args...),
		env:       append([]string(nil), c.Env...),
		dir:       c.Dir,
		waitDelay: c.WaitDelay,
	})
	f.inFlight++
	if f.inFlight > f.maxSeen {
		f.maxSeen = f.inFlight
	}
	fn, release := f.runFn, f.release
	f.mu.Unlock()

	if release != nil {
		<-release
	}
	var err error
	if fn != nil {
		err = fn(c)
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	return err
}

func (f *fakeExec) Output(c *exec.Cmd) ([]byte, error) {
	return nil, errors.New("probe must use Run with capped writers, never Output")
}

func (f *fakeExec) numCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeExec) call(idx int) recordedRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[idx]
}

func (f *fakeExec) peakConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxSeen
}

// newTestRunner builds a Runner against a temp AF home with one registered
// codex account ("work") and the given base environment.
func newTestRunner(t *testing.T, exec *fakeExec, baseEnv []string, mutate func(*Options)) (*Runner, string) {
	t.Helper()
	home := t.TempDir()
	_, err := agentaccount.Register(home, "codex", "work")
	require.NoError(t, err)
	opts := Options{BaseEnv: baseEnv, Now: time.Now}
	if mutate != nil {
		mutate(&opts)
	}
	r, err := NewRunner(home, exec, opts)
	require.NoError(t, err)
	return r, home
}

// collect waits for exactly one verdict on ch. Every Request channel delivers
// one verdict then closes; a test that deadlocks here found a real bug.
func collect(t *testing.T, ch <-chan Verdict) Verdict {
	t.Helper()
	select {
	case v, ok := <-ch:
		require.True(t, ok, "verdict channel closed without a verdict")
		_, stillOpen := <-ch
		require.False(t, stillOpen, "verdict channel must close after one verdict")
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("no verdict within 10s")
		return Verdict{}
	}
}

func writeSuccess(c *exec.Cmd, text string) {
	if err := os.WriteFile(filepath.Join(c.Dir, codexMessageFile), []byte(text), 0o600); err != nil {
		panic(err)
	}
}

// exitErr fabricates a nonzero *exec.ExitError — the shape cmd.Run returns
// for a process that ran and failed, distinguished from a start failure.
func exitErr() error {
	return &exec.ExitError{ProcessState: &os.ProcessState{}}
}

func envValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix), true
		}
	}
	return "", false
}

func envHas(env []string, name string) bool {
	_, ok := envValue(env, name)
	return ok
}

// --- The boundary requirements, one test each -------------------------------

// Env reaches the child ONLY through sessionenv's account math: the filtered
// base minus every competing identity, plus the account's credential root. The
// strongest form of the check is equality with the math itself — an env built
// by any other path cannot match byte-for-byte.
func TestProbe_EnvBuiltOnlyThroughSessionenv(t *testing.T) {
	fx := &fakeExec{runFn: func(c *exec.Cmd) error { return nil }}
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/tester",
		"OPENAI_API_KEY=sk-ambient-must-not-leak",
		"CODEX_HOME=/home/tester/.codex",
		"CODEX_API_KEY=cx-ambient",
		"AF_SESSION=af_test-pane",
		"AF_HOME=/home/tester/.agent-factory",
		"AF_SESSION_GEN=7",
		"AWS_SECRET_ACCESS_KEY=not-allowlisted",
	}
	r, home := newTestRunner(t, fx, base, nil)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	v := collect(t, p.Request("work"))
	require.Equal(t, KindUnknown, v.Kind) // no token written

	got := fx.call(0).env
	scope, err := agentaccount.Selected(home, "codex", "work")
	require.NoError(t, err)
	scrubbed := make([]string, 0, len(base))
	for _, kv := range base {
		name := strings.SplitN(kv, "=", 2)[0]
		if name == "AF_SESSION" || name == "AF_HOME" || name == "AF_SESSION_GEN" ||
			name == "NO_BROWSER" || name == "BROWSER" {
			continue
		}
		scrubbed = append(scrubbed, kv)
	}
	want, err := sessionenv.ApplyAccountEnvironment(
		sessionenv.FilterForCommand(scrubbed, "codex", "", nil), "", scope)
	require.NoError(t, err)
	require.Equal(t, want, got, "env must be exactly sessionenv's account math over the scrubbed base")

	dir, err := agentaccount.Dir(home, "codex", "work")
	require.NoError(t, err)
	val, ok := envValue(got, "CODEX_HOME")
	require.True(t, ok)
	require.Equal(t, dir, val)
	assert.False(t, envHas(got, "OPENAI_API_KEY"))
	assert.False(t, envHas(got, "CODEX_API_KEY"))
	assert.False(t, envHas(got, "AF_SESSION"))
	assert.False(t, envHas(got, "AWS_SECRET_ACCESS_KEY"))
}

// argv is a fixed literal: identical across accounts and runs, carrying
// nothing derived from the account name, the home, or any caller input.
func TestProbe_ArgvFixedLiterals(t *testing.T) {
	fx := &fakeExec{runFn: func(c *exec.Cmd) error { return nil }}
	r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	collect(t, p.Request("work"))
	collect(t, p.Request("work"))
	require.Equal(t, 2, fx.numCalls())

	want := []string{
		"codex", "exec",
		"--skip-git-repo-check",
		"-c", `model_reasoning_effort="low"`,
		"--output-last-message", codexMessageFile,
		"Reply with exactly the single word: ok",
	}
	for i := 0; i < 2; i++ {
		require.Equal(t, want, fx.call(i).args)
		for _, word := range fx.call(i).args {
			assert.NotContains(t, word, "work")
			assert.NotContains(t, word, string(filepath.Separator), "no path may reach argv: %q", word)
		}
	}
}

// The probe cwd is a private, af-owned 0700 directory under <af-home>/probe —
// never /tmp (world-writable, a prompt-injection channel into a credential
// home) and never a session worktree (repo-controlled files must not execute
// under credentials). It is removed when the run ends.
func TestProbe_CwdPrivateAfOwned0700(t *testing.T) {
	fx := &fakeExec{runFn: func(c *exec.Cmd) error {
		info, err := os.Stat(c.Dir)
		require.NoError(t, err)
		require.True(t, info.IsDir())
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
			"probe run dir must be owner-only")
		return nil
	}}
	r, home := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	collect(t, p.Request("work"))
	runDir := fx.call(0).dir

	probeDir := filepath.Join(home, "probe")
	require.Equal(t, probeDir, filepath.Dir(runDir),
		"run dir must be a direct child of the af-owned probe dir, never the system temp root or a worktree")
	info, err := os.Stat(probeDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	_, err = os.Lstat(runDir)
	require.True(t, os.IsNotExist(err), "run dir must be removed after the verdict")
}

// The default login is refused: an empty or unregistered account never
// reaches the executor, because ambient has no credential directory af can
// scope — a probe there would run under whoever the daemon is.
func TestProbe_RefusesDefaultLogin(t *testing.T) {
	fx := &fakeExec{}
	r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	for _, name := range []string{"", "   ", "ghost", "../escape", "."} {
		v := collect(t, p.Request(name))
		require.Equal(t, KindUnknown, v.Kind, "account %q", name)
		require.Equal(t, ClassRefused, v.Class, "account %q", name)
	}
	require.Equal(t, 0, fx.numCalls(), "a refused probe must never exec")
}

// A hard timeout is Unknown, never Walled — unconditionally: a killed probe
// is the absence of evidence, and the deadline is checked before the token
// file and the wall scan, so a run that shows the banner or even writes the
// token and then hangs past its deadline still reads timeout.
func TestProbe_TimeoutIsUnknownNeverWalled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		runFn func(c *exec.Cmd) error
	}{
		{
			name: "ordinary output",
			runFn: func(c *exec.Cmd) error {
				_, _ = c.Stdout.Write([]byte("thinking\nsome ordinary output\n"))
				time.Sleep(300 * time.Millisecond)
				return nil
			},
		},
		{
			name: "wall banner in the tail",
			runFn: func(c *exec.Cmd) error {
				_, _ = c.Stderr.Write([]byte("You've hit your usage limit. Try again at Nov 6, 2026 3:30 PM.\n"))
				time.Sleep(300 * time.Millisecond)
				return nil
			},
		},
		{
			name: "success token written",
			runFn: func(c *exec.Cmd) error {
				writeSuccess(c, "ok")
				time.Sleep(300 * time.Millisecond)
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &fakeExec{runFn: tc.runFn}
			r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, func(o *Options) {
				o.Timeout = 50 * time.Millisecond
			})
			p, ok := r.AccountProbe("codex")
			require.True(t, ok)

			v := collect(t, p.Request("work"))
			require.Equal(t, KindUnknown, v.Kind)
			require.Equal(t, ClassTimeout, v.Class)
		})
	}
}

// The hard timeout bounds the whole wait, not just the direct child's life:
// WaitDelay keeps a descendant holding the output pipes open from stalling
// Run — and its semaphore slot — forever.
func TestProbe_BoundsPostKillPipeWait(t *testing.T) {
	fx := &fakeExec{runFn: func(c *exec.Cmd) error { return nil }}
	r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	collect(t, p.Request("work"))
	require.Equal(t, waitDelay, fx.call(0).waitDelay,
		"WaitDelay must bound the wait on inherited pipes after a kill")
}

// Output is capped and treated as data. The cap keeps the TAIL, where the
// wall message lands; a run that floods stdout still yields its verdict, and
// the captured bytes stay bounded.
func TestProbe_OutputCappedToTail(t *testing.T) {
	banner := "You've hit your usage limit. Try again in 1 hour."
	fx := &fakeExec{runFn: func(c *exec.Cmd) error {
		_, _ = c.Stdout.Write([]byte(strings.Repeat("x", outputCap*4)))
		_, _ = c.Stderr.Write([]byte("ERROR: " + banner + "\n"))
		return exitErr()
	}}
	r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	v := collect(t, p.Request("work"))
	require.Equal(t, KindWalled, v.Kind)

	var big strings.Builder
	big.Grow(outputCap * 2)
	for i := 0; i < outputCap*2; i++ {
		big.WriteByte('y')
	}
	fx2 := &fakeExec{runFn: func(c *exec.Cmd) error {
		_, _ = c.Stdout.Write([]byte(banner + "\n"))
		_, _ = c.Stdout.Write([]byte(big.String()))
		return nil
	}}
	r2, home2 := newTestRunner(t, fx2, []string{"PATH=/usr/bin"}, nil)
	_ = home2
	p2, ok := r2.AccountProbe("codex")
	require.True(t, ok)
	// The banner sat only in the discarded head: without it the run is
	// unparseable, and unparseable is never walled.
	v2 := collect(t, p2.Request("work"))
	require.Equal(t, KindUnknown, v2.Kind)
}

// Single-flight: concurrent requests for one account join one exec and share
// its verdict; the TTL cache then serves repeats without any exec at all. The
// verdict stamps at completion — a two-second run's verdict is dated by when
// it finished, not when it was asked for, so the TTL measures real freshness.
func TestProbe_SingleFlightPerAccountAndTTLCache(t *testing.T) {
	release := make(chan struct{})
	now := time.Now()
	clock := func() time.Time { return now }
	fx := &fakeExec{
		release: release,
		runFn: func(c *exec.Cmd) error {
			now = now.Add(2 * time.Second) // the run took two seconds
			writeSuccess(c, "ok\n")
			return nil
		},
	}
	r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, func(o *Options) {
		o.Now = clock
		o.TTL = time.Minute
	})
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	chA := p.Request("work")
	chB := p.Request("work")
	close(release)
	vA := collect(t, chA)
	require.Equal(t, vA, collect(t, chB))
	require.True(t, vA.At.Equal(now), "verdict must stamp at completion, not at request time")
	require.Equal(t, 1, fx.numCalls(), "two requests for one account must share one exec")

	// Inside the TTL the verdict is served from cache — no second exec.
	v := collect(t, p.Request("work"))
	require.Equal(t, KindHealthy, v.Kind)
	require.Equal(t, vA, v)
	require.Equal(t, 1, fx.numCalls())

	// Past the TTL the evidence is stale and the probe runs again.
	now = now.Add(2 * time.Minute)
	collect(t, p.Request("work"))
	require.Equal(t, 2, fx.numCalls())
}

// A small global cap bounds concurrent execs across accounts.
func TestProbe_GlobalConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	fx := &fakeExec{release: release}
	r, home := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, func(o *Options) {
		o.MaxConcurrent = 1
		o.MaxInFlight = 8
	})
	_, err := agentaccount.Register(home, "codex", "other")
	require.NoError(t, err)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	chA := p.Request("work")
	chB := p.Request("other")
	close(release)
	collect(t, chA)
	collect(t, chB)
	require.Equal(t, 2, fx.numCalls())
	require.Equal(t, 1, fx.peakConcurrency(), "execs must run one at a time under cap=1")
}

// The exec never runs on the caller's goroutine: Request returns while the
// executor is still parked, so a poll tick can never be blocked by a probe.
func TestProbe_NeverOnTheCallingGoroutine(t *testing.T) {
	release := make(chan struct{})
	fx := &fakeExec{release: release}
	r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	ch := p.Request("work")
	select {
	case v := <-ch:
		t.Fatalf("verdict %v arrived while the executor was still parked", v)
	default:
	}
	close(release)
	collect(t, ch)
}

// Verdicts: a written success token is Healthy; the limit banner is Walled
// with its reset; everything else is Unknown of some class — and unparseable
// output is never walled.
func TestProbe_VerdictShapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runFn     func(c *exec.Cmd) error
		wantKind  Kind
		wantClass string
		wantReset bool
	}{
		{
			name:     "healthy on exact token",
			runFn:    func(c *exec.Cmd) error { writeSuccess(c, "ok\n"); return nil },
			wantKind: KindHealthy,
		},
		{
			name: "token wins over a stray banner",
			runFn: func(c *exec.Cmd) error {
				writeSuccess(c, "ok")
				_, _ = c.Stderr.Write([]byte("You've hit your usage limit."))
				return nil
			},
			wantKind: KindHealthy,
		},
		{
			name: "walled with absolute reset",
			runFn: func(c *exec.Cmd) error {
				_, _ = c.Stderr.Write([]byte("ERROR: You've hit your usage limit. Try again at Nov 6, 2026 3:30 PM.\n"))
				return exitErr()
			},
			wantKind:  KindWalled,
			wantReset: true,
		},
		{
			name: "walled with relative reset",
			runFn: func(c *exec.Cmd) error {
				_, _ = c.Stderr.Write([]byte("You've hit your usage limit. Try again in 4 days 2 hours 46 minutes.\n"))
				return exitErr()
			},
			wantKind:  KindWalled,
			wantReset: true,
		},
		{
			name: "walled without reset",
			runFn: func(c *exec.Cmd) error {
				_, _ = c.Stderr.Write([]byte("You've hit your usage limit.\n"))
				return exitErr()
			},
			wantKind: KindWalled,
		},
		{
			name:      "exit nonzero without banner",
			runFn:     func(c *exec.Cmd) error { return exitErr() },
			wantKind:  KindUnknown,
			wantClass: ClassExit,
		},
		{
			name:      "start failure",
			runFn:     func(c *exec.Cmd) error { return errors.New("codex: executable not found") },
			wantKind:  KindUnknown,
			wantClass: ClassStart,
		},
		{
			name:      "clean exit without token is unexpected, never walled",
			runFn:     func(c *exec.Cmd) error { _, _ = c.Stdout.Write([]byte("codex\nnot the token\n")); return nil },
			wantKind:  KindUnknown,
			wantClass: ClassUnexpected,
		},
		{
			name:      "wrong token text is unexpected",
			runFn:     func(c *exec.Cmd) error { writeSuccess(c, "yes"); return nil },
			wantKind:  KindUnknown,
			wantClass: ClassUnexpected,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &fakeExec{runFn: tc.runFn}
			r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
			p, ok := r.AccountProbe("codex")
			require.True(t, ok)
			v := collect(t, p.Request("work"))
			require.Equal(t, tc.wantKind, v.Kind)
			if tc.wantClass != "" {
				require.Equal(t, tc.wantClass, v.Class)
			}
			require.Equal(t, tc.wantReset, !v.ResetsAt.IsZero())
			require.False(t, v.At.IsZero())
		})
	}
}

// Agents without a probe report ok=false — they fall back to reading-only
// confirmation rather than running an unreviewed invocation.
func TestProbe_UnsupportedAgentHasNoProbe(t *testing.T) {
	fx := &fakeExec{}
	r, _ := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	for _, agent := range []string{"claude", "gemini", "aider", "amp", "opencode", "devin", "codexx", ""} {
		_, ok := r.AccountProbe(agent)
		require.False(t, ok, agent)
	}
}

// Past the in-flight bound, a request is refused busy instead of stacking
// another parked goroutine against the same saturated executor.
func TestProbe_BusyBeyondInFlightBound(t *testing.T) {
	release := make(chan struct{})
	fx := &fakeExec{release: release}
	r, home := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, func(o *Options) {
		o.MaxConcurrent = 1
		o.MaxInFlight = 2
	})
	for _, name := range []string{"b1", "b2"} {
		_, err := agentaccount.Register(home, "codex", name)
		require.NoError(t, err)
	}
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	p.Request("work")
	p.Request("b1")
	v := collect(t, p.Request("b2"))
	require.Equal(t, KindUnknown, v.Kind)
	require.Equal(t, ClassBusy, v.Class)
	close(release)
}

// The runner refuses a home it cannot place the probe dir under: relative or
// empty homes are rejected at construction, and a symlinked probe dir is a
// refusal, not a follow.
func TestNewRunner_RefusesBadHomes(t *testing.T) {
	fx := &fakeExec{}
	_, err := NewRunner("", fx, Options{})
	require.Error(t, err)
	_, err = NewRunner("relative/home", fx, Options{})
	require.Error(t, err)
	_, err = NewRunner(filepath.Join(t.TempDir(), "missing"), nil, Options{})
	require.Error(t, err, "nil executor must be refused")

	home := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.Symlink(target, filepath.Join(home, "probe")))
	_, err = NewRunner(home, fx, Options{})
	require.Error(t, err, "a symlinked probe dir is refused, never followed")
}

// A probe dir swapped out for a symlink between construction and exec is
// refused per-run: the boundary is re-verified where the run happens.
func TestProbe_ProbeDirSwapRefusedPerRun(t *testing.T) {
	fx := &fakeExec{}
	r, home := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	require.NoError(t, os.Remove(filepath.Join(home, "probe")))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(home, "probe")))
	v := collect(t, p.Request("work"))
	require.Equal(t, KindUnknown, v.Kind)
	require.Equal(t, ClassSetup, v.Class)
	require.Equal(t, 0, fx.numCalls())
}

// tailWriter keeps the last cap bytes and nothing else — the cap is a hard
// bound, exercised directly.
func TestTailWriter_KeepsLastBytes(t *testing.T) {
	w := newTailWriter(8)
	_, _ = w.Write([]byte("0123456789abcdef"))
	require.Equal(t, "89abcdef", string(w.Bytes()))
	_, _ = w.Write([]byte("ZZ"))
	require.Equal(t, "abcdefZZ", string(w.Bytes()))
}

// The probe package never exposes the captured output: the verdict carries
// no output text by construction, and the only classifications a caller can
// observe are the declared set.
func TestVerdict_ClassesAreDeclared(t *testing.T) {
	declared := []string{
		ClassRefused, ClassBusy, ClassSetup, ClassTimeout,
		ClassStart, ClassExit, ClassUnexpected,
	}
	for _, class := range declared {
		require.NotEmpty(t, class)
	}
}
