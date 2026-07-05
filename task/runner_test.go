package task

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestMain initializes the logger so that functions under test that write
// WarningLog/ErrorLog messages do not nil-deref.
func TestMain(m *testing.M) {
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: default the whole package into a sandboxed AGENT_FACTORY_HOME so
	// stray config/state/log writes land in a temp dir instead of the
	// developer's real one. Sandbox AFTER the tripwire snapshots the real
	// environment, BEFORE logging resolves its file path.
	restoreHome := testguard.SandboxHome()
	log.Initialize(false)
	code := m.Run()
	log.Close()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// codexYOLOBanner is the actual codex startup pane from
// sachiniyer/agent-factory#714 — codex rendered its banner, the YOLO-mode
// header, and the "›" (U+203A) input prompt, but the claude-only
// isReadyContent never matched it, so waitForReady spun for the full 60s.
const codexYOLOBanner = "╭───────────────────────────────────────────────╮\n" +
	"│ >_ OpenAI Codex (v0.135.0)                    │\n" +
	"│ permissions: YOLO mode                        │\n" +
	"╰───────────────────────────────────────────────╯\n" +
	"› Use /skills to list available skills"

func TestIsReadyContent(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		content string
		want    bool
	}{
		// claude
		{"empty", "claude", "", false},
		{"claude input prompt", "claude", "some output\n\n❯ ", true},
		{"claude trust prompt", "claude", "Do you trust the files in this folder?\n1. Yes\n2. No", true},
		{"claude mcp trust prompt", "claude", "Claude Code detected a new MCP server from `.mcp.json`.\n1. Use this MCP server", true},
		{
			name:  "claude doc trust prompt",
			agent: "claude",
			content: "Open documentation url for more info: https://docs/\n" +
				"(Y)es/(N)o/(D)on't ask again [Yes]:",
			want: true,
		},
		{"claude not ready", "claude", "installing dependencies...\nready soon", false},

		// No known agent in the resolved command (ResolvedAgent returned "",
		// e.g. program_overrides pointing "claude" at bash, #1131): there is
		// no prompt glyph to wait for, so any non-blank pane output counts as
		// ready. Before #1131 this fell through to the claude signals and a
		// bare shell spun out the full 60s timeout.
		{"non-agent ready on shell prompt (#1131)", "", "sandbox$ ", true},
		{"non-agent ready on any output", "", "hello from some tool", true},
		{"non-agent empty pane not ready", "", "", false},
		{"non-agent blank pane not ready", "", "\n   \n\n", false},

		// codex — regression case from #714.
		{"codex YOLO banner with prompt (#714)", "codex", codexYOLOBanner, true},
		{"codex bare prompt glyph", "codex", "some output\n› ", true},
		// #729: the workspace-trust dialog must NOT be treated as ready —
		// no codex dismissal exists for it, so the prompt would be typed into
		// the dialog. Wait for the real "›" prompt. Regression from #714/#715.
		{"codex trust folder prompt is not ready (#729)", "codex", "Do you trust this folder?\n> Yes", false},
		{"codex trust dialog with later prompt is ready (#729)", "codex", "Do you trust this folder?\n› ", true},
		// Codex must NOT be considered ready on claude's "❯" alone, and the
		// banner box border ("╰") is not a codex ready signal by itself.
		{"codex not ready on claude glyph", "codex", "rendering\n❯ ", false},
		{"codex not ready on box border alone", "codex", "╭──╮\n│ x │\n╰──╯", false},

		// aider
		{"aider banner", "aider", "Aider v0.74.0\nMain model: ...", true},
		{"aider input prompt", "aider", "some output\n> ", true},
		{
			name:  "aider doc trust prompt",
			agent: "aider",
			content: "Open documentation url for more info: https://aider.chat/docs/\n" +
				"(Y)es/(N)o/(D)on't ask again [Yes]:",
			want: true,
		},
		{"aider not ready", "aider", "loading model weights…", false},

		// gemini (best-guess box-border signal — see #714)
		{"gemini box frame", "gemini", "╭──╮\n│ Gemini │\n╰──╯", true},
		{
			name:    "gemini doc trust prompt",
			agent:   "gemini",
			content: "Gemini CLI\nOpen documentation url for more info.\n(D)on't ask again",
			want:    true,
		},
		{"gemini not ready", "gemini", "starting gemini-cli…", false},

		// shared doc-trust guard: both substrings required.
		{"only open documentation url without confirm", "claude", "See Open documentation url for details.", false},
		{"only dont ask again without doc url", "aider", "Some prompt asking (D)on't ask again", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadyContent(tc.content, tc.agent); got != tc.want {
				t.Errorf("isReadyContent(%q, %q) = %v, want %v", tc.content, tc.agent, got, tc.want)
			}
		})
	}
}

