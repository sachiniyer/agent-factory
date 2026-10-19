package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// clearDaemonTarget forces isRemoteTarget to report a local target, so an
// ambient AF_DAEMON_URL cannot turn a local-target test remote.
func clearDaemonTarget(t *testing.T) {
	t.Helper()
	previousURL, previousToken := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = "", ""
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = previousURL, previousToken })
	t.Setenv("AF_DAEMON_URL", "")
	t.Setenv("AF_DAEMON_TOKEN", "")
}

// stubRemoteTarget makes the home believe it is connected through --daemon-url
// without standing up a server: refreshDefaultProgram must skip the read
// entirely, so a test that also traps the resolver proves nothing was read.
func stubRemoteTarget(t *testing.T) {
	t.Helper()
	prev := isRemoteTarget
	isRemoteTarget = func() bool { return true }
	t.Cleanup(func() { isRemoteTarget = prev })
}

// trapConfigRead swaps the resolver seam for one that records calls and fails
// loudly: a code path expected not to read config must never touch it.
func trapConfigRead(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := resolveConfiguredProgramContext
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		calls++
		return "", errors.New("test trap: config read must not run")
	}
	t.Cleanup(func() { resolveConfiguredProgramContext = prev })
	return &calls
}

// configFollowingHome is activeProjectHome built the way a launch with no
// --program flag builds it: the default program is derived from config, and the
// cache is seeded from the config loaded at that moment.
func configFollowingHome(t *testing.T) *home {
	t.Helper()
	clearDaemonTarget(t)
	h := activeProjectHome(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	h.appConfig = cfg
	h.programChoice = newProgramChoice("", cfg.DefaultProgram, cfg)
	require.Equal(t, tmux.ProgramClaude, h.program, "precondition: the launch snapshot is the built-in default")
	return h
}

// programIndex reports program's row in the picker's item order.
func programIndex(t *testing.T, program string) int {
	t.Helper()
	for i, p := range tmux.SupportedPrograms {
		if p == program {
			return i
		}
	}
	t.Fatalf("%q is not in SupportedPrograms", program)
	return -1
}

// TestNewSessionFormFollowsALiveGlobalDefaultProgramChange is #4889: `af config
// set default_program` applies live (#2480), so a form opened after it must
// default to the new value, not the one the TUI read at launch. The re-read is
// synchronous — by the time the naming form is on screen the field already
// holds the live answer.
func TestNewSessionFormFollowsALiveGlobalDefaultProgramChange(t *testing.T) {
	h := configFollowingHome(t)

	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)

	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state)
	require.Equal(t, tmux.ProgramAider, h.pendingProgram,
		"the field opens on the live default_program — the synchronous read already ran")
	require.Equal(t, tmux.ProgramAider, h.program,
		"the cache refills so the NEXT use of the default sees it too")
}

// TestNewSessionFormFollowsALiveProjectDefaultProgram pins the scope: the form
// resolves default_program the way `af config get` does for the project —
// project override > global > built-in.
func TestNewSessionFormFollowsALiveProjectDefaultProgram(t *testing.T) {
	h := configFollowingHome(t)

	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)
	writeInRepoConfig(t, h.repoRoot, "default_program = \"codex\"\n")

	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state)
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram,
		"the project's own default_program outranks the global one")
}

// TestNewSessionFormKeepsTheLaunchProgramFlag is the other half: `af --program
// gemini` is an explicit choice for this run, and a config change must not
// override it — the form does not even read config.
func TestNewSessionFormKeepsTheLaunchProgramFlag(t *testing.T) {
	h := activeProjectHome(t)
	h.programChoice = newProgramChoice(tmux.ProgramGemini, "", h.appConfig)
	calls := trapConfigRead(t)

	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)

	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state)
	require.Equal(t, tmux.ProgramGemini, h.pendingProgram, "the --program flag outranks config")
	require.Zero(t, *calls, "an explicit launch choice runs no config re-read")
}

// TestSwitchProjectMakesTheDefaultProgramFollowConfig: a launch --program flag
// has always been replaced by the incoming project's resolution on a switch, so
// from then on the default follows that project's live config — which the next
// synchronous re-read delivers.
func TestSwitchProjectMakesTheDefaultProgramFollowConfig(t *testing.T) {
	clearDaemonTarget(t)

	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) { return daemon.SnapshotResponse{}, nil }
	h.programChoice = newProgramChoice(tmux.ProgramGemini, "", h.appConfig)

	root := initTestGitRepo(t)
	h.switchProject(&config.RepoContext{Root: root, ID: config.RepoIDFromRoot(root)})
	require.True(t, h.programFollowsConfig)

	writeInRepoConfig(t, root, "default_program = \"codex\"\n")
	h.refreshDefaultProgram()
	require.Equal(t, tmux.ProgramCodex, h.defaultProgram())
}

// TestNewHomeSeedsTheLaunchResolvedDefault: launch already resolved
// default_program for the active project before the TUI starts, and that value
// — not the bare global default — must seed the config-following cache, so a
// failed first re-read cannot swap the project's own agent for the global one.
// Drives the real newHome, as tui_state_test.go does, with a nil repo so the
// registry-mode construction stays hermetic.
func TestNewHomeSeedsTheLaunchResolvedDefault(t *testing.T) {
	clearDaemonTarget(t)

	tmp := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return nil, nil
	}))

	h := newHome(context.Background(), "", tmux.ProgramCodex, nil)

	require.True(t, h.programFollowsConfig, "no --program flag means the default still follows config")
	require.Equal(t, tmux.ProgramCodex, h.program,
		"the launch-time resolved default_program seeds the cache a failed re-read falls back to")

	// Corrupt the global config so the re-read fails; the cached seed must
	// survive rather than collapsing to the global value.
	cfgPath, err := config.GlobalConfigPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, []byte("invalid toml {{{\n[broken"), 0o644))
	h.refreshDefaultProgram()
	require.Equal(t, tmux.ProgramCodex, h.program,
		"a failed re-read keeps the launch-resolved default, not the bare global")
	require.Equal(t, tmux.ProgramCodex, h.defaultProgram())
}

// TestNewSessionFormKeepsTheLaunchResolvedDefaultWhenReReadFails is the
// project-scoped half of the same guarantee: the launch resolved the project's
// own default_program (here codex), so when the project re-read fails — a
// transiently malformed in-repo config — the open form keeps codex rather than
// silently falling back to the global default.
func TestNewSessionFormKeepsTheLaunchResolvedDefaultWhenReReadFails(t *testing.T) {
	clearDaemonTarget(t)

	h := activeProjectHome(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	h.appConfig = cfg
	h.programChoice = newProgramChoice("", tmux.ProgramCodex, cfg)

	writeInRepoConfig(t, h.repoRoot, "invalid toml {{{\n[broken")

	_, _ = h.startNewInstance()
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram,
		"a failed project re-read leaves the field on the launch-resolved default")
	require.Equal(t, tmux.ProgramCodex, h.program)
}

