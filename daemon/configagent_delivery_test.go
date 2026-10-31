package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

const (
	configAgentCodexFixtureEnv      = "AF_CONFIG_AGENT_CODEX_FIXTURE"
	configAgentCodexFixtureModeEnv  = "AF_CONFIG_AGENT_CODEX_FIXTURE_MODE"
	configAgentCodexFixtureSentinel = "AF_CONFIG_AGENT_2220_RECEIVER_SENTINEL"
)

func captureConfigAgentReceiptLogs(t *testing.T) (info, warnings *logCapture) {
	t.Helper()
	return captureInfo(t), captureWarnings(t)
}

// TestConfigAgentCodexFixtureProcess is re-exec'd through a symlink named
// "codex" by the two tests below. It models the real Codex 0.144.6 terminal
// contract at the byte boundary af drives:
//
//   - the new directory-trust modal contains a selected `› 1. Yes` row;
//   - the modal is a ListSelectionView, so it HIDES the terminal cursor; the
//     ordinary composer it reveals after Enter exposes one (same contract
//     renderFixturePane in session/tmux/codex_safety_real_test.go models, and
//     the contract inspectCodexSafetyPrompt / the directory-trust handler key
//     off to distinguish an active modal from quoted text);
//   - bracketed paste is enabled, but pasted data is ignored while that modal is
//     active;
//   - Enter accepts trust and reveals the composer;
//   - only a bracketed paste followed by Enter at the composer creates a Codex
//     rollout user_message event.
//
// The rollout is the receiver acknowledgement. Pane content is deliberately
// not the oracle: real Codex renders `› [Pasted Content …]` both for a pending
// composer paste and for a submitted user message (#2220).
func TestConfigAgentCodexFixtureProcess(t *testing.T) {
	if os.Getenv(configAgentCodexFixtureEnv) != "1" {
		t.Skip("Codex terminal fixture; re-exec'd by config-agent delivery tests")
	}
	// If the pane's tmux server dies, this fixture's ReadByte would block forever
	// as an orphan (#4412).
	testguard.ExitWhenOrphaned(50 * time.Millisecond)
	if err := runConfigAgentCodexFixture(os.Getenv(configAgentCodexFixtureModeEnv)); err != nil {
		t.Fatal(err)
	}
}

func TestConfigAgentCodexTrustModalSubmitsBriefing(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, rollout := newConfigAgentCodexFixture(t, "accept")

	prompt := strings.Repeat("config-agent briefing line\n", 256) + configAgentCodexFixtureSentinel
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  prompt,
	})
	if err != nil {
		data, _ := os.ReadFile(rollout)
		t.Fatalf("config-agent spawn: %v\nreceiver rollout:\n%s", err, data)
	}
	t.Cleanup(func() {
		if err := manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName}); err != nil {
			t.Errorf("reap config-agent fixture: %v", err)
		}
	})

	data, err := os.ReadFile(rollout)
	if err != nil {
		t.Fatalf("read fake Codex rollout: %v", err)
	}
	if !strings.Contains(string(data), configAgentCodexFixtureSentinel) {
		t.Fatalf("Spawn reported success without a receiver-side Codex user turn; rollout:\n%s", data)
	}
}

// TestConfigAgentCodexTrimsRecordedTurnAndConfirmsReceipt models Codex 0.144.6's
// chat_composer.rs trim of a submitted turn before it is recorded, then asserts
// the spawn's receiver-side receipt (session.WaitForPromptReceipt) confirms a
// prompt that ends in the trailing whitespace configagent.BuildBriefing produces.
//
// The hermetic fixture's default "accept" mode records the pasted draft verbatim,
// so a prompt with no trailing whitespace never exercises the #5488 mismatch.
// This test pads the prompt with a trailing "\n\n" (the shape BuildBriefing
// ships) and uses the "accept-trim" fixture mode so the recorded turn is
// trailing-whitespace stripped — exactly what real Codex stores. The receipt
// must still confirm within the initial window; before #5488 it never did
// because the untrimmed prompt could not be a substring of the trimmed record.
func TestConfigAgentCodexTrimsRecordedTurnAndConfirmsReceipt(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, rollout := newConfigAgentCodexFixture(t, "accept-trim")
	info, warnings := captureConfigAgentReceiptLogs(t)

	// Mirror configagent.BuildBriefing's trailing shape: a long multi-line
	// prompt ending in trailing whitespace ("\n\n"), with a sentinel payload
	// so the recorded turn can be inspected. Real Codex trims the turn before
	// recording; the fixture's "accept-trim" mode does the same.
	prompt := strings.Repeat("config-agent briefing line\n", 256) +
		configAgentCodexFixtureSentinel + "\n\n"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  prompt,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	// The receipt must be CONFIRMED within the initial window. With the #5488
	// bug, the trimmed recorded turn never contained the untrimmed prompt, so
	// the spawn logged "briefing receipt was not observed" and escalated.
	require.NotContains(t, info.String(), "briefing receipt was not observed",
		"a trailing-whitespace prompt must confirm against the trimmed recorded turn within the window")
	require.Empty(t, warnings.String(),
		"a confirmed receipt must not escalate to a warning")

	// Prove the fixture actually modeled the trim, so the confirmation above is
	// meaningful: the recorded user_message is the prompt with its trailing
	// whitespace stripped (it ends in the sentinel, not in "\n\n"). Without this
	// check the test could pass against a verbatim recording for the wrong
	// reason. The hermetic tmux paste delivers "\r" line endings (tmux converts
	// "\n" to "\r" inside a bracketed paste), while real Codex records "\n";
	// normalize the recorded turn's line endings so the comparison reflects the
	// shape real Codex stores.
	require.True(t, strings.HasSuffix(prompt, "\n\n"),
		"the prompt must end in trailing whitespace so the test is non-vacuous")
	message, err := readConfigAgentCodexUserMessage(rollout)
	require.NoError(t, err)
	require.NotEmpty(t, message, "the accept-trim fixture must record a user turn")
	normalizedMessage := strings.ReplaceAll(strings.ReplaceAll(message, "\r\n", "\n"), "\r", "\n")
	require.Equal(t, strings.TrimSpace(prompt), normalizedMessage,
		"the accept-trim fixture must record the trailing-whitespace-stripped turn (modeling Codex text.trim())")
}

