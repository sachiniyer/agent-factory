package tmux

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/log"
)

// PromptDeliveryStatus is the closed set of outcomes a caller may learn from
// the pane observation performed during prompt submission. It reports only
// what the daemon observed; neither unverified status guesses whether the
// agent ultimately received the prompt.
type PromptDeliveryStatus string

const (
	PromptDelivered    PromptDeliveryStatus = "delivered"
	PromptNotDelivered PromptDeliveryStatus = "not-delivered"
	// PromptSentUnverified means tmux accepted both the paste and Enter while a
	// readable pane never rendered prompt-specific proof. Real Claude and Codex
	// panes collapse long pastes into placeholders, so this is deliberately not
	// promoted to PromptDelivered.
	PromptSentUnverified  PromptDeliveryStatus = "sent-unverified"
	PromptCouldNotConfirm PromptDeliveryStatus = "could-not-confirm"
)

// Valid reports whether the status is one of the four wire-supported
// outcomes. An empty or future value from an older/newer peer is not evidence
// of delivery and must be normalized to PromptCouldNotConfirm by callers.
func (s PromptDeliveryStatus) Valid() bool {
	return s == PromptDelivered || s == PromptNotDelivered ||
		s == PromptSentUnverified || s == PromptCouldNotConfirm
}

func (o deliveryOutcome) promptDeliveryStatus() PromptDeliveryStatus {
	switch o {
	case deliveryObservedLanded:
		return PromptDelivered
	case deliveryObservedAbsent:
		return PromptNotDelivered
	case deliveryObservedUnverified:
		return PromptSentUnverified
	default:
		return PromptCouldNotConfirm
	}
}

// absenceProof is the evidence that authorizes the one #3293 redelivery: the
// pane showed this payload's newest render cut short — its render witness with
// no completion tail after it — at the Enter boundary. input is the normalized
// pane text from that newest witness to the bottom of the frame: the stranded
// render and everything drawn below it.
type absenceProof struct {
	probe deliveryProbe
	input string
}

// renderRegion returns the part of a normalized frame a paste can have drawn
// into: everything above the frame's trailing text that is still identical to
// the pre-paste baseline. That unchanged bottom is chrome under the composer —
// a footer, a status line — and text in it predates this paste, so it can
// neither vouch for a render nor BE the render (#4885 review). Trimming too much
// only hides content, which can only make a frame read as unproven.
func (p deliveryProbe) renderRegion(normalized string) string {
	b := p.baselineText
	n := 0
	for n < len(normalized) && n < len(b) && normalized[len(normalized)-1-n] == b[len(b)-1-n] {
		n++
	}
	end := len(normalized) - n
	for end < len(normalized) && !utf8.RuneStart(normalized[end]) {
		end++
	}
	return normalized[:end]
}

// newestRender locates this payload's NEWEST render in a normalized frame — the
// last occurrence of its render witness above the unchanged chrome — and
// reports its offset and whether it is whole: one contiguous copy of the entire
// payload covers it. Position, not count, is what makes this sound on a
// history-less pane, where scrolling can remove an older identical copy in the
// same frame that adds this one (#4884).
//
// Both anchors are restricted to renderRegion. A completion found anywhere
// after the newest witness would let a footer the prompt ENDS with vouch for a
// truncated render; a witness found anywhere would let a footer the prompt
// BEGINS with become the "newest render", so a whole composer render read as
// cut short and the absence proof sat on chrome that never changes. Covering,
// rather than starting at, the newest witness keeps a payload that repeats its
// own opening text whole.
func (p deliveryProbe) newestRender(normalized string) (at int, witnessed, whole bool) {
	if p.renderWitness == "" || p.payload == "" {
		return -1, false, false
	}
	region := p.renderRegion(normalized)
	w := strings.LastIndex(region, p.renderWitness)
	if w < 0 {
		return -1, false, false
	}
	c := strings.LastIndex(region, p.payload)
	return w, true, c >= 0 && c <= w && c+len(p.payload) >= w+len(p.renderWitness)
}

// absenceAt returns proof of absence when the frame's newest render of this
// payload is cut short, and nil when the frame holds no render of it or a whole
// one. Nil means "not proven", never "delivered". The proof's input runs from
// that render to the very bottom of the frame, chrome included, so a change
// anywhere below the anchor breaks it.
func (p deliveryProbe) absenceAt(normalized string) *absenceProof {
	at, witnessed, whole := p.newestRender(normalized)
	if !witnessed || whole {
		return nil
	}
	return &absenceProof{probe: p, input: normalized[at:]}
}

// absenceStillProven re-reads the pane immediately before a redelivery and
// requires that nothing happened to the stranded render since the Enter
// boundary: the same newest render, still cut short, with exactly the same text
// from it to the bottom of the frame. Anything else is possible evidence of
// receipt — the render completing, the prompt echoing into the transcript, the
// agent's working indicator or reply appearing under it, the render vanishing —
// or a pane that cannot be read, and none of those proves absence (#4884). The
// test is deliberately agent-agnostic: it asks only whether the strand stood
// still, never what a given agent's spinner looks like.
func (t *TmuxSession) absenceStillProven(proof *absenceProof) bool {
	pane, ok := t.capturePaneForDelivery()
	if !ok {
		return false
	}
	now := proof.probe.absenceAt(normalizeDelivery(pane))
	return now != nil && now.input == proof.input
}

// SendKeysCommand sends text to the tmux pane using the reliable command path.
// Legacy callers keep the error-only contract while status-aware callers use
// SendKeysCommandObserved.
func (t *TmuxSession) SendKeysCommand(text string) error {
	_, err := t.SendKeysCommandObserved(text)
	return err
}