// TestRemoteTargetMakesNoLocalRead is the remote half of the design (#4889
// review): pointed at a remote daemon, the TUI keeps master's behaviour — the
// field shows the launch-time default, and no read runs at all, because the
// remote daemon's default_program is remote state the client's local config
// files cannot see and there is no synchronous remote read to make.
func TestRemoteTargetMakesNoLocalRead(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	// The remote open still asks the DAEMON (ListPrograms) — the no-read claim
	// is about the client's local config files, so stub the dial away.
	stubRemoteProgramFeed(t, tmux.ProgramClaude)

	h := activeProjectHome(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	h.appConfig = cfg
	h.programChoice = newProgramChoice("", cfg.DefaultProgram, cfg)
	calls := trapConfigRead(t)

	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state)
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram,
		"a remote target's field shows the launch-time default — master's behaviour")
	require.Zero(t, *calls,
		"opening the form on a remote target must not read the client's local config")
}

// settleRemoteForm drains a form-opening cmd's async replies (the account
// preselect and the remote ListPrograms answer) back through Update, the way
// the real event loop delivers them — the remote submit gate lifts only when
// that answer arrives (#4889 review).
func settleRemoteForm(t *testing.T, h *home, cmd tea.Cmd) {
	t.Helper()
	for _, msg := range drainCmd(t, cmd, time.Second) {
		_, _ = h.Update(msg)
	}
}

// stubRemoteProgramFeed answers the remote ListPrograms fetch and the account
// preselect without a dial, so a remote-target test can deliver the replies.
func stubRemoteProgramFeed(t *testing.T, program string) {
	t.Helper()
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		return daemon.ListProgramsResponse{Default: program}, nil
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
}

// TestRemoteUntouchedSubmitLeavesTheProgramImplicit: for a remote daemon an
// untouched field still DISPLAYS the launch-time local default — the remote
// daemon's default_program is remote state no synchronous read can see — but
// the wire carries "" so the remote daemon resolves its own live default
// (#4889 review). Sending the local seed explicitly would override the remote
// default with a program that may not even exist on that host.
func TestRemoteUntouchedSubmitLeavesTheProgramImplicit(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	stubRemoteProgramFeed(t, tmux.ProgramClaude)

	h := activeProjectHome(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	h.appConfig = cfg
	h.programChoice = newProgramChoice("", cfg.DefaultProgram, cfg)
	got := recordStartRequest(t)

	_, cmd := h.startNewInstance()
	settleRemoteForm(t, h, cmd)
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, "demo", got.Title)
	assert.Equal(t, "", got.Program,
		"an untouched remote submit stays implicit — the remote daemon resolves its own default_program")
}

// TestProgramPickerPickSubmitsTheChosenProgram: a program the user confirmed in
// the picker — not merely displayed — goes out concretely on the wire, never
// rewritten or made implicit by the default-following machinery (#4889 review).
func TestProgramPickerPickSubmitsTheChosenProgram(t *testing.T) {
	h := configFollowingHome(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	got := recordStartRequest(t)

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, stateSelectProgram, h.state)
	h.selectionOverlay.SetSelectedIndex(programIndex(t, tmux.ProgramAider))
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state)
	require.Equal(t, tmux.ProgramAider, h.pendingProgram)
	require.True(t, h.pendingProgramChosen)

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, "demo", got.Title)
	assert.Equal(t, tmux.ProgramAider, got.Program,
		"a picked program submits concretely — never implicit")
}

// TestProgramPickerReselectingTheShownValueIsStillAChoice is the case string
// equality cannot see: the user opened the picker and confirmed the value
// ALREADY shown — a deliberate re-confirmation, so the wire carries it
// concretely rather than as "" (#4889 review).
func TestProgramPickerReselectingTheShownValueIsStillAChoice(t *testing.T) {
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))
	seed := h.pendingProgram

	// The picker opens preselected on the seeded value; the user submits it
	// unchanged — a decision, not an untouched field.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, stateSelectProgram, h.state)
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state)
	require.Equal(t, seed, h.pendingProgram, "the re-pick changes nothing by value")
	require.True(t, h.pendingProgramChosen, "but it IS an explicit choice")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, "demo", got.Title)
	assert.Equal(t, seed, got.Program,
		"a same-value re-pick submits concretely — only untouched fields go implicit")
}

// TestNewSessionFormResolvesAccountForTheLiveProgram: the account default
// fetched at form-open belongs to the program the field shows — so when the
// synchronous re-read moves the program, the fetch must already ask for the
// NEW agent's registry (#4889 review).
func TestNewSessionFormResolvesAccountForTheLiveProgram(t *testing.T) {
	h := configFollowingHome(t)
	defaults := twoAgentsWithAccounts()
	defaults.Defaults = map[string]string{"claude": "work", "codex": "ops"}
	stubAccounts(t, defaults, nil)

	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramCodex)
	require.NoError(t, err)

	_, cmd := h.startNewInstance()
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram)

	// The account default for the LIVE program — codex, not the seeded claude —
	// is what the open-form fetch asks for.
	var delivered accountDefaultMsg
	for _, msg := range drainCmd(t, cmd, time.Second) {
		if am, ok := msg.(accountDefaultMsg); ok {
			delivered = am
		}
	}
	require.Equal(t, "codex", delivered.agent,
		"the account fetch answers for the agent the live read delivered")
	h.Update(delivered)
	require.Equal(t, "ops", h.pendingAccount,
		"the new agent's project default lands on the field")
}

// TestUntouchedNamingFormSubmitsAnImplicitProgram is the implicit-submit
// contract (#4889 review): a program the field only DISPLAYED — never picked —
// goes out as "" so the daemon resolves default_program at create time, on the
// daemon's host, against the session's own repo. The placeholder still carries
// the resolved value for display and a failed-create restore.
func TestUntouchedNamingFormSubmitsAnImplicitProgram(t *testing.T) {
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, "demo", got.Title)
	assert.Empty(t, got.Program,
		"a program the field only showed is not the user's pick — the daemon resolves it live")
	assert.Equal(t, tmux.ProgramClaude, naming.Program,
		"the placeholder keeps the resolved value — it is what a failed-create restore reseeds")
}

// TestTaskCreateStoresProgramDefaultUnresolved is the run-time-resolution
// contract (#4889 review): the inline create's "Use config default" choice
// must be STORED as an empty program — task/task.go resolves default_program
// against the task's repo when the task RUNS — never folded to the launch-time
// cache at save, which would bake a possibly-stale agent into the task.
func TestTaskCreateStoresProgramDefaultUnresolved(t *testing.T) {
	h := configFollowingHome(t)
	t.Chdir(h.repoRoot)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	tp := h.automations.TaskPane()

	// Fill the inline create exactly as a user does — name, default schedule,
	// prompt — and leave the program field on "Use config default".
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, tp.IsCreating(), "'n' must open the inline create form")
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("nightly-default")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> schedule picker (daily default)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do a thing")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> save button
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 1, "the create should have committed one task")
	assert.Empty(t, disk[0].Program,
		"\"Use config default\" stores empty so the runner resolves the live default_program per run")
}