// readConfigAgentCodexUserMessage returns the message of the first Codex
// user_message event recorded in rollout, or "" if none is present.
func readConfigAgentCodexUserMessage(rollout string) (string, error) {
	data, err := os.ReadFile(rollout)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Type == "event_msg" && event.Payload.Type == "user_message" && event.Payload.Message != "" {
			return event.Payload.Message, nil
		}
	}
	return "", nil
}

// TestConfigAgentUnconfirmedReceiptStillAttaches is #2483. A receiver that never
// records the briefing turn within the confirmation window is exactly what a
// slow or degraded Codex looks like — and it is what broke the assistant on
// Sachin's box, where the 5s window hard-closed the session. The paste+Enter
// already succeeded, so an unconfirmed receipt is not proof of a failed
// delivery: the spawn must attach a live session anyway, matching the
// send-and-attach every normal session does, rather than reaping the operator's
// only path to configuring af.
//
// The "drop" fixture is the reproduction: it accepts trust and reveals the
// composer but never writes a user turn to its rollout, so WaitForPromptReceipt
// runs out its budget and returns ErrPromptReceiptNotObserved — the exact error
// from the report.
func TestConfigAgentUnconfirmedReceiptStillAttaches(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, rollout := newConfigAgentCodexFixture(t, "drop")
	info, warnings := captureConfigAgentReceiptLogs(t)

	oldTimeout := configAgentPromptReceiptTimeout
	configAgentPromptReceiptTimeout = 250 * time.Millisecond
	t.Cleanup(func() { configAgentPromptReceiptTimeout = oldTimeout })
	oldEscalationTimeout := configAgentPromptReceiptEscalationTimeout
	configAgentPromptReceiptEscalationTimeout = 250 * time.Millisecond
	t.Cleanup(func() { configAgentPromptReceiptEscalationTimeout = oldEscalationTimeout })

	const prompt = "briefing the receiver never records as a turn"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  prompt,
	})
	if err != nil {
		t.Fatalf("an unconfirmed briefing receipt closed the config agent instead of attaching it (#2483): %v", err)
	}
	if sessionName == "" {
		t.Fatal("spawn returned no session name, so there is nothing to attach to")
	}
	t.Cleanup(func() {
		if rerr := manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName}); rerr != nil {
			t.Errorf("reap unconfirmed-receipt config agent: %v", rerr)
		}
	})
	if !strings.Contains(info.String(), "briefing receipt was not observed within 250ms") {
		t.Fatalf("the initial missing-receipt fact was not recorded at INFO:\n%s", info.String())
	}
	if got := warnings.String(); got != "" {
		t.Fatalf("attaching the live session after the initial receipt window emitted a warning:\n%s", got)
	}
	require.Eventually(t, func() bool {
		return strings.Contains(warnings.String(), "briefing receipt was still not observed after 500ms")
	}, 2*time.Second, 25*time.Millisecond,
		"a receipt that remains absent beyond the post-attach window must escalate")
	require.NotContains(t, warnings.String(), "attaching anyway",
		"designed attach behavior is not the degraded fact")

	// Prove we attached WITHOUT a confirmed receipt: the receiver recorded only
	// its session_meta, never the briefing turn. If this ever contains the prompt
	// the fixture stopped modelling the degraded case and the test is vacuous.
	data, err := os.ReadFile(rollout)
	if err != nil {
		t.Fatalf("read fake Codex rollout: %v", err)
	}
	if strings.Contains(string(data), prompt) {
		t.Fatalf("the drop fixture unexpectedly recorded the briefing turn, so this no longer exercises an unconfirmed receipt:\n%s", data)
	}
}

