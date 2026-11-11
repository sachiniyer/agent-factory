package daemon

import (
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The agent-program catalog RPC (#1970). It is the ListBackends pattern (#1933)
// applied one level down: not a missing FIELD, but a stale copy of an enum's
// VALUES.
//
// tmux.SupportedPrograms has always been the canonical agent list. The TUI reads
// it (app/handle_input.go) and the CLI advertises it (commands/root.go,
// api/api.go), so both pick up a new agent for free. The web kept a hand-written
// copy in two pickers, which meant adding a sixth agent server-side left the web
// silently offering five — no failing test, no error, first signal a user asking
// where their agent went. The lists happened to match; the COUPLING was the bug.
//
// ListPrograms removes the copy rather than re-syncing it: the daemon owns the
// enum and every client renders what it is told. The acceptance criterion for
// this RPC is therefore a property, not a value — a seventh agent must reach the
// web with NO edit under web/src.
//
// What this RPC deliberately does NOT do is claim availability, which is where it
// diverges from ListBackends. A backend's preconditions are properties of the
// repo's config, which the daemon can read and check. Whether `aider` is
// installed is a property of the machine the AGENT will run on — and for a docker
// or ssh backend that is not the daemon's machine at all. Probing the daemon's
// own PATH would answer a question nobody asked and, worse, answer it with a
// NEGATIVE: an agent present in the container would be reported missing, hiding a
// working choice behind a confident lie. A probe that cannot know must not answer
// (#1933), so this one reports the enum and stops.

// ListProgramsRequest asks which agent programs a create may select, and which
// one an unspecified create resolves to.
//
// RepoPath is OPTIONAL, which is the deliberate difference from
// ListBackendsRequest. The enum itself is global — the same agents exist
// everywhere — so unlike backends there IS a repo-less answer, and a caller with
// no repo in hand (the task form before a project is picked) still deserves the
// list. RepoPath only sharpens Default, whose in-repo override is repo-scoped.
type ListProgramsRequest struct {
	RepoPath string `json:"repo_path,omitempty"`
}

// ProgramOption is one selectable agent program.
//
// A single-field struct rather than a bare string: it is the shape that lets a
// later field (a display label, a deprecation note) be added without breaking
// every client's parse, which is the mistake that makes an enum expensive to
// grow — and growing this enum cheaply is the entire point of the RPC.
type ProgramOption struct {
	// Name is the wire value: what a client sends back as CreateSessionRequest.Program,
	// and — absent a label to render — what it shows the user verbatim.
	Name string `json:"name"`
}

// ListProgramsResponse is the agent catalog.
type ListProgramsResponse struct {
	// Programs is every supported agent in canonical presentation order
	// (tmux.SupportedPrograms, which is append-only so this order is stable).
	Programs []ProgramOption `json:"programs"`
	// Default is the program a create with NO explicit program resolves to for
	// this repo. Clients render it as the label of their "repo default" choice and
	// must send NO program to get it, not this value echoed back: sending it
	// explicitly would freeze today's default into the request and ignore a later
	// config change.
	//
	// EMPTY when there is no default to report — no manager (so no global config
	// to read) or a config that resolves to no program. Clients render the
	// "repo default" choice with no parenthetical then, rather than naming a
	// program nobody chose.
	Default string `json:"default"`
}

// ListPrograms reports the selectable agent programs, and which one an
// unspecified create resolves to. It is read-only: it starts nothing, dials
// nothing, and touches no session state.
func (s *controlServer) ListPrograms(req ListProgramsRequest, resp *ListProgramsResponse) error {
	resp.Programs = programCatalog(tmux.SupportedPrograms)

	// A nil manager (test control servers, and the window before the manager is
	// ready) means the global config is unavailable — so there is no default to
	// report. Reporting one anyway is the failure mode this whole issue is about.
	global := ""
	if s.manager != nil {
		// Single read in this RPC = its op-entry snapshot (#2480), so a saved
		// default_program is reported by the next ListPrograms with no restart.
		if c := s.manager.Config(); c != nil {
			global = c.DefaultProgram
		}
	}
	// A picker asks once and read-only. When the project config cannot be read
	// it reports NO default — the documented "nothing to report" answer — rather
	// than label the global default as this repo's: the create re-reads it and
	// refuses an unknown instead of using it (#7077 review).
	resolved, rerr := inspectRepoConfigForDefault(req.RepoPath)
	def, derr := defaultProgramFromResolution(global, resolved, rerr)
	if derr != nil {
		log.WarningLog.Printf("programs: no default reported for %q: %v", req.RepoPath, derr)
		return nil
	}
	resp.Default = def
	return nil
}

// programCatalog turns a list of agent names into the options a client renders,
// preserving order and adding, removing and rewriting nothing.
//
// It is a pure function taking the list as a PARAMETER, which is the whole point:
// the property this RPC exists to guarantee — whatever the canonical enum holds
// flows through untouched — is then testable by passing a list rather than by
// swapping tmux.SupportedPrograms out from under the process.
//
// That swap is not merely inelegant, it is a DATA RACE. The enum is a package-level
// global that tmux.DetectAgentFromCommand reads on a hot path (session/tmux/resume.go
// findAgentToken), so a test assigning to it races every other test in the binary
// that resolves an agent — which `go test -race` duly caught. Nothing in production
// ever writes the enum; it is read-only after init, and it must stay that way for
// the read path to be safe without a lock.
func programCatalog(programs []string) []ProgramOption {
	out := make([]ProgramOption, 0, len(programs))
	for _, name := range programs {
		out = append(out, ProgramOption{Name: name})
	}
	return out
}

// defaultProgramFor reports the agent program a create with no explicit program
// resolves to for repoPath, given the daemon's global default.
//
// This is the SHARED decision function: Manager.CreateSession calls it to pick
// the program, and ListPrograms calls it to report the pick. That sharing is the
// point — a catalog that restated the precedence could drift from the create it
// describes, which is the same class of bug as the hardcoded enum this RPC
// exists to delete, one layer down again.
//
// The global default is the answer for a repo with no project config to read —
// no path, a path Git does not recognize — and, as before, for a config the repo
// answered as invalid (reserveCreate surfaces that problem right after, with
// more context). It is NEVER the answer for a project config that could not be
// read: that is unknown, not absent, and a create started on the global default
// in its place runs the wrong agent with nothing to show for it (#7077 review).
// The create resolves with a launch's retry budget and refuses the unknown.
func defaultProgramFor(globalDefault, repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		return globalDefault, nil
	}
	resolved, err := resolveProjectConfigForLaunch(repoPath)
	return defaultProgramFromResolution(globalDefault, resolved, err)
}

// The project-config reads a create's defaults make, as seams so tests can
// stand in an unreadable config without a slow filesystem.
var (
	resolveProjectConfigForLaunch = session.ResolveProjectConfigForLaunch
	checkProjectConfigForLaunch   = session.CheckProjectConfigForLaunch
)

// defaultProgramFromResolution is the precedence itself, shared by the create
// and every reader of its pick: the repo's resolved default, the global one for
// a repo with no readable project config, and an error for an unknown.
func defaultProgramFromResolution(globalDefault string, resolved *config.ResolvedConfig, err error) (string, error) {
	if session.IsProjectConfigUnknown(err) {
		return "", err
	}
	if resolved == nil {
		return globalDefault, nil
	}
	return resolved.DefaultProgram, nil
}

// inspectRepoConfigForDefault is the read-only, one-attempt resolve a picker or
// an inert record label uses: no durable load observation (#4430), and an
// unknown answered as unknown.
func inspectRepoConfigForDefault(repoPath string) (*config.ResolvedConfig, error) {
	if strings.TrimSpace(repoPath) == "" {
		return nil, nil
	}
	return session.InspectProjectConfig(repoPath)
}
