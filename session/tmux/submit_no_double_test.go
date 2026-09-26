package tmux

import (
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// heartbeatPrompt has the shape of the #4884 production prompt: a ~480-rune
// cron heartbeat delivered verbatim every cycle, so identical copies of it sit
// in the transcript above each new delivery.
const heartbeatPrompt = "[lanes heartbeat] Run: python3 /srv/monitors/lanes/lanes.py status " +
	"and python3 /srv/monitors/lanes/coord_check.py, and confirm with af tasks list " +
	"that lanes-watch is still running. Fix anything stuck per /srv/monitors/lanes/RUNBOOK.md. " +
	"RULE: the main lane is not done until it completes a 2k test with an eval. " +
	"If its goal is paused/complete, or it has been idle >15 min, keep it going per " +
	"the RUNBOOK 'Coordinator keep-going' section. Reply in one or two lines."

// claudeWidth is the production pane's width; claude wraps its composer and
// transcript to it.
const claudeWidth = 52

// wrapClaude lays text out the way claude renders a user message: "❯ " on the
// first row, a two-space hanging indent after, words wrapped at the pane width
// and over-long words (paths) hard-broken.
func wrapClaude(text string) []string {
	const indent = 2
	limit := claudeWidth - indent
	var rows []string
	line := ""
	for _, word := range strings.Fields(text) {
		for len([]rune(word)) > limit {
			if line != "" {
				rows = append(rows, line)
				line = ""
			}
			r := []rune(word)
			rows = append(rows, string(r[:limit]))
			word = string(r[limit:])
		}
		switch {
		case line == "":
			line = word
		case len([]rune(line))+1+len([]rune(word)) <= limit:
			line += " " + word
		default:
			rows = append(rows, line)
			line = word
		}
	}
	if line != "" {
		rows = append(rows, line)
	}
	for i := range rows {
		if i == 0 {
			rows[i] = "❯ " + rows[i]
		} else {
			rows[i] = "  " + rows[i]
		}
	}
	return rows
}

// claudeTurn is one completed heartbeat turn as it sits in the transcript: the
// echoed prompt, a tool call, a short reply and the done line.
func claudeTurn(prompt string) []string {
	rows := append([]string{}, wrapClaude(prompt)...)
	return append(rows,
		"",
		"  Ran 1 shell command",
		"",
		"● No change since the last check: the main lane is",
		"  working with its goal active, nothing is walled,",
		"  the accounts are at 33–83% and disk has 221 GB",
		"  free. The watcher is running and every recorded",
		"  id matches its lane.",
		"",
		"✻ Sautéed for 8s · done 3:40 AM",
		"",
	)
}

// slidingClaudePane models the production pane: claude draws in the main
// screen with no scrollback (history_size=0), so capture-pane sees only the
// bottom `height` rows of transcript + composer + footer. Growing the composer
// scrolls transcript rows off the top — the mechanism behind #4884.
type slidingClaudePane struct {
	mu         sync.Mutex
	height     int
	transcript []string
	composer   []string
	lastLoaded string
	pastes     int
	enters     int
	// onPaste returns the composer rows attempt n renders for payload.
	onPaste func(n int, payload string) []string
	// onEnter runs after the boundary capture (tmux captures in the same command
	// queue as Enter, before the application reacts). It may submit (append to
	// the transcript and empty the composer) or leave the composer as is.
	onEnter func(p *slidingClaudePane, n int)
}

func (p *slidingClaudePane) rows() []string {
	composer := p.composer
	if len(composer) == 0 {
		composer = []string{"❯ "}
	}
	rule := strings.Repeat("─", claudeWidth)
	all := append([]string{}, p.transcript...)
	all = append(all, rule)
	all = append(all, composer...)
	all = append(all, rule, "", "  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab")
	if len(all) > p.height {
		all = all[len(all)-p.height:]
	}
	return all
}

func (p *slidingClaudePane) frame() string {
	return strings.Join(p.rows(), "\n") + "\n"
}

func (p *slidingClaudePane) submitComposerAs(prompt string, extra ...string) {
	p.transcript = append(p.transcript, wrapClaude(prompt)...)
	p.transcript = append(p.transcript, extra...)
	p.composer = nil
}

func (p *slidingClaudePane) exec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			var stdin string
			if c.Stdin != nil {
				b, _ := io.ReadAll(c.Stdin)
				stdin = string(b)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			switch {
			case strings.Contains(joined, "load-buffer"):
				p.lastLoaded = stdin
			case strings.Contains(joined, "send-keys") && hasArg(c.Args, "C-u"):
				p.composer = nil
			case strings.Contains(joined, "paste-buffer"):
				p.pastes++
				p.composer = p.onPaste(p.pastes, p.lastLoaded)
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if isDeliveryBoundaryCommand(c) {
				p.enters++
				boundary := p.frame()
				p.onEnter(p, p.enters)
				return []byte(deliveryBoundarySentinel + "\n" + boundary), nil
			}
			if hasArg(c.Args, "display-message") {
				// Cursor reads for the #2225 clear diagnostic; irrelevant here.
				return []byte("0 0 0"), nil
			}
			return []byte(p.frame()), nil
		},
	}
}

func (p *slidingClaudePane) counts() (pastes, enters int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pastes, p.enters
}

