package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
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
	if s.manager != nil && s.manager.cfg != nil {
		global = s.manager.cfg.DefaultProgram
	}
	resp.Default = defaultProgramFor(global, req.RepoPath)
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
// The global default is the fallback at every step where the repo cannot be
// resolved, matching what a real create does: an unreadable in-repo config does
// not fail the create, it falls back (reserveCreate surfaces the path problem
// right after, with more context).
func defaultProgramFor(globalDefault, repoPath string) string {
	if repoPath == "" {
		return globalDefault
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		return globalDefault
	}
	resolved, rerr := config.ResolveConfig(repo.Root)
	if rerr != nil {
		return globalDefault
	}
	return resolved.DefaultProgram
}
