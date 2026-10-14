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

// TestRemoteUntouchedSubmitSendsTheLaunchDefault: for a remote daemon an
// untouched field still submits the launch-time default explicitly — master's
// wire behaviour — since the client's "follow config" flag speaks for a
// resolution only the remote daemon could produce.
func TestRemoteUntouchedSubmitSendsTheLaunchDefault(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)

	h := activeProjectHome(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	h.appConfig = cfg
	h.programChoice = newProgramChoice("", cfg.DefaultProgram, cfg)
	got := recordStartRequest(t)

	_, _ = h.startNewInstance()
	naming := h.namingInstance
	require.NoError(t, naming.SetTitle("demo"))

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, "demo", got.Title)
	assert.Equal(t, tmux.ProgramClaude, got.Program,
		"a remote target's untouched submit sends the launch-time default explicitly")
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

// TestTaskSaveRefreshesTheProgramCache: task save is the second place the
// default turns into stored intent, so it runs the same synchronous re-read —
// a `default_program` change after launch must reach the cache that labels the
// form's "Use config default" choice (#4889 review).
func TestTaskSaveRefreshesTheProgramCache(t *testing.T) {
	h := configFollowingHome(t)
	t.Chdir(h.repoRoot)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	_, err = config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)

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

	require.Equal(t, tmux.ProgramAider, h.program,
		"the save's synchronous re-read refreshed the cache the label reads")
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

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, tmux.ProgramAider, checked,
		"preflight validates the program the daemon will resolve, not the stale seed")
	assert.Empty(t, got.Program, "the create itself still goes out implicit")
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
