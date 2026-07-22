package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

const (
	configAgentCodexFixtureEnv      = "AF_CONFIG_AGENT_CODEX_FIXTURE"
	configAgentCodexFixtureModeEnv  = "AF_CONFIG_AGENT_CODEX_FIXTURE_MODE"
	configAgentCodexFixtureSentinel = "AF_CONFIG_AGENT_2220_RECEIVER_SENTINEL"
)

// TestConfigAgentCodexFixtureProcess is re-exec'd through a symlink named
// "codex" by the two tests below. It models the real Codex 0.144.6 terminal
// contract at the byte boundary af drives:
//
//   - the new directory-trust modal contains a selected `› 1. Yes` row;
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

func TestConfigAgentCodexMissingReceiptFailsSpawn(t *testing.T) {
	testguard.IsolateTmux(t)
	manager, program, _ := newConfigAgentCodexFixture(t, "drop")

	oldTimeout := configAgentPromptReceiptTimeout
	configAgentPromptReceiptTimeout = 250 * time.Millisecond
	t.Cleanup(func() { configAgentPromptReceiptTimeout = oldTimeout })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionName, _, err := manager.SpawnConfigAgent(ctx, SpawnConfigAgentRequest{
		Program: program,
		Prompt:  "briefing deliberately dropped by receiver",
	})
	if err == nil {
		if sessionName != "" {
			_ = manager.ReapConfigAgent(ReapConfigAgentRequest{SessionName: sessionName})
		}
		t.Fatal("a config agent whose receiver recorded no briefing turn reported success")
	}
	if !strings.Contains(err.Error(), "could not verify that Codex accepted the briefing") {
		t.Fatalf("missing receiver receipt returned an unactionable error: %v", err)
	}
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
		{name: "cleared environment without home", program: "env -i codex", wantErr: "no literal HOME fallback"},
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
	cfg.SessionEnvPassthrough = []string{configAgentCodexFixtureEnv, configAgentCodexFixtureModeEnv}
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
	if err := os.WriteFile(rollout, []byte(`{"type":"session_meta"}`+"\n"), 0600); err != nil {
		return fmt.Errorf("create fake Codex rollout: %w", err)
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
				if err := appendConfigAgentCodexUserTurn(rollout, draft.String()); err != nil {
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
	fmt.Print("\x1b[2J\x1b[H> You are in /tmp/throwaway-af-home\r\n\r\n" +
		"  Do you trust the contents of this directory? Working with untrusted contents\r\n" +
		"  comes with higher risk of prompt injection.\r\n\r\n" +
		"› 1. Yes, continue\r\n" +
		"  2. No, quit\r\n\r\n" +
		"  Press enter to continue\r\n")
}

func drawConfigAgentCodexComposer() {
	fmt.Print("\x1b[2J\x1b[H╭ OpenAI Codex (fixture) ╮\r\n\r\n› Use /skills to list available skills\r\n")
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