// TestTaskSaveDoesNotReadTheProgramConfig: a task save must NOT synchronously
// re-read the repo config on the event loop (#4889 review). The read's result
// cannot affect the stored task — an empty draft program stays empty for
// run-time resolution, a non-empty draft stores verbatim, and an explicit
// launch short-circuits the refresh anyway — so the bounded lookup only risked
// freezing every save for up to its timeout to warm a cache its real consumer
// (the next session form) re-reads anyway.
func TestTaskSaveDoesNotReadTheProgramConfig(t *testing.T) {
	h := configFollowingHome(t)
	t.Chdir(h.repoRoot)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID
	calls := trapConfigRead(t)

	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	tp := h.automations.TaskPane()

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, tp.IsCreating())
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("nightly-default")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do a thing")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	require.Zero(t, *calls,
		"task save performs no program-config read on the event loop")
	require.Equal(t, tmux.ProgramClaude, h.program,
		"the display cache keeps the launch seed — the save does not refresh it")
	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 1, "the create should have committed one task")
	assert.Empty(t, disk[0].Program,
		"\"Use config default\" still stores empty for run-time resolution")
}

// TestRefreshKeepsTheCacheOnAReadFailure: a config read that errors — a
// malformed file, a wedged mount that outlasted its bound — keeps the
// last-known default rather than blanking the field (#4889 review).
func TestRefreshKeepsTheCacheOnAReadFailure(t *testing.T) {
	h := configFollowingHome(t)

	writeInRepoConfig(t, h.repoRoot, "invalid toml {{{\n[broken")
	h.refreshDefaultProgram()
	require.Equal(t, tmux.ProgramClaude, h.program,
		"a failed re-read keeps the last-known default")
	require.Equal(t, tmux.ProgramClaude, h.defaultProgram())
}

// TestImplicitProgramDropsAnUnchosenAccountPreselect is the account half of the
// implicit-send contract (#4889 review): an account the daemon PRESELECTED for
// the displayed program belongs to that program's registry, so when the
// program goes out as "" — resolved live on the daemon — the unchosen preselect
// must go out as "" too. Otherwise a default_program change while the form is
// open pairs the resolved program with an account fetched for a different
// agent: a refusal, or worse a same-named identity in the new agent's registry.
func TestImplicitProgramDropsAnUnchosenAccountPreselect(t *testing.T) {
	h := configFollowingHome(t)
	defaults := twoAgentsWithAccounts()
	defaults.Defaults = map[string]string{"claude": "work"}
	stubAccounts(t, defaults, nil)
	got := recordStartRequest(t)

	_, cmd := h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))
	for _, msg := range drainCmd(t, cmd, time.Second) {
		if am, ok := msg.(accountDefaultMsg); ok {
			_, _ = h.Update(am)
		}
	}
	require.Equal(t, "work", h.pendingAccount, "the preselect landed on the field")
	require.False(t, h.pendingAccountChosen, "a preselect is not a user choice")

	// The implicit submit re-checks the displayed account first (#4893
	// review): the preselect is a cache of default_accounts, which may have
	// moved since the fetch answered.
	msgs := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "the verify round-trip runs before the wire goes out")
	for _, msg := range msgs {
		if am, ok := msg.(accountDefaultMsg); ok {
			_, _ = h.handleAccountDefault(am)
		}
	}
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Program, "the untouched program stays implicit")
	assert.Empty(t, got.Account,
		"an account preselected FOR the displayed program must not become an explicit account for the resolved one")
}

// TestChosenAccountPinsTheDisplayedProgram is the other half of the pairing
// (#4889 review): an account the USER picked is a commitment to the program on
// screen — an identity in that agent's registry — so the program it was chosen
// against pins explicit on the wire. Sending "" would let the daemon resolve a
// different default and pair it with an identity from the wrong registry.
func TestChosenAccountPinsTheDisplayedProgram(t *testing.T) {
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))
	h.pendingAccount = "work"
	h.pendingAccountChosen = true

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, tmux.ProgramClaude, got.Program,
		"a chosen account pins the displayed program — the pair the user committed")
	assert.Equal(t, "work", got.Account)
}

// TestFailedCreateRestoreKeepsAnImplicitProgramImplicit is the draft-restore
// half (#4889 review): a create whose untouched program went out as "" keeps
// "" on retry — the failure may BE the old default going away, and marking the
// draft's display value "chosen" would freeze it into an explicit retry.
func TestFailedCreateRestoreKeepsAnImplicitProgramImplicit(t *testing.T) {
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	inst := newLoadingInstance(t, "demo")
	inst.Path = h.repoRoot
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)

	// The default moves after the failure — exactly the case where replaying
	// the displayed program would retry the dead agent.
	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)

	// The failed request's wire program was implicit — "" for the daemon to
	// resolve — which is the only thing the restore can faithfully re-arm. With
	// the user still watching, the refusal re-arms the form inline.
	req := sessionStartRequest{Title: "demo", RepoPath: h.repoRoot, Program: ""}
	_, _ = h.Update(instanceStartedMsg{instance: inst, draft: &req, err: errors.New("The daemon refused this create.")})
	require.Equal(t, stateNew, h.state, "the draft re-armed the form")
	require.False(t, h.pendingProgramChosen,
		"a draft that went out implicit must restore implicit")
	require.Equal(t, tmux.ProgramAider, h.pendingProgram,
		"the restored field shows the live default the retry resolves")
	require.NoError(t, h.namingInstance.SetTitle("demo2"))
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Program,
		"the retry resolves the live default rather than replaying the dead one")
}

// TestPreflightChecksTheProgramTheDaemonWillResolve is the preflight half
// (#4889 review): an untouched field sends "", so the daemon resolves the live
// default_program — preflight must check THAT program. Checking the displayed
// seed would refuse a create that succeeds (old default uninstalled, new one
// present) or wave through one that fails.
func TestPreflightChecksTheProgramTheDaemonWillResolve(t *testing.T) {
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	var checked string
	t.Cleanup(SetLocalSessionPreflightForTest(func(_ *config.Config, program string) error {
		checked = program
		return nil
	}))

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram,
		"the field opened on the old default")

	// The default moves while the form is open; the daemon will resolve aider.
	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)

	// The first Enter refreshes the field and holds for review (#4893
	// review); the second submits the implicit "" — and preflight must check
	// what the daemon resolves, not the stale seed the form opened on.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, tmux.ProgramAider, h.pendingProgram)
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, tmux.ProgramAider, checked,
		"preflight validates the program the daemon will resolve, not the stale seed")
	assert.Empty(t, got.Program, "the create itself still goes out implicit")
}

