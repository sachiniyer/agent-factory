package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests cover the #2216 Phase 6 PR2 daemon integration: the canonical
// [root_agent] singleton (global + personal-project) resolved through
// config.ResolveRootAgent, alongside the legacy root_agents map. They are
// hermetic on the same rules as rootagent_test.go — temp AGENT_FACTORY_HOME,
// the in-process fake backend, no real daemon.

// registerTestProject registers repoPath as a durable project and returns it.
func registerTestProject(t *testing.T, repoPath string) config.Project {
	t.Helper()
	p, err := config.RegisterProject(repoPath)
	if err != nil {
		t.Fatalf("RegisterProject(%s): %v", repoPath, err)
	}
	return p
}

// writePersonalRootAgent writes a registered project's personal config.toml with
// a [root_agent] table whose body is the given TOML (e.g. `enabled = true`). It
// bypasses the write path so resolver/daemon behavior is exercised directly.
func writePersonalRootAgent(t *testing.T, projectID, body string) {
	t.Helper()
	path, err := config.ProjectConfigTomlPath(projectID)
	if err != nil {
		t.Fatalf("ProjectConfigTomlPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir personal config dir: %v", err)
	}
	content := "[root_agent]\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write personal config: %v", err)
	}
}

// loadGlobalConfigWithRootAgent writes a global config.toml carrying a
// [root_agent] table and loads it through config.LoadConfig, so the loaded
// Config's source shape records that `enabled` was set — the presence the global
// layer extractor reads. AGENT_FACTORY_HOME must already point at a temp home.
func loadGlobalConfigWithRootAgent(t *testing.T, body string) *config.Config {
	t.Helper()
	dir, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir AF home: %v", err)
	}
	content := "[root_agent]\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, config.TomlConfigFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// repoID resolves repoPath to its daemon repo ID.
func repoID(t *testing.T, repoPath string) string {
	t.Helper()
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath(%s): %v", repoPath, err)
	}
	return repo.ID
}

// TestEnsureRootAgentsSingletonOnlyProjectMaterializes: a registered project
// enabled purely by its personal [root_agent] — with NO legacy root_agents entry
// — is now visited by the ensure loop (the old early-return on an empty map
// skipped it entirely), gets a root created, and its resolved program flows
// through losslessly. This is the whole point of PR2's enumeration rewrite.
func TestEnsureRootAgentsSingletonOnlyProjectMaterializes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	// A custom program doubles as the lossless-forward check: the resolved
	// profile's program must reach CreateSession verbatim.
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/claude --model opus\"")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("a singleton-only project must materialize a root, got %d creates", len(*seen))
	}
	opts := (*seen)[0]
	if opts.Title != session.RootSessionTitle || !opts.InPlace {
		t.Fatalf("root must be the reserved title created in place, got title=%q inPlace=%v", opts.Title, opts.InPlace)
	}
	if opts.Program != "/opt/claude --model opus" {
		t.Fatalf("the resolved personal program must reach CreateSession verbatim, got %q", opts.Program)
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatalf("root instance not registered for the singleton-only project")
	}
	if !manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
		t.Fatalf("repoRootAgentWillMaterialize must be true for a singleton-only project the loop creates — else a send-prompt to it is wrongly rejected (#1835)")
	}
}

// TestEnsureRootAgentsGlobalEnablesRegisteredProject: a global [root_agent]
// enabled=true fans out to a registered project with no legacy entry and no
// personal override — the global default's registered-project-only reach.
func TestEnsureRootAgentsGlobalEnablesRegisteredProject(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	cfg := loadGlobalConfigWithRootAgent(t, "enabled = true")

	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("a global enabled=true must fan out to the registered project, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatalf("root instance not registered for the globally-enabled project")
	}
	if !manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
		t.Fatalf("repoRootAgentWillMaterialize must be true for a globally-enabled registered project")
	}
}

