package tmux

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

const (
	deliveryBoundarySentinel = "__af_delivery_enter_sent_v1__"
	// deliveryBoundaryGridSentinel prefixes the delimiter that separates the
	// -J (joined) capture from the plain grid capture in one Enter boundary
	// command. Both describe the same instant — tmux finishes a client's
	// command list before servicing pane output — but row indexes are only
	// comparable within one capture mode.
	//
	// The delimiter tmux prints is this prefix plus a per-capture nonce. The
	// prefix alone is never the delimiter: it sits BETWEEN two frames of pane
	// content in one stdout stream, so a pane line merely equal to it — the
	// submitted prompt discussing this mechanism, or anything the agent
	// rendered — must never be able to stand in for the delimiter and steer
	// the split (#4530 review). The nonce is minted after the pane was last
	// serviced, so no pane content can know it.
	deliveryBoundaryGridSentinel = "__af_delivery_grid_v1__"
)

// newBoundaryNonce mints the per-capture suffix on the grid delimiter, so the
// exact marker the boundary split looks for exists only in this process and in
// the tmux output stream — never in anything the pane rendered beforehand. It
// is a package var so tests can pin a deterministic marker. The timestamp
// fallback keeps the delimiter unique (not merely unpredictable) if the
// entropy read fails; either property defeats the forge.
var newBoundaryNonce = func() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}

// sendEnterAndCaptureBoundary submits whatever is pending and captures the pane
// in the same tmux command queue — twice. The first capture keeps the status
// monitor's `-e -J` convention (escaped, wrapped lines joined) for text
// evidence; the second is a plain grid capture whose row indexes the staged
// remedy compares against the post-grace grid capture — a row number means
// nothing across capture modes (#4530 review). Both frames describe the same
// instant: tmux runs a client's command list to completion before servicing
// pane output again.
//
// display-message emits a sentinel before each capture: if a capture fails
// after Enter was accepted, partial stdout still proves the send succeeded and
// we preserve the existing best-effort delivery contract without inventing a
// send failure. Only a complete capture can seed the status monitor.
//
// The delimiter between the two captures carries a per-capture nonce: a bare
// sentinel line is pane content like any other, and pane content must never be
// able to steer where the boundary splits (#4530 review).
//
// Bounded by tmuxCommandTimeout (#2099): it is the last step of a submit the
// daemon drives while holding the per-session op lock, so an unbounded stall
// here leaves the session unpromptable rather than merely dropping one Enter.
func (t *TmuxSession) sendEnterAndCaptureBoundary() (joined, grid string, enterOK bool, err error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	target := exactTarget(t.sanitizedName)
	gridMarker := deliveryBoundaryGridSentinel + "-" + newBoundaryNonce()
	out, err := t.outputTmuxBounded(ctx,
		"send-keys", "-t", target, "Enter", ";",
		"display-message", "-p", deliveryBoundarySentinel, ";",
		"capture-pane", "-p", "-e", "-J", "-t", target, ";",
		"display-message", "-p", gridMarker, ";",
		"capture-pane", "-p", "-t", target,
	)
	boundary, gridFrame, enterSent := deliveryBoundaryOutput(out, gridMarker)
	if err != nil && !enterSent {
		if ctx.Err() != nil {
			return "", "", false, fmt.Errorf("%w: send-keys Enter after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return "", "", false, err
	}
	if !enterSent {
		// A successful command queue proves both commands completed. Keeping this
		// fallback also lets executor-level tests return only capture-pane output;
		// the sentinel is load-bearing only on the partial-output error path.
		frame := string(out)
		return frame, frame, true, nil
	}
	if err != nil {
		log.WarningLog.Printf("submit: Enter reached session %q, but its delivery-boundary capture failed; the next successful pane capture will establish the baseline: %v",
			t.sanitizedName, err)
		return "", "", false, nil
	}
	return boundary, gridFrame, true, nil
}

// deliveryBoundaryOutput splits the Enter boundary output into its two
// captures at gridMarker — the exact nonce'd delimiter this capture asked tmux
// to print. Pane content cannot forge it: the nonce was minted after the pane
// was last serviced, so a pane line wearing the bare sentinel prefix, or a
// nonce it cannot have seen, is just text inside the joined frame. Output that
// lacks the marker entirely — executor-level tests answering a single frame —
// reads as both modes at once, which is the same content a no-wrap pane
// produces.
func deliveryBoundaryOutput(out []byte, gridMarker string) (joined, grid string, enterSent bool) {
	s := string(out)
	if s == deliveryBoundarySentinel {
		return "", "", true
	}
	prefix := deliveryBoundarySentinel + "\n"
	if !strings.HasPrefix(s, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, prefix)
	if i := strings.Index(rest, gridMarker+"\n"); i >= 0 {
		return rest[:i], rest[i+len(gridMarker)+1:], true
	}
	return rest, rest, true
}