// TestPreflightChecksTheProgramAChosenAccountPins is the account-pick half
// (#4889 review): when the user picks an account, the submit pins the
// DISPLAYED program to keep the pair — so a default_program that moves while
// the form is open is NOT what the wire carries. Preflighting the resolved
// default would check a program the create never sends: refuse a valid
// request when the pinned one is installed, or approve a missing one.
func TestPreflightChecksTheProgramAChosenAccountPins(t *testing.T) {
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	var checked string
	t.Cleanup(SetLocalSessionPreflightForTest(func(_ *config.Config, program string) error {
		checked = program
		return nil
	}))

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram,
		"the field opened on the old default")

	// The default moves while the form is open AND the user picks an account —
	// the pair on the wire is the pinned display program + the account.
	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)
	h.pendingAccount = "work"
	h.pendingAccountChosen = true

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, tmux.ProgramClaude, checked,
		"preflight validates the program the wire pins, not the moved default")
	assert.Equal(t, tmux.ProgramClaude, got.Program,
		"the create carries the pinned program the account was chosen against")
	assert.Equal(t, "work", got.Account)
}

// TestTaskCreatePreservesAnExplicitLaunchProgram is the task-side contract
// (#4889 review): launched with an explicit --program, "Use config default"
// means the launch's choice, so the task stores it — the empty program the
// runner re-resolves is only for a launch that follows config.
func TestTaskCreatePreservesAnExplicitLaunchProgram(t *testing.T) {
	h := activeProjectHome(t)
	clearDaemonTarget(t)
	h.programChoice = newProgramChoice(tmux.ProgramGemini, "", h.appConfig)
	t.Chdir(h.repoRoot)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	tp := h.automations.TaskPane()

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, tp.IsCreating())
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("launch-pinned")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do a thing")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 1)
	assert.Equal(t, tmux.ProgramGemini, disk[0].Program,
		"an explicit launch --program is the default \"Use config default\" refers to")
}

// TestPreflightSkipsClientLocalChecksForRemoteTarget is the remote half of the
// implicit-program finding (#4889 review): an untouched form sends "" so the
// REMOTE daemon resolves its own default_program — substituting the client's
// cfg.DefaultProgram and checking THIS host's binary would refuse a create the
// remote can serve (or vice versa). Remote creates cannot be validated by
// client-local prereqs at all: tmux and the agent binary live on the daemon's
// host, and the daemon rechecks the program itself.
func TestPreflightSkipsClientLocalChecksForRemoteTarget(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	stubRemoteProgramFeed(t, tmux.ProgramClaude)

	h := activeProjectHome(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	h.appConfig = cfg
	h.programChoice = newProgramChoice("", cfg.DefaultProgram, cfg)
	got := recordStartRequest(t)

	calls := 0
	t.Cleanup(SetLocalSessionPreflightForTest(func(*config.Config, string) error {
		calls++
		return nil
	}))

	_, cmd := h.startNewInstance()
	settleRemoteForm(t, h, cmd)
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, 0, calls,
		"a remote target skips client-local prereqs — the daemon's host owns tmux, PATH and default_program")
	require.Equal(t, "demo", got.Title)
	assert.Equal(t, "", got.Program,
		"the remote submit still goes out implicit for the daemon to resolve")
}

// TestFailedCreateRestoreRefetchesTheDefaultAccount pins the account half of
// the implicit-draft restore (#4889 review): an implicit submit clears the
// wire account, so the draft proves nothing about what was DISPLAYED — but a
// retry re-applies the configured default. The restore must re-fetch that
// default exactly as a fresh form does, or the reopened form shows "no
// account" beside a create that will run scoped to one.
func TestFailedCreateRestoreRefetchesTheDefaultAccount(t *testing.T) {
	h := configFollowingHome(t)
	recordStartRequest(t)
	calls, _ := stubAccounts(t, withDefaults(map[string]string{"claude": "work"}), nil)

	inst := newLoadingInstance(t, "demo")
	inst.Path = h.repoRoot
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)

	// The failed submit went out implicit on BOTH fields: the account the form
	// displayed was the daemon's preselect, never a user choice.
	req := sessionStartRequest{Title: "demo", RepoPath: h.repoRoot, Program: "", Account: ""}
	_, cmd := h.Update(instanceStartedMsg{instance: inst, draft: &req, err: errors.New("The daemon refused this create.")})
	require.Equal(t, stateNew, h.state, "the draft re-armed the form")
	require.False(t, h.pendingAccountChosen)
	require.Empty(t, h.pendingAccount,
		"the draft only proves nothing was SENT — the field waits on the refetch")

	require.NotNil(t, cmd, "the restore asks the daemon which account the retry would apply")
	for _, msg := range drainCmd(t, cmd, time.Second) {
		if def, ok := msg.(accountDefaultMsg); ok {
			_, _ = h.handleAccountDefault(def)
		}
	}
	assert.Equal(t, 1, *calls, "one ListAccounts for the restored form")
	assert.Equal(t, "work", h.pendingAccount,
		"the restored form shows the default the retry will apply")
}

// TestFailedCreateRestoreFreshensAChosenAmbientAccount pins the #4889 review
// finding on the chosen-EMPTY account: a deliberate "Ambient identity" pick
// sends Account="" — the same wire encoding as an untouched field — so the
// daemon resolves default_accounts fresh on retry. Skipping the refresh and
// refetch would leave the display naming defaults that may already have moved:
// the wire cannot see the difference, so restore treats it as unchosen and
// freshens both fields (#4889 review).
func TestFailedCreateRestoreFreshensAChosenAmbientAccount(t *testing.T) {
	h := configFollowingHome(t)
	recordStartRequest(t)
	calls, _ := stubAccounts(t,
		withDefaults(map[string]string{"claude": "work", "codex": "ci"}), nil)

	inst := newLoadingInstance(t, "demo")
	inst.Path = h.repoRoot
	inst.Program = tmux.ProgramClaude
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)

	// Both defaults move after the failure — the wire's ""+"" resolves them
	// fresh, so the restored display must too.
	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramCodex)
	require.NoError(t, err)

	// The failed submit carried an explicit ambient choice: Account="" on the
	// wire, AccountChosen=true beside it.
	req := sessionStartRequest{Title: "demo", RepoPath: h.repoRoot, Program: "", Account: "", AccountChosen: true}
	_, cmd := h.Update(instanceStartedMsg{instance: inst, draft: &req, err: errors.New("The daemon refused this create.")})
	require.Equal(t, stateNew, h.state, "the draft re-armed the form")
	require.False(t, h.pendingAccountChosen,
		"an empty account pins nothing — the wire sends \"\" either way, so restore treats it as implicit")
	require.Empty(t, h.pendingAccount, "ambient — until the refetch answers")
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram,
		"the displayed program re-follows the live default — the retry resolves it too")

	require.NotNil(t, cmd, "the restore re-fires the account-default fetch")
	for _, msg := range drainCmd(t, cmd, time.Second) {
		if def, ok := msg.(accountDefaultMsg); ok {
			_, _ = h.handleAccountDefault(def)
		}
	}
	assert.Equal(t, 1, *calls, "one ListAccounts for the restored form")
	assert.Equal(t, "ci", h.pendingAccount,
		"the refetch preselects the default for the LIVE program, not the failed one")
}

