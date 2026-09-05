//go:build linux

package accountlogin

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The three things gemini 0.51.0 can put on a login pane's first frame, as the
// operator reads them. Captured verbatim from the real CLI on a throwaway
// GEMINI_CLI_HOME (#3858).
const (
	geminiTrustDialog = "Do you trust the files in this folder?"
	geminiAuthPicker  = "How would you like to authenticate for this project?"
	geminiCodePrompt  = "Enter the authorization code:"
)

// TestGeminiLoginPaneOpensOnTheDeviceCodePrompt is #3858 asserted the way the
// operator meets it: the FIRST thing the login pane says.
//
// The claim is not "af wrote two files" — a test of that lives in
// internal/agentaccount and would pass on settings the CLI ignores. It is that a
// gemini flow, launched through the production chain (Start → tmux → the exec
// shim → the agent), asks its human for the authorization code and nothing else
// first. Before the fix the same pane opens on the folder-trust dialog, which is
// a question about af's own directory.
//
// The pane text is read through the SUPERVISOR'S OWN STREAM — the same
// BareSessionStreamer the web login overlay attaches to — rather than through a
// capture-pane of af's making, so what the test reads is what a client reads.
func TestGeminiLoginPaneOpensOnTheDeviceCodePrompt(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	binDir := t.TempDir()
	writeGeminiPromptFixture(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := supervisor.Start(ctx, Request{Home: home, Agent: "gemini", Name: "work"}); err != nil {
		t.Fatalf("start the gemini login pane: %v", err)
	}

	streamer, err := supervisor.Streamer("gemini", "work")
	if err != nil {
		t.Fatalf("open the login pane's stream: %v", err)
	}
	pane := readLoginPane(t, ctx, streamer, geminiTrustDialog, geminiAuthPicker, geminiCodePrompt)

	if strings.Contains(pane, geminiTrustDialog) {
		t.Fatalf("the login pane opened on gemini's folder-trust dialog for af's own account directory, "+
			"not on the sign-in:\n%s", pane)
	}
	if strings.Contains(pane, geminiAuthPicker) {
		t.Fatalf("the login pane opened on gemini's auth picker, not on the sign-in:\n%s", pane)
	}
	if !strings.Contains(pane, geminiCodePrompt) {
		t.Fatalf("the login pane never reached %q:\n%s", geminiCodePrompt, pane)
	}
}

// TestGeminiLoginPaneStillAsksWhenTheAccountAnsweredForItself is the
// anti-vacuity half, and it is what makes the test above mean something. The
// fixture must be able to FAIL — if it printed the device-code prompt
// unconditionally, the assertion would hold no matter what af wrote.
//
// So this drives the same pane against an account whose settings say the opposite
// of af's answer, and requires the picker. It is also the real behaviour worth
// keeping: an account that chose a different auth type keeps it, prompt and all.
func TestGeminiLoginPaneStillAsksWhenTheAccountAnsweredForItself(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	binDir := t.TempDir()
	writeGeminiPromptFixture(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Registered first, then overwritten with a choice of its own — the shape of
	// an operator who picked "Use Gemini API key" in the pane once.
	first, err := supervisor.Start(ctx, Request{Home: home, Agent: "gemini", Name: "work"})
	if err != nil {
		t.Fatalf("register the account: %v", err)
	}
	if err := supervisor.Reap("gemini", "work"); err != nil {
		t.Fatalf("reap the first pane: %v", err)
	}
	settings := filepath.Join(first.Dir, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatalf("stage the account's gemini home: %v", err)
	}
	if err := os.WriteFile(settings, []byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`), 0o600); err != nil {
		t.Fatalf("stage the account's own auth choice: %v", err)
	}

	if _, err := supervisor.Start(ctx, Request{Home: home, Agent: "gemini", Name: "work"}); err != nil {
		t.Fatalf("start the gemini login pane: %v", err)
	}
	streamer, err := supervisor.Streamer("gemini", "work")
	if err != nil {
		t.Fatalf("open the login pane's stream: %v", err)
	}
	pane := readLoginPane(t, ctx, streamer, geminiTrustDialog, geminiAuthPicker, geminiCodePrompt)
	if !strings.Contains(pane, geminiAuthPicker) {
		t.Fatalf("an account that chose its own auth type did not reach the picker, so the fixture cannot "+
			"distinguish af's answer from an unconditional device-code prompt:\n%s", pane)
	}
}

// writeGeminiPromptFixture installs a stand-in for `gemini` that makes the SAME
// decision the real CLI makes, from the same two files, in the same order.
//
// Measured against gemini 0.51.0 on a throwaway GEMINI_CLI_HOME, and the order is
// the surprising part rather than an implementation detail: with
// security.auth.selectedType set, the CLI runs the sign-in BEFORE it mounts the
// interactive UI, so the folder-trust dialog — which that UI raises — never gets
// a turn either. A fixture that checked trust first would report a pane the real
// CLI does not produce.
//
//	settings.json says oauth-personal   → "Please visit the following URL…"
//	otherwise, the folder is not trusted → the folder-trust dialog
//	otherwise                            → the auth picker
//
// The real CLI is not run here for the ordinary reason: it is not installed on
// the CI runner, its sign-in reaches Google, and a login pane in a test must
// never be able to complete one. The verification against the real binary is in
// the PR, from this same pane.
//
// Every command in it is absolute or a shell builtin: a stand-in that shadows
// the agent name sits FIRST on PATH, and a relative helper call resolved through
// that same directory would silently do nothing.
func writeGeminiPromptFixture(t *testing.T, dir string) {
	t.Helper()
	settings := "${GEMINI_CLI_HOME:-}/.gemini/settings.json"
	trusted := "${GEMINI_CLI_HOME:-}/.gemini/trustedFolders.json"
	script := "#!/bin/sh\n" +
		"if /bin/grep -q '\"oauth-personal\"' \"" + settings + "\" 2>/dev/null; then\n" +
		"  printf '%s\\n\\n%s\\n\\n%s ' 'Please visit the following URL to authorize the application:' " +
		"'https://accounts.google.com/o/oauth2/v2/auth?redirect_uri=https%3A%2F%2Fcodeassist.google.com%2Fauthcode' " +
		shellquote.Quote(geminiCodePrompt) + "\n" +
		"elif ! /bin/grep -qF \"\\\"$PWD\\\"\" \"" + trusted + "\" 2>/dev/null; then\n" +
		"  printf '%s\\n' " + shellquote.Quote(geminiTrustDialog) + "\n" +
		"  printf '%s\\n' '  1. Trust folder   2. Trust parent folder   3. Do not trust'\n" +
		"else\n" +
		"  printf '%s\\n' " + shellquote.Quote(geminiAuthPicker) + "\n" +
		"  printf '%s\\n' '  1. Sign in with Google   2. Use Gemini API key   3. Vertex AI'\n" +
		"fi\n" +
		// Then WAIT, because every one of those three states is a flow holding the
		// terminal for its human. A fixture that exited would exercise the
		// flow-ended-early path instead.
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(dir, "gemini"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the gemini prompt fixture: %v", err)
	}
}

// readLoginPane accumulates the pane's bytes off the supervisor's stream until
// one of the markers appears, and returns everything seen.
//
// A fresh subscription is served a REPAINT of the whole current screen before
// any live byte, which is what makes this free of a start-up race: output the
// pane produced before Subscribe ran is in the snapshot, not lost to pipe-pane's
// lack of history.
func readLoginPane(t *testing.T, ctx context.Context, streamer *session.BareSessionStreamer, markers ...string) string {
	t.Helper()
	sub, err := streamer.Subscribe(0)
	if err != nil {
		t.Fatalf("subscribe to the login pane: %v", err)
	}
	defer func() { _ = sub.Close() }()

	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var seen strings.Builder
	for {
		event, err := sub.NextEvent(deadline)
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatalf("the login pane's stream ended before it said anything recognisable:\n%s", seen.String())
			}
			t.Fatalf("read the login pane after %q: %v", seen.String(), err)
		}
		switch event.Kind {
		case session.PTYData, session.PTYRepaint:
			seen.Write(event.Data)
		default:
			continue
		}
		for _, marker := range markers {
			if strings.Contains(seen.String(), marker) {
				return seen.String()
			}
		}
	}
}
