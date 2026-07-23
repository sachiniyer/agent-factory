package task

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var (
	waitForReadyTimeout      = 60 * time.Second
	waitForReadyPollInterval = 500 * time.Millisecond
	// waitForReadyCaptureTimeout bounds a single pane capture so a slow or wedged
	// `tmux capture-pane` can never block the poll loop indefinitely — the loop
	// must always be able to observe cancellation and its own deadline, and
	// release the per-repo start lock, within a bounded window (Greptile P1 on
	// #1756). Generous vs. a normal capture (milliseconds).
	waitForReadyCaptureTimeout = 10 * time.Second
	// waitForReadyHookGrace bounds how long in-flight post-worktree hooks may
	// hold the readiness deadline open (see WaitForReady). It only matters for a
	// misconfigured non-terminating hook; real provisioning builds finish far
	// inside it, so it is generous rather than tight.
	waitForReadyHookGrace = 10 * time.Minute
	// paneAnsiEscape strips the ANSI CSI/OSC escape sequences tmux's capture
	// (capture-pane -e keeps them) leaves in the pane so an agent's prompt frame
	// can be matched on its plain-text skeleton. amp wraps the mode label in a
	// truecolor escape ("\x1b[38;2;61;255;166mmedium\x1b[39m") and the repo/branch
	// in a dim escape, both sitting *between* the box-drawing glyphs — which is
	// exactly why the old box regex never matched in the wild and amp creates spun
	// the full readiness timeout. Covers CSI (ESC [ … final byte) and OSC (ESC ] …
	// ST/BEL); amp's banner uses an OSC-8 hyperlink.
	//
	// Shared by amp and opencode: opencode colorizes its status bar word-by-word
	// ("ctrl+p \x1b[38;2;128;128;128mcommands", "esc \x1b[…minterrupt"), so its
	// indicator strings are NOT contiguous in a raw capture either and must be
	// matched on the stripped skeleton for the same reason.
	paneAnsiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	// ampPromptFrameTop matches the top rule of amp's input-prompt frame: a
	// box-drawing top run carrying the mode label near its right end, e.g.
	// "╭──── medium ────╮" (matched after ANSI stripping). The left side is
	// tolerant of decorations amp interleaves into the rule (e.g. a "$0.06" cost
	// indicator once a turn has spent tokens: "╭──── $0.06 ─ medium ─╮"), so the
	// match keeps working across those format variants; the mode label followed
	// by the closing "─╮" is the stable anchor.
	ampPromptFrameTop = regexp.MustCompile(`╭[─ ].*[─ ](low|medium|high|deep)[─ ]+╮`)
	// ampPromptFrameBottom matches the bottom rule of the same frame, which carries
	// the repo/branch, e.g. "╰──── repo (branch) ────╯". Requiring BOTH the labeled
	// top and the closing bottom confirms the input box fully rendered and is
	// accepting input — it excludes amp's blank loading pane and its "Welcome to
	// Amp" banner, neither of which draws the box (the reason the match is strict).
	ampPromptFrameBottom = regexp.MustCompile(`╰[─ ].*[─ ]╯`)
	// ampWorkingIndicator matches the status segment amp draws into the LEFT end
	// of its prompt frame's bottom rule while a turn is in flight — a spinner
	// glyph plus a verb, e.g. "╰ ∼ Streaming ────…" or "╰ ∼ Thinking ────…". An
	// idle frame closes with the rule immediately after the corner ("╰────…"), so
	// the discriminator is: anything that is not the rule itself sitting between
	// the corner and the run of "─".
	//
	// It is deliberately blind to WHICH verb amp prints. The label vocabulary is
	// amp's private UI detail (Streaming/Thinking/… today, more tomorrow) and
	// enumerating it is how #1756's frame regex broke in the first place — a
	// version bump would silently reopen this bug. Presence of a status segment
	// AT ALL is the signal; its wording is not.
	ampWorkingIndicator = regexp.MustCompile(`╰ +[^─\s]`)
	// opencodeFrameBottomRule matches the bottom rule of opencode's composer:
	// a "╹" (U+2579) corner immediately followed by a run of "▀" (U+2580), e.g.
	// "╹▀▀▀▀▀▀▀▀…" (matched after ANSI stripping).
	//
	// "╹" is the load-bearing glyph. opencode's startup ASCII-art banner is drawn
	// out of "█"/"▀"/"▄" ("█▀▀█ █▀▀█ █▀▀█ …"), so a bare "▀" check false-positives
	// on the banner — and the banner is on screen while opencode is still booting,
	// which is precisely when a false ready signal does damage. "╹" appears
	// nowhere in the banner; it is drawn only as the composer's corner.
	opencodeFrameBottomRule = regexp.MustCompile(`╹▀▀+`)
	// opencodeWorkingIndicator matches the "esc interrupt" hint opencode prints
	// into its status bar while a turn is in flight, and removes when the turn
	// ends. Matched only against the status bar BELOW the composer (see
	// IsWorkingContent), never the whole pane.
	//
	// Deliberately NOT the braille spinner (⠋⠙⠹…): that is animated, so which
	// frame a capture lands on is timing-dependent. Deliberately NOT "▣"/"Build ·":
	// a FINISHED turn leaves a static "▣  Build · Claude Opus 4.5 (latest) · 30.2s"
	// line in the transcript forever, and "Build ·" also sits inside the live
	// composer — keying on either would pin an idle session at Running permanently.
	opencodeWorkingIndicator = regexp.MustCompile(`esc +interrupt`)
)

