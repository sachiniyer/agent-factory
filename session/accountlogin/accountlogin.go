// Package accountlogin runs an agent's OWN login flow in a bare tmux session
// scoped to one registered account (#3384).
//
// af already held both halves of this and only printed them: internal/agentaccount
// knows the agent's login invocation, internal/sessionenv knows which variable
// relocates its credential root. What was missing is a place to RUN that
// invocation with that variable set — and, since the flow is interactive, a
// place for it to run that a human can reach.
//
// The shape is the config agent's (daemon/configagent.go), and deliberately so:
// a BARE tmux session with no session.Instance behind it. An Instance is a row —
// persisted, listed in the sidebar, restorable, killable — and a login pane must
// be none of those. It is also, structurally, the only shape available: the
// WebSocket PTY attach route resolves its byte source by looking the session up
// in the daemon's instance map, so being attachable THAT way is the same thing
// as being a row. A login pane is attached to the way a config agent is, with
// `tmux attach-session` against a named session on a named socket.
//
// It needs no worktree and no repo, which is what makes that possible at all:
// tmux.Start takes any directory, while every Instance provisioning path
// hard-errors outside a git repo.
//
// THE DESIGN PRINCIPLE, unchanged from #3051/#2983 and what bounds everything
// here: af never reads, stores, or forwards the credential. It sets one
// variable, hands the terminal to the agent's own flow, and afterwards asks the
// filesystem — by stat — whether the agent wrote its own artifact.
package accountlogin

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Request is one login: which account, in which agent-factory home.
type Request struct {
	// Home is the agent-factory home whose accounts directory holds the account.
	// Passed in rather than resolved here so this package stays free of config
	// and a test can point it at a throwaway home without touching the real one.
	Home  string
	Agent string
	Name  string
	// Passthrough is the operator's session_env_passthrough list, applied to the
	// login pane exactly as it is to a session.
	//
	// It matters more here than it looks: af's environment allowlist is
	// subtractive and default-deny, and an interactive OAuth flow may need names
	// af does not pass by default — DISPLAY and BROWSER, if the daemon host has a
	// browser to open at all. Proxies and private CA roots are already allowed
	// (sessionenv.commonNames), so the identity subtraction never costs an
	// operator their network configuration; this is the escape hatch for the
	// rest. The subtraction removes IDENTITY, not environment.
	Passthrough []string
}

// Session is a live login pane, described well enough for a caller to attach to
// it and to report what it is.
type Session struct {
	Agent string
	Name  string
	// Dir is the account's credential directory: the value af pointed the
	// agent's credential-root variable at, and the directory whose artifact
	// answers LoggedIn. Never a credential.
	Dir string
	// Program is the exact invocation the pane runs — the agent's own login
	// command. Reported so an operator can see what af is running rather than
	// having to trust it.
	Program string
	// TmuxName is the session name to attach to, as tmux knows it.
	TmuxName string
	// SocketPath is the absolute tmux socket the pane lives on, queried from tmux
	// itself. Empty when it could not be resolved, in which case an attach falls
	// back to the default socket. It is returned because the daemon and whoever
	// attaches are different processes that can resolve different TMUX_TMPDIRs,
	// so "default" is not something the attaching side may assume (#2019).
	SocketPath string
	// Reused is true when this call joined a flow that was already open rather
	// than starting a second one.
	Reused bool
	// Finished is true when the agent's login command ran to completion before af
	// could hand a terminal over, leaving no pane to attach to. TmuxName is empty
	// in that case, and a caller must report the outcome rather than attach.
	//
	// It is a real outcome, not an edge case: `codex login` against an account
	// that already holds a credential prints and exits, and so does a flow that
	// refuses without asking anything. Reporting it as a failed spawn would tell
	// the operator their login broke when it did not.
	Finished bool
	// LoggedIn is the account's artifact state as observed at this call — before
	// the flow this call opened has had a chance to change it. It is what lets a
	// caller say "already logged in; this will replace it" instead of pretending
	// the account was empty.
	LoggedIn bool
	// Notices are the things the operator must be told before they use the
	// account, from agentaccount.CheckLoginPreconditions.
	Notices []string
}