func TestConfigAgentLateReceiptIsConfirmedWithoutWarning(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, rollout := newConfigAgentCodexFixture(t, "drop")
	info, warnings := captureConfigAgentReceiptLogs(t)

	oldTimeout := configAgentPromptReceiptTimeout
	configAgentPromptReceiptTimeout = 250 * time.Millisecond
	t.Cleanup(func() { configAgentPromptReceiptTimeout = oldTimeout })
	oldEscalationTimeout := configAgentPromptReceiptEscalationTimeout
	configAgentPromptReceiptEscalationTimeout = time.Second
	t.Cleanup(func() { configAgentPromptReceiptEscalationTimeout = oldEscalationTimeout })

	const prompt = "briefing recorded shortly after the initial receipt window"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  prompt,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName}))
	})

	require.Contains(t, info.String(), "briefing receipt was not observed within 250ms",
		"the slow-start fact remains visible")
	require.Empty(t, warnings.String(),
		"crossing the initial receipt window is not yet a degraded delivery")
	require.NoError(t, appendConfigAgentCodexUserTurn(rollout, prompt),
		"model the receiver writing the same Codex user_message receipt, only after Spawn returned")
	require.Eventually(t, func() bool {
		return strings.Contains(info.String(), "briefing receipt confirmed after the initial 250ms window")
	}, 2*time.Second, 25*time.Millisecond,
		"a receipt that arrives during the post-attach observation window should resolve at INFO")
	require.Empty(t, warnings.String(),
		"a late-but-confirmed receipt must stay distinguishable from a persistently missing one")
}

func TestConfigAgentCodexInlineHomeSubmitsAndVerifiesBriefing(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, rollout := newConfigAgentCodexFixture(t, "accept-inline-home")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  "briefing whose receipt lives under the inline CODEX_HOME",
	})
	if err != nil {
		t.Fatalf("config-agent spawn with inline CODEX_HOME: %v", err)
	}
	t.Cleanup(func() { _ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName}) })

	data, err := os.ReadFile(rollout)
	if err != nil {
		t.Fatalf("read inline-home rollout: %v", err)
	}
	if !strings.Contains(string(data), "briefing whose receipt lives under the inline CODEX_HOME") {
		t.Fatalf("receiver did not record the briefing under the command-specific CODEX_HOME:\n%s", data)
	}
}

func TestConfigAgentCodexReceiptHome(t *testing.T) {
	workDir := t.TempDir()
	envChdir := t.TempDir()
	daemonCodexHome := t.TempDir()
	includeHome := t.TempDir()
	t.Setenv("CODEX_HOME", daemonCodexHome)
	t.Setenv("HOME", includeHome)

	tests := []struct {
		name    string
		program string
		want    string
		wantErr string
	}{
		{name: "inherits daemon", program: "codex", want: daemonCodexHome},
		{name: "inline absolute wins", program: "CODEX_HOME=/tmp/inline-codex codex", want: "/tmp/inline-codex"},
		{name: "inline relative uses launch cwd", program: "CODEX_HOME=relative-codex codex", want: filepath.Join(workDir, "relative-codex")},
		{name: "unset uses command home", program: "env -u CODEX_HOME HOME=/tmp/inline-home codex", want: "/tmp/inline-home/.codex"},
		{name: "attached unset uses command home", program: "env -uCODEX_HOME HOME=/tmp/inline-home codex", want: "/tmp/inline-home/.codex"},
		{name: "relative home uses env chdir", program: "env -C " + envChdir + " CODEX_HOME=relative-codex codex", want: filepath.Join(envChdir, "relative-codex")},
		{name: "cleared environment falls back to the OS account home (#5986)", program: "env -i codex", want: filepath.Join(osAccountHome(t), ".codex")},
		{name: "dynamic path refused", program: "CODEX_HOME=$OTHER codex", wantErr: "uses shell expansion"},
		{name: "dynamic env chdir refused", program: "env -C $OTHER CODEX_HOME=relative codex", wantErr: "use a literal value"},
		{name: "unknown env option refused", program: "env --future-option CODEX_HOME=/tmp codex", wantErr: "unknown option"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configAgentCodexReceiptHome(tc.program, workDir)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve receipt home: %v", err)
			}
			if got != tc.want {
				t.Fatalf("receipt home = %q, want %q", got, tc.want)
			}
		})
	}
}

func newConfigAgentCodexFixture(t *testing.T, mode string) (*Manager, string, string) {
	t.Helper()
	afHome := testguard.SocketTempDir(t)
	codexHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv(configAgentCodexFixtureEnv, "1")
	t.Setenv(configAgentCodexFixtureModeEnv, mode)
	// The fixture runs inside a tmux pane, so its parent is the pane's tmux
	// server — a daemon that outlives this test process rather than dying
	// with it. Name the test as the fixture's owner so the orphan watchdog's
	// existence arm fires when the test goes away, however it goes away; a
	// pane-only ppid watch would leave the ReadByte fixture running forever
	// on a crashed run (#4412).
	for _, kv := range testguard.ExpectedOwnerEnvPairs() {
		t.Setenv(kv[0], kv[1])
	}

	binDir := testguard.SocketTempDir(t)
	codexBin := filepath.Join(binDir, "codex")
	if err := os.Symlink(os.Args[0], codexBin); err != nil {
		t.Fatalf("symlink Codex fixture: %v", err)
	}
	program := codexBin + " -test.run=^TestConfigAgentCodexFixtureProcess$"
	if strings.Contains(mode, "inline-home") {
		// The daemon environment intentionally points elsewhere. Only parsing the
		// exact launched command finds the rollout the fixture will write.
		t.Setenv("CODEX_HOME", t.TempDir())
		program = "CODEX_HOME=" + shSingleQuote(codexHome) + " " + program
	}

	cfg := config.DefaultConfig()
	cfg.SessionEnvPassthrough = []string{configAgentCodexFixtureEnv, configAgentCodexFixtureModeEnv, testguard.ExpectedParentEnv, testguard.ExpectedOwnerStartEnv}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("save config-agent fixture config: %v", err)
	}
	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("construct manager: %v", err)
	}
	return manager, program, configAgentCodexFixtureRollout(codexHome)
}