// SendKeysCommandObserved sends a prompt exactly like SendKeysCommand and also
// returns the terminal observation already made by the bounded submit path.
// It does not add another capture or wait to a delivery that confirmed or ended
// ambiguous: callers receive the status the submit path observed. The one
// exception is the observed-ABSENT outcome, which is redelivered once (#3293)
// before the final observation is reported.
func (t *TmuxSession) SendKeysCommandObserved(text string) (PromptDeliveryStatus, error) {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	status, proof, err := t.sendKeysPasteBuffer(text)
	if err != nil || status != PromptNotDelivered || proof == nil {
		return status, err
	}

	// One automatic redelivery, only for the observed-ABSENT outcome (#3293).
	//
	// PromptNotDelivered is the single status this path may retry, because it is
	// the only outcome the observation machinery can honestly prove: the final
	// capture rendered THIS payload's prefix and never its completion tail, so
	// the paste did not land whole (#1982). Both unconfirmed outcomes stay final.
	// Sent-unverified means tmux accepted the paste and Enter while a readable
	// pane rendered no payload-specific proof either way (#3170), and
	// could-not-confirm means observation itself was unavailable — in both, the
	// first prompt may have SUBMITTED, and a redelivery would hand the agent the
	// same instruction twice. sendKeysPasteBuffer pairs PromptNotDelivered
	// exclusively with a nil error (every error path reports could-not-confirm)
	// and with an absence proof taken from the post-Enter boundary frame — see
	// the authorization comment in sendKeysPasteBuffer for why absence must hold
	// at the submit itself, not only at the observation deadline before it.
	//
	// The property is: redeliver ONLY when absence is proven (#4884). "The tail
	// did not match" is not proof — a healthy claude pane produced exactly that
	// and received every heartbeat twice. So after the wait the pane is read
	// once more, and any sign the Enter was received vetoes the retry; the
	// delivery is then reported sent-unverified, visible and retryable by a
	// human. A missed redelivery is recoverable; a doubled non-idempotent prompt
	// is not.
	//
	// The redelivery is the SAME full clear-observe submit, never a bare
	// re-paste: sendKeysPasteBuffer's unconditional pre-paste clear removes
	// whatever the failed attempt stranded in the composer, so the retry
	// replaces the strand instead of fusing with it into
	// STRANDED-DRAFTNEW-PROMPT (#2070). That clear is the entire reason an
	// automatic retry is safe by construction here.
	//
	// Exactly one retry, after a seconds-scale wait. Observed-absent clusters on
	// a pane that was busy mid-render, and an immediate retry re-enters the very
	// render that stranded the first paste. A pane that strands twice is a
	// persistent condition the caller must hear about: the second attempt's
	// observation is reported as-is and nothing loops.
	//
	// inputMu IS held across the wait, on purpose: the strand and its
	// replacement must stay adjacent. Released, a concurrent same-session
	// submission could run inside the gap — its unconditional clear consumes
	// this attempt's strand, and if that delivery then strands too, THIS retry's
	// clear would destroy a newer prompt whose Enter may still be pending, and
	// the older instruction would submit after the newer one. Both violate the
	// serialization inputMu exists to provide (#2178/#2181): the two attempts
	// are one submission transaction, not two. The cost is that the handlers
	// sharing this lock (trust prompt, codex safety) wait out the seconds-scale
	// delay — only on the rare stranded path, and strictly shorter than the
	// stall a wedged tmux server can already impose here.
	//
	// What the hold does NOT cover: the raw attach stream. SendRawKeys is
	// lockless by design (clientless.go), so an interactively typed draft can
	// land during this wait and be consumed by the retry's pre-paste clear —
	// the same frame-granularity interleaving every delivery's unconditional
	// clear already tolerates, recurring once more on the rare absent path.
	// The designed guard for a user typing in a pane is the daemon's attach
	// defer (#1586), applied where attach state lives; no keystroke-sound
	// suppression exists at this layer (#2065/#2225).
	log.WarningLog.Printf("submit: redelivering prompt to session %q once in %s; delivery was observed absent through the submit boundary and the pre-paste clear makes redelivery safe (#3293)",
		t.sanitizedName, redeliverAfterAbsentDelay)
	time.Sleep(redeliverAfterAbsentDelay)
	if !t.absenceStillProven(proof) {
		log.WarningLog.Printf("submit: withholding redelivery to session %q and reporting sent-unverified: the stranded render changed or could not be read during the wait, so the prompt may have been received (#4884)",
			t.sanitizedName)
		return PromptSentUnverified, nil
	}
	status, _, err = t.sendKeysPasteBuffer(text)
	return status, err
}

// SendPromptWorstCaseBound returns an upper bound on one SendKeysCommandObserved
// call: two full submit attempts plus the redelivery wait (#3293). It exists for
// transport callers whose own deadline must OUTLIVE the submit — the remote
// send-prompt route budget — because a transport that gives up mid-retry leaves
// the in-sandbox submit running and possibly delivering, inviting the caller to
// re-send an already-delivered instruction (the AgentArchiveCallTimeout lesson,
// #2923).
//
// The bound is computed here, next to the submit path it describes, from the
// same knobs that bound the path itself. An attempt is counted as its
// individually bounded tmux commands plus the delivery observation window, and
// the pre-redelivery absence re-check (#4884) adds one more bounded capture; the
// command count is deliberately GENEROUS (the longest success path issues 8:
// load, pre-clear capture, two cursor reads, clear, post-clear capture, paste,
// Enter+boundary) so a future added capture does not silently outgrow the bound.
func SendPromptWorstCaseBound() time.Duration {
	const boundedCommandsPerAttempt = 10
	attempt := boundedCommandsPerAttempt*tmuxCommandTimeout + pasteDeliveryMaxWait
	return 2*attempt + redeliverAfterAbsentDelay + tmuxCommandTimeout
}