// Supervisor owns every login pane one daemon has spawned.
//
// The daemon owns them for the reason it owns config agents: a login pane has no
// Instance, so NOTHING else knows it exists — not instances.json, not the
// roster, not the restore loop. A daemon that exits without reaping them leaves
// orphans no future daemon can find (#1093/#1104). Stop() belongs in the
// daemon's teardown beside configAgents.Stop().
type Supervisor struct {
	// startMu serializes Start, so two clients asking for the same login at once
	// cannot both miss the reuse check and race to create one tmux name. The
	// loser of that race gets "tmux session already exists", which this package
	// would then have to diagnose backwards from a string.
	//
	// It is deliberately a separate lock from mu, and deliberately coarse: logins
	// are human-driven and rare, every tmux call under it is deadline-bounded, and
	// serializing them costs nothing measurable — while Stop, Reap and Live keep
	// taking only mu, so daemon teardown never waits behind a login that is
	// starting.
	startMu sync.Mutex

	mu       sync.Mutex
	sessions map[string]*tmux.TmuxSession
	// streamers is the WebSocket data plane for a login pane, one per account,
	// built on first subscribe and closed with the pane.
	//
	// It lives HERE rather than beside the routes that serve it because a streamer
	// and the pane it reads are one lifetime: a capture goroutine left running
	// against a torn-down pane is the #1632 leak, and the only way to be sure it
	// cannot happen is for the thing that closes the pane to be the thing that
	// closes the stream. Same map, same mutex, same Stop.
	streamers map[string]*session.BareSessionStreamer
	// stopped latches on teardown so a spawn racing shutdown cannot register a
	// session nothing will ever reap.
	stopped bool
}

// New builds an empty supervisor.
func New() *Supervisor {
	return &Supervisor{
		sessions:  make(map[string]*tmux.TmuxSession),
		streamers: make(map[string]*session.BareSessionStreamer),
	}
}

// key namespaces a login pane by ACCOUNT, which is what makes reuse correct:
// account names live in per-agent namespaces, so codex/work and claude/work are
// different accounts and must not share a flow.
func key(agent, name string) string { return agent + "/" + name }

