package ui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// The project half of the `,` editor's read (#7009). The pane asks for the
// active project's effective values and reports which scope it actually got —
// honored, degraded to global with the daemon's reason, or global because the
// daemon predates the field — so the header and the explain view never claim a
// scope the rows were not read in.

const remoteProjectRoot = "/srv/checkouts/app"

func remoteProjectRepo() *config.RepoContext {
	return &config.RepoContext{Root: remoteProjectRoot}
}

// requestRepoPath decodes the repo_path a stub route received, and whether the
// key was on the wire at all.
func requestRepoPath(t *testing.T, d *stubDaemon, route string) (string, bool) {
	t.Helper()
	body, called := d.body(route)
	if !called {
		t.Fatalf("%s was never called", route)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("%s received an undecodable body %q: %v", route, body, err)
	}
	v, present := fields["repo_path"]
	s, _ := v.(string)
	return s, present
}

func TestConfigEditorAsksTheTargetedDaemonForTheActiveProject(t *testing.T) {
	localPath, original := seedLocalConfig(t)
	d := serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/GetConfig": func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.GetConfigResponse{Entries: remoteManifest(), Path: remotePath, ProjectRoot: remoteProjectRoot})
		},
	})

	repo := remoteProjectRepo()
	_, location, scope, err := ReadConfigForEditor(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := requestRepoPath(t, d, "/v1/GetConfig"); got != remoteProjectRoot {
		t.Fatalf("GetConfig was asked for repo_path %q, want the active project %q", got, remoteProjectRoot)
	}
	if scope != repo {
		t.Errorf("an honored project read must report the project as its scope, got %+v", scope)
	}
	if want := d.url + " · " + remotePath + " · project " + remoteProjectRoot; location != want {
		t.Errorf("header location = %q, want %q", location, want)
	}
	assertLocalConfigUntouched(t, localPath, original)
}

func TestConfigEditorWithNoProjectSendsTodaysEmptyRequest(t *testing.T) {
	seedLocalConfig(t)
	d := serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/GetConfig": func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.GetConfigResponse{Entries: remoteManifest(), Path: remotePath})
		},
	})

	_, location, scope, err := ReadConfigForEditor(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := requestRepoPath(t, d, "/v1/GetConfig"); present {
		t.Error("with no active project the request must not carry repo_path")
	}
	if scope != nil {
		t.Errorf("a global read has no scope, got %+v", scope)
	}
	if want := d.url + " · " + remotePath; location != want {
		t.Errorf("header location = %q, want the unchanged %q", location, want)
	}
}

func TestConfigEditorShowsWhyTheDaemonFellBackToGlobalValues(t *testing.T) {
	seedLocalConfig(t)
	const reason = "not a git repository: " + remoteProjectRoot
	serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/GetConfig": func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.GetConfigResponse{Entries: remoteManifest(), Path: remotePath, ProjectError: reason})
		},
	})

	entries, location, scope, err := ReadConfigForEditor(remoteProjectRepo())
	if err != nil {
		t.Fatalf("a degraded project read still opens the editor with global rows: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the degraded read returned no rows")
	}
	if scope != nil {
		t.Errorf("a degraded read resolved nothing for the project, so its scope must be global; got %+v", scope)
	}
	if !strings.HasSuffix(location, " · "+reason) {
		t.Errorf("the header must carry the daemon's reason; got %q", location)
	}
}

func TestConfigEditorSaysWhenTheDaemonIsTooOldForProjectValues(t *testing.T) {
	seedLocalConfig(t)
	serveRemoteDaemon(t, "0.9.1", map[string]func([]byte) apiproto.Envelope{
		// An older daemon decodes repo_path into nothing and answers globally,
		// with neither project field set.
		"/v1/GetConfig": func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.GetConfigResponse{Entries: remoteManifest(), Path: remotePath})
		},
	})

	_, location, scope, err := ReadConfigForEditor(remoteProjectRepo())
	if err != nil {
		t.Fatal(err)
	}
	if scope != nil {
		t.Errorf("an unanswered project request must report global scope, got %+v", scope)
	}
	if want := "this daemon is too old to resolve project values — the rows are global"; !strings.Contains(location, want) {
		t.Errorf("the header must say the rows are global; got %q", location)
	}
}

func TestConfigEditorExplainAsksTheTargetedDaemonForTheProject(t *testing.T) {
	seedLocalConfig(t)
	d := serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/ExplainConfig": func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.ExplainConfigResponse{Explanation: *explainFixture(), Scope: "project"})
		},
	})

	if _, err := ExplainConfigForEditor("default_program", remoteProjectRepo()); err != nil {
		t.Fatal(err)
	}
	if got, _ := requestRepoPath(t, d, "/v1/ExplainConfig"); got != remoteProjectRoot {
		t.Fatalf("ExplainConfig was asked for repo_path %q, want %q", got, remoteProjectRoot)
	}

	if _, err := ExplainConfigForEditor("default_program", nil); err != nil {
		t.Fatal(err)
	}
	if _, present := requestRepoPath(t, d, "/v1/ExplainConfig"); present {
		t.Error("a global explain must not carry repo_path")
	}
}

// localScopeRepo makes a real git checkout carrying a checked-in config, so
// the local read resolves exactly as `af config get --repo` would.
func localScopeRepo(t *testing.T, inRepo string) *config.RepoContext {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	dir := filepath.Join(root, config.InRepoConfigDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.TomlConfigFileName), []byte(inRepo), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := config.RepoFromPath(root)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestConfigEditorLocalReadShowsTheProjectsEffectiveValue(t *testing.T) {
	localTarget(t)
	localPath, _ := seedLocalConfig(t)
	repo := localScopeRepo(t, "default_program = \"aider\"\n")

	entries, location, scope, err := ReadConfigForEditor(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryValue(t, entries, "default_program"); got != "aider" {
		t.Errorf("default_program = %q, want the project's effective %q", got, "aider")
	}
	if scope != repo {
		t.Errorf("an honored local read must report the project as its scope, got %+v", scope)
	}
	if want := localPath + " · project " + repo.Root; location != want {
		t.Errorf("header location = %q, want %q", location, want)
	}
}

func TestConfigEditorLocalReadDegradesToGlobalWithTheReason(t *testing.T) {
	localTarget(t)
	localPath, _ := seedLocalConfig(t)
	repo := localScopeRepo(t, "this is = = not toml\n")

	entries, location, scope, err := ReadConfigForEditor(repo)
	if err != nil {
		t.Fatalf("a project that will not resolve must still open the global editor: %v", err)
	}
	if got := entryValue(t, entries, "default_program"); got != localValue {
		t.Errorf("default_program = %q, want the global %q the degraded read falls back to", got, localValue)
	}
	if scope != nil {
		t.Errorf("a degraded read must report global scope, got %+v", scope)
	}
	if want := localPath + " · af could not read the project config for " + repo.Root; !strings.HasPrefix(location, want) {
		t.Errorf("header location = %q, want it to start %q", location, want)
	}
}