func configAgentCodexFixtureRollout(codexHome string) string {
	return filepath.Join(
		codexHome,
		"sessions", "2026", "07", "20",
		"rollout-2026-07-20T12-00-00-019f386f-7206-7fc2-803b-f7045e07a242.jsonl",
	)
}

func runConfigAgentCodexFixture(mode string) error {
	rollout := configAgentCodexFixtureRollout(os.Getenv("CODEX_HOME"))
	if err := os.MkdirAll(filepath.Dir(rollout), 0755); err != nil {
		return fmt.Errorf("create fake Codex sessions dir: %w", err)
	}
	// session_meta carries the pane's real working directory, the same field a
	// bound receipt snapshot correlates on (#5471): without it this rollout is
	// uncorrelatable and no briefing it records can ever be observed.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve fake Codex working dir: %w", err)
	}
	meta, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": cwd},
	})
	if err != nil {
		return fmt.Errorf("encode fake Codex session_meta: %w", err)
	}
	// A pre-seeded rollout models `codex resume` in place: the same file keeps
	// its history and takes new turns appended (#4367 measured the real
	// behavior), so the fixture must not truncate it away.
	if _, statErr := os.Stat(rollout); errors.Is(statErr, os.ErrNotExist) {
		if err := os.WriteFile(rollout, append(meta, '\n'), 0600); err != nil {
			return fmt.Errorf("create fake Codex rollout: %w", err)
		}
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("put fake Codex tty in raw mode: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()

	// Request bracketed-paste mode before drawing the marker. tmux has processed
	// the DECSET by the time the trust text is capturable, so every test paste is
	// framed exactly as it is for real Codex.
	fmt.Print("\x1b[?2004h")
	drawConfigAgentCodexTrust()

	reader := bufio.NewReader(os.Stdin)
	phase := "trust"
	var draft strings.Builder
	for {
		b, readErr := reader.ReadByte()
		if readErr != nil {
			return readErr
		}
		if b == '\x1b' {
			marker := make([]byte, 5)
			if _, err := io.ReadFull(reader, marker); err != nil {
				return err
			}
			switch string(marker) {
			case "[200~":
				pasted, err := readConfigAgentCodexPaste(reader)
				if err != nil {
					return err
				}
				if phase == "composer" {
					draft.WriteString(pasted)
				}
			}
			continue
		}
		switch b {
		case 0x15: // C-u, af's pre-paste composer clear.
			if phase == "composer" {
				draft.Reset()
			}
		case '\r', '\n':
			switch phase {
			case "trust":
				phase = "composer"
				drawConfigAgentCodexComposer()
			case "composer":
				if mode == "drop" {
					drawConfigAgentCodexComposer()
					draft.Reset()
					continue
				}
				recorded := draft.String()
				if mode == "accept-trim" {
					// Model Codex 0.144.6's chat_composer.rs
					// prepare_submission_text_with_options, which trims a
					// submitted turn before recording it
					// (text.trim().to_string()). The default "accept" mode
					// records the pasted draft verbatim and so hides the
					// trailing-whitespace mismatch the receipt matcher must
					// handle (#5488); this mode reproduces the real recorded
					// shape so the receipt path is exercised in CI.
					recorded = strings.TrimSpace(recorded)
				}
				if err := appendConfigAgentCodexUserTurn(rollout, recorded); err != nil {
					return err
				}
				phase = "working"
				fmt.Print("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n")
			}
		}
	}
}

func readConfigAgentCodexPaste(reader *bufio.Reader) (string, error) {
	var pasted strings.Builder
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		if b != '\x1b' {
			pasted.WriteByte(b)
			continue
		}
		marker := make([]byte, 5)
		if _, err := io.ReadFull(reader, marker); err != nil {
			return "", err
		}
		if string(marker) == "[201~" {
			return pasted.String(), nil
		}
		pasted.WriteByte(b)
		pasted.Write(marker)
	}
}

func drawConfigAgentCodexTrust() {
	// \x1b[?25l hides the terminal cursor: Codex's directory-trust modal is a
	// ListSelectionView, which never exposes a cursor position. af's trust
	// dismissal reads cursor visibility to tell an active modal apart from the
	// dialog text quoted in ordinary output, so the fixture must match the real
	// modal's hidden-cursor state for the dismissal to proceed.
	fmt.Print("\x1b[2J\x1b[H\x1b[?25l> You are in /tmp/throwaway-af-home\r\n\r\n" +
		"  Do you trust the contents of this directory? Working with untrusted contents\r\n" +
		"  comes with higher risk of prompt injection.\r\n\r\n" +
		"› 1. Yes, continue\r\n" +
		"  2. No, quit\r\n\r\n" +
		"  Press enter to continue\r\n")
}

func drawConfigAgentCodexComposer() {
	// \x1b[?25h restores the visible cursor the ordinary Codex composer owns
	// once the trust modal has been accepted (mirrors renderFixturePane's
	// cursorVisible=true transition).
	fmt.Print("\x1b[2J\x1b[H\x1b[?25h╭ OpenAI Codex (fixture) ╮\r\n\r\n› Use /skills to list available skills\r\n")
}