// TestEnsureRootAgentsPersonalDisablesLegacyRoot is THE case #2216 exists for: a
// registered project carries a legacy root_agents entry (which alone means
// enabled=true, program unset — the empty entry af writes for every registered
// project) AND a personal enabled=false. The personal disable must win, so NO
// root is created and repoRootAgentWillMaterialize reports false. With legacy
// outranking personal this would be an unbreakable always-on root — the silent
// no-op this epic kills.
func TestEnsureRootAgentsPersonalDisablesLegacyRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")

	// Legacy entry present for the same repo (the ubiquitous empty entry).
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("a personal enabled=false must disable a legacy-enabled root, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) != nil {
		t.Fatalf("no root instance may exist for a personal-disabled project")
	}
	if manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
		t.Fatalf("repoRootAgentWillMaterialize must be false for a personal-disabled root — a send-prompt must not wait for a root that will never come")
	}
}

// makePersonalRootAgentUnreadable revokes read permission on a registered
// project's personal config file, so LoadProjectConfig fails with EACCES
// instead of reporting an absent layer. It skips — never silently passes —
// where the fixture cannot exist: under root, and on mounts that do not
// enforce permission bits, both proven by an actual probe read after the
// chmod (the worktree_copy_link_unreadable_test.go idiom). Without the probe
// the fixture would be a readable enabled=false, which produces the same
// assertions' outcome through the ordinary personal-disable path and turns
// the regression test into a silent no-op.
func makePersonalRootAgentUnreadable(t *testing.T, projectID string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0o000 does not make a file unreadable")
	}
	path, err := config.ProjectConfigTomlPath(projectID)
	if err != nil {
		t.Fatalf("ProjectConfigTomlPath: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod personal config unreadable: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(path, 0o644)
	})
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("environment does not enforce file permission bits; the unreadable fixture cannot exist here")
	}
}

// breakPersonalRootAgentToml overwrites a registered project's personal config
// with syntactically invalid TOML, so LoadProjectConfig fails at parse — the
// portable member of the #3241 failure class: no chmod, no skips, it runs on
// every platform and runner.
func breakPersonalRootAgentToml(t *testing.T, projectID string) {
	t.Helper()
	path, err := config.ProjectConfigTomlPath(projectID)
	if err != nil {
		t.Fatalf("ProjectConfigTomlPath: %v", err)
	}
	if err := os.WriteFile(path, []byte("[root_agent]\nenabled = tru\n"), 0o644); err != nil {
		t.Fatalf("write malformed personal config: %v", err)
	}
}