// Start opens (or rejoins) the login flow for one account.
//
// The order is load-bearing. Everything that can refuse runs BEFORE anything is
// created: an agent with no verified login command, a missing binary, and the
// codex keyring collapse are all decided while the only side effect would be the
// account directory itself. A refusal must not leave a pane behind, and it must
// not leave the operator staring at a pane that exited 127.
func (s *Supervisor) Start(ctx context.Context, req Request) (Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if strings.TrimSpace(req.Home) == "" {
		return Session{}, fmt.Errorf("cannot log in to an agent account without an agent-factory home")
	}
	// The agent's own invocation, or a refusal naming the agents that have one.
	// This is the roster gate: amp, aider, opencode and devin have no verified
	// credential root AND no verified login command, and af guesses neither.
	program, err := agentaccount.LoginProgram(req.Agent)
	if err != nil {
		return Session{}, err
	}
	// PATH is resolved HERE rather than left to the pane, because a pane whose
	// program does not exist exits 127 into a window the operator is about to be
	// handed, and tmux reports that as a session that vanished.
	//
	// The BARE agent name, never config's program_overrides. An override is a
	// SESSION launch command: it may carry session-only flags that make no sense
	// in front of `auth login`, and it is reachable from a repository's checked-in
	// config, which must not get to choose what af runs against a credential
	// directory. The login flow is the agent's own documented invocation, so it
	// resolves the way the agent's own documentation resolves it.
	if _, err := exec.LookPath(req.Agent); err != nil {
		return Session{}, fmt.Errorf(
			"cannot run %s's login flow: %s was not found on PATH where the agent-factory daemon runs (%v). "+
				"af runs the agent's own login command, so the agent has to be installed on this host",
			req.Agent, req.Agent, err)
	}

	// Registration is idempotent and comes before the preconditions, because the
	// preconditions read the account directory. Logging in to an account nobody
	// has registered yet is the ordinary first use of this verb, not an error.
	dir, err := agentaccount.Register(req.Home, req.Agent, req.Name)
	if err != nil {
		return Session{}, err
	}
	notices, err := agentaccount.CheckLoginPreconditions(req.Agent, dir)
	if err != nil {
		return Session{}, err
	}
	loggedIn, err := agentaccount.LoggedIn(req.Home, req.Agent, req.Name)
	if err != nil {
		return Session{}, err
	}

	base := Session{
		Agent:    req.Agent,
		Name:     req.Name,
		Dir:      dir,
		Program:  program,
		LoggedIn: loggedIn,
		Notices:  notices,
	}

	pane := tmux.NewTmuxSession(agentaccount.LoginSessionName(req.Agent, req.Name), program)

	// An open flow is JOINED, never duplicated. Two logins into one directory race
	// over the same auth.json, and the second pane would be invisible to the
	// operator already sitting in the first.
	//
	// The question is asked of TMUX, not of this supervisor's map, and that is the
	// difference between joining a flow and failing on it. A login pane can outlive
	// the process that tracked it — a daemon killed with SIGKILL never runs its
	// teardown — so the map says "no flow" while the tmux name is very much taken,
	// and creating it would fail with "tmux session already exists" for a login
	// that is sitting there waiting for its human. The name is derived from the
	// account, so an existing one under it IS this account's flow.
	if existing := s.adopt(req.Agent, req.Name, pane); existing != nil {
		out := base
		out.Reused = true
		out.TmuxName = existing.SanitizedName()
		out.SocketPath = socketPath(ctx, existing)
		return out, nil
	}

	if err := pane.SetEnvPassthrough(req.Passthrough); err != nil {
		return Session{}, fmt.Errorf("invalid session environment pass-through for the login pane: %w", err)
	}
	// The ENVIRONMENT-ONLY form of the account boundary, which is the correct one
	// here and not a weaker one. Its strict sibling (SetAccountForAgent) proves
	// the pane command is the agent plus exactly the words af declared — a claim
	// about an AGENT launch. This pane runs `claude auth login`, which is the
	// agent's own subcommand rather than an af-authored argument, so the
	// environment-only boundary is what matches the claim being made: scope this
	// process to the account, and refuse a command that would set an identity
	// variable itself. The child shim still applies the FULL boundary — inject the
	// account's root, remove every other identity — immediately before exec.
	pane.SetAccountEnvironmentForAgent(req.Agent, req.Name)

	// The account directory is the working directory: it is a real, durable,
	// 0700 directory af created, outside any temp dir (codex refuses to create
	// helper binaries under /tmp), and it is the directory the flow is about.
	if err := pane.Start(dir); err != nil {
		return finishedOrFailed(req, base, err)
	}
	// The name tmux actually knows — sanitized, with the af_ prefix — not the one
	// handed to NewTmuxSession. Returning the latter is what made the config
	// agent's attach fail with "can't find session" (#2019).
	name := pane.SanitizedName()
	if !s.track(req.Agent, req.Name, pane) {
		// Shutting down: do not leak the pane we just made. No worktree exists, so
		// pane state gates nothing destructive.
		_, _ = pane.Close()
		return Session{}, fmt.Errorf("the agent-factory daemon is shutting down")
	}

	// NOTHING IS SENT TO THIS PANE. Not a readiness probe's keystroke, not a
	// trust-dialog dismissal, not a briefing. Every other spawn in af drives its
	// pane, and this one must not: af cannot recognize an agent's login prompts,
	// the keys it would send are the ones a login flow reads as answers, and
	// af pressing Enter into a dialog it misread is exactly how #3579's trust
	// dismissal killed the session it was trying to start. The human at the
	// terminal drives this flow; af's job ends at the environment.
	log.InfoLog.Printf("account login: started %s in %s for %s account %q",
		name, dir, req.Agent, req.Name)

	out := base
	out.TmuxName = name
	out.SocketPath = socketPath(ctx, pane)
	return out, nil
}

// Live reports whether this account's login flow is still open.
//
// It PROBES tmux rather than trusting the map. A login pane exits on its own
// when the agent's flow finishes — that is the signal that the login is over —
// so a tracked entry is a claim about the past, not the present.
func (s *Supervisor) Live(agent, name string) bool {
	return s.live(agent, name) != nil
}