func appendConfigAgentCodexUserTurn(rollout, prompt string) error {
	event := map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type":    "user_message",
			"message": prompt,
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// shortenConfigAgentReceiptWindows shrinks all three receipt budgets at once so
// the #5471 watcher phases — escalation window, pane excerpt, late re-check —
// run in milliseconds. It restores them all on cleanup, the same pattern the
// tests above apply per-var.
func shortenConfigAgentReceiptWindows(t *testing.T, receipt, escalation, recheckDelay, recheckWindow time.Duration) {
	t.Helper()
	oldReceipt := configAgentPromptReceiptTimeout
	oldEscalation := configAgentPromptReceiptEscalationTimeout
	oldDelay := configAgentLateReceiptRecheckDelay
	oldWindow := configAgentLateReceiptRecheckWindow
	configAgentPromptReceiptTimeout = receipt
	configAgentPromptReceiptEscalationTimeout = escalation
	configAgentLateReceiptRecheckDelay = recheckDelay
	configAgentLateReceiptRecheckWindow = recheckWindow
	t.Cleanup(func() {
		configAgentPromptReceiptTimeout = oldReceipt
		configAgentPromptReceiptEscalationTimeout = oldEscalation
		configAgentLateReceiptRecheckDelay = oldDelay
		configAgentLateReceiptRecheckWindow = oldWindow
	})
}

// TestConfigAgentUnconfirmedReceiptLogsPaneExcerpt covers the first half of
// #5471: when the receipt is still unconfirmed after the escalation window,
// the spawn-time pane capture is logged as a sanitized excerpt — the only
// place a trust/login/update modal that absorbed Enter would still be visible
// (the same Enter usually dismisses it, so the buffer is taken pre-send).
func TestConfigAgentUnconfirmedReceiptLogsPaneExcerpt(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, _ := newConfigAgentCodexFixture(t, "drop")
	info, _ := captureConfigAgentReceiptLogs(t)
	shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
		10*time.Minute, 250*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  "briefing the receiver never records as a turn",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	require.Eventually(t, func() bool {
		return strings.Contains(info.String(), "pane excerpt after unconfirmed briefing receipt")
	}, 3*time.Second, 25*time.Millisecond,
		"an unconfirmed receipt must leave the pane's visible state in the log")
	// The drop fixture leaves the composer drawn; its banner text is what the
	// excerpt must carry once the ANSI is stripped.
	require.Contains(t, info.String(), "OpenAI Codex",
		"the excerpt should contain the pane's visible text")
}

// TestConfigAgentLateReceiptRecheckObserved is the "slow rollout" arm of #5471:
// the receipt lands after the 35s-equivalent windows but before the late
// re-check, which must then log the observed latency since delivery.
func TestConfigAgentLateReceiptRecheckObserved(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, rollout := newConfigAgentCodexFixture(t, "drop")
	info, warnings := captureConfigAgentReceiptLogs(t)
	shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
		4*time.Second, 250*time.Millisecond)

	const prompt = "briefing recorded minutes after every window closed"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  prompt,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	// The turn must arrive AFTER the escalation window closes unconfirmed — a
	// receipt the escalation poll can still see resolves as "confirmed late"
	// and never reaches the re-check — but BEFORE the re-check fires.
	require.Eventually(t, func() bool {
		return strings.Contains(warnings.String(), "briefing receipt was still not observed")
	}, 3*time.Second, 25*time.Millisecond)
	require.NoError(t, appendConfigAgentCodexUserTurn(rollout, prompt))

	require.Eventually(t, func() bool {
		return strings.Contains(info.String(), "briefing receipt observed at the late re-check") &&
			strings.Contains(info.String(), "after delivery")
	}, 6*time.Second, 50*time.Millisecond,
		"a receipt that lands past the windows must surface its observed latency at the late re-check")
	require.NotContains(t, warnings.String(), "briefing receipt never arrived")
}

// TestConfigAgentLateReceiptRecheckNeverArrived is the "lost briefing" arm of
// #5471: no turn is ever recorded, so the late re-check must say so plainly.
func TestConfigAgentLateReceiptRecheckNeverArrived(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, _ := newConfigAgentCodexFixture(t, "drop")
	_, warnings := captureConfigAgentReceiptLogs(t)
	shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
		1500*time.Millisecond, 250*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  "briefing the receiver never records as a turn",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	require.Eventually(t, func() bool {
		return strings.Contains(warnings.String(), "briefing receipt never arrived; still unobserved")
	}, 6*time.Second, 50*time.Millisecond,
		"a receipt still absent at the late re-check must be reported as never arrived")
}

