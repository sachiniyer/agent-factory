package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// errHangGuardExpired marks the harness giving up on the CLI, as distinct from
// the CLI reporting a failure. They are different outcomes and only one of them
// is a verdict on the contract under test: af returning an error means af
// decided, while this means af never got to. Collapsing them is how a slow box
// gets reported as a readiness regression (#2879), which is the bug these tests
// had — so the distinction is a sentinel rather than a comment.
var errHangGuardExpired = errors.New("hang guard expired before the CLI answered")

func runAFUntilDaemonAdmits(ctx context.Context, retryDelay time.Duration, run func() (string, error)) (string, error) {
	for {
		out, err := run()
		if !daemon.IsDaemonStartingErr(err) {
			return out, err
		}

		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(retryDelay):
		}
	}
}

func runAFWithin(limit time.Duration, bin, repo, home string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	// Capture stdout separately from stderr: the CLI writes the session JSON
	// to stdout but a "wrote logs to …" line to stderr, so CombinedOutput
	// would corrupt the JSON parse.
	run := func() (string, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "AGENT_FACTORY_HOME="+home, "TERM=xterm")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			return stdout.String(), fmt.Errorf("%w; stderr=%s", err, stderr.String())
		}
		return stdout.String(), nil
	}

	// #3111's fourth and fifth sightings ended before a session existed: the
	// first CLI call started a fresh daemon, its bounded internal admission retry
	// expired while restore was still running, and CreateSession was refused.
	// The refusal happens before any create work, so retry only that documented
	// condition. The readiness verdict and launch-marker assertions below remain
	// untouched and still get exactly one successfully admitted create.
	out, err := runAFUntilDaemonAdmits(ctx, 100*time.Millisecond, run)
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%w after %s; last error=%v", errHangGuardExpired, limit, err)
	}
	return out, err
}

