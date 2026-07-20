package tmux

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// lineComposerProgram models a composer at the byte level using the tty's own
// COOKED-mode line discipline: bytes accumulate in the kernel's line buffer
// until a CR arrives, at which point the completed line is delivered to the
// reader. That is exactly the shape of an agent composer (accumulate, submit on
// Enter), and it is a REAL line discipline — the same mechanism a readline/bash
// pane uses — rather than a hand-rolled imitation. C-u is the tty KILL character
// in this mode, so the pre-paste clear (#2070) drains the pending line exactly
// as it would on a readline-class composer.
//
// Each completed line is appended to outPath wrapped in [] so the test can tell
// "two separate submissions" from "one concatenated submission".
func lineComposerProgram(outPath string) string {
	return fmt.Sprintf(`stty -echo; printf "AF-RECEIVER-READY\r\n"; while IFS= read -r l; do printf "[%%s]" "$l" >> %s; done`,
		shellQuoteArg(outPath))
}

func startLineComposerPane(t *testing.T, name string) (*TmuxSession, string) {
	t.Helper()
	testguard.IsolateTmux(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "submitted.txt")
	ts := NewTmuxSession(name, lineComposerProgram(out))
	require.NoError(t, ts.Start(dir))
	t.Cleanup(func() { _, _ = ts.Close() })

	waitFor(t, func() bool {
		content, err := ts.CapturePaneContent()
		return err == nil && strings.Contains(content, receiverReadyMarker)
	}, "line-composer receiver never started")
	return ts, out
}

// strandDraft pastes text into the pane WITHOUT the trailing Enter, which is
// precisely the #1982 end state: the bytes landed in the composer and the submit
// never took. No inference is involved — the test CREATES the strand, so the
// regression it guards is a real fused submission, not a scraped guess about one.
func strandDraft(t *testing.T, ts *TmuxSession, text string) {
	t.Helper()
	const buf = "af_strand_probe_buf"
	load := exec.Command("tmux", "load-buffer", "-b", buf, "-")
	load.Stdin = strings.NewReader(text)
	require.NoError(t, ts.cmdExec.Run(load))
	paste := exec.Command("tmux", "paste-buffer", "-d", "-p", "-b", buf, "-t", exactTarget(ts.sanitizedName))
	require.NoError(t, ts.cmdExec.Run(paste))
}

// TestStrandedDraftDoesNotConcatenateWithNextPrompt is the #2070 / #1982-half-b
// regression. A draft stranded in the composer must not fuse with the NEXT
// prompt: the receiver must see the new prompt as its own submission, not as
// STRANDED-DRAFTNEW-PROMPT. It fails on pre-#2070 code (which has no pre-paste
// clear, so the receiver gets one fused line) and passes once the clear drains
// the strand first.
func TestStrandedDraftDoesNotConcatenateWithNextPrompt(t *testing.T) {
	ts, out := startLineComposerPane(t, "af_strand_probe")

	const stranded = "STRANDED-DRAFT"
	const next = "NEW-PROMPT"

	strandDraft(t, ts, stranded)
	require.NoError(t, ts.SendKeysCommand(next))

	got := readReceived(t, out, "]")
	t.Logf("receiver saw: %q", got)

	require.NotContains(t, got, stranded+next,
		"the stranded draft fused with the next prompt — the next instruction is corrupted")
	require.Contains(t, got, "["+next+"]",
		"the next prompt must arrive as its own submission")
}

// TestDeliveryStillSucceedsWithNoStrandedDraft is the safety half of #2070: the
// unconditional pre-paste clear must be a NO-OP on a clean composer. On an empty
// pending line the clear kills nothing, so a normal delivery still arrives whole
// and by itself — the clear must never eat or corrupt a healthy prompt.
func TestDeliveryStillSucceedsWithNoStrandedDraft(t *testing.T) {
	ts, out := startLineComposerPane(t, "af_nostrand_probe")

	const prompt = "PLAIN-PROMPT"
	require.NoError(t, ts.SendKeysCommand(prompt))

	got := readReceived(t, out, "]")
	require.Equal(t, "["+prompt+"]", got,
		"a delivery to a clean composer must arrive exactly once, unaltered by the pre-paste clear")
}