// TestEnsureRootAgentsUnloadablePersonalConfigFailsClosed is the #3241
// regression, across the LoadProjectConfig failure class and both
// lower-precedence enables. In every case the personal config held
// `enabled = false` and then became unloadable — unreadable permissions, or
// TOML that no longer parses — and the daemon must fail CLOSED: collapsing the
// failed load into "no personal layer" let the lower enable start a root the
// user deliberately disabled. The decision is unknown, and unknown is not
// permission. (Commit 1 of this PR carried the unreadable cases standalone;
// CI showed them red against the unfixed code with `got 1 creates`.)
func TestEnsureRootAgentsUnloadablePersonalConfigFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		// corrupt makes the personal config unloadable after the disable was
		// written (it may skip where its fixture cannot exist).
		corrupt func(t *testing.T, projectID string)
		// managerConfig supplies the lower-precedence enable that must not win.
		managerConfig func(t *testing.T, repoPath string) *config.Config
	}{
		{
			name:    "unreadable file under a legacy enable",
			corrupt: makePersonalRootAgentUnreadable,
			managerConfig: func(t *testing.T, repoPath string) *config.Config {
				return rootTestConfig(repoPath, config.RootAgentConfig{})
			},
		},
		{
			name:    "unreadable file under a global enable",
			corrupt: makePersonalRootAgentUnreadable,
			managerConfig: func(t *testing.T, repoPath string) *config.Config {
				return loadGlobalConfigWithRootAgent(t, "enabled = true")
			},
		},
		{
			name:    "unparseable TOML under a legacy enable",
			corrupt: breakPersonalRootAgentToml,
			managerConfig: func(t *testing.T, repoPath string) *config.Config {
				return rootTestConfig(repoPath, config.RootAgentConfig{})
			},
		},
		{
			// The portable case for the SINGLETON sweep: on root runners both
			// chmod cases skip, and without this row the
			// ensureSingletonRootAgent arm of the gate would go untested there.
			name:    "unparseable TOML under a global enable",
			corrupt: breakPersonalRootAgentToml,
			managerConfig: func(t *testing.T, repoPath string) *config.Config {
				return loadGlobalConfigWithRootAgent(t, "enabled = true")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			seen := installOptionsRecordingBackend(t)
			repoPath := setupControlRepo(t)
			project := registerTestProject(t, repoPath)
			writePersonalRootAgent(t, project.ID, "enabled = false")
			tc.corrupt(t, project.ID)

			// The fail-closed WARNING fires from the snapshot inside NewManager,
			// so the capture goes in first (httpserver_test.go idiom).
			var warnings bytes.Buffer
			prevWarning := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warnings)
			t.Cleanup(func() { log.WarningLog.SetOutput(prevWarning) })

			manager, err := NewManager(tc.managerConfig(t, repoPath))
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			manager.EnsureRootAgents()

			if len(*seen) != 0 {
				t.Fatalf("an unloadable personal config must fail closed, got %d creates — a failed load is not an absent enabled=false", len(*seen))
			}
			if findRootInstance(t, manager, repoPath) != nil {
				t.Fatalf("no root instance may exist while the personal config is unloadable")
			}
			if manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
				t.Fatalf("repoRootAgentWillMaterialize must be false while the personal config is unloadable — a delivery must not wait for a root the ensure loop will not create")
			}
			if got := warnings.String(); !strings.Contains(got, "failing closed") || !strings.Contains(got, project.ID) {
				t.Fatalf("the snapshot must warn that project %s fails closed; warnings were:\n%s", project.ID, got)
			}
		})
	}
}

// corruptProjectRegistry makes config.ListProjects fail on every platform and
// runner: loadProjectRecords rejects a stray non-directory entry in the
// registry directory, no permission bits involved. The probe call proves the
// fixture actually fails — a registry that still lists would route every
// assertion below through the ordinary resolution path and turn the
// regression test into a silent no-op.
func corruptProjectRegistry(t *testing.T) {
	t.Helper()
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir: %v", err)
	}
	dir := filepath.Join(home, config.ProjectRegistryDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir project registry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("not a project record"), 0o644); err != nil {
		t.Fatalf("write stray registry file: %v", err)
	}
	if _, err := config.ListProjects(); err == nil {
		t.Fatalf("fixture failed: ListProjects still succeeds on a corrupt registry")
	}
}

