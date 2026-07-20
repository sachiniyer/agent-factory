package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// repoRoot resolves the checkout this test file belongs to, so the drift test
// can compare the generator's output against the committed artifacts.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repo root")
	}
	return filepath.Dir(filepath.Dir(file)) // .../commands/x_test.go -> .../commands -> repo root
}

// TestGeneratedPluginsAreCommitted is the drift gate, run as a normal test so a
// stale artifact fails `go test ./commands/...` and not only CI. It mirrors the
// docs gate in .github/workflows/docs.yml: regenerate, and require the result to
// equal what is committed — byte for byte, with no extra files left over.
func TestGeneratedPluginsAreCommitted(t *testing.T) {
	root := repoRoot(t)
	files := generatedPluginFiles()
	if len(files) == 0 {
		t.Fatal("the generator produced no files")
	}

	expected := make(map[string]bool, len(files))
	for _, f := range files {
		expected[f.path] = true
		committed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.path)))
		if err != nil {
			t.Errorf("%s is generated but not committed (run scripts/gen-docs.sh): %v", f.path, err)
			continue
		}
		if string(committed) != f.content {
			t.Errorf("%s is stale — run scripts/gen-docs.sh and commit the result", f.path)
		}
	}

	// A file that is committed but no longer generated is drift in the other
	// direction: it would keep serving content nothing regenerates.
	pluginsRoot := filepath.Join(root, pluginsDir)
	err := filepath.Walk(pluginsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if !expected[filepath.ToSlash(rel)] {
			t.Errorf("%s is committed under %s/ but the generator no longer emits it", rel, pluginsDir)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", pluginsRoot, err)
	}
}

// TestGeneratedSkillsCarryTheCanonicalUsageText locks the single-source
// property: every agent's skill is the shared af usage reference verbatim, so
// editing session/systemprompt.go changes all of them and none of them can be
// edited on its own.
func TestGeneratedSkillsCarryTheCanonicalUsageText(t *testing.T) {
	skills := 0
	for _, f := range generatedPluginFiles() {
		if !strings.HasSuffix(f.path, "SKILL.md") {
			continue
		}
		skills++
		if !strings.Contains(f.content, session.AfPluginUsageReference) {
			t.Errorf("%s does not carry session.AfPluginUsageReference verbatim", f.path)
		}
		if !strings.Contains(f.content, "name: "+session.AfSkillName) {
			t.Errorf("%s is missing the skill name frontmatter", f.path)
		}
		if !strings.Contains(f.content, "description: "+session.AfSkillDescription) {
			t.Errorf("%s is missing the shared skill description", f.path)
		}
	}
	if skills != len(pluginAgents) {
		t.Errorf("got %d generated SKILL.md files, want one per agent (%d)", skills, len(pluginAgents))
	}
}

// TestEveryAgentEmitsFiles guards the agent table itself: an entry that emits
// nothing is a half-added agent that the README would still advertise.
func TestEveryAgentEmitsFiles(t *testing.T) {
	for _, a := range pluginAgents {
		if len(a.files()) == 0 {
			t.Errorf("agent %q emits no files", a.name)
		}
		if a.packaging == "" {
			t.Errorf("agent %q has no packaging line for the README", a.name)
		}
	}
}

// TestGeneratedSkillIsReframed is the framing shim's lock. The canonical text
// opens "You are running inside Agent Factory (af)", which is FALSE for someone
// who installed this plugin into an agent af never launched — the generated
// artifacts must carry the plugin framing, never a verbatim copy of the inside
// framing (#2172).
func TestGeneratedSkillIsReframed(t *testing.T) {
	const insideFraming = "You are running inside Agent Factory"
	for _, f := range generatedPluginFiles() {
		if strings.Contains(f.content, insideFraming) {
			t.Errorf("%s contains the inside-af framing %q; the generator must reframe for plugin users",
				f.path, insideFraming)
		}
	}
	// The reframing has to actually say the two things the inside framing got
	// wrong, or it is just a deletion.
	skill := afSkillMarkdown()
	for _, want := range []string{
		"You are NOT running inside af",
		"this plugin does not install",
		session.AfInstallCommand,
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the generated skill is missing the plugin framing %q", want)
		}
	}
}

// TestSkillReframingKeepsTheBody proves the two framings are two renderings of
// ONE body rather than two texts: a command reference present for af's own
// agents is present for plugin users too.
func TestSkillReframingKeepsTheBody(t *testing.T) {
	skill := afSkillMarkdown()
	for _, want := range []string{
		"af sessions create --name <title>",
		"af tasks add --name <n> --prompt <p> --cron",
		"af sessions tab-create <title>",
		`Never run "af reset"`,
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the generated skill dropped %q from the shared usage body", want)
		}
	}
}

// TestGeneratedJSONIsWellFormed catches a manifest broken by a stray quote in a
// description before a user's plugin install does.
func TestGeneratedJSONIsWellFormed(t *testing.T) {
	seen := 0
	for _, f := range generatedPluginFiles() {
		if !strings.HasSuffix(f.path, ".json") {
			continue
		}
		seen++
		var v any
		if err := json.Unmarshal([]byte(f.content), &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", f.path, err)
		}
	}
	if seen == 0 {
		t.Fatal("no JSON manifests were generated")
	}
}