// TestFailedCreateRestoreUnpinsAnAccountPinnedProgram is the program half of
// the draft-flags finding (#4889 review): a program that reached the wire only
// because a chosen account pinned it is NOT a program choice — but it IS the
// pair the account belongs to, so the restore must keep the displayed program
// rather than re-resolve a moved default beneath a live pin. Flipping the
// account back to ambient dissolves the pair and returns the retry to
// config-following: Program "" on the wire.
func TestFailedCreateRestoreUnpinsAnAccountPinnedProgram(t *testing.T) {
	h := configFollowingHome(t)
	got := recordStartRequest(t)
	stubAccounts(t, withDefaults(map[string]string{"claude": "work"}), nil)

	inst := newLoadingInstance(t, "demo")
	inst.Path = h.repoRoot
	inst.Program = tmux.ProgramClaude // the field displayed claude; the account pinned it
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)

	// The default moves after the failure — a re-resolve would silently
	// re-pair the chosen claude account "work" with a different agent.
	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramCodex)
	require.NoError(t, err)

	// The failed submit: account "work" chosen, the program on the wire only
	// because the pairing pinned it — ProgramChosen stays false.
	req := sessionStartRequest{Title: "demo", RepoPath: h.repoRoot, Program: "claude", Account: "work", AccountChosen: true}
	_, _ = h.Update(instanceStartedMsg{instance: inst, draft: &req, err: errors.New("The daemon refused this create.")})
	require.Equal(t, stateNew, h.state)
	require.True(t, h.pendingAccountChosen)
	require.False(t, h.pendingProgramChosen,
		"an account-pinned program restores config-following, not chosen")
	require.Equal(t, "work", h.pendingAccount)
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram,
		"the displayed program stays — the chosen account keeps its agent pair")

	// The user reconsiders: ambient identity. The program pairing dissolves
	// and the retry resolves the live default_program instead of replaying
	// the pinned agent.
	h.clearPendingAccount()
	require.NoError(t, h.namingInstance.SetTitle("demo2"))
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Program,
		"with the account back at ambient, the retry follows the config default again")
	assert.Empty(t, got.Account)
}

// TestStaleAccountDefaultReplyCannotOverwriteTheRefetch pins the #4889 review
// finding on reply ordering: a submit issued while the form-open
// fetchAccountDefault is still in flight, followed by a failure restore,
// launches a SECOND fetch — and the daemon's default may have moved in
// between. The older reply arriving LAST must not overwrite the newer
// preselection, so each fetch is stamped with an issuance seq the handler
// fences on.
func TestStaleAccountDefaultReplyCannotOverwriteTheRefetch(t *testing.T) {
	h := configFollowingHome(t)
	recordStartRequest(t)

	var calls int
	t.Cleanup(SetAccountListerForTest(func(_, _ string) (daemon.ListAccountsResponse, error) {
		calls++
		if calls == 1 {
			return withDefaults(map[string]string{"claude": "stale"}), nil
		}
		return withDefaults(map[string]string{"claude": "fresh"}), nil
	}))

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))

	// Fetch one is still in flight when the create fails and the restore
	// launches fetch two. The seam answers on invocation, so the cmds are
	// invoked in issuance order — fetch one reads the OLD default, fetch two
	// the moved one — then their replies are DELIVERED out of order: newer
	// lands first, stale lands last.
	first := h.fetchAccountDefault(naming, "claude")
	second := h.fetchAccountDefault(naming, "claude")
	require.NotNil(t, first)
	require.NotNil(t, second)

	older := first().(accountDefaultMsg)
	newer := second().(accountDefaultMsg)

	_, _ = h.Update(newer)
	require.Equal(t, "fresh", h.pendingAccount, "the newer answer applies")

	_, _ = h.Update(older)
	assert.Equal(t, "fresh", h.pendingAccount,
		"an older in-flight reply must not overwrite the newer preselection")
	assert.Equal(t, 2, calls)
}

// TestFailedCreateDraftKeepsALaunchFixedProgram is the draft-capture half
// (#4889 review): `af --program X` sends X concretely without the picker's
// pendingProgramChosen bit, so the draft must record the program as CHOSEN —
// otherwise a post-failure project switch's config-following reset makes the
// restore re-resolve and swap the launch program for the moved default.
func TestFailedCreateDraftKeepsALaunchFixedProgram(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	// The --program shape: a concrete program with no config following.
	h.programChoice = newProgramChoice(tmux.ProgramGemini, "", h.appConfig)
	require.False(t, h.programFollowsConfig, "precondition: --program pins, it does not follow")

	var captured sessionStartRequest
	t.Cleanup(SetSessionStarterForTest(func(inst *session.Instance, req sessionStartRequest) (*session.Instance, error) {
		captured = req
		return inst, errors.New("The daemon refused this create.")
	}))

	inst := startNaming(t, h, "demo")
	// Model the launch-fixed state honestly: the form opens with the pinned
	// program seeded into the field (startNaming's generic "claude" seed is
	// the config-following shape, which this case is not). The user stays on
	// the failed instance, in its project, so the refusal re-arms the form
	// inline rather than stashing the draft for a later create.
	h.pendingProgram = tmux.ProgramGemini
	inst.Program = tmux.ProgramGemini
	inst.Path = h.repoRoot
	h.sidebar.SelectInstance(inst)
	msgs := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, tmux.ProgramGemini, captured.Program,
		"the launch-fixed program goes out concretely")
	require.True(t, captured.ProgramChosen,
		"a concrete program is a chosen program — the retry must not re-resolve it")

	// The refusal re-arms the form with the draft intact: the launch program
	// stays the restored pick rather than being replaced by the default.
	for _, msg := range msgs {
		if _, ok := msg.(instanceStartedMsg); ok {
			_, _ = h.Update(msg)
		}
	}
	require.Equal(t, stateNew, h.state, "the draft re-armed the form")
	require.True(t, h.pendingProgramChosen,
		"the launch-fixed draft restores as a choice, not an implicit field")
	require.Equal(t, tmux.ProgramGemini, h.pendingProgram)
}