// TestConfigAgentLateReceiptRecheckIgnoresSiblingRollout is the #5471 review's
// shared-home case: two config agents launch into the same AF home, so a
// sibling rollout appearing mid-wait lands inside the same working-directory
// correlation. The receipt check pins this agent's own rollout the moment it is
// the sole classifiable candidate, so the late re-check still resolves
// observed/never-arrived instead of reporting ambiguity it cannot classify.
func TestConfigAgentLateReceiptRecheckIgnoresSiblingRollout(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, rollout := newConfigAgentCodexFixture(t, "drop")
	info, warnings := captureConfigAgentReceiptLogs(t)
	shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
		2*time.Second, 250*time.Millisecond)

	const prompt = "briefing recorded minutes after every window closed"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  prompt,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	// A second config agent's rollout appears in the same AF home after this
	// spawn's receipt wait already identified its own file during the initial
	// window.
	sibling := filepath.Join(os.Getenv("CODEX_HOME"),
		"sessions", "2026", "07", "21",
		"rollout-2026-07-21T09-00-00-029f386f-7206-7fc2-803b-f7045e07a243.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sibling), 0755))
	meta, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": os.Getenv("AGENT_FACTORY_HOME")},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sibling, append(meta, '\n'), 0600))
	require.NoError(t, appendConfigAgentCodexUserTurn(sibling, "a different config agent's briefing"))

	// Our turn must land AFTER the escalation window expires unconfirmed — a
	// receipt visible to the escalation poll resolves as "confirmed late" and
	// never reaches the re-check — but before the re-check fires.
	require.Eventually(t, func() bool {
		return strings.Contains(warnings.String(), "briefing receipt was still not observed")
	}, 3*time.Second, 25*time.Millisecond)
	require.NoError(t, appendConfigAgentCodexUserTurn(rollout, prompt))
	require.Eventually(t, func() bool {
		return strings.Contains(info.String(), "briefing receipt observed at the late re-check")
	}, 6*time.Second, 50*time.Millisecond,
		"a same-cwd sibling rollout must not make the late re-check ambiguous")
	require.NotContains(t, warnings.String(), "late receipt re-check could not determine")
	require.NotContains(t, warnings.String(), "briefing receipt never arrived")
}

// newConfigAgentCodexResumeFixture builds the same fake Codex as
// newConfigAgentCodexFixture but through a wrapper SCRIPT so the program
// string can carry `resume` argv the test binary's own flag parsing would
// reject. It also pre-seeds the deterministic rollout — a resumed rollout
// predates the receipt snapshot by definition — with session_meta bound to
// the AF home the pane launches in. Returns the manager, the codex binary
// path (to which the caller appends the resume selector), and the rollout
// path.
func newConfigAgentCodexResumeFixture(t *testing.T, mode string) (*Manager, string, string) {
	t.Helper()
	afHome := testguard.SocketTempDir(t)
	codexHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv(configAgentCodexFixtureEnv, "1")
	t.Setenv(configAgentCodexFixtureModeEnv, mode)

	binDir := testguard.SocketTempDir(t)
	codexBin := filepath.Join(binDir, "codex")
	script := "#!/bin/sh\nexec " + shSingleQuote(os.Args[0]) +
		" -test.run=^TestConfigAgentCodexFixtureProcess$\n"
	require.NoError(t, os.WriteFile(codexBin, []byte(script), 0755))

	rollout := configAgentCodexFixtureRollout(codexHome)
	require.NoError(t, os.MkdirAll(filepath.Dir(rollout), 0755))
	meta, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": pathutil.ResolveForCompare(afHome)},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(rollout, append(meta, '\n'), 0600))

	cfg := config.DefaultConfig()
	cfg.SessionEnvPassthrough = []string{configAgentCodexFixtureEnv, configAgentCodexFixtureModeEnv}
	require.NoError(t, config.SaveConfig(cfg))
	manager, err := NewManager(cfg)
	require.NoError(t, err)
	return manager, codexBin, rollout
}