// postWorktreeHooksDoneForWait resolves the instance's post-worktree hook
// completion channel. Indirected so tests can drive the hook-aware readiness
// timing without standing up a real worktree and hook run.
var postWorktreeHooksDoneForWait = func(instance *session.Instance) <-chan struct{} {
	return instance.PostWorktreeHooksDone()
}

// LimitReachedError is what WaitForReady returns when the agent shows a
// usage-limit banner during startup instead of its input prompt (#1146 PR4):
// the plan is exhausted, so the agent will never become ready and spinning the
// full readiness timeout would record the task run as FAILED. It is a distinct,
// NON-failure outcome — callers PARK the session (mark it LimitReached, record a
// "waiting for the limit window" status, fire no failure side-effects) and let
// the existing resume machinery (the daemon auto-resume scheduler, or the manual
// `c` retry) re-deliver the stored prompt once the window resets. ResetAt is the
// parsed reset time, zero when the banner carried none.
type LimitReachedError struct {
	ResetAt time.Time
}

// ErrLimitReached is the sentinel LimitReachedError wraps so callers can match
// it with errors.As(&LimitReachedError) or errors.Is(ErrLimitReached) without
// depending on the concrete type.
var ErrLimitReached = errors.New("agent hit a usage limit during startup")

func (e *LimitReachedError) Error() string { return ErrLimitReached.Error() }

func (e *LimitReachedError) Unwrap() error { return ErrLimitReached }

// Indirected so tests can pin a fixed detector + clock instead of loading config
// and calling time.Now(). Production resolves the same limit_patterns overrides
// the daemon poll uses, so a hand-tuned detect regex parks a task run identically
// to how it surfaces one (#1146).
var (
	newLimitDetectorForWait = defaultLimitDetectorForWait
	waitLimitNow            = time.Now
)

// defaultLimitDetectorForWait builds the usage-limit detector WaitForReady uses,
// honoring config.LimitPatterns. A config-load failure degrades to the built-in
// claude/codex matchers rather than blocking startup — detection is best-effort
// and must never break the create path.
func defaultLimitDetectorForWait() LimitDetector {
	cfg, err := config.LoadConfig()
	if err != nil {
		return NewLimitDetector(nil)
	}
	return NewLimitDetector(cfg.LimitPatterns)
}