// newHeartbeatPane stages the #4884 alignment: two earlier identical heartbeat
// turns, with the window's top row being the LAST row of the older one. That
// row carries the older copy's completion tail while its prefix has already
// scrolled away; the newer copy is whole.
func newHeartbeatPane(t *testing.T) *slidingClaudePane {
	t.Helper()
	older := claudeTurn(heartbeatPrompt)
	newer := claudeTurn(heartbeatPrompt)
	p := &slidingClaudePane{transcript: append(append([]string{"● earlier work"}, older...), newer...)}
	// The window starts at the first older-copy row from which the completion
	// tail is still whole, i.e. the lowest cut that keeps the tail and drops the
	// prefix.
	promptRows := wrapClaude(heartbeatPrompt)
	completion := newDeliveryProbe(heartbeatPrompt).completion
	top := len(promptRows) - 1
	for !strings.Contains(normalizeDelivery(strings.Join(promptRows[top:], "")), completion) {
		top--
	}
	p.height = len(p.transcript) - (1 + top) + 5 // rule, empty composer, rule, blank, footer
	return p
}

// TestHealthyClaudeHeartbeatIsNotRedelivered is the #4884 production shape.
// The paste lands whole in claude's composer and Enter submits it. But the
// composer's growth scrolls the older copy's tail-bearing row off the top of a
// history-less pane, so the completion-tail COUNT never grows past the
// baseline while the render-witness count does — and the old count-only
// classifier called that "observed absent", the boundary veto (also a count)
// agreed, and #3293 pasted and submitted the heartbeat a second time.
func TestHealthyClaudeHeartbeatIsNotRedelivered(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	errors := captureErrorLog(t)

	pane := newHeartbeatPane(t)
	pane.onPaste = func(_ int, payload string) []string { return wrapClaude(payload) }
	pane.onEnter = func(p *slidingClaudePane, _ int) {
		p.submitComposerAs(strings.Join(p.composer, " "), "", "✻ Crunching… (esc to interrupt)")
	}

	probe := newDeliveryProbe(heartbeatPrompt)
	idle := normalizeDelivery(pane.frame())
	require.Equal(t, 2, strings.Count(idle, probe.completion),
		"fixture: the idle frame holds the older copy's tail row and the newer copy")
	require.Equal(t, 1, strings.Count(idle, probe.renderWitness),
		"fixture: the older copy's prefix has already scrolled off the top")

	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())
	status, err := session.SendKeysCommandObserved(heartbeatPrompt)
	require.NoError(t, err)

	pastes, enters := pane.counts()
	require.Equal(t, 1, pastes,
		"a heartbeat that landed whole and submitted must never be pasted a second time")
	require.Equal(t, 1, enters, "one submit, one Enter")
	require.Equal(t, PromptDelivered, status,
		"the whole prompt rendered below its own newest prefix: that is a landed paste")
	require.NotContains(t, errors.String(), "prompt delivery observed absent")
}

// TestTruncatedRenderThatSubmittedIsNotRedelivered pins the receipt veto. The
// observation deadline and the Enter boundary both show a truncated render
// (the #1982 absent shape), but the paste finished draining behind the Enter
// and claude submitted the whole prompt: by the time the redelivery would run,
// the pane shows it echoed into the transcript with the agent working. That is
// positive evidence of receipt, so the redelivery must be withheld and the
// delivery reported as unconfirmed rather than not-delivered.
func TestTruncatedRenderThatSubmittedIsNotRedelivered(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	pane := newHeartbeatPane(t)
	truncated := wrapClaude(heartbeatPrompt)[:4]
	pane.onPaste = func(int, string) []string { return truncated }
	pane.onEnter = func(p *slidingClaudePane, _ int) {
		p.submitComposerAs(heartbeatPrompt, "", "✻ Crunching… (esc to interrupt)")
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())

	status, err := session.SendKeysCommandObserved(heartbeatPrompt)
	require.NoError(t, err)

	pastes, _ := pane.counts()
	require.Equal(t, 1, pastes,
		"the pane shows the prompt submitted; a redelivery would hand the agent the instruction twice")
	require.Equal(t, PromptSentUnverified, status,
		"absence was not proven, so the caller must hear unconfirmed, not a retryable not-delivered")
}

// TestDroppedPasteInClaudePaneIsRedeliveredExactlyOnce keeps #3293 working for
// the case it exists for, on the same sliding pane: the first paste strands
// truncated in the composer and its Enter is absorbed, so nothing submitted and
// the composer still holds the partial text when the redelivery re-checks. The
// retry clears it, lands whole, and submits — exactly one redelivery.
func TestDroppedPasteInClaudePaneIsRedeliveredExactlyOnce(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	errors := captureErrorLog(t)

	pane := newHeartbeatPane(t)
	pane.onPaste = func(n int, payload string) []string {
		if n == 1 {
			return wrapClaude(payload)[:4]
		}
		return wrapClaude(payload)
	}
	submitted := 0
	pane.onEnter = func(p *slidingClaudePane, n int) {
		if n == 1 {
			return // absorbed: the composer keeps the stranded text
		}
		submitted++
		p.submitComposerAs(strings.Join(p.composer, " "), "", "✻ Crunching… (esc to interrupt)")
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())

	status, err := session.SendKeysCommandObserved(heartbeatPrompt)
	require.NoError(t, err)

	pastes, enters := pane.counts()
	require.Equal(t, 2, pastes, "a proven-absent strand must be redelivered exactly once")
	require.Equal(t, 2, enters)
	require.Equal(t, 1, submitted, "the agent must receive the prompt exactly once")
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 1, strings.Count(errors.String(), "prompt delivery observed absent"))
}