// live returns the tracked session only when tmux confirms it is still there,
// forgetting it otherwise.
//
// The probe's answer must be POSITIVE to count. ProbeSession collapses several
// failures into "absent", so a wedged or unreachable tmux server would read as
// "the flow ended" — and the caller would start a second login into a directory
// the first one is still writing. An unknown answer keeps the session tracked.
func (s *Supervisor) live(agent, name string) *tmux.TmuxSession {
	s.mu.Lock()
	pane, tracked := s.sessions[key(agent, name)]
	s.mu.Unlock()
	if !tracked {
		return nil
	}
	exists, known := pane.ProbeSession()
	if known && !exists {
		s.forget(agent, name, pane)
		return nil
	}
	return pane
}

// adopt returns the live login pane for this account, tracking a pane this
// supervisor did not spawn if tmux says one is there.
//
// Both answers must be POSITIVE to act on. A tracked session is returned only
// when live() confirms it with tmux, and an untracked NAME is adopted only on
// tmux's determinate "this session exists" — ProbeSession's unknown answer means
// the server did not answer, which is not evidence either way, and adopting on
// it would hand a caller a name nothing is running behind.
func (s *Supervisor) adopt(agent, name string, candidate *tmux.TmuxSession) *tmux.TmuxSession {
	if existing := s.live(agent, name); existing != nil {
		return existing
	}
	exists, known := candidate.ProbeSession()
	if !known || !exists {
		return nil
	}
	if !s.track(agent, name, candidate) {
		return nil
	}
	return candidate
}

func (s *Supervisor) track(agent, name string, pane *tmux.TmuxSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.sessions[key(agent, name)] = pane
	return true
}

// forget drops a tracked entry only if it is STILL the one the caller observed.
// A pane that ended while a second login was starting must not have the new
// pane deleted out from under it by the old one's cleanup.
func (s *Supervisor) forget(agent, name string, pane *tmux.TmuxSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.sessions[key(agent, name)]; ok && current == pane {
		delete(s.sessions, key(agent, name))
		s.closeStreamerLocked(key(agent, name))
	}
}

// Reap closes one account's login pane. An unknown account is a no-op success:
// the desired end state is "no pane", and a flow that already finished has
// reached it.
func (s *Supervisor) Reap(agent, name string) error {
	s.mu.Lock()
	pane, tracked := s.sessions[key(agent, name)]
	delete(s.sessions, key(agent, name))
	s.closeStreamerLocked(key(agent, name))
	s.mu.Unlock()
	if !tracked {
		return nil
	}
	// Pane state ignored: a login pane has no worktree, so there is nothing
	// destructive for it to gate.
	if _, err := pane.Close(); err != nil {
		return fmt.Errorf("could not close the login pane for %s account %q: %w", agent, name, err)
	}
	return nil
}

// Stop closes every login pane this supervisor owns, and refuses to open more.
// Wired into the daemon's teardown at construction, so a SIGTERM during warm-up
// cannot skip it.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopped = true
	panes := make([]*tmux.TmuxSession, 0, len(s.sessions))
	for _, pane := range s.sessions {
		panes = append(panes, pane)
	}
	s.sessions = make(map[string]*tmux.TmuxSession)
	// The streams FIRST, and while still holding the lock: each is a tmux
	// pipe-pane capture goroutine reading the pane about to be torn down, and one
	// left running against a dead pane is the #1632 leak.
	for account := range s.streamers {
		s.closeStreamerLocked(account)
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, pane := range panes {
		wg.Add(1)
		go func(pane *tmux.TmuxSession) {
			defer wg.Done()
			if _, err := pane.Close(); err != nil {
				log.WarningLog.Printf("account login: closing a login pane during shutdown failed: %v", err)
			}
		}(pane)
	}
	wg.Wait()
}