// isReadyContent reports whether the captured pane content indicates that the
// given agent is ready for input — or is showing a trust/confirmation prompt
// that downstream handlers know how to dismiss.
//
// The ready signals differ per agent, so callers resolve the canonical agent
// the pane actually runs (session.Instance.ResolvedAgent, which handles
// program_overrides and legacy free-form Program values) and pass it here. An
// empty agent means the resolved command runs no known agent — a
// program_overrides entry pointing at a plain shell or arbitrary tool
// (#1131) — so no agent's prompt glyph will ever appear; the generic signal
// is any non-blank pane output (the process launched and rendered something;
// WaitForReady separately fails fast if the session dies). This replaces the
// pre-#1131 behavior of falling through to the Claude signals, which spun the
// full 60s timeout for anything that never prints "❯". This is the single
// copy: the daemon reaches it via task.StartAndSendPrompt (daemon imports
// task since #782 inverted the old task→daemon dependency).
func isReadyContent(content, agent string) bool {
	switch agent {
	case tmux.ProgramCodex:
		// codex renders "›" (U+203A — distinct from claude's "❯" U+276F) as
		// its input-prompt glyph after the banner (#714).
		//
		// The older workspace-trust dialog ("Do you trust this folder") is
		// deliberately NOT a ready signal (#729): af has no observed, anchored
		// dismissal for it. Codex 0.144.6's newer directory-trust modal IS a
		// readiness stop because tmux.CodexTrustPromptPresent positively
		// identifies it and CheckAndHandleTrustPrompt dismisses it before any
		// prompt can be sent (#2220). Its selected option also contains `›`, so
		// name the handled case explicitly instead of pretending every glyph is
		// necessarily the composer.
		return tmux.CodexTrustPromptPresent(content) || strings.Contains(content, "›")
	case tmux.ProgramAider:
		// aider prints an "Aider v…" banner, then a line-start "> " prompt.
		return strings.Contains(content, "\n> ") ||
			strings.Contains(content, "Aider v") ||
			isDocTrustPrompt(content)
	case tmux.ProgramGemini:
		// Best-guess (#714): no in-the-wild gemini-cli capture yet. The "╰"
		// box-drawing corner of gemini-cli's frame is a weak readiness signal.
		// TODO(#714): replace with a confirmed gemini-specific ready string.
		return strings.Contains(content, "╰") ||
			isDocTrustPrompt(content)
	case tmux.ProgramAmp:
		// Amp is ready when its input-prompt frame is visible. A bare
		// box-drawing character is too broad: loading frames, banners, and
		// stale scrollback can all contain the same glyphs before the prompt is
		// accepting input.
		return isAmpPromptFrame(content) ||
			isDocTrustPrompt(content)
	case tmux.ProgramOpencode:
		// opencode is ready when its composer frame is visible (see
		// isOpencodePromptFrame for why the escapes-only boot window makes this
		// case load-bearing rather than cosmetic).
		//
		// opencode has NO trust/onboarding gate — a fresh `git init` repo goes
		// straight to the composer (verified on 0.0.0-main-202604230742) — so the
		// isDocTrustPrompt arm never fires for it in practice. It is kept for
		// consistency with the other agents: it costs nothing and means a future
		// opencode doc-link dialog degrades the same way theirs does.
		return isOpencodePromptFrame(content) ||
			isDocTrustPrompt(content)
	case tmux.ProgramDevin:
		// devin renders "❭" (U+276D) as its composer prompt glyph once it is past
		// the boot splash and accepting input — distinct from codex's "›" (U+203A)
		// and claude's "❯" (U+276F). Matched on the raw pane like codex's glyph
		// (devin does not split the glyph with color escapes), so no ANSI strip.
		//
		// Deliberately NO isDocTrustPrompt arm (unlike the file-seam agents),
		// because af wires no devin trust dismissal: tmux.ProgramNeedsTrustDismissal
		// excludes devin, as DocTrustPromptPresent cannot match its modal wording.
		// Treating a trust/doc prompt as ready without an anchored way to clear it
		// is the #729 trap — af would type the first prompt into an undismissed
		// modal. The "❭" composer is the only ready signal.
		//
		// Note the modal CAN appear. af normally launches devin with
		// --respect-workspace-trust false, but that is injected by
		// injectSystemPrompt, which the config-agent spawn path does not call
		// (#2435), and a program_overrides.devin carrying the flag explicitly wins
		// over the injection. Omitting the arm is what keeps such a pane from being
		// called ready.
		return strings.Contains(content, "❭")
	case tmux.ProgramClaude:
		if strings.Contains(content, "❯") ||
			strings.Contains(content, "Do you trust") ||
			strings.Contains(content, "new MCP server") {
			return true
		}
		return isDocTrustPrompt(content)
	default:
		// No known agent in the resolved command (#1131): generic readiness —
		// the pane rendered any non-blank output.
		return strings.TrimSpace(content) != ""
	}
}