// TestEnsureRootAgentsUnlistableRegistryFailsClosed is the #3247 ListProjects
// arm. The registry is the only index of the personal configs that hold the
// highest-precedence enabled=false, so when it cannot be listed at daemon
// start NO repo can be proven un-disabled and no root agent may start —
// legacy-only entries included. The unfixed early return dropped only the
// singleton layers: fail-closed for singleton-enabled roots, fail-open for
// singleton-DISABLED ones, because the legacy sweep kept ensuring with no
// personal layer at all.
func TestEnsureRootAgentsUnlistableRegistryFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		// setup lays down project state before the registry is corrupted and
		// returns the manager config carrying the legacy enable.
		setup func(t *testing.T, repoPath string) *config.Config
	}{
		{
			// The headline regression: the personal disable sits readable on
			// disk; only the registry that would have NAMED it does not list.
			name: "registered personal disable under a legacy enable",
			setup: func(t *testing.T, repoPath string) *config.Config {
				project := registerTestProject(t, repoPath)
				writePersonalRootAgent(t, project.ID, "enabled = false")
				return rootTestConfig(repoPath, config.RootAgentConfig{})
			},
		},
		{
			// The blast-radius case the issue scopes deliberately: a legacy-only
			// root with no registration is suppressed too, because an unlistable
			// registry means "there may be a project record with a disable we
			// cannot see" — for every repo.
			name: "legacy-only entry",
			setup: func(t *testing.T, repoPath string) *config.Config {
				return rootTestConfig(repoPath, config.RootAgentConfig{})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			seen := installOptionsRecordingBackend(t)
			repoPath := setupControlRepo(t)
			cfg := tc.setup(t, repoPath)
			corruptProjectRegistry(t)

			// The fail-closed ERROR fires from the snapshot inside NewManager,
			// so the capture goes in first (httpserver_test.go idiom).
			var errorLog bytes.Buffer
			prevError := log.ErrorLog.Writer()
			log.ErrorLog.SetOutput(&errorLog)
			t.Cleanup(func() { log.ErrorLog.SetOutput(prevError) })

			manager, err := NewManager(cfg)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			manager.EnsureRootAgents()

			if len(*seen) != 0 {
				t.Fatalf("an unlistable project registry must fail every root agent closed, got %d creates — the registry is the only index of personal disables", len(*seen))
			}
			if findRootInstance(t, manager, repoPath) != nil {
				t.Fatalf("no root instance may exist while the project registry is unlistable")
			}
			if manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
				t.Fatalf("repoRootAgentWillMaterialize must be false while the project registry is unlistable — a delivery must not wait for a root the ensure loop will not create")
			}
			if got := errorLog.String(); !strings.Contains(got, config.ProjectRegistryDirName) || !strings.Contains(got, "failing closed") {
				t.Fatalf("the snapshot must log an ERROR naming the registry and the fail-closed consequence; errors were:\n%s", got)
			}
		})
	}
}

// TestEnsureRootAgentsUnresolvableProjectRootKeepsPersonalLayer is the #3247
// RepoFromPath arm: a registered project whose recorded root does not resolve
// at daemon start (an absent mount, a checkout deleted and restored later)
// must keep its personal [root_agent] layer, attributed by the recorded path —
// whose hash IS the repo ID a checkout resolving at that path gets. The
// unfixed code skipped the project wholesale, so the moment the path returned
// the legacy sweep's per-tick retry (#1122) ensured the root with
// Personal=nil, bypassing an enabled=false that sat readable in the AF home
// the whole time.
func TestEnsureRootAgentsUnresolvableProjectRootKeepsPersonalLayer(t *testing.T) {
	cases := []struct {
		name string
		// personal is the [root_agent] body written before the root vanishes;
		// corrupt optionally makes the file unloadable afterwards.
		personal    string
		corrupt     func(t *testing.T, projectID string)
		wantCreates int
		wantProgram string
	}{
		{
			// The headline regression: a readable personal disable must win even
			// though the repo was unresolvable when the daemon snapshotted.
			name:        "readable personal disable",
			personal:    "enabled = false",
			wantCreates: 0,
		},
		{
			// Fail closed composes (#3241): unloadable personal AND unresolvable
			// root is still "decision unknown", keyed by the recorded path.
			name:        "unloadable personal config",
			personal:    "enabled = false",
			corrupt:     breakPersonalRootAgentToml,
			wantCreates: 0,
		},
		{
			// Attribution is a true resolution, not a blanket refusal: a personal
			// enable's program must reach the create verbatim. The unfixed code
			// also creates here — but with the default profile, proving the
			// personal layer was dropped rather than merged.
			name:        "readable personal program",
			personal:    "enabled = true\nprogram = \"/opt/claude --model opus\"",
			wantCreates: 1,
			wantProgram: "/opt/claude --model opus",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			seen := installOptionsRecordingBackend(t)
			repoPath := setupControlRepo(t)
			project := registerTestProject(t, repoPath)
			writePersonalRootAgent(t, project.ID, tc.personal)
			if tc.corrupt != nil {
				tc.corrupt(t, project.ID)
			}

			// The recorded root must NOT resolve while NewManager snapshots …
			hidden := repoPath + ".hidden"
			if err := os.Rename(repoPath, hidden); err != nil {
				t.Fatalf("hide repo dir: %v", err)
			}
			manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			// … and must resolve again by the time the ensure loop ticks: the
			// mount is back, and the legacy retry sees the repo for the first time.
			if err := os.Rename(hidden, repoPath); err != nil {
				t.Fatalf("restore repo dir: %v", err)
			}
			manager.EnsureRootAgents()

			if len(*seen) != tc.wantCreates {
				t.Fatalf("want %d creates, got %d — a project unresolvable at snapshot time must keep its personal layer by recorded path", tc.wantCreates, len(*seen))
			}
			if tc.wantProgram != "" {
				if len(*seen) == 0 {
					t.Fatalf("test row error: wantProgram is set but the row expects no create, so the program could never be asserted")
				}
				if (*seen)[0].Program != tc.wantProgram {
					t.Fatalf("the personal program must reach CreateSession verbatim, got %q", (*seen)[0].Program)
				}
			}
			if want := tc.wantCreates == 1; manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) != want {
				t.Fatalf("repoRootAgentWillMaterialize must be %v here, agreeing with the ensure loop", want)
			}
		})
	}
}