// TestCLICreateCodexSessionBecomesReady is the regression test for
// sachiniyer/agent-factory#714. Before isReadyContent became agent-aware, a
// codex session's "›" (U+203A) prompt glyph never matched the claude-only
// ready check, so `af sessions create --program codex` blocked inside
// waitForReady for its full 60s readiness budget and then failed. The
// empty-prompt early-return removed in #709 is what newly routed every create
// through waitForReady and exposed the blind spot.
//
// The fake codex wrapper prints codex's banner and the "›" prompt, then reads
// stdin — mirroring the claude fixture in newHarness.
//
// This test used to discriminate by OUTRACING af: it bounded the create at 30s,
// so the buggy 60s block failed the deadline while a healthy create beat it.
// That made a busy runner indistinguishable from the regression, and it flaked
// on master on both darwin/amd64 and darwin/arm64 while passing linux/arm64 —
// same test, different platforms, which rules out a platform bug and leaves
// timing (#2879).
//
// It now reads af's OWN verdict instead. WaitForReadyOn always terminates: it
// arms a 60s readiness budget (immediately here, since this config declares no
// post-worktree hooks) and the create tears the half-started session down and
// exits non-zero if the prompt is never recognized. So the #714 regression still
// fails this test — as an error FROM af rather than as a lost race — and load
// can no longer change the answer, only how long it takes to get it.
func TestCLICreateCodexSessionBecomesReady(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")

	// #1056: private tmux server so the exec'd af CLI and its daemon cannot
	// leak af_ sessions onto the developer's server.
	testguard.IsolateTmux(t)

	home := testguard.SocketTempDir(t)
	repo := setupGitRepo(t)

	// printf interprets the \n (backslash-n) into a newline; the "›" is the
	// literal U+203A glyph. The wrapper ignores any injected flags
	// (-c developer_instructions=…) exactly like the claude fixture. Its
	// basename must be "codex": since #1116/#1131 the readiness heuristic is
	// selected by the agent detected in the RESOLVED command (token-basename
	// match), so an arbitrarily-named wrapper would get the generic
	// any-output readiness instead of codex's "›" check.
	wrapperDir := filepath.Join(home, "fakebin")
	if err := os.MkdirAll(wrapperDir, 0755); err != nil {
		t.Fatalf("mkdir wrapper dir: %v", err)
	}
	wrapper := filepath.Join(wrapperDir, "codex")
	// The wrapper appends one line per launch, and that COUNT is what keeps three
	// situations from collapsing into one failure message. "af did not recognize
	// the prompt", "af was never allowed to answer", and "the fake codex never ran
	// at all" used to be the same `t.Fatalf`, and only the first means the #714
	// contract broke. A count also has no granularity and no clock, so load
	// cannot change it.
	//
	// The append comes BEFORE the prompt is printed, and the order is the whole
	// point. Printed first, the wrapper could be descheduled between the two
	// writes: tmux exposes the "›", af matches it and the create returns green,
	// all before the append lands — and the test would then read an empty log and
	// report a fixture failure on a run where the readiness path worked. That is
	// the same load-dependent shape this change exists to remove, so the marker
	// has to be durable before anything af can observe exists. Recording first
	// makes "af saw a ready prompt" imply "the marker is already written"; the
	// reverse ordering only implies it eventually.
	launchLog := filepath.Join(home, "codex-launches.log")
	writeFile(t, wrapper, "#!/bin/sh\nprintf 'launched\\n' >> '"+launchLog+"'\nprintf 'OpenAI Codex (vX)\\npermissions: YOLO mode\\n› '\nexec cat\n", 0755)

	cfg := testConfig()
	cfg.DefaultProgram = tmux.ProgramCodex
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: wrapper}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeFile(t, filepath.Join(home, config.ConfigFileName), string(raw), 0644)

	bin := buildBinary(t)

	t.Cleanup(func() {
		_, _ = runAFWithin(30*time.Second, bin, repo, home, "sessions", "kill", "codex-ready")
		killDaemonFromHome(home)
	})

	// A hang guard, NOT an expectation. Its only job is to stop CI hanging if af
	// wedges somewhere with no budget of its own; af's readiness path already
	// self-bounds at 60s and returns an error. It must stay far above any
	// legitimate create, and must never be tuned to "how long a create takes" —
	// the moment it is, it starts racing af's verdict again, which is the bug
	// this test had.
	const createHangGuard = 4 * time.Minute

	out, err := runAFWithin(createHangGuard, bin, repo, home, "sessions", "--repo", repo, "create", "--name", "codex-ready", "--program", tmux.ProgramCodex)

	// Read the count BEFORE judging the error, so the three outcomes stay
	// distinguishable (#2879).
	launches := countFileLines(t, launchLog)
	if launches == 0 {
		// af was never shown a codex prompt to recognize, so nothing here is a
		// verdict on #714. Named separately so a fixture or environment problem
		// can never be waved through as the contract breaking.
		t.Fatalf("the fake codex wrapper never launched, so this run says nothing about whether af "+
			"recognizes the codex prompt — fixture or environment problem, NOT #714: create err=%v\n%s", err, out)
	}
	if errors.Is(err, errHangGuardExpired) {
		// af never rendered a verdict, so this is not one either. This is the
		// "af was never allowed to answer" outcome — the ORIGINAL bug in this
		// test — and labelling it #714 would be that bug wearing the new
		// diagnostics. af self-bounds at 60s, so reaching a 4-minute guard means
		// something wedged, not that a prompt went unrecognized.
		t.Fatalf("the create never returned within the %s hang guard, so af rendered no readiness "+
			"verdict — a hang or environment failure, NOT #714 (the fake codex launched %d time(s)): %v\n%s",
			createHangGuard, launches, err, out)
	}
	if err != nil {
		// A real answer from af — but not necessarily a READINESS answer. The
		// marker proves the shell began, and it deliberately runs before the prompt
		// is printed, so it says nothing about what reached the pane. A create can
		// fail after launch for reasons unrelated to recognizing a glyph: the
		// session disappearing, a worktree or daemon failure. Blaming #714 for
		// those is the same mislabelling this test was fixed to stop, one branch
		// over. af names its own failure, so read it instead of assuming.
		//
		// Match the readiness-TIMEOUT verdict, not the wrapper around it.
		// ErrAgentReadiness reads "agent did not become ready" and wraps several
		// distinct failures, including "session died while waiting for agent to
		// start" — a dead pane, which is not a prompt-recognition regression. Only
		// formatWaitForReadyTimeoutError's text means af actually polled to the end
		// of its budget and never recognized the prompt, which is #714.
		if strings.Contains(err.Error(), "timed out waiting for program to start") {
			t.Fatalf("regression #714: the fake codex launched %d time(s) and af polled the pane until "+
				"its readiness budget expired without recognizing the codex prompt: %v\n%s",
				launches, err, out)
		}
		t.Fatalf("the create failed after the fake codex launched %d time(s), but NOT on readiness — "+
			"af reported a different failure, so this is not a #714 verdict: %v\n%s", launches, err, out)
	}

	var data instanceData
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("parse create response: %v\n%s", err, out)
	}
	if data.Title != "codex-ready" {
		t.Fatalf("unexpected create response: %+v", data)
	}
}