// TestRemoteTargetSeedsTheDaemonsDefault is the remote half of #4889 review:
// a remote daemon resolves its OWN default_program, so the form must not keep
// displaying the client's launch-time local seed — the account preselect that
// fired for the local agent is fetched again for the resolved one.
func TestRemoteTargetSeedsTheDaemonsDefault(t *testing.T) {
	h := configFollowingHome(t)
	// Set the remote target AFTER configFollowingHome — its clearDaemonTarget
	// resets AF_DAEMON_URL, so the env must be written after the home exists.
	t.Setenv("AF_DAEMON_URL", "http://buildbox:8443")

	t.Cleanup(SetProgramListerForTest(func(repoPath string) (daemon.ListProgramsResponse, error) {
		return daemon.ListProgramsResponse{Default: tmux.ProgramCodex}, nil
	}))
	// The fetch is agent-agnostic — the daemon returns a Defaults map for the
	// whole roster and msg.agent selects the entry — so the refetch is visible
	// only in the RESULT: codex's default lands once the program moves.
	var fetches int
	t.Cleanup(SetAccountListerForTest(func(agent, repoPath string) (daemon.ListAccountsResponse, error) {
		fetches++
		return daemon.ListAccountsResponse{Defaults: map[string]string{"codex": "work"}}, nil
	}))

	_, cmd := h.startNewInstance()
	msgs := drainCmd(t, cmd, time.Second)
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram,
		"the local seed renders provisionally until the daemon answers")
	require.True(t, len(msgs) >= 2,
		"a remote target fires the remote-default fetch beside the account fetch")

	for _, msg := range msgs {
		_, next := h.Update(msg)
		for _, m2 := range drainCmd(t, next, time.Second) {
			_, _ = h.Update(m2)
		}
	}
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram,
		"the displayed program follows the daemon's resolved default")
	require.Equal(t, "work", h.pendingAccount,
		"the account preselect lands for the resolved agent, not the local guess")
	require.Equal(t, 2, fetches,
		"the correction re-fetches the preselect under the resolved agent")
}

// TestRemoteSubmitWaitsForTheDefaultAnswer is the submit-gate half (#4889
// review): while the remote ListPrograms fetch is unanswered, Enter must not
// send "" — the daemon would resolve a default the field has not yet shown.
func TestRemoteTargetSubmitWaitsForTheDefaultAnswer(t *testing.T) {
	h := configFollowingHome(t)
	t.Setenv("AF_DAEMON_URL", "http://buildbox:8443")

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	t.Cleanup(SetProgramListerForTest(func(repoPath string) (daemon.ListProgramsResponse, error) {
		<-release
		return daemon.ListProgramsResponse{Default: tmux.ProgramCodex}, nil
	}))
	t.Cleanup(SetAccountListerForTest(func(agent, repoPath string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
	got := recordStartRequest(t)

	_, cmd := h.startNewInstance()
	// The blocked fetch's msg never lands inside the drain window; the account
	// fetch still does.
	_ = drainCmd(t, cmd, 200*time.Millisecond)
	require.True(t, h.remoteDefaultPending, "the remote fetch armed the submit gate")

	require.NoError(t, h.namingInstance.SetTitle("demo"))
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "a pending remote default must not be submitted past")
	require.Equal(t, stateNew, h.state)

	// The answer lands; the gate lifts and the corrected program shows.
	_, _ = h.Update(remoteDefaultMsg{naming: h.namingInstance, program: tmux.ProgramCodex})
	require.False(t, h.remoteDefaultPending)
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram)
}

// TestRemoteDefaultDoesNotReplaceALaunchFixedProgram guards the second guard
// clause (#4889 review): a launch --program pins via programFollowsConfig, not
// pendingProgramChosen — a remote default reply must not overwrite it (the
// submit would then send the remote program CONCRETELY, replacing the flag's
// program outright).
func TestRemoteDefaultDoesNotReplaceALaunchFixedProgram(t *testing.T) {
	h := configFollowingHome(t)
	t.Setenv("AF_DAEMON_URL", "http://buildbox:8443")
	h.programChoice = newProgramChoice(tmux.ProgramGemini, "", h.appConfig)

	inst := startNaming(t, h, "demo")
	h.pendingProgram = tmux.ProgramGemini
	inst.Program = tmux.ProgramGemini

	_, _ = h.Update(remoteDefaultMsg{naming: inst, program: tmux.ProgramCodex})
	require.Equal(t, tmux.ProgramGemini, h.pendingProgram,
		"the launch-fixed program survives the remote answer")
	assert.Equal(t, tmux.ProgramGemini, h.sendProgramForCreate(),
		"the wire keeps the user's --program, not the daemon's default")
}

// TestRemoteDefaultClearsTheOldAgentsAccount covers the cross-agent pair
// (#4889 review): an account PRESELECTED under the local-guess agent belongs
// to that agent's registry — a program correction must not carry it onto the
// resolved agent.
func TestRemoteDefaultClearsTheOldAgentsAccount(t *testing.T) {
	h := configFollowingHome(t)
	t.Setenv("AF_DAEMON_URL", "http://buildbox:8443")
	t.Cleanup(SetAccountListerForTest(func(agent, repoPath string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))

	inst := startNaming(t, h, "demo")
	h.pendingAccount = "work"

	_, _ = h.Update(remoteDefaultMsg{naming: inst, program: tmux.ProgramCodex})
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram)
	assert.Empty(t, h.pendingAccount, "the old agent's preselect cannot pair with the new program")
	assert.False(t, h.pendingAccountChosen)
}

// TestRemoteDefaultPreservesAChosenAccountPair is the chosen half of the same
// contract (#4893 review): a named account the user picked commits the form to
// the displayed program — sendProgramForCreate already pins it concrete, so
// the remote reply must not correct the program out from under the pair and
// produce (resolved-agent, old-account) on the wire.
func TestRemoteDefaultPreservesAChosenAccountPair(t *testing.T) {
	h := configFollowingHome(t)
	t.Setenv("AF_DAEMON_URL", "http://buildbox:8443")

	inst := startNaming(t, h, "demo")
	h.pendingAccount = "work"
	h.pendingAccountChosen = true

	_, _ = h.Update(remoteDefaultMsg{naming: inst, program: tmux.ProgramCodex})
	assert.Equal(t, tmux.ProgramClaude, h.pendingProgram,
		"a chosen account already committed the pair — the correction must not unpair it")
	assert.Equal(t, "work", h.pendingAccount)
	assert.True(t, h.pendingAccountChosen)
	assert.Equal(t, tmux.ProgramClaude, h.sendProgramForCreate(),
		"the wire keeps the pair the user chose")
}

// TestProgramPickerUsesTheRemoteCatalog covers the catalog half (#4889
// review): a remote daemon newer than this client can report programs the
// local enum lacks; the picker must offer the remote list so the resolved
// default is a selectable, displayable row.
func TestProgramPickerUsesTheRemoteCatalog(t *testing.T) {
	h := configFollowingHome(t)
	t.Setenv("AF_DAEMON_URL", "http://buildbox:8443")

	inst := startNaming(t, h, "demo")
	_, _ = h.Update(remoteDefaultMsg{
		naming: inst, program: "newagent",
		programs: []string{"claude", "newagent"},
	})

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, stateSelectProgram, h.state)
	require.Equal(t, []string{"claude", "newagent"}, h.programPickerChoices,
		"the picker offers the daemon's catalog")
}

// TestEmptyAccountDefaultClearsAStalePreselect covers the restore-echo half
// (#4889 review): a refetch answering with no default must clear an unchosen
// name an earlier reply left, not leave it to be echoed on the retry.
func TestEmptyAccountDefaultClearsAStalePreselect(t *testing.T) {
	h := configFollowingHome(t)

	inst := startNaming(t, h, "demo")
	h.pendingAccount = "work" // an unchosen preselect the first fetch applied

	_, _ = h.Update(accountDefaultMsg{
		naming: inst, agent: "claude",
		resp: daemon.ListAccountsResponse{Defaults: map[string]string{}},
	})
	assert.Empty(t, h.pendingAccount,
		"an empty live default must clear the stale name, not leave it to echo")
}