// finishedOrFailed decides what a pane that did not survive to the handover
// MEANS, by asking the account rather than by reading the launch error.
//
// tmux.Start fails when the pane program exits within milliseconds of launch,
// and for a login flow that is genuinely ambiguous: it is what `codex login`
// does against an account that already holds a credential, and equally what a
// flow that died on a bad install does. The launch error cannot tell those
// apart, and it words itself for the second one ("check that it runs and accepts
// its flags"), which is a confusing thing to say to someone whose login just
// worked.
//
// So the artifact decides, which is the same evidence #3384 requires everywhere
// else: a credential is there now, or it is not. When it is not, this is the
// no-op login the issue requires be reported as a FAILURE rather than left as a
// dead account that only fails later, at session start — so the error says that
// in as many words instead of leaving the operator to infer it.
func finishedOrFailed(req Request, base Session, startErr error) (Session, error) {
	loggedIn, probeErr := agentaccount.LoggedIn(req.Home, req.Agent, req.Name)
	if probeErr != nil {
		return Session{}, fmt.Errorf(
			"could not start the %s login pane for account %q: %w (and af could not then check whether the "+
				"account holds a credential: %v)", req.Agent, req.Name, startErr, probeErr)
	}
	if loggedIn {
		out := base
		out.Finished = true
		out.LoggedIn = true
		out.Notices = append(out.Notices, fmt.Sprintf(
			"%s's login command ended before af could hand over the terminal, and %s now holds a credential — "+
				"nothing was left to attach to.", req.Agent, req.Name))
		log.InfoLog.Printf("account login: %s flow for account %q ended before the handover; the account holds a credential",
			req.Agent, req.Name)
		return out, nil
	}
	return Session{}, fmt.Errorf(
		"the %s login flow for account %q ended without leaving a credential in the account, so it is "+
			"registered but not logged in — a session started with --account %s would run unauthenticated. "+
			"Underlying launch error: %w",
		req.Agent, req.Name, req.Name, startErr)
}

// socketPath asks tmux which socket the session lives on.
//
// A failure is NOT fatal. The attach can still resolve the session through its
// own default socket when both sides share a TMUX_TMPDIR, which is the common
// case, so this degrades to an empty path and lets that fall back rather than
// failing a login that would otherwise attach fine (#2019).
func socketPath(ctx context.Context, pane *tmux.TmuxSession) string {
	path, err := pane.SocketPath(ctx)
	if err != nil {
		log.WarningLog.Printf(
			"account login: could not resolve the tmux socket for %s (%v); an attach will fall back to the default socket",
			pane.SanitizedName(), err)
		return ""
	}
	return path
}

// Streamer returns the WebSocket data plane for this account's login pane,
// building it on first use.
//
// It exists because the web cannot attach the way the CLI and the TUI do. A
// login pane has no Instance, so the normal PTY stream route — which resolves
// its byte source through the daemon's instance map — cannot reach it, and
// `tmux attach-session` is not something a browser can run. session's
// BareSessionStreamer is the answer the config assistant already found for the
// same shape (#2467): the broker fans bytes from a `tmux pipe-pane` capture,
// which needs a tmux session and nothing else.
//
// A pane that is not live gets no streamer. Building one for a dead pane would
// start a capture against a session that is gone and hand the caller a stream
// that never speaks.
func (s *Supervisor) Streamer(agent, name string) (*session.BareSessionStreamer, error) {
	pane := s.live(agent, name)
	if pane == nil {
		return nil, fmt.Errorf("%w: no login flow is running for %s account %q", ErrNoLoginPane, agent, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fmt.Errorf("%w: the agent-factory daemon is shutting down", ErrNoLoginPane)
	}
	// Re-checked under the lock: live() released it, so a reap could have landed
	// in between and the entry it dropped must not be recreated here.
	if _, tracked := s.sessions[key(agent, name)]; !tracked {
		return nil, fmt.Errorf("%w: the login flow for %s account %q ended", ErrNoLoginPane, agent, name)
	}
	streamer, ok := s.streamers[key(agent, name)]
	if !ok {
		streamer = session.NewBareSessionStreamer(pane)
		s.streamers[key(agent, name)] = streamer
	}
	return streamer, nil
}

// ErrNoLoginPane reports that no login flow is running for an account. The
// stream route maps it to 404, which is the browser terminal's settle-and-stop
// signal — a login that has ENDED is not a transient drop to reconnect through.
var ErrNoLoginPane = errors.New("no login pane")

// closeStreamerLocked tears down one account's stream. Callers hold mu.
//
// Closing is what stops the pipe-pane capture goroutine. It runs wherever the
// pane stops being ours — reap, an observed death, and shutdown — because a
// capture outliving its pane is a goroutine nothing will ever close (#1632).
func (s *Supervisor) closeStreamerLocked(account string) {
	streamer, ok := s.streamers[account]
	if !ok {
		return
	}
	delete(s.streamers, account)
	streamer.Close()
}