// TestCLICreateCodexWaitsPastTrustPrompt is the regression test for
// sachiniyer/agent-factory#729. The #714/#715 fix added "Do you trust this
// folder" to codex's ready signals so waitForReady would exit on the trust
// dialog — but codex has no trust-dismissal in CheckAndHandleTrustPrompt, so
// the next user prompt was typed into the dialog instead of the agent.
//
// The fix removes the trust string from codex's ready set: a codex pane
// showing only the trust dialog is NOT ready, and waitForReady must keep
// waiting until the real "›" prompt appears. The fake codex wrapper prints
// the trust dialog, holds it for a few seconds, then prints the "›" prompt —
// mirroring codex resolving the dialog before becoming ready.
//
// Discriminator: with the bug, waitForReady exits on the trust dialog and the
// create returns almost immediately; with the fix it must wait for the "›"
// prompt, so the create cannot complete before the wrapper emits it.
func TestCLICreateCodexWaitsPastTrustPrompt(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")

	// #1056: private tmux server so the exec'd af CLI and its daemon cannot
	// leak af_ sessions onto the developer's server.
	testguard.IsolateTmux(t)

	home := testguard.SocketTempDir(t)
	repo := setupGitRepo(t)

	// The wrapper prints codex's workspace-trust dialog (no "›"), holds it for
	// 12s, then prints the "›" prompt and reads stdin — the shape a real codex
	// startup takes when it resolves the dialog before becoming ready. The hold
	// gives af a window in which it COULD wrongly accept the dialog; it is not
	// measured, and nothing below asserts on it. See the create for why.
	//
	// The wrapper's basename must be "codex" so the resolved-command agent
	// detection (#1116/#1131) selects codex's readiness heuristic — see the
	// fixture comment in TestCLICreateCodexSessionBecomesReady.
	wrapperDir := filepath.Join(home, "fakebin")
	if err := os.MkdirAll(wrapperDir, 0755); err != nil {
		t.Fatalf("mkdir wrapper dir: %v", err)
	}
	wrapper := filepath.Join(wrapperDir, "codex")
	// No handshake, no gate, and deliberately NO timing assertion — see the
	// comment on the create below.
	writeFile(t, wrapper,
		"#!/bin/sh\n"+
			"printf 'OpenAI Codex (vX)\\nDo you trust this folder?\\n> 1. Yes\\n'\n"+
			"sleep 12\n"+
			"printf '\\342\\200\\272 '\n"+ // U+203A "›" in octal-escaped UTF-8
			"exec cat\n",
		0755)

	cfg := testConfig()
	cfg.DefaultProgram = tmux.ProgramCodex
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: wrapper}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeFile(t, filepath.Join(home, config.ConfigFileName), string(raw), 0644)

	bin := buildBinary(t)

	t.Cleanup(func() {
		_, _ = runAFWithin(30*time.Second, bin, repo, home, "sessions", "kill", "codex-trust")
		killDaemonFromHome(home)
	})

	// A hang guard, not an expectation — see the sibling test.
	const createHangGuard = 4 * time.Minute

	// This test asserts END-TO-END WIRING only: a codex agent that shows its trust
	// dialog and then its prompt still ends up ready through the real CLI, daemon,
	// tmux and worktree. It deliberately makes NO claim about the dialog being
	// rejected, and that is a deliberate retreat (#2879).
	//
	// Three attempts in this PR tried to prove that claim from out here — an
	// elapsed-time floor, a marker written before the prompt, then a fixture that
	// held the dialog until the test released it. Each one replaced a clock with
	// another clock, because the thing that has to be observed is af COMPLETING A
	// READINESS POLL, and af leaves no trace of one: the loop logs only on timeout.
	// The last attempt was the worst of them — it could fail a CORRECT af, because
	// af's 60s readiness budget starts when WaitForReadyOn does while the fixture's
	// hold started when this goroutine happened to notice a marker, so a delayed
	// test goroutine starves a healthy create. A test that fails on correct code
	// while gating master is worse than the flake it replaced.
	//
	// The claim is not lost, it is just made where it can be PROVEN. isReadyContent
	// is a pure function of pane content, and task/runner_test.go asserts it with
	// no clock and no window at all:
	//
	//	{"codex trust folder prompt is not ready (#729)", "codex", "Do you trust this folder?\n> Yes", false},
	//	{"codex trust dialog with later prompt is ready (#729)", "codex", "Do you trust this folder?\n› ", true},
	//
	// If you are about to add a timing assertion back here, add a case there
	// instead.
	out, err := runAFWithin(createHangGuard, bin, repo, home, "sessions", "--repo", repo, "create", "--name", "codex-trust", "--program", tmux.ProgramCodex)
	if errors.Is(err, errHangGuardExpired) {
		t.Fatalf("the create never returned within the %s hang guard, so af rendered no verdict — "+
			"a hang or environment failure: %v\n%s", createHangGuard, err, out)
	}
	if err != nil {
		t.Fatalf("a codex session whose agent shows a trust dialog before its prompt did not become "+
			"ready: %v\n%s", err, out)
	}

	var data instanceData
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("parse create response: %v\n%s", err, out)
	}
	if data.Title != "codex-trust" {
		t.Fatalf("unexpected create response: %+v", data)
	}
}