// TestFailedRemoteDefaultKeepsImplicitSubmitGated is the P1 fail-closed half of
// the submit gate (#4893 review): a ListPrograms answer that errored — or
// reported no default — must NOT free an implicit submit. "" still resolves
// something daemon-side, a program the field has never shown. The form instead
// asks for an explicit pick, which the still-open remote catalog can satisfy.
func TestFailedRemoteDefaultKeepsImplicitSubmitGated(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	stubRemoteProgramFeed(t, tmux.ProgramClaude)
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	_, cmd := h.startNewInstance()
	_ = drainCmd(t, cmd, 200*time.Millisecond)
	require.True(t, h.remoteDefaultPending, "the remote fetch armed the gate")
	require.NoError(t, h.namingInstance.SetTitle("demo"))

	// The lookup answered unusably: the gate flips to its failed form and an
	// implicit Enter is refused — no silent "" to an unknown resolver.
	_, _ = h.Update(remoteDefaultMsg{naming: h.namingInstance, err: errors.New("daemon unreachable")})
	require.False(t, h.remoteDefaultPending)
	require.True(t, h.remoteDefaultFailed)
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "a failed remote lookup must not enable a blind implicit submit")

	// An explicit pick is the way out: the chosen program submits concretely.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, stateSelectProgram, h.state)
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "demo", got.Title)
	assert.NotEmpty(t, got.Program, "the explicit pick goes out concretely")
}

// TestChosenProgramSubmitsWhileRemoteLookupPending scopes the gate to the case
// it protects (#4893 review): a form that already carries a concrete program —
// launch --program, a picker pick, or a chosen named account — sends that
// program, not "", so a slow remote ListPrograms must not hold it hostage.
func TestChosenProgramSubmitsWhileRemoteLookupPending(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		select {} // never answers: the gate's pending state
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	_, cmd := h.startNewInstance()
	_ = drainCmd(t, cmd, 200*time.Millisecond)
	require.True(t, h.remoteDefaultPending, "the remote fetch armed the gate")
	require.NoError(t, h.namingInstance.SetTitle("demo"))

	// Implicit is refused — the daemon's default is what "" would resolve to
	// and the field has not been corrected yet.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "an implicit remote submit still waits on the lookup")

	// A picker pick is a commitment the wire already honors concretely — the
	// pending lookup is irrelevant to it and must not block it.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, stateSelectProgram, h.state)
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "demo", got.Title)
	assert.NotEmpty(t, got.Program, "the chosen program submits without waiting on the remote answer")
}

// TestRefusedImplicitSubmitLeavesTheTitleUncommitted orders the gate before
// the title commit (#4893 review): a refused Enter must leave the form exactly
// as the user left it — including the shadow placeholder still uncommitted.
// Previously the shadow was SetTitle'd first, so a refused submit mutated the
// title field the user had never typed into.
func TestRefusedImplicitSubmitLeavesTheTitleUncommitted(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		select {} // pending forever
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	_, cmd := h.startNewInstance()
	_ = drainCmd(t, cmd, 200*time.Millisecond)
	require.True(t, h.remoteDefaultPending)
	require.Empty(t, h.namingInstance.Title, "the form shows the shadow, uncommitted")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "nothing submitted")
	assert.Empty(t, h.namingInstance.Title,
		"a refused submit must not commit the shadow placeholder as a real title")
}

// TestRemoteDefaultDismissesAnOpenProgramPicker covers the stale-overlay half
// of the correction (#4893 review): a program picker opened before the
// ListPrograms answer lands was built from the client-local catalog; leaving
// it up lets Enter commit a program the daemon may not support. The account
// picker already dismisses — the program picker must too.
func TestRemoteDefaultDismissesAnOpenProgramPicker(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	stubRemoteProgramFeed(t, tmux.ProgramClaude)
	h := configFollowingHome(t)

	_, cmd := h.startNewInstance()
	_ = drainCmd(t, cmd, 200*time.Millisecond)
	require.True(t, h.remoteDefaultPending)

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, stateSelectProgram, h.state)
	require.NotNil(t, h.selectionOverlay, "picker open on the local catalog")

	_, _ = h.Update(remoteDefaultMsg{
		naming:   h.namingInstance,
		program:  tmux.ProgramCodex,
		programs: []string{"claude", "codex", "newagent"},
	})
	require.Equal(t, stateNew, h.state, "the stale-catalog picker closes")
	assert.Nil(t, h.selectionOverlay)
	assert.Nil(t, h.programPickerChoices)
	assert.Equal(t, []string{"claude", "codex", "newagent"}, h.remoteProgramCatalog,
		"reopening the picker offers the daemon's catalog")
}

// TestRemoteOnlyProgramResolvesAccountsFromTheDaemonRoster covers the
// client-enum gap (#4893 review): a remote-only program — one this client's
// tmux.SupportedPrograms lacks — resolves to no agent locally. The account
// preselect and the picker must therefore ask the daemon: resolved_agents maps
// the program to its registry agent, Defaults to the account it applies.
func TestRemoteOnlyProgramResolvesAccountsFromTheDaemonRoster(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	h := configFollowingHome(t)
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		return daemon.ListProgramsResponse{}, nil
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		// "newagent" sits in the daemon's account roster under its own label.
		// resolved_agents says the runtime would launch codex — but account
		// validation uses the LABEL's registry (#4893 review): "newagent"'s
		// default is "work", not codex's "ops".
		return daemon.ListAccountsResponse{
			Agents:         []string{"codex", "newagent"},
			ResolvedAgents: map[string]string{"newagent": "codex"},
			Defaults:       map[string]string{"newagent": "work", "codex": "ops"},
		}, nil
	}))

	_, cmd := h.startNewInstance()
	_ = drainCmd(t, cmd, 200*time.Millisecond)

	// The remote default names a program the local enum cannot classify.
	_, refetch := h.Update(remoteDefaultMsg{
		naming:   h.namingInstance,
		program:  "newagent",
		programs: []string{"newagent"},
	})
	require.Equal(t, "newagent", h.pendingProgram)

	// The account fetch went out anyway — remote targets skip the local
	// classifier's skip-list — so deliver its answer.
	require.NotNil(t, refetch, "the correction refires the account fetch even for a remote-only program")
	for _, msg := range drainCmd(t, refetch, time.Second) {
		_, _ = h.Update(msg)
	}
	assert.Equal(t, "work", h.pendingAccount,
		"the label's own registry supplies the default — not the runtime agent's")
}