// TestMarketplacesPointAtGeneratedPlugins keeps each catalog honest: the path a
// marketplace advertises must be a plugin root the generator actually emits,
// spelled the way that agent's loader requires ("./"-relative, inside the repo).
func TestMarketplacesPointAtGeneratedPlugins(t *testing.T) {
	files := generatedPluginFiles()
	has := func(path string) bool {
		for _, f := range files {
			if f.path == path {
				return true
			}
		}
		return false
	}

	var codex codexMarketplace
	mustUnmarshalGenerated(t, files, ".agents/plugins/marketplace.json", &codex)
	if len(codex.Plugins) != 1 {
		t.Fatalf("codex marketplace lists %d plugins, want 1", len(codex.Plugins))
	}
	codexPath := codex.Plugins[0].Source.Path
	if !strings.HasPrefix(codexPath, "./") {
		t.Errorf("codex plugin source path %q must start with \"./\"", codexPath)
	}
	if manifest := strings.TrimPrefix(codexPath, "./") + "/.codex-plugin/plugin.json"; !has(manifest) {
		t.Errorf("codex marketplace points at %q, but %q is not generated", codexPath, manifest)
	}

	var claude claudeMarketplace
	mustUnmarshalGenerated(t, files, ".claude-plugin/marketplace.json", &claude)
	if len(claude.Plugins) != 1 {
		t.Fatalf("claude marketplace lists %d plugins, want 1", len(claude.Plugins))
	}
	claudePath := claude.Plugins[0].Source
	if !strings.HasPrefix(claudePath, "./") {
		t.Errorf("claude plugin source %q must start with \"./\"", claudePath)
	}
	if manifest := strings.TrimPrefix(claudePath, "./") + "/.claude-plugin/plugin.json"; !has(manifest) {
		t.Errorf("claude marketplace points at %q, but %q is not generated", claudePath, manifest)
	}
}

func mustUnmarshalGenerated(t *testing.T, files []pluginFile, path string, out any) {
	t.Helper()
	for _, f := range files {
		if f.path == path {
			if err := json.Unmarshal([]byte(f.content), out); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			return
		}
	}
	t.Fatalf("%s was not generated", path)
}

// TestPreflightHookNeverInstalls is the #2172/#2174 rule as a test: the hook
// runs as the user, af's releases carry no checksum to verify, so the hook may
// PRINT the install command and must never run it. Every mention of the
// installer therefore has to sit inside an echo or a comment.
func TestPreflightHookNeverInstalls(t *testing.T) {
	hook := afPreflightHook()
	if !strings.Contains(hook, session.AfInstallCommand) {
		t.Error("the preflight hook does not print the canonical install command")
	}
	for _, line := range strings.Split(hook, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "curl") && !strings.Contains(trimmed, "install.sh") {
			continue
		}
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "echo ") {
			continue
		}
		t.Errorf("the preflight hook must never execute the installer, found: %q", trimmed)
	}
}

// TestWriteAgentPluginsPrunesStaleArtifacts locks the property that makes the
// committed tree pure output: a file an earlier revision of the table emitted
// is removed, not orphaned.
func TestWriteAgentPluginsPrunesStaleArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(root, pluginsDir, "retired-agent", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("left over\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeAgentPlugins(root); err != nil {
		t.Fatalf("writeAgentPlugins: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale artifact %s survived regeneration (err=%v)", stale, err)
	}
	if _, err := os.Stat(filepath.Join(root, pluginsDir, "retired-agent")); !os.IsNotExist(err) {
		t.Error("the emptied directory of a retired agent was not removed")
	}

	// The hook has to stay executable through a regenerate, or Codex's
	// `bash "${PLUGIN_ROOT}/hooks/af-preflight.sh"` is the only thing keeping
	// it runnable.
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(codexPluginRoot), "hooks", "af-preflight.sh"))
	if err != nil {
		t.Fatalf("preflight hook not written: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("preflight hook is not executable (mode %v)", info.Mode().Perm())
	}
}

// TestWriteAgentPluginsRefusesNonCheckout keeps the pruner pointed at an af
// checkout. It deletes files under <root>/plugins, so a mistyped --plugin-root
// must fail before anything is removed rather than after.
func TestWriteAgentPluginsRefusesNonCheckout(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, pluginsDir, "someone-elses-file")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("not ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeAgentPlugins(root); err == nil {
		t.Fatal("writeAgentPlugins accepted a directory with no go.mod")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a refused run still touched %s: %v", victim, err)
	}
}

// TestGeneratedFilesAreDeterministic guards the drift gate's premise: two runs
// of an unchanged tree must be byte-identical, or CI would fail at random.
func TestGeneratedFilesAreDeterministic(t *testing.T) {
	first, second := generatedPluginFiles(), generatedPluginFiles()
	if len(first) != len(second) {
		t.Fatalf("file count changed between runs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].path != second[i].path || first[i].content != second[i].content {
			t.Errorf("%s is not deterministic across runs", first[i].path)
		}
	}
}