func TestTaskRunBaseTitleFallsBackToTaskID(t *testing.T) {
	got := TaskRunBaseTitle(Task{ID: "abc123"})
	if got != "task-abc123" {
		t.Fatalf("unexpected fallback title: %q", got)
	}
}

// TestFormatWaitForReadyTimeoutError covers the UX half of
// sachiniyer/agent-factory#502: when WaitForReady gives up, the returned
// error must carry a trimmed snippet of the captured pane content so the
// user-facing TUI shows what the agent was doing — not just "timed out".
// Empty captured content collapses back to the bare timeout message so
// users don't see a dangling "last pane content:" header.
func TestFormatWaitForReadyTimeoutError(t *testing.T) {
	timeout := 60 * time.Second

	t.Run("happy case appends trimmed snippet", func(t *testing.T) {
		// >5 lines and well under 400 bytes — should keep only the last 5.
		content := "boot 1\nboot 2\nboot 3\nLoading config...\nConnecting to MCP server...\nStill waiting on handshake\nAlmost there..."
		got := formatWaitForReadyTimeoutError(timeout, content).Error()

		wantHeader := "timed out waiting for program to start (1m0s)\nlast pane content:"
		if !strings.HasPrefix(got, wantHeader) {
			t.Fatalf("missing header.\n got=%q\nwant prefix=%q", got, wantHeader)
		}
		if !strings.Contains(got, "  Almost there...") {
			t.Errorf("expected indented snippet line in error, got %q", got)
		}
		if !strings.Contains(got, "  Loading config...") {
			t.Errorf("expected last-5-lines window to include 'Loading config...', got %q", got)
		}
		if strings.Contains(got, "boot 1") || strings.Contains(got, "boot 2") {
			t.Errorf("expected oldest lines to be trimmed off, got %q", got)
		}
	})

	t.Run("empty content omits header entirely", func(t *testing.T) {
		got := formatWaitForReadyTimeoutError(timeout, "").Error()
		want := "timed out waiting for program to start (1m0s)"
		if got != want {
			t.Fatalf("empty content error mismatch.\n got=%q\nwant=%q", got, want)
		}
	})

	t.Run("whitespace-only content treated as empty", func(t *testing.T) {
		got := formatWaitForReadyTimeoutError(timeout, "\n\n   \n\n").Error()
		want := "timed out waiting for program to start (1m0s)"
		if got != want {
			t.Fatalf("whitespace-only content error mismatch.\n got=%q\nwant=%q", got, want)
		}
	})

	t.Run("long content is byte-capped", func(t *testing.T) {
		// One huge line well over the 400-byte cap.
		long := strings.Repeat("x", 1200)
		got := formatWaitForReadyTimeoutError(timeout, long).Error()
		// Header + "\n  " + at most 400 bytes of snippet.
		header := "timed out waiting for program to start (1m0s)\nlast pane content:\n  "
		if !strings.HasPrefix(got, header) {
			t.Fatalf("missing header prefix, got %q", got)
		}
		snippet := strings.TrimPrefix(got, header)
		if len(snippet) > 400 {
			t.Errorf("snippet not capped: len=%d, want <=400", len(snippet))
		}
	})
}

// fakePreviewBackend embeds the package FakeBackend and overrides only
// Preview so WaitForReady tests can drive the captured pane content (and the
// error returned for it) without any real tmux session.
type fakePreviewBackend struct {
	*session.FakeBackend
	previewFn func() (string, error)
}

func (b *fakePreviewBackend) Preview(*session.Instance) (string, error) {
	return b.previewFn()
}

// newPreviewInstance returns an instance whose Preview() is driven by
// previewFn, backed by a FakeBackend so no tmux/git resources are touched.
func newPreviewInstance(t *testing.T, previewFn func() (string, error)) *session.Instance {
	t.Helper()
	return newPreviewInstanceWithProgram(t, "claude", previewFn)
}

// newPreviewInstanceWithProgram is newPreviewInstance with an explicit
// program, so tests can drive the non-agent readiness path (#1131).
func newPreviewInstanceWithProgram(t *testing.T, program string, previewFn func() (string, error)) *session.Instance {
	t.Helper()
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		return &fakePreviewBackend{FakeBackend: session.NewFakeBackend(), previewFn: previewFn}, nil
	})
	defer restore()
	inst, err := session.NewInstance(session.InstanceOptions{Title: "wait-ready", Path: t.TempDir(), Program: program})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	return inst
}