// isAmpPromptFrame reports whether content shows amp's input-prompt frame in its
// accepting-input state. amp colorizes the frame, so the ANSI escapes are stripped
// first (see ampAnsiEscape).
//
// Readiness requires a CONTIGUOUS frame: a labeled top rule, immediately followed
// by the box-interior rows (each a "│ … │" line) and then the closing bottom rule,
// with no other line in between. Matching the top and bottom rules independently
// anywhere in the pane would let an old top border in scrollback pair with a newer
// bottom border (or vice-versa) and read as ready before amp is accepting input
// (Greptile P2 on #1756). The capture is visible-only, so a single current box is
// what a real ready pane shows.
func isAmpPromptFrame(content string) bool {
	_, ok := ampFrameBottomRule(content)
	return ok
}

// ampFrameBottomRule locates the CURRENT amp prompt frame and returns its bottom
// rule line (ANSI already stripped), reporting false when the pane shows no such
// frame. Both amp pane questions reduce to it: "is amp accepting input?" is
// whether a frame exists at all (isAmpPromptFrame), and "is amp mid-turn?" is
// what that frame's bottom rule says (IsWorkingContent).
//
// Returning the LINE rather than a bool is what keeps the two answers on one
// contiguity walk. The walk is the load-bearing part: matching a bottom rule
// anywhere in the pane would pair an old border in scrollback with a newer one
// (Greptile P2 on #1756), and for the working check that is not just a stale
// ready signal but a permanent one — a "╰ ∼ Streaming" left above the live frame
// by a finished turn would hold the session at Running forever.
// opencodeFrameLines locates opencode's CURRENT composer frame and returns the
// ANSI-stripped pane lines alongside the index of the frame's bottom rule,
// reporting false when the pane shows no such frame. Both opencode pane
// questions reduce to it, the same way ampFrameBottomRule serves both amp
// questions: "is opencode accepting input?" is whether the frame exists at all,
// and "is opencode mid-turn?" is what the status bar BELOW that frame says.
//
// opencode's composer has no labeled top rule (unlike amp's "╭──── medium ────╮"),
// so the walk anchors on the distinctive bottom rule and steps UP one line: a real
// composer always has a "┃" box-interior row directly above its rule. Requiring
// that pairing rather than matching a lone "╹" anywhere excludes stray glyphs.
// A transcript can still contain a complete copied frame, so the CURRENT frame
// is the bottom-most complete pairing in the visible pane, never the first.
func opencodeFrameLines(content string) (lines []string, ruleIdx int, ok bool) {
	plain := paneAnsiEscape.ReplaceAllString(content, "")
	lines = strings.Split(plain, "\n")
	lastRuleIdx := -1
	for i, line := range lines {
		if !opencodeFrameBottomRule.MatchString(line) {
			continue
		}
		if i > 0 && strings.Contains(lines[i-1], "┃") {
			lastRuleIdx = i
		}
	}
	if lastRuleIdx >= 0 {
		return lines, lastRuleIdx, true
	}
	return lines, -1, false
}

// isOpencodePromptFrame reports whether content shows opencode's composer, i.e.
// opencode has finished booting and is accepting input.
//
// This case is load-bearing rather than cosmetic. Without it opencode would fall
// to isReadyContent's default branch ("the pane printed anything non-blank"), and
// opencode's boot has a window — measured at ~2.8-3.2s on
// 0.0.0-main-202604230742 — where the pane holds ~3.9KB of pure colour-setup
// escapes and NOTHING else: no banner, no composer. strings.TrimSpace does not
// treat ESC as whitespace, so the default branch calls that pane ready roughly a
// third of a second before the composer paints, and af types the first prompt
// into a TUI that is not listening yet.
func isOpencodePromptFrame(content string) bool {
	_, _, ok := opencodeFrameLines(content)
	return ok
}

func ampFrameBottomRule(content string) (string, bool) {
	plain := paneAnsiEscape.ReplaceAllString(content, "")
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		if !ampPromptFrameTop.MatchString(line) {
			continue
		}
		// Walk downward from the labeled top rule: box-interior rows begin with
		// the "│" border; the first bottom rule closes a real, current frame.
		// Any other line before the bottom rule means these borders are not one
		// contiguous box, so this top rule is stale scrollback — keep scanning.
		for j := i + 1; j < len(lines); j++ {
			if ampPromptFrameBottom.MatchString(lines[j]) {
				return lines[j], true
			}
			if !strings.HasPrefix(strings.TrimSpace(lines[j]), "│") {
				break
			}
		}
	}
	return "", false
}