// TestConfigAgentResumeLastConfirmsBriefingFromPool is the #5486 direction:
// `codex resume --last` no longer predicts WHICH rollout Codex reopens — a
// newer same-directory rollout with no turns is exactly the empty-thread trap
// every naive "newest" pick falls for — so af bounds the whole same-directory
// pool at each file's current size, and the briefing appended to the resumed
// file confirms the receipt on its own post-launch bytes.
func TestConfigAgentResumeLastConfirmsBriefingFromPool(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, codexBin, rollout := newConfigAgentCodexResumeFixture(t, "accept")
	_, warnings := captureConfigAgentReceiptLogs(t)

	// The empty-thread trap: a newer same-directory rollout with no recorded
	// turns. Any prediction of Codex's selection rules can land here; the pool
	// covers both files so the receipt needs no prediction.
	trap := filepath.Join(os.Getenv("CODEX_HOME"),
		"sessions", "2026", "07", "21",
		"rollout-2026-07-21T23-59-59-039f386f-7206-7fc2-803b-f7045e07a244.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(trap), 0755))
	trapMeta, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": pathutil.ResolveForCompare(os.Getenv("AGENT_FACTORY_HOME"))},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(trap, append(trapMeta, '\n'), 0600))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: codexBin + " resume --last",
		Prompt:  "briefing recorded on the resumed rollout",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	require.Empty(t, warnings.String(),
		"a receipt confirmed inside the pool must not warn")
	data, err := os.ReadFile(rollout)
	require.NoError(t, err)
	require.Contains(t, string(data), "briefing recorded on the resumed rollout",
		"the resumed rollout must carry the appended briefing turn")
	require.Equal(t, 1, strings.Count(string(data), "session_meta"),
		"the resumed rollout kept its history — the fixture appended, not truncated")
}

// TestConfigAgentResumeLastUnconfirmedStaysIndeterminate is the positive-only
// verdict: when the resumed rollout never records the briefing the result is
// indeterminate, not "never arrived" — af cannot pin which file Codex
// reopened, so absence from the appended ranges disproves nothing (#5486).
func TestConfigAgentResumeLastUnconfirmedStaysIndeterminate(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, codexBin, _ := newConfigAgentCodexResumeFixture(t, "drop")
	info, warnings := captureConfigAgentReceiptLogs(t)
	shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
		1500*time.Millisecond, 250*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: codexBin + " resume --last",
		Prompt:  "briefing the resumed receiver never records",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	require.Contains(t, info.String(),
		"briefing receipt was indeterminate (the resumed rollout is not pinned to an identifiable file) within 250ms",
		"the initial window already knows an unpinned resume cannot go negative")
	require.Eventually(t, func() bool {
		return strings.Contains(warnings.String(),
			"briefing receipt could not be verified; the resumed rollout is not pinned to an identifiable file")
	}, 6*time.Second, 50*time.Millisecond,
		"an unpinned resume must end indeterminate at the late re-check")
	require.NotContains(t, warnings.String(), "never arrived",
		"an unpinned resume can never support a never-arrived verdict")
}

// TestConfigAgentResumeRemoteIgnoresLocalRollout covers `--remote`: the
// resume runs against a remote app-server endpoint, so the local CODEX_HOME
// can never carry its receipt — even when, as the fixture does here, a
// same-named local rollout does record the briefing bytes. af must not
// resolve a remote launch against the local store: the verdict stays
// indeterminate (#5486 review).
func TestConfigAgentResumeRemoteIgnoresLocalRollout(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, codexBin, rollout := newConfigAgentCodexResumeFixture(t, "accept")
	info, warnings := captureConfigAgentReceiptLogs(t)
	shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
		1500*time.Millisecond, 250*time.Millisecond)

	const prompt = "briefing a remote resume writes remotely"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: codexBin + " --remote ws://endpoint.invalid resume --last",
		Prompt:  prompt,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	// The fixture still appends the turn to the local rollout — proving the
	// indeterminate verdict comes from refusing to resolve a remote launch
	// against the local store, not from the bytes being absent.
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(rollout)
		return err == nil && strings.Contains(string(data), prompt)
	}, 3*time.Second, 25*time.Millisecond,
		"the fixture itself must still record the turn locally")

	// A concurrent LOCAL agent landing the same deterministic briefing in a
	// brand-new rollout is the false-positive shape from the finding: local
	// scanning is disabled for remote launches, so this candidate must be
	// invisible to the remote session's receipt check (#5486 review).
	sibling := filepath.Join(os.Getenv("CODEX_HOME"),
		"sessions", "2026", "07", "21",
		"rollout-2026-07-21T00-00-01-049f386f-7206-7fc2-803b-f7045e07a245.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sibling), 0755))
	siblingMeta, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": pathutil.ResolveForCompare(os.Getenv("AGENT_FACTORY_HOME"))},
	})
	require.NoError(t, err)
	siblingTurn, err := json.Marshal(map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": prompt},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sibling,
		append(append(siblingMeta, '\n'), append(siblingTurn, '\n')...), 0600))

	require.Eventually(t, func() bool {
		return strings.Contains(warnings.String(),
			"briefing receipt could not be verified; the resumed rollout is not pinned to an identifiable file")
	}, 6*time.Second, 50*time.Millisecond,
		"a remote resume must never resolve against the local store")
	require.NotContains(t, warnings.String(), "never arrived")
	require.NotContains(t, info.String(), "receipt confirmed",
		"bytes in the local rollout must not confirm a remote resume")
}

// TestConfigAgentResumeExplicitIdKeepsHardVerdict: `codex resume <id>` pins
// the rollout by name, so a bounded explicit target keeps the full
// never-arrived verdict — only UNPINNED resumes are positive-only (#5486).
func TestConfigAgentResumeExplicitIdKeepsHardVerdict(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, codexBin, _ := newConfigAgentCodexResumeFixture(t, "drop")
	_, warnings := captureConfigAgentReceiptLogs(t)
	shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
		1500*time.Millisecond, 250*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		// 019f386f-… is the fixture rollout's own id — the resume is pinned.
		Program: codexBin + " resume 019f386f-7206-7fc2-803b-f7045e07a242",
		Prompt:  "briefing the pinned receiver never records",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
	})

	require.Eventually(t, func() bool {
		return strings.Contains(warnings.String(), "briefing receipt never arrived; still unobserved")
	}, 6*time.Second, 50*time.Millisecond,
		"a pinned resume target keeps the hard never-arrived verdict")
}