// TestWaitForReadyNonAgentBecomesReadyOnAnyOutput pins the #1131 fix at the
// WaitForReady level: an instance whose program runs no known agent (e.g. the
// play-test sandbox's "claude"→"bash" override) must become ready as soon as
// the pane shows any non-blank content — not spin the full timeout waiting
// for a claude prompt glyph. The 2s watchdog (well under the 10s timeout)
// fails the test on the pre-fix behavior.
func TestWaitForReadyNonAgentBecomesReadyOnAnyOutput(t *testing.T) {
	defer setWaitForReadyTimingForTest(10*time.Second, time.Millisecond)()

	inst := newPreviewInstanceWithProgram(t, "bash", func() (string, error) {
		return "sandbox$ ", nil
	})

	done := make(chan error, 1)
	go func() { done <- WaitForReady(inst) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected non-agent pane with output to be ready, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady still polling a non-agent pane that already has output (#1131 pre-fix behavior)")
	}
}

// setWaitForReadyTimingForTest shrinks the poll/timeout knobs so the polling
// loop runs in milliseconds, and returns a restore func.
func setWaitForReadyTimingForTest(timeout, poll time.Duration) func() {
	prevTimeout, prevPoll := waitForReadyTimeout, waitForReadyPollInterval
	waitForReadyTimeout, waitForReadyPollInterval = timeout, poll
	return func() {
		waitForReadyTimeout, waitForReadyPollInterval = prevTimeout, prevPoll
	}
}

// TestWaitForReadyFailsFastWhenSessionGone is the core #976 fix: when
// Preview() reports tmux.ErrSessionGone (a definitive, non-retryable death),
// WaitForReady must return immediately with a clear "session died" error —
// not poll the full timeout and return a misleading "timed out" message. The
// 2s watchdog (well under the 10s timeout) fails the test if the loop is
// still spinning, which is exactly the pre-fix behavior.
func TestWaitForReadyFailsFastWhenSessionGone(t *testing.T) {
	defer setWaitForReadyTimingForTest(10*time.Second, time.Millisecond)()

	inst := newPreviewInstance(t, func() (string, error) {
		return "", tmux.ErrSessionGone
	})

	done := make(chan error, 1)
	go func() { done <- WaitForReady(inst) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the session is gone, got nil")
		}
		if !errors.Is(err, tmux.ErrSessionGone) {
			t.Fatalf("error must wrap tmux.ErrSessionGone, got %v", err)
		}
		if !strings.Contains(err.Error(), "session died while waiting for agent to start") {
			t.Fatalf("error must explain the session died, got %q", err.Error())
		}
		if strings.Contains(err.Error(), "timed out") {
			t.Fatalf("must not be a misleading timeout error, got %q", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady did not fail fast on ErrSessionGone; it is still polling")
	}
}

// TestWaitForReadyTimeoutCaseChecksErrSessionGone guards the second Preview()
// call site (#989): if the session dies right at the timeout boundary so the
// timeout case observes ErrSessionGone, WaitForReady must surface the same
// actionable "session died" error (wrapping the sentinel) as the ticker case —
// not a misleading generic timeout. Timing is set so the timeout (50ms) fires
// before the first ticker tick (100ms), forcing the timeout branch.
func TestWaitForReadyTimeoutCaseChecksErrSessionGone(t *testing.T) {
	defer setWaitForReadyTimingForTest(50*time.Millisecond, 100*time.Millisecond)()

	inst := newPreviewInstance(t, func() (string, error) {
		return "", tmux.ErrSessionGone
	})

	done := make(chan error, 1)
	go func() { done <- WaitForReady(inst) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the session is gone, got nil")
		}
		if !errors.Is(err, tmux.ErrSessionGone) {
			t.Fatalf("error must wrap tmux.ErrSessionGone, got %v", err)
		}
		if !strings.Contains(err.Error(), "session died while waiting for agent to start") {
			t.Fatalf("error must explain the session died, got %q", err.Error())
		}
		if strings.Contains(err.Error(), "timed out") {
			t.Fatalf("must not be a misleading timeout error, got %q", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady did not return on ErrSessionGone in the timeout case")
	}
}

// TestWaitForReadyKeepsPollingThroughTransientErrors guards the other half of
// the fix: a transient (non-ErrSessionGone) Preview error must NOT abort the
// loop — it keeps polling until the agent becomes ready.
func TestWaitForReadyKeepsPollingThroughTransientErrors(t *testing.T) {
	defer setWaitForReadyTimingForTest(10*time.Second, time.Millisecond)()

	var calls int32
	inst := newPreviewInstance(t, func() (string, error) {
		if atomic.AddInt32(&calls, 1) < 5 {
			return "", errors.New("transient capture failure")
		}
		return "ready now\n❯ ", nil
	})

	done := make(chan error, 1)
	go func() { done <- WaitForReady(inst) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil once the agent is ready, got %v", err)
		}
		if got := atomic.LoadInt32(&calls); got < 5 {
			t.Fatalf("expected polling to continue through transient errors (>=5 previews), got %d", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady never became ready despite transient-then-ready previews")
	}
}

// TestWaitForReadyTimesOutOnPersistentTransientErrors confirms a persistent
// transient error still falls through to the normal timeout path — it is not
// misclassified as session death.
func TestWaitForReadyTimesOutOnPersistentTransientErrors(t *testing.T) {
	defer setWaitForReadyTimingForTest(50*time.Millisecond, time.Millisecond)()

	inst := newPreviewInstance(t, func() (string, error) {
		return "", errors.New("transient capture failure")
	})

	done := make(chan error, 1)
	go func() { done <- WaitForReady(inst) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error, got nil")
		}
		if errors.Is(err, tmux.ErrSessionGone) {
			t.Fatalf("a transient error must not be reported as session-gone, got %v", err)
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("expected a timeout error, got %q", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady never timed out on persistent transient errors")
	}
}

// setWaitLimitForTest pins WaitForReady's usage-limit detector and clock so a
// park test resolves a deterministic reset time without loading config or
// touching the wall clock (#1146 PR4). Returns a restore func.
func setWaitLimitForTest(detector LimitDetector, now func() time.Time) func() {
	prevDet, prevNow := newLimitDetectorForWait, waitLimitNow
	newLimitDetectorForWait = func() LimitDetector { return detector }
	waitLimitNow = now
	return func() {
		newLimitDetectorForWait, waitLimitNow = prevDet, prevNow
	}
}

// TestWaitForReadyParksOnUsageLimit is the core PR4 change (#1146): when the
// agent shows a usage-limit banner during startup instead of its ready prompt,
// WaitForReady must STOP waiting and return a *LimitReachedError carrying the
// parsed reset time — a distinct, NON-failure outcome the create path parks on —
// rather than spinning the full timeout and returning a failure. The 2s watchdog
// (well under the 10s test timeout) fails the test on the pre-fix spin-to-timeout
// behavior.
func TestWaitForReadyParksOnUsageLimit(t *testing.T) {
	defer setWaitForReadyTimingForTest(10*time.Second, time.Millisecond)()
	fixed := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	defer setWaitLimitForTest(NewLimitDetector(nil), func() time.Time { return fixed })()

	cases := []struct {
		name    string
		program string
		banner  string
		wantUTC time.Time
	}{
		{
			name:    "claude",
			program: "claude",
			banner:  "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)",
			// 2pm America/New_York (EDT, UTC-4) on 2026-07-04 = 18:00 UTC.
			wantUTC: time.Date(2026, 7, 4, 18, 0, 0, 0, time.UTC),
		},
		{
			name:    "codex",
			program: "codex",
			banner:  "You've hit your usage limit. Try again at Jul 25th, 2026 5:55 PM.",
			// codex renders local time; the fixed clock's zone is UTC, so 5:55 PM UTC.
			wantUTC: time.Date(2026, 7, 25, 17, 55, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := newPreviewInstanceWithProgram(t, tc.program, func() (string, error) {
				return tc.banner, nil
			})
			done := make(chan error, 1)
			go func() { done <- WaitForReady(inst) }()

			select {
			case err := <-done:
				var limitErr *LimitReachedError
				if !errors.As(err, &limitErr) {
					t.Fatalf("expected *LimitReachedError, got %v", err)
				}
				if !errors.Is(err, ErrLimitReached) {
					t.Fatalf("park error must wrap ErrLimitReached, got %v", err)
				}
				if strings.Contains(err.Error(), "timed out") {
					t.Fatalf("a park must not be a timeout/failure error, got %q", err.Error())
				}
				if got := limitErr.ResetAt.UTC(); !got.Equal(tc.wantUTC) {
					t.Fatalf("ResetAt = %v, want %v", got, tc.wantUTC)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("WaitForReady did not park on a usage-limit banner; still polling")
			}
		})
	}
}

// TestWaitForReadyDoesNotParkNonLimitAgent guards the scope boundary (#1146): a
// gemini/aider pane has no usage-limit matcher, so even a limit-looking banner
// must NOT park — WaitForReady keeps polling and times out as before. gemini's
// readiness glyph never appears here, so it hits the (shortened) timeout.
func TestWaitForReadyDoesNotParkNonLimitAgent(t *testing.T) {
	defer setWaitForReadyTimingForTest(200*time.Millisecond, time.Millisecond)()
	defer setWaitLimitForTest(NewLimitDetector(nil), time.Now)()

	inst := newPreviewInstanceWithProgram(t, "gemini", func() (string, error) {
		return "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)", nil
	})
	done := make(chan error, 1)
	go func() { done <- WaitForReady(inst) }()

	select {
	case err := <-done:
		if errors.Is(err, ErrLimitReached) {
			t.Fatalf("gemini has no limit matcher and must not park, got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("expected a plain timeout for a non-limit agent, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady never returned for a non-limit agent")
	}
}