// TestEnsureRootAgentsLegacyOnlyUnchangedThroughResolver: a plain legacy entry
// with no project registration and no global/personal layer still ensures a
// root, exactly as before PR2 — proving the resolver rewrite preserved the
// legacy contract end to end (create AND willMaterialize).
func TestEnsureRootAgentsLegacyOnlyUnchangedThroughResolver(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("a legacy entry must still ensure a root through the resolver, got %d creates", len(*seen))
	}
	if !manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
		t.Fatalf("repoRootAgentWillMaterialize must be true for a legacy-enabled root")
	}
}

// TestRootAgentEnsureAndWillMaterializeAgree drives the two call sites across
// every layer combination in ONE manager and asserts they never disagree: a
// root is created exactly when repoRootAgentWillMaterialize is true. Divergence
// is the specific trap this PR guards — a root created but not marked
// materializing (or vice versa) rejects a send-prompt at the reserved-name gate.
func TestRootAgentEnsureAndWillMaterializeAgree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)

	// Four repos exercising each path. legacyOnly is not registered as a project;
	// the rest are registered so they carry personal layers.
	legacyOnly := setupControlRepo(t)
	personalOn := setupControlRepo(t)
	disabledLegacy := setupControlRepo(t)
	registeredIdle := setupControlRepo(t)

	pOn := registerTestProject(t, personalOn)
	pDisabled := registerTestProject(t, disabledLegacy)
	registerTestProject(t, registeredIdle) // registered, no override, no legacy → idle

	writePersonalRootAgent(t, pOn.ID, "enabled = true")
	writePersonalRootAgent(t, pDisabled.ID, "enabled = false")

	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{
		legacyOnly:     {},
		disabledLegacy: {}, // legacy-enabled, but personal disables it
	}

	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	cases := []struct {
		name        string
		repoPath    string
		wantEnabled bool
	}{
		{"legacy only", legacyOnly, true},
		{"personal enabled, no legacy", personalOn, true},
		{"legacy + personal disabled", disabledLegacy, false},
		{"registered but unconfigured", registeredIdle, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := findRootInstance(t, manager, tc.repoPath) != nil
			willMaterialize := manager.repoRootAgentWillMaterialize(repoID(t, tc.repoPath))
			if created != willMaterialize {
				t.Fatalf("ensure loop and repoRootAgentWillMaterialize disagree: created=%v willMaterialize=%v", created, willMaterialize)
			}
			if created != tc.wantEnabled {
				t.Fatalf("want root enabled=%v, got created=%v", tc.wantEnabled, created)
			}
		})
	}
}
