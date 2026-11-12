package task

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// geminiBareComposerSettle is how long a Gemini composer that carries neither
// later-render anchor (tmux.GeminiComposerFrame) must stay on screen before a
// readiness wait calls it ready (#7168).
//
// It is a timer standing in for a property, so it applies only where the
// property cannot be read: with ui.hideBanner and ui.hideFooter both set, a
// frame holds nothing that proves Gemini is past its first render. The
// property it stands in for is "the folder-trust dialog, if it is coming, has
// already replaced the composer". Gemini opens that dialog 80–156 ms after the
// composer's first paint (measured over the #7161 runs and #7162's captures);
// 500 ms is about three times the worst of those. A Gemini so starved that the
// dialog takes longer than this is the case the timer cannot rule out, which is
// why the anchors come first.
const geminiBareComposerSettle = 500 * time.Millisecond

// readinessJudge is isReadyContent plus the one piece of memory a readiness
// wait carries across polls: when it first saw a bare Gemini composer.
// isReadyContent stays the single-frame answer; this only adds the fallback for
// the frame shape that cannot answer by itself.
type readinessJudge struct {
	agent string
	now   func() time.Time
	// bareSince is when the composer now on screen was first seen without an
	// anchor. Any frame without that composer — including the folder-trust
	// dialog replacing it — clears it, so the two sightings must be of one
	// unbroken composer.
	bareSince time.Time
}

func newReadinessJudge(agent string, clock readinessClock) *readinessJudge {
	now := clock.now
	if now == nil {
		now = time.Now
	}
	return &readinessJudge{agent: agent, now: now}
}

// ready reports whether content shows a ready pane, given what this wait has
// already seen.
func (j *readinessJudge) ready(content string) bool {
	if isReadyContent(content, j.agent) {
		return true
	}
	if j.agent != tmux.ProgramGemini {
		return false
	}
	composer, _ := tmux.GeminiComposerFrame(content)
	if !composer || tmux.GeminiFolderTrustDialogOnScreen(content) || tmux.GeminiFolderTrustFingerprinted(content) {
		j.bareSince = time.Time{}
		return false
	}
	now := j.now()
	if j.bareSince.IsZero() {
		j.bareSince = now
		return false
	}
	return now.Sub(j.bareSince) >= geminiBareComposerSettle
}

// isGeminiReadyContent is isReadyContent's Gemini arm. It lives beside
// readinessJudge because the two decide Gemini readiness together: this one
// from a single frame, the judge across polls for the one frame shape that
// cannot decide itself (#7162, #7168).
func isGeminiReadyContent(content string) bool {
	// Gemini CLI's folder-trust dialog is a readiness STOP, named the way
	// the codex arm names CodexTrustPromptPresent: CheckAndHandleTrustPrompt
	// positively identifies it and answers "Trust folder" by label before
	// any prompt is sent (#7162). Gemini then relaunches itself, and the
	// relaunched composer — painted below its own restart notice — is ready:
	// that process has the folder's trust recorded and cannot open the
	// dialog again.
	if tmux.GeminiFolderTrustPromptPresent(content) || tmux.GeminiRelaunchedAfterTrust(content) {
		return true
	}
	// Otherwise the dialog's own box is not a ready signal. Its "╰" is
	// what the weak glyph check below used to read as ready, while the
	// dialog was still painting or while Gemini was relaunching after the
	// answer, and the prompt went into the dialog or the exiting process.
	//
	// Nor is anything else on a pane that carries the folder-trust flow's
	// fingerprints — its question, its "Trust parent folder" label or its
	// restart notice, found even when a narrow pane wraps or truncates
	// them. Such a pane is ready only as the dialog the strict parser reads
	// or as the relaunched composer above (#7169 review).
	if tmux.GeminiFolderTrustDialogOnScreen(content) || tmux.GeminiFolderTrustFingerprinted(content) {
		return false
	}
	// Gemini 0.51's composer decides any frame it is on, by the composer rules
	// alone (#7168):
	//   - proven past its first render by an anchor in the same frame, it is
	//     ready: the dialog decision is already on screen, so this is not a
	//     composer the dialog is about to replace. That includes the composer
	//     a relaunch paints after the trust answer once the restart notice has
	//     scrolled away on a short pane (#7169 review, P2-2);
	//   - bare — no anchor — it is not ready from one frame, whatever else is
	//     on screen; waitForReadyOn's readinessJudge decides it across polls.
	//     An unrelated rounded box (an update notice) must not make a first
	//     paint ready through the "╰" check below (#7169 review, P3-2).
	if composer, anchored := tmux.GeminiComposerFrame(content); composer {
		return anchored
	}
	// Best-guess (#714) for everything else. The "╰" box-drawing corner of
	// gemini-cli's frame is a weak readiness signal.
	// TODO(#714): replace with a confirmed gemini-specific ready string.
	return strings.Contains(content, "╰") ||
		isDocTrustPrompt(content)
}
