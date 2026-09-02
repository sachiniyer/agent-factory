package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The account boundary holds against a REAL image that aliases it (#3598).
//
// Everything else about this defect can be tested with fixtures. This cannot:
// the whole point is that Docker and the kernel resolve a symlink af never sees,
// so the only way to know af's check reads what actually happened is to build
// the issue's image, run a real container from it, and ask.
//
// The image is the round-trip image plus one line — `ln -s /af-account
// /device-target` — so it is fully capable of carrying a session. That matters
// for what this test proves on MASTER: the create there does not fail for want
// of git or tmux, it SUCCEEDS, and the session comes up with the operator's
// account shadowed by repository content. Building a bare alpine instead would
// have made the create fail at the clone and let a "the create was refused"
// assertion pass for entirely the wrong reason.
//
// Runs on a host with a real docker daemon (`make backend-docker-roundtrip`, the
// same target as the other docker-backend proofs). It SKIPS inside the testbox
// fence, which has no docker socket.
func TestDockerBackendAccountMountAliasRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive real-backend E2E/integration; skipped under -short — see #2052")
	}
	requireDocker(t)
	requireTool(t, "git")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// The pane shim resolves an account NAME against this machine's AF home, and
	// main.go installs this hook for the real binary. Install it here for the same
	// reason: a nil lookup refuses an account-scoped launch outright, which would
	// make this test pass without ever reaching the boundary it is about.
	previousLookup := sessionenv.AccountLookup
	sessionenv.AccountLookup = func(agent, name string) (sessionenv.Account, error) {
		return agentaccount.Selected(home, agent, name, "")
	}
	t.Cleanup(func() { sessionenv.AccountLookup = previousLookup })

	// The operator's registered account, standing in for a real logged-in one.
	accountDir, err := agentaccount.Register(home, "codex", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(accountDir, ".config"), 0o700); err != nil {
		t.Fatalf("seed account config dir: %v", err)
	}
	const accountSettings = "LEGIT\n"
	writeFile(t, filepath.Join(accountDir, ".config", "settings.json"), accountSettings, 0600)

	// The repository's substitute for it.
	evil := t.TempDir()
	writeFile(t, filepath.Join(evil, "settings.json"), "ATTACKER\n", 0600)

	// The static `af` the runtime copies in. It is never reached once the check
	// lands — the refusal comes before copyAfBinary — but it is what lets this
	// test provision for real on MASTER, where the create succeeds and the
	// failure message above can say what actually happened.
	defer session.SetDockerSelfBinaryForTest(buildStaticBinary(t))()

	image := buildAccountAliasImage(t)

	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "account alias\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")

	// The reproduction, verbatim from the issue: a container path that is not a
	// protected one, which the image resolves onto the account's own config
	// directory. validateAccountDockerRunArgs returns nil for this.
	writeDockerRepoConfig(t, repo, image, []string{
		"-v", bare + ":/repo.git:ro",
		"-v", evil + ":/device-target/.config",
	})

	title := "docker-account-alias"
	slug := session.Slugify(title)
	t.Cleanup(func() { reapByLabel(slug) })

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repo,
		Program: "codex",
		Account: "work",
		Backend: session.BackendDocker,
	})
	if err == nil {
		// Master's behaviour: the session is UP, on a shadowed account. Prove it
		// in the failure message rather than only reporting "expected an error",
		// then tear it down.
		shadowed := accountFileInsideContainer(t, slug)
		_ = inst.Kill()
		t.Fatalf("an image aliasing the account mount must refuse the create, but the session started; "+
			"/af-account/.config/settings.json inside the container reads %q, and the operator's account holds %q",
			shadowed, accountSettings)
	}

	// The refusal names both halves the operator needs: where the alias landed,
	// and which run_args entry to remove.
	message := err.Error()
	for _, want := range []string{"/af-account", "/device-target/.config", "work"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal must name %q so the operator can act on it; got: %s", want, message)
		}
	}

	// The refusal TEARS THE CONTAINER DOWN. Leaving it running would keep the
	// account bind-mounted into the very image that was just refused.
	if got := containersForLabel(t, slug); len(got) != 0 {
		t.Errorf("a refused account session must leave no container behind; found %v", got)
	}

	// And the account directory on the host is exactly as it was seeded — no
	// planted node, no rewritten settings.
	assertAccountDirectoryUntouched(t, accountDir, accountSettings)
}

// buildAccountAliasImage is the round-trip image plus the issue's symlink: an
// image that can carry a session AND aliases the account boundary.
func buildAccountAliasImage(t *testing.T) string {
	t.Helper()
	const tag = "af-docker-account-alias:test"
	dir := t.TempDir()
	dockerfile := dockerRoundTripDockerfile(requireRoundTripBaseImage(t)) +
		"RUN mkdir -p /af-account && ln -s /af-account /device-target\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the aliasing image failed. Its base is already in the local image store and its FROM names no registry, so this is a real build error, not a registry outage: %v\n%s", err, out)
	}
	return tag
}

// accountFileInsideContainer reads the account's settings through the container's
// own view — which is what the alias replaces. Best-effort: it exists to make a
// master-branch failure self-explaining, never to gate anything.
func accountFileInsideContainer(t *testing.T, slug string) string {
	t.Helper()
	ids := containersForLabel(t, slug)
	if len(ids) == 0 {
		return "<no container>"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", "--user", "0:0", ids[0],
		"cat", "/af-account/.config/settings.json").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("<unreadable: %v: %s>", err, strings.TrimSpace(string(out)))
	}
	return string(out)
}

// assertAccountDirectoryUntouched proves the refusal cost the operator nothing:
// the credential directory holds exactly what it was seeded with, and no extra
// entry — a --device node planted through the bind mount would show up here.
func assertAccountDirectoryUntouched(t *testing.T, accountDir, wantSettings string) {
	t.Helper()
	var found []string
	err := filepath.Walk(accountDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, rerr := filepath.Rel(accountDir, path)
		if rerr != nil {
			return rerr
		}
		if relative == "." {
			return nil
		}
		found = append(found, fmt.Sprintf("%s (%s)", relative, info.Mode().Type()))
		return nil
	})
	if err != nil {
		t.Fatalf("walk the account directory: %v", err)
	}
	sort.Strings(found)
	want := []string{".config (d---------)", ".config/settings.json (----------)"}
	if strings.Join(found, ",") != strings.Join(want, ",") {
		t.Errorf("the refused session must leave the account directory exactly as it was; want %v, got %v", want, found)
	}
	body, err := os.ReadFile(filepath.Join(accountDir, ".config", "settings.json"))
	if err != nil {
		t.Fatalf("read the account settings: %v", err)
	}
	if string(body) != wantSettings {
		t.Errorf("the account's own settings must be untouched; want %q, got %q", wantSettings, string(body))
	}
}