// TestConfigAgentLateReceiptRecheckSkipsGoneSession is the #5471 review's
// gone-session case: if the config agent left tracking — or its pane exited on
// its own without a reap — the late re-check must skip quietly rather than log
// a rollout verdict for a session that is gone.
func TestConfigAgentLateReceiptRecheckSkipsGoneSession(t *testing.T) {
	for _, gone := range []string{"untracked", "pane exited"} {
		t.Run(gone, func(t *testing.T) {
			testguard.IsolateTmux(t)
			manager, program, _ := newConfigAgentCodexFixture(t, "drop")
			info, warnings := captureConfigAgentReceiptLogs(t)
			// The re-check horizon (3s) must sit comfortably past the excerpt
			// observation bound (2s) so the session is gone BEFORE the watcher
			// reaches its late-check guards; otherwise the test races the timer.
			shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
				3*time.Second, 250*time.Millisecond)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
				Program: program,
				Prompt:  "briefing the receiver never records as a turn",
			})
			require.NoError(t, err)

			ts := manager.configAgents.session(sessionName)
			require.NotNil(t, ts)
			t.Cleanup(func() { _, _ = ts.Close() })

			// The excerpt line is the last thing logged before the watcher parks
			// on the re-check timer, so observing it means the wait has begun.
			require.Eventually(t, func() bool {
				return strings.Contains(info.String(), "pane excerpt after unconfirmed briefing receipt")
			}, 2*time.Second, 25*time.Millisecond)

			switch gone {
			case "untracked":
				manager.configAgents.mu.Lock()
				delete(manager.configAgents.sessions, sessionName)
				manager.configAgents.mu.Unlock()
			case "pane exited":
				_, err := ts.Close()
				require.NoError(t, err)
			}

			// The re-check horizon passes with no verdict logged either way.
			require.Never(t, func() bool {
				return strings.Contains(info.String(), "observed at the late re-check") ||
					strings.Contains(warnings.String(), "briefing receipt never arrived") ||
					strings.Contains(warnings.String(), "late receipt re-check could not determine")
			}, 4*time.Second, 50*time.Millisecond,
				"a gone session must not get a late re-check verdict")
		})
	}
}

// TestConfigAgentReapDuringLateRecheckWaitUnwindsWatcher is the #5471 review's
// join-budget case: a watcher parked on the 5m re-check timer must still unwind
// instantly when reap cancels it — joinReceiptWatchers must never have to wait
// out its timeout, which is the only way teardown could tell the difference.
func TestConfigAgentReapDuringLateRecheckWaitUnwindsWatcher(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, _ := newConfigAgentCodexFixture(t, "drop")
	info, warnings := captureConfigAgentReceiptLogs(t)
	shortenConfigAgentReceiptWindows(t, 250*time.Millisecond, 250*time.Millisecond,
		10*time.Minute, 250*time.Millisecond)
	shortenReceiptJoinTimeout(t, 3*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  "briefing the receiver never records as a turn",
	})
	require.NoError(t, err)

	// Once the excerpt line lands the watcher is parked on the re-check timer;
	// reaping now cancels that wait, and the join can only return quickly if the
	// goroutine actually observed receiptCtx.Done() — an abandoned watcher would
	// hold the join for the whole configAgentReceiptJoinTimeout and log it.
	require.Eventually(t, func() bool {
		return strings.Contains(info.String(), "pane excerpt after unconfirmed briefing receipt")
	}, 3*time.Second, 25*time.Millisecond)

	start := time.Now()
	require.NoError(t, manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName}))
	elapsed := time.Since(start)
	require.Less(t, elapsed, configAgentReceiptJoinTimeout,
		"reap returned only because the watcher unwound on receiptCtx.Done(), "+
			"not because the join abandoned it after %s", configAgentReceiptJoinTimeout)
	require.NotContains(t, warnings.String(), "did not unwind",
		"the watcher parked on the re-check timer must unwind on cancellation, not be abandoned")
}

func TestConfigAgentPaneExcerpt(t *testing.T) {
	t.Run("strips ansi and residual controls", func(t *testing.T) {
		got := configAgentPaneExcerpt("\x1b[31mDo you trust\x1b[0m this dir?\x07\n\x9b2Ky/n\r")
		require.NotContains(t, got, "\x1b")
		require.NotContains(t, got, "\x07")
		require.NotContains(t, got, "\x9b")
		require.NotContains(t, got, "\r")
		require.Contains(t, got, "Do you trust this dir?")
	})

	t.Run("redacts credential shapes", func(t *testing.T) {
		got := configAgentPaneExcerpt(
			"pat: ghp_0123456789abcdefghij\nkeyed: api_key=hunter2secret\n")
		require.NotContains(t, got, "ghp_0123456789abcdefghij")
		require.NotContains(t, got, "hunter2secret")
	})

	t.Run("keeps the tail within the line cap", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < configAgentPaneExcerptMaxLines+10; i++ {
			fmt.Fprintf(&b, "line-%02d\n", i)
		}
		got := configAgentPaneExcerpt(b.String())
		require.True(t, strings.HasPrefix(got, "… (truncated)"))
		require.NotContains(t, got, "line-00")
		require.Contains(t, got, fmt.Sprintf("line-%02d", configAgentPaneExcerptMaxLines+9))
	})

	t.Run("keeps the tail within the byte cap", func(t *testing.T) {
		got := configAgentPaneExcerpt(strings.Repeat("x", configAgentPaneExcerptMaxBytes+100))
		require.True(t, strings.HasPrefix(got, "… (truncated)"))
		require.LessOrEqual(t, len(got), len("… (truncated)\n")+configAgentPaneExcerptMaxBytes)
		require.True(t, utf8.ValidString(got))
	})

	t.Run("marks an empty pane", func(t *testing.T) {
		require.Equal(t, "(empty pane)", configAgentPaneExcerpt("\x1b[2J  \n\t\n"))
	})
}

// osAccountHome is the passwd home codex falls back to when HOME is absent.
func osAccountHome(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no account home to fall back to: %v", err)
	}
	return filepath.Clean(u.HomeDir)
}