// TestRemoteChosenDraftRefreshesTheCatalog covers the restore half of the
// remote picker (#4893 review): a failed remote create whose draft carried a
// CHOSEN program re-arms with the program pinned — but the submit cleared
// remoteProgramCatalog, so without a fresh ListPrograms the program picker
// falls back to the client-local list and hides remote-only agents.
func TestRemoteChosenDraftRefreshesTheCatalog(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	programCalls := 0
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		programCalls++
		return daemon.ListProgramsResponse{Default: tmux.ProgramCodex, Programs: []daemon.ProgramOption{{Name: "newagent"}}}, nil
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
	h := configFollowingHome(t)

	inst := newLoadingInstance(t, "demo")
	inst.Path = h.repoRoot
	inst.Program = tmux.ProgramGemini
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)

	// The failed submit went out with a CHOSEN program — the restore keeps it
	// pinned, but the catalog still needs the daemon's answer.
	req := sessionStartRequest{Title: "demo", RepoPath: h.repoRoot, Program: tmux.ProgramGemini, ProgramChosen: true}
	_, cmd := h.Update(instanceStartedMsg{instance: inst, draft: &req, err: errors.New("The daemon refused this create.")})
	require.Equal(t, stateNew, h.state, "the draft re-armed the form")
	require.True(t, h.pendingProgramChosen, "the chosen program stays pinned")
	require.False(t, h.remoteDefaultPending, "no implicit wire value to gate — the program is concrete")

	require.NotNil(t, cmd)
	for _, msg := range drainCmd(t, cmd, time.Second) {
		_, _ = h.Update(msg)
	}
	assert.Equal(t, 1, programCalls, "the remote restore refires ListPrograms for the picker's catalog")
	assert.Equal(t, []string{"newagent"}, h.remoteProgramCatalog)
	assert.Equal(t, tmux.ProgramGemini, h.pendingProgram, "the reply cannot correct a chosen program")
}

// TestMovedLocalDefaultRequiresAReviewEnter is the local half of the implicit
// honesty contract (#4893 review): default_program can move after the form
// opens — a wire "" resolves the NEW value, so the submit refreshes the field
// and asks for one more Enter rather than launching an agent the form never
// showed.
func TestMovedLocalDefaultRequiresAReviewEnter(t *testing.T) {
	h := configFollowingHome(t)
	resolved := tmux.ProgramCodex
	prev := resolveConfiguredProgramContext
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		return resolved, nil
	}
	t.Cleanup(func() { resolveConfiguredProgramContext = prev })
	got := recordStartRequest(t)

	startNaming(t, h, "demo")
	h.namingInstance.Program = ""
	require.NoError(t, h.namingInstance.SetTitle("demo"))
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram, "seeded with the old default")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "the moved default must not submit past the user's eyes")
	assert.Equal(t, tmux.ProgramCodex, h.pendingProgram, "the field refreshes to what the wire will resolve")

	// The review Enter sees the refreshed value match the resolver — the
	// second press submits the same default the field now shows.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "demo", got.Title)
	assert.Equal(t, "", got.Program, "the submit stays implicit — now showing what it resolves to")
}

// TestFailedLocalRevalidateRefusesImplicitSubmit pins the #4893 review finding
// on the re-read failure path: an implicit submit that cannot re-read
// default_program has no way to know what the wire's "" resolves to, so it
// must refuse retryably — the earlier draft fell through and sent a blind ""
// anyway.
func TestFailedLocalRevalidateRefusesImplicitSubmit(t *testing.T) {
	h := configFollowingHome(t)
	prev := resolveConfiguredProgramContext
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		return "", errors.New("config read wedged")
	}
	t.Cleanup(func() { resolveConfiguredProgramContext = prev })
	got := recordStartRequest(t)

	startNaming(t, h, "demo")
	require.NoError(t, h.namingInstance.SetTitle("demo"))

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "an unverifiable display must not send a blind implicit program")
	require.Equal(t, stateNew, h.state, "the refusal leaves the form open")

	// Once the read recovers, the same Enter submits — the refusal is a hold,
	// not a dead end.
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		return tmux.ProgramClaude, nil
	}
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "demo", got.Title)
}

// TestMovedDefaultUpdatesTheFieldBeforePreflightRuns pins the ordering the
// #4893 review asked for: the live re-read must run BEFORE preflight. A
// default that moved to a program preflight rejects must still land on the
// field first — otherwise the refusal names a program the form never showed.
func TestMovedDefaultUpdatesTheFieldBeforePreflightRuns(t *testing.T) {
	h := configFollowingHome(t)
	prevRequired := localPrereqsRequired
	localPrereqsRequired = func(session.InstanceOptions, string) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { localPrereqsRequired = prevRequired })
	t.Cleanup(SetLocalSessionPreflightForTest(func(_ *config.Config, program string) error {
		if program == tmux.ProgramCodex {
			return errors.New("codex is not installed")
		}
		return nil
	}))
	// The real config carries the moved default — the re-read and preflight
	// resolve the same value, so the field the refusal names is the field the
	// form showed.
	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramCodex)
	require.NoError(t, err)
	got := recordStartRequest(t)

	startNaming(t, h, "demo")
	require.NoError(t, h.namingInstance.SetTitle("demo"))
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram, "seeded with the old default")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, tmux.ProgramCodex, h.pendingProgram,
		"the re-read runs before preflight — the field now shows the program any refusal will name")
	assert.Empty(t, got.Title, "the moved default holds the submit for review")

	// The review Enter now reaches preflight — which refuses the program the
	// form has been showing since the first press.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "preflight refuses the unrunnable program the field displayed")
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram)
}

// TestMovedLocalDefaultClearsAndRefetchesTheAccount pins the #4893 review
// pairing: a local default that moves agents must drop the old agent's
// account preselect and re-ask for the new one — leaving "work" on the field
// while the display says codex advertises a pair the wire cannot honor.
func TestMovedLocalDefaultClearsAndRefetchesTheAccount(t *testing.T) {
	h := configFollowingHome(t)
	defaults := twoAgentsWithAccounts()
	defaults.Defaults = map[string]string{"claude": "work", "codex": "ci"}
	calls, _ := stubAccounts(t, defaults, nil)
	prev := resolveConfiguredProgramContext
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		return tmux.ProgramCodex, nil
	}
	t.Cleanup(func() { resolveConfiguredProgramContext = prev })

	startNaming(t, h, "demo")
	require.NoError(t, h.namingInstance.SetTitle("demo"))
	h.pendingAccount = "work" // the claude preselect the form-open fetch applied
	require.False(t, h.pendingAccountChosen)

	msgs := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, tmux.ProgramCodex, h.pendingProgram, "the field refreshes to the moved default")
	assert.Empty(t, h.pendingAccount, "the old agent's preselect cannot pair with the new program")
	assert.False(t, h.pendingAccountChosen)
	assert.Equal(t, 1, *calls, "the correction re-asks for the new program's default")

	for _, msg := range msgs {
		if am, ok := msg.(accountDefaultMsg); ok {
			_, _ = h.handleAccountDefault(am)
		}
	}
	assert.Equal(t, "ci", h.pendingAccount, "the refetch preselects the LIVE program's default")
}
