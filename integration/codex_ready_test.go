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
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
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
	// Capture stdout separately from stderr: the CLI writes the session JSON
	// to stdout but a "wrote logs to …" line to stderr, so CombinedOutput
	// would corrupt the JSON parse.
	runAFWithin := func(limit time.Duration, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "AGENT_FACTORY_HOME="+home, "TERM=xterm")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), fmt.Errorf("%w after %s; stderr=%s", errHangGuardExpired, limit, stderr.String())
		}
		if err != nil {
			return stdout.String(), fmt.Errorf("%w; stderr=%s", err, stderr.String())
		}
		return stdout.String(), nil
	}

	t.Cleanup(func() {
		_, _ = runAFWithin(30*time.Second, "sessions", "kill", "codex-ready")
		killDaemonFromHome(home)
	})

	// A hang guard, NOT an expectation. Its only job is to stop CI hanging if af
	// wedges somewhere with no budget of its own; af's readiness path already
	// self-bounds at 60s and returns an error. It must stay far above any
	// legitimate create, and must never be tuned to "how long a create takes" —
	// the moment it is, it starts racing af's verdict again, which is the bug
	// this test had.
	const createHangGuard = 4 * time.Minute

	out, err := runAFWithin(createHangGuard, "sessions", "--repo", repo, "create", "--name", "codex-ready", "--program", tmux.ProgramCodex)

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
		// A real answer from af. The count proves the wrapper LAUNCHED; it does not
		// prove what reached the pane, so this says only that, and af's own error
		// text names the pane content it actually saw.
		t.Fatalf("regression #714: the fake codex launched %d time(s), but the create did not "+
			"report the session ready: %v\n%s", launches, err, out)
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
	// trustHold, then prints the "›" prompt and reads stdin. The trust dialog
	// alone must not satisfy waitForReady; only the trailing "›" does. The
	// hold is set well above session-startup overhead (daemon launch + git
	// worktree, observed ~6s) so the timing assertion below is unambiguous.
	// The wrapper's basename must be "codex" so the resolved-command agent
	// detection (#1116/#1131) selects codex's readiness heuristic — see the
	// fixture comment in TestCLICreateCodexSessionBecomesReady.
	const trustHold = 12 * time.Second
	wrapperDir := filepath.Join(home, "fakebin")
	if err := os.MkdirAll(wrapperDir, 0755); err != nil {
		t.Fatalf("mkdir wrapper dir: %v", err)
	}
	wrapper := filepath.Join(wrapperDir, "codex")
	// The wrapper shows the trust dialog and then BLOCKS until the test releases
	// it. That gate is what makes the #729 verdict provable rather than inferred
	// (#2879).
	//
	// A marker written just before the prompt is not enough, and the previous
	// attempt here got that wrong: it proved af could not have matched "›"
	// without the marker existing, but not the converse. Between writing a marker
	// and executing the next printf, only the DIALOG is on screen, so a buggy
	// readiness poll landing in that window accepts the dialog while the marker
	// assertion still passes.
	//
	// While the wrapper is blocked, "›" provably does not exist yet, so any ready
	// verdict af reaches in that window is a verdict on the trust dialog alone —
	// which is exactly the regression. The test checks for that BEFORE releasing.
	dialogLog := filepath.Join(home, "codex-dialog.log")
	promptLog := filepath.Join(home, "codex-prompt.log")
	releaseFile := filepath.Join(home, "release-prompt")
	writeFile(t, wrapper,
		"#!/bin/sh\n"+
			"printf 'OpenAI Codex (vX)\\nDo you trust this folder?\\n> 1. Yes\\n'\n"+
			"printf 'dialog\\n' >> '"+dialogLog+"'\n"+
			// Bounded so a broken test cannot wedge the wrapper forever; the
			// create's own hang guard is the outer net.
			"i=0\n"+
			"while [ ! -f '"+releaseFile+"' ] && [ \"$i\" -lt 4000 ]; do i=$((i+1)); sleep 0.05; done\n"+
			"printf 'prompt\\n' >> '"+promptLog+"'\n"+
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
	runAFWithin := func(limit time.Duration, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "AGENT_FACTORY_HOME="+home, "TERM=xterm")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), fmt.Errorf("%w after %s; stderr=%s", errHangGuardExpired, limit, stderr.String())
		}
		if err != nil {
			return stdout.String(), fmt.Errorf("%w; stderr=%s", err, stderr.String())
		}
		return stdout.String(), nil
	}

	t.Cleanup(func() {
		_, _ = runAFWithin(30*time.Second, "sessions", "kill", "codex-trust")
		killDaemonFromHome(home)
	})

	// A hang guard, not an expectation — see the sibling test.
	const createHangGuard = 4 * time.Minute

	// The create runs on its own goroutine so the test can observe it WHILE the
	// wrapper is still holding the dialog. t.Fatalf is illegal off the test
	// goroutine, so results come back on a channel and every assertion below runs
	// here.
	type createResult struct {
		out string
		err error
	}
	done := make(chan createResult, 1)
	go func() {
		out, err := runAFWithin(createHangGuard, "sessions", "--repo", repo, "create", "--name", "codex-trust", "--program", tmux.ProgramCodex)
		done <- createResult{out: out, err: err}
	}()

	// Wait for the dialog on the CONDITION, but watch `done` at the same time.
	// A create that has already failed — daemon startup, session launch, a wrapper
	// that could not exec — has its result sitting on the channel, and ignoring it
	// here would poll for the full ceiling and then report a generic missing-dialog
	// message while discarding the actual error that explains it.
	//
	// The loop runs on the TEST goroutine on purpose. testify's Eventually runs its
	// condition in a worker goroutine, where countFileLines' t.Fatalf on an
	// unreadable file would be an illegal FailNow off the test goroutine: it exits
	// the callback without signalling, so the real read error is replaced by a
	// timeout two minutes later.
	dialogDeadline := time.After(2 * time.Minute)
	for countFileLines(t, dialogLog) == 0 {
		select {
		case res := <-done:
			// Decided before the dialog even appeared. Only a SUCCESSFUL create
			// here could mean the dialog satisfied readiness, and it cannot even
			// mean that yet — the dialog is not on screen. Either way this is a
			// startup/environment failure, not a #729 verdict.
			t.Fatalf("the create finished before the fake codex printed its trust dialog, so nothing "+
				"here is a verdict on #729 — startup or environment failure (create err=%v)\n%s",
				res.err, res.out)
		case <-dialogDeadline:
			t.Fatal("the fake codex never printed its trust dialog, so nothing here is a verdict on #729")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// THE DISCRIMINATOR, and the limit of what a black-box test can prove here.
	//
	// The wrapper is blocked, so "›" provably does not exist yet: a create that
	// SUCCEEDS now reported ready with only the trust dialog on screen, which is
	// #729. An ERROR now is not that — af declared nothing ready — so the two are
	// classified apart rather than both blamed on the contract.
	//
	// trustHold is the OPPORTUNITY for af to make that mistake at its 500ms poll,
	// never a measurement, so load cannot turn a correct af into a failure. What
	// load CAN do is deny af a poll in this window entirely, and then a regression
	// slips past — under-detection, never a false failure. That residue is not
	// fixable from out here: af leaves no observable trace of a readiness poll, so
	// no handshake can wait for one. It does not need to be, either. The #729 rule
	// is decided by isReadyContent, a PURE function of pane content, and
	// task/runner_test.go asserts it directly and deterministically:
	//
	//	{"codex trust folder prompt is not ready (#729)", "codex", "Do you trust this folder?\n> Yes", false},
	//	{"codex trust dialog with later prompt is ready (#729)", "codex", "Do you trust this folder?\n› ", true},
	//
	// That is where the contract is PROVEN. What this test adds is the end-to-end
	// wiring — real CLI, daemon, tmux, worktree — with best-effort detection, and
	// it is documented as such so nobody mistakes it for the proof and starts
	// tuning it to be one.
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatalf("#729 regression: the create reported the session READY while the wrapper was "+
				"still holding the trust dialog and had not emitted its › prompt — the dialog was "+
				"treated as ready\n%s", res.out)
		}
		t.Fatalf("the create failed while the wrapper was still holding the trust dialog, so af "+
			"declared nothing ready — startup or environment failure, NOT #729: %v\n%s", res.err, res.out)
	case <-time.After(trustHold):
		// Still waiting with only the dialog visible: af was given its chance to
		// accept it and did not.
	}

	// Release the real prompt and take af's verdict.
	writeFile(t, releaseFile, "go\n", 0644)
	res := <-done
	out, err := res.out, res.err
	if errors.Is(err, errHangGuardExpired) {
		t.Fatalf("the create never returned within the %s hang guard after the prompt was released, "+
			"so af rendered no verdict — a hang or environment failure, NOT #729: %v\n%s",
			createHangGuard, err, out)
	}
	if err != nil {
		t.Fatalf("create codex session failed after the › prompt was released: %v\n%s", err, out)
	}
	if prompts := countFileLines(t, promptLog); prompts == 0 {
		t.Fatalf("the create reported ready but the wrapper never recorded emitting its › prompt: %s", out)
	}

	var data instanceData
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("parse create response: %v\n%s", err, out)
	}
	if data.Title != "codex-trust" {
		t.Fatalf("unexpected create response: %+v", data)
	}
}