// IsWorkingContent reports whether the captured pane shows POSITIVE evidence
// that the given agent is mid-turn — the agent itself saying "I am busy", not an
// inference drawn from the pane changing.
//
// It exists because the daemon's liveness poll infers "working" from pane CHURN:
// content changed since the last tick → LiveRunning, unchanged → idle → LiveReady
// (the green dot). That inference silently assumes an agent REPAINTS CONTINUOUSLY
// while it works, which claude and codex do (an animated spinner plus an elapsed
// timer, so their pane can never hold still for a whole poll interval) — and amp
// does not. amp draws a static frame and repaints only when output actually
// arrives, so every quiet gap in a turn (between token bursts, during a tool call,
// while the model thinks) holds the pane byte-identical past the 1s poll and reads
// as idle. The dot then flips green mid-turn and back on the next burst: the #1766
// green-dot-means-waiting contract inverted into a flash (Sachin, live repro:
// three consecutive false-Ready ticks while amp displayed "Thinking").
//
// A stability debounce cannot fix this class — it only buys a longer quiet window
// before the same misread, and amp's quiet gaps are unbounded (a long tool call
// prints nothing for as long as it runs). Positive evidence is what closes it: an
// agent that says it is working is working, however still its pane sits.
//
// Only agents that actually publish an in-progress indicator can answer here.
// Everything else returns false and keeps the churn inference verbatim — this is
// purely an ADDITIONAL "definitely working" signal layered over the poll's idle
// branch, never a replacement for it, so no existing agent's behavior moves.
func IsWorkingContent(content, agent string) bool {
	switch agent {
	case tmux.ProgramAmp:
		// Ask the CURRENT frame's bottom rule only (see ampFrameBottomRule): a
		// stale "╰ ∼ Streaming" left in scrollback by a finished turn must not
		// pin a genuinely idle session at Running forever — the mirror-image bug
		// (a dot that never goes green) of the flash this fixes.
		//
		// No frame at all means amp is still booting or has died. Neither is
		// "working": the poll's own liveness probe owns those, and claiming
		// Running here would paper over a dead pane that must read Lost.
		rule, ok := ampFrameBottomRule(content)
		if !ok {
			return false
		}
		return ampWorkingIndicator.MatchString(rule)
	case tmux.ProgramOpencode:
		// opencode prints "esc interrupt" into the status bar for exactly the
		// duration of a turn, so unlike amp this is not repairing a broken churn
		// inference: opencode repaints continuously while working (its spinner
		// ticks ~600ms — a 25s silent tool call still changed the pane hash every
		// second in testing), so the poll's churn signal happens to be correct for
		// it. We read opencode's own indicator anyway because an agent stating
		// that it is busy is strictly better evidence than inferring it from
		// pixels moving, and it stays correct if opencode ever adds a quiet
		// render path — the #1890 trap amp fell into.
		//
		// Scope the match to the status bar BELOW the composer rather than the
		// whole pane. The transcript sits ABOVE the composer, so a whole-pane
		// substring check would read an agent that merely PRINTS the words "esc
		// interrupt" (entirely possible in a repo whose agents discuss af's own
		// key hints) as working forever — the same permanent-Running failure that
		// keying on the stale "▣ … 30.2s" line would cause.
		lines, ruleIdx, ok := opencodeFrameLines(content)
		if !ok {
			// No composer: opencode is still booting or has died. Neither is
			// "working" — the poll's own liveness probe owns those, and claiming
			// Running here would paper over a dead pane that must read Lost.
			return false
		}
		for _, line := range lines[ruleIdx+1:] {
			if opencodeWorkingIndicator.MatchString(line) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// isDocTrustPrompt reports whether content shows the documentation-link trust
// dialog shared by aider/gemini (and surfaced by claude).
//
// The predicate now lives in session/tmux (DocTrustPromptPresent) and is shared
// with the dismissal path that types 'D'+Enter into the pane. The two had drifted
// — the dismissal matched the prose alone and injected keystrokes into ordinary
// agent output (#1952) — so they are one copy now. See DocTrustPromptPresent for
// why both substrings are load-bearing.
//
// This TIGHTENED the local predicate: it required only the prefix "Open
// documentation url", the shared one requires the dialog's full prose. That is the
// safe direction here. Readiness gates prompt delivery (task/start.go SendPrompt),
// so a false positive types the user's prompt into whatever is actually on screen
// — the #729 hazard the codex branch of isReadyContent documents above. A miss
// only costs a wait, and the real dialog carries the full prose regardless.
func isDocTrustPrompt(content string) bool {
	return tmux.DocTrustPromptPresent(content)
}

// WaitForReady polls the instance's tmux pane until the program shows its
// input prompt or trust prompt, or times out after 60 seconds.
//
// The poll is bound to ctx: if the caller abandons the create (client
// disconnect, parent cancellation), the loop stops capturing the pane and
// returns ctx.Err() instead of spinning to the internal timeout. That, plus the
// 60s cap, is the guard against a stuck create leaving a capture-pane poll
// pinning the daemon indefinitely (the amp hang). A nil ctx is treated as
// context.Background() so the internal timeout still governs.
func WaitForReady(ctx context.Context, instance *session.Instance) error {
	return WaitForReadyOn(ctx, instanceReadinessTarget{inst: instance})
}

// ReadinessTarget is the narrow contract WaitForReadyOn polls. It exists so the
// readiness logic — the per-agent isReadyContent matching, the usage-limit park,
// the hook-aware budget — has exactly ONE implementation, shared by a full
// session.Instance and by a bare tmux session that has no Instance behind it
// (the config agent, which must never become a row in the session list).
//
// Forking a second copy of this loop was the obvious alternative and would have
// been a mistake: isReadyContent is per-agent, heuristic, and already on the
// regression-prone list, so two copies would drift and only one of them would
// get the next fix.
type ReadinessTarget interface {
	// ResolvedAgent names the agent the pane ACTUALLY runs, detected from its
	// command rather than a config enum, so an override pointing "claude" at
	// something else gets that program's readiness heuristic (#1116, #1131).
	ResolvedAgent() string
	// PreviewContent captures the pane, abandoning the capture when ctx is done.
	PreviewContent(ctx context.Context) (string, error)
	// HooksDone is closed when post-worktree provisioning finishes, or nil when
	// none is in flight (no worktree, an external one, or no hooks configured) —
	// in which case the readiness budget is armed immediately.
	HooksDone() <-chan struct{}
}

// instanceReadinessTarget adapts a full session.Instance to ReadinessTarget.
// Keeping the adapter here (rather than changing WaitForReady's signature) means
// every existing caller and test is untouched by the split.
type instanceReadinessTarget struct{ inst *session.Instance }

func (t instanceReadinessTarget) ResolvedAgent() string { return t.inst.ResolvedAgent() }

func (t instanceReadinessTarget) PreviewContent(ctx context.Context) (string, error) {
	return capturePreview(ctx, t.inst)
}

func (t instanceReadinessTarget) HooksDone() <-chan struct{} {
	return postWorktreeHooksDoneForWait(t.inst)
}

// WaitForReadyOn is WaitForReady against any ReadinessTarget — the seam a bare
// tmux session uses. See WaitForReady for the cancellation contract.
func WaitForReadyOn(ctx context.Context, target ReadinessTarget) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Resolve the canonical agent once so isReadyContent matches the right
	// per-agent prompt signals (#714). ResolvedAgent detects the agent from
	// the command the pane actually runs — not the config-name enum — so a
	// program_overrides entry pointing at a different program gets that
	// program's readiness heuristic, and a non-agent override gets the
	// generic one instead of waiting 60s for a claude glyph (#1116, #1131).
	agent := target.ResolvedAgent()
	// Resolve the usage-limit detector once so a claude/codex pane that shows a
	// limit banner mid-startup is recognized and PARKED, not spun into a failure
	// (#1146 PR4). Only claude/codex ever match; other agents never park here.
	detector := newLimitDetectorForWait()

	// The readiness budget measures the AGENT's startup. Post-worktree hooks
	// (e.g. a `make dev_install` build in post_worktree_commands) run
	// concurrently with the agent and can saturate the machine, starving the
	// agent's process so it renders its ready prompt late. That hook runtime must
	// not be charged against readiness, or a perfectly healthy agent looks like
	// it failed to start — the amp "does-amp-work" timeout. So the timeout stays
	// disarmed while provisioning is in flight and starts fresh only once the
	// hooks drain. A fast agent that becomes ready mid-hook still returns
	// immediately from the ticker branch below, and when no hooks are in flight
	// (hooksDone == nil — no worktree, external worktree, or no hooks configured)
	// the timeout is armed right away, exactly as before.
	hooksDone := target.HooksDone()
	var timeout <-chan time.Time
	var hookGrace <-chan time.Time
	if hooksDone == nil {
		timeout = time.After(waitForReadyTimeout)
	} else {
		// Safety valve: a misconfigured non-terminating post_worktree_command
		// (e.g. a foreground server) would otherwise hold the deadline open
		// forever and wedge session startup. Cap how long hooks may defer the
		// readiness clock; normal provisioning builds finish well within it.
		hookGrace = time.After(waitForReadyHookGrace)
	}
	ticker := time.NewTicker(waitForReadyPollInterval)
	defer ticker.Stop()

	for {
		// Observe cancellation at the top of every iteration, before starting
		// another capture: if the create was abandoned/cancelled while the
		// previous capture ran, return NOW rather than spending a poll on a pane
		// nobody is waiting for. This — plus the bounded, cancellable capture
		// below — is what releases the per-repo start lock within ~one poll
		// interval of cancellation instead of stalling inside tmux (Greptile P1).
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			// The caller gave up (abandoned/cancelled create): stop polling
			// immediately so no capture-pane loop lingers after the caller is
			// gone. Returns "context canceled"/"deadline exceeded" — the create
			// path tears the half-started instance down on any error.
			return ctx.Err()
		case <-hooksDone:
			// Provisioning finished: arm the full readiness budget from here and
			// stop watching the hooks channels.
			hooksDone, hookGrace = nil, nil
			timeout = time.After(waitForReadyTimeout)
		case <-hookGrace:
			// Hooks ran too long to be normal provisioning; stop deferring to
			// them and enforce the readiness budget so startup can't hang forever.
			hooksDone, hookGrace = nil, nil
			timeout = time.After(waitForReadyTimeout)
		case <-timeout:
			content, err := target.PreviewContent(ctx)
			if err != nil {
				// Mirror the ticker case: ErrSessionGone is a definitive,
				// non-retryable death, so surface the actionable "session died"
				// cause even when it happens right at the timeout boundary —
				// never the misleading generic timeout error (#979 fixed only
				// the ticker case; #989 closes this gap so both Preview() call
				// sites handle the sentinel identically).
				if errors.Is(err, tmux.ErrSessionGone) {
					return fmt.Errorf("session died while waiting for agent to start: %w", err)
				}
				log.ErrorLog.Printf("waitForReady timed out (preview also failed: %v)", err)
				return formatWaitForReadyTimeoutError(waitForReadyTimeout, "")
			}
			// A limit banner may be all that ever rendered: park instead of
			// failing even at the timeout boundary, symmetric with the ticker case.
			if perr := limitParkError(detector, content, agent); perr != nil {
				return perr
			}
			// The deadline firing is not itself proof of failure — only the pane
			// is. The pane is sampled every waitForReadyPollInterval, so an agent
			// that renders its prompt during the final poll gap is first observed
			// HERE, and at the exact boundary the runtime may pick this branch over
			// an equally-ready ticker tick. Either way the capture above already
			// shows the agent is up, so honor it exactly as the ticker branch does:
			// reporting a timeout would make the create path kill a healthy, ready
			// session (#1783). Checked after limitParkError, mirroring the ticker
			// branch's ordering.
			if isReadyContent(content, agent) {
				return nil
			}
			log.ErrorLog.Printf("waitForReady timed out. Last pane content: %s", content)
			return formatWaitForReadyTimeoutError(waitForReadyTimeout, content)
		case <-ticker.C:
			content, err := target.PreviewContent(ctx)
			if err != nil {
				// A cancelled/timed-out create surfaces here as context.Canceled /
				// DeadlineExceeded: return at once (the top-of-loop check would
				// catch it next hop, but returning here frees the lock a poll
				// sooner).
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// ErrSessionGone is a definitive, non-retryable failure: the
				// tmux session no longer exists, so it can never become ready.
				// Fail fast with a clear cause instead of polling the full
				// timeout and returning a misleading "timed out" error (#976).
				// Other errors (incl. a capture that hit waitForReadyCaptureTimeout)
				// are transient — keep polling.
				if errors.Is(err, tmux.ErrSessionGone) {
					return fmt.Errorf("session died while waiting for agent to start: %w", err)
				}
				continue
			}
			// Check the usage-limit banner BEFORE readiness: a limit-blocked pane
			// never shows the ready glyph, but checking limit first keeps the
			// intent explicit and future-proofs against a banner that also carried
			// one. On a hit, stop waiting and return the park sentinel (#1146).
			if perr := limitParkError(detector, content, agent); perr != nil {
				return perr
			}
			if isReadyContent(content, agent) {
				return nil
			}
		}
	}
}

// contextPreviewer is the optional capability an agent server exposes when its
// pane capture can be bound to a context — cancelling the context tears down the
// in-flight capture (the local tmux runtime kills the `capture-pane` subprocess).
// WaitForReady uses it so an abandoned create leaves no lingering capture.
type contextPreviewer interface {
	PreviewContext(ctx context.Context, tab int, full bool) (string, error)
}

// capturePreview captures the agent pane but abandons the wait the moment ctx is
// done OR the capture exceeds waitForReadyCaptureTimeout, so a cancelled/timed-out
// create returns — and releases the per-repo start lock — within a bounded window
// instead of blocking inside a slow/wedged `tmux capture-pane` (Greptile P1 on
// #1756).
//
// When the agent server supports a context-bound capture (the local tmux runtime),
// cctx is threaded all the way into the capture so cancellation SIGKILLs the
// capture subprocess — the goroutine then returns promptly too, leaving nothing
// behind. Runtimes without that capability (a future remote capture) fall back to
// the ctx-free Preview; the wait still returns promptly via the cctx race, and the
// goroutine completes its own bounded work. The buffered channel keeps the
// goroutine from blocking on send after the wait has moved on.
func capturePreview(ctx context.Context, instance *session.Instance) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, waitForReadyCaptureTimeout)
	defer cancel()
	server := instance.AgentServer()
	type previewResult struct {
		content string
		err     error
	}
	ch := make(chan previewResult, 1)
	go func() {
		var content string
		var err error
		if cp, ok := server.(contextPreviewer); ok {
			content, err = cp.PreviewContext(cctx, 0, false)
		} else {
			var snapshot session.PreviewSnapshot
			snapshot, err = server.Preview(0, false)
			content = snapshot.Content
		}
		ch <- previewResult{content: content, err: err}
	}()
	select {
	case <-cctx.Done():
		return "", cctx.Err()
	case r := <-ch:
		return r.content, r.err
	}
}

