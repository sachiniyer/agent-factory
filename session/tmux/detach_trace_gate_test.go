package tmux

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	aflog "github.com/sachiniyer/agent-factory/log"
)

// newDrainableSession builds a TmuxSession wired up like a live Attach so
// Detach() has bookkeeping to tear down, but with an empty WaitGroup so
// wg.Wait returns immediately and the SIGKILL fallback never runs. This is
// the ordinary "user hits the detach key on a healthy session" path — the
// one that must be silent in the WARN stream (#1157).
func newDrainableSession(t *testing.T, name string) *TmuxSession {
	t.Helper()
	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}
	session := newTmuxSession(toTmuxName(name, ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}
	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())
	return session
}

// TestDetach_NoTraceWarnByDefault is the direct regression guard for #1157:
// the leftover #599 [detach-trace] instrumentation logged ~13 WARN lines on
// every detach and grew to 93% of all WARN volume in production logs. With
// AF_DETACH_TRACE unset (the default for every external user), a normal
// detach must emit ZERO [detach-trace] lines on the WARN stream — and, since
// the markers now live at INFO, none on INFO either while the gate is off.
func TestDetach_NoTraceWarnByDefault(t *testing.T) {
	t.Setenv("AF_DETACH_TRACE", "")

	prevWarn := aflog.WarningLog
	var warnBuf bytes.Buffer
	aflog.WarningLog = log.New(&warnBuf, "WARN: ", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	prevInfo := aflog.InfoLog
	var infoBuf bytes.Buffer
	aflog.InfoLog = log.New(&infoBuf, "INFO: ", 0)
	t.Cleanup(func() { aflog.InfoLog = prevInfo })

	session := newDrainableSession(t, "trace-off")
	session.Detach()

	if strings.Contains(warnBuf.String(), "[detach-trace]") {
		t.Fatalf("detach with AF_DETACH_TRACE unset must emit no [detach-trace] WARN lines; got %q", warnBuf.String())
	}
	if strings.Contains(infoBuf.String(), "[detach-trace]") {
		t.Fatalf("detach with AF_DETACH_TRACE unset must emit no [detach-trace] INFO lines; got %q", infoBuf.String())
	}
}

// TestDetach_TraceGoesToInfoWhenEnabled is the paired positive case: with
// AF_DETACH_TRACE=1 the step-level breakdown that localized the #598 stall
// returns — but on the INFO stream, never WARN. This keeps the diagnostic
// one flag away (matching the #788/#790 app-layer gate) without polluting
// the WARN stream that log triage and af bug-report rely on.
func TestDetach_TraceGoesToInfoWhenEnabled(t *testing.T) {
	t.Setenv("AF_DETACH_TRACE", "1")

	prevWarn := aflog.WarningLog
	var warnBuf bytes.Buffer
	aflog.WarningLog = log.New(&warnBuf, "WARN: ", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	prevInfo := aflog.InfoLog
	var infoBuf bytes.Buffer
	aflog.InfoLog = log.New(&infoBuf, "INFO: ", 0)
	t.Cleanup(func() { aflog.InfoLog = prevInfo })

	session := newDrainableSession(t, "trace-on")
	session.Detach()

	info := infoBuf.String()
	for _, marker := range []string{
		"[detach-trace] tmux.Detach-entry",
		"[detach-trace] tmux.Detach-wg.Wait-done",
		"[detach-trace] tmux.Detach-exit",
	} {
		if !strings.Contains(info, marker) {
			t.Fatalf("expected INFO trace to contain %q when AF_DETACH_TRACE=1; got %q", marker, info)
		}
	}
	if strings.Contains(warnBuf.String(), "[detach-trace]") {
		t.Fatalf("even when enabled, [detach-trace] must stay off the WARN stream; got %q", warnBuf.String())
	}
}

// TestDetachTraceEnabled_ParsesEnv pins the gate's env contract: exactly
// "1" enables it; everything else (unset, "0", "true", "yes") leaves it
// off. This mirrors app.detachTraceEnabledFromEnv so the two [detach-trace]
// layers share one opt-in convention.
func TestDetachTraceEnabled_ParsesEnv(t *testing.T) {
	cases := map[string]bool{
		"":     false,
		"0":    false,
		"true": false,
		"yes":  false,
		"1":    true,
	}
	for value, want := range cases {
		t.Setenv("AF_DETACH_TRACE", value)
		if got := detachTraceEnabled(); got != want {
			t.Errorf("detachTraceEnabled() with AF_DETACH_TRACE=%q = %v; want %v", value, got, want)
		}
	}
}

// TestDetachTracef_RespectsGate is a unit-level guard on the helper itself,
// independent of the full Detach path: it must be silent when the gate is
// off and emit on INFO (never WARN) when on. Guards against a future refactor
// re-pointing the helper at WarningLog and quietly reintroducing the spam.
func TestDetachTracef_RespectsGate(t *testing.T) {
	prevWarn := aflog.WarningLog
	var warnBuf bytes.Buffer
	aflog.WarningLog = log.New(&warnBuf, "WARN: ", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	prevInfo := aflog.InfoLog
	var infoBuf bytes.Buffer
	aflog.InfoLog = log.New(&infoBuf, "INFO: ", 0)
	t.Cleanup(func() { aflog.InfoLog = prevInfo })

	t.Setenv("AF_DETACH_TRACE", "")
	detachTracef("marker-off elapsed=%v", time.Millisecond)
	if warnBuf.Len() != 0 || infoBuf.Len() != 0 {
		t.Fatalf("detachTracef must be silent when gated off; warn=%q info=%q", warnBuf.String(), infoBuf.String())
	}

	t.Setenv("AF_DETACH_TRACE", "1")
	detachTracef("marker-on elapsed=%v", time.Millisecond)
	if !strings.Contains(infoBuf.String(), "[detach-trace] marker-on") {
		t.Fatalf("detachTracef must emit on INFO when enabled; got %q", infoBuf.String())
	}
	if warnBuf.Len() != 0 {
		t.Fatalf("detachTracef must never write to WARN; got %q", warnBuf.String())
	}
}