// limitParkError returns a *LimitReachedError when content shows a usage-limit
// banner for agent, else nil. Factored out so both the ticker and timeout
// branches of WaitForReady detect the banner identically (#1146 PR4).
func limitParkError(detector LimitDetector, content, agent string) error {
	if hit, resetAt, _ := detector.Check(content, agent, waitLimitNow()); hit {
		return &LimitReachedError{ResetAt: resetAt}
	}
	return nil
}

// formatWaitForReadyTimeoutError builds the user-facing timeout error. When
// the captured pane content is non-empty, the error body carries a trimmed
// snippet of the last few lines so users see what the agent was doing instead
// of an opaque "timed out" message. See sachiniyer/agent-factory#502.
func formatWaitForReadyTimeoutError(timeout time.Duration, content string) error {
	base := fmt.Sprintf("timed out waiting for program to start (%s)", timeout)
	snippet := trimPaneSnippet(content)
	if snippet == "" {
		return errors.New(base)
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\nlast pane content:")
	for _, line := range strings.Split(snippet, "\n") {
		b.WriteString("\n  ")
		b.WriteString(line)
	}
	return errors.New(b.String())
}

// trimPaneSnippet returns at most the last 5 non-empty trailing lines of the
// captured pane content, capped at 400 bytes. ANSI escape sequences are left
// intact — keeping the snippet short matters more than stripping them.
func trimPaneSnippet(content string) string {
	lines := strings.Split(content, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return out
}

// TaskRunBaseTitle returns the preferred title for a task-created session.
func TaskRunBaseTitle(t Task) string {
	if t.Name != "" {
		return t.Name
	}
	return fmt.Sprintf("task-%s", t.ID)
}
