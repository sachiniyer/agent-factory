package agentaccount

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// geminiAccountFile reads one of the settings documents af writes into a gemini
// account's own credential home.
func geminiAccountFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".gemini", name))
	require.NoError(t, err)
	return string(data)
}

// TestRegisterAnswersGeminisLoginPromptsInTheAccountsOwnHome pins the BYTES,
// not the behaviour, because the bytes are the interface: gemini fatals on a
// trustedFolders.json whose values are outside its enum, and ignores a
// selectedType it does not recognise. A test that only asserted "some JSON is
// there" would pass on a document the CLI rejects.
func TestRegisterAnswersGeminisLoginPromptsInTheAccountsOwnHome(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)

	require.JSONEq(t,
		`{"security":{"auth":{"selectedType":"oauth-personal"}}}`,
		geminiAccountFile(t, dir, "settings.json"),
		"the auth picker's answer must be the Google login, spelled the way gemini spells it")
	require.JSONEq(t,
		`{`+jsonString(dir)+`:"TRUST_FOLDER"}`,
		geminiAccountFile(t, dir, "trustedFolders.json"),
		"the folder-trust dialog's answer must name the account directory itself, absolutely")
}

// jsonString quotes a path as a JSON string so a directory containing a quote or a
// backslash still produces a valid expectation.
func jsonString(s string) string {
	quoted, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(quoted)
}

// TestRegisterWritesOnlyNonCredentialKeys is the boundary assertion. af's whole
// account model rests on af never handling the secret, so a registration that
// started writing into the agent's home has to be shown to write settings and
// nothing else.
//
// It is asserted three ways, because "no credential" is not a property one check
// establishes: the exact set of leaf keys af touched, the absence of every file
// gemini keeps a credential in, and the account's own logged-in verdict — which
// is the same evidence `af accounts list` reports to the operator.
func TestRegisterWritesOnlyNonCredentialKeys(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)

	var leaves []string
	for _, name := range []string{"settings.json", "trustedFolders.json"} {
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(geminiAccountFile(t, dir, name)), &doc))
		for _, leaf := range jsonLeafPaths("", doc) {
			leaves = append(leaves, name+":"+leaf)
		}
	}
	sort.Strings(leaves)
	require.Equal(t, []string{
		"settings.json:security.auth.selectedType=oauth-personal",
		"trustedFolders.json:" + dir + "=TRUST_FOLDER",
	}, leaves, "af wrote a key outside the two prompt answers #3858 authorises")

	// gemini's own credential artifacts, from accountCredentialArtifacts, plus
	// the account file its OAuth flow writes beside them.
	for _, artifact := range append(append([]string{}, accountCredentialArtifacts["gemini"]...),
		filepath.Join(".gemini", "google_accounts.json")) {
		_, err := os.Lstat(filepath.Join(dir, artifact))
		require.True(t, os.IsNotExist(err), "registration created %s", artifact)
	}

	loggedIn, err := LoggedIn(home, "gemini", "work")
	require.NoError(t, err)
	require.False(t, loggedIn, "writing settings made a never-signed-in account look logged in")
}

// jsonLeafPaths flattens a document to `a.b.c=value` lines, so a test can state
// the COMPLETE set of keys af wrote rather than the ones it remembered to look
// for. A key af adds without updating the expectation fails the test.
func jsonLeafPaths(prefix string, doc map[string]any) []string {
	var out []string
	for key, value := range doc {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if child, ok := value.(map[string]any); ok {
			out = append(out, jsonLeafPaths(path, child)...)
			continue
		}
		out = append(out, path+"="+valueText(value))
	}
	return out
}

func valueText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "?"
	}
	return string(encoded)
}

// TestRegisterLeavesAgentsWithNoMeasuredPromptsUntouched keeps the table honest
// from the other side. Claude needs no settings, and Codex must not invent
// a runtime policy when the operator has no ambient configuration.
func TestRegisterLeavesAgentsWithNoMeasuredPromptsUntouched(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := t.TempDir()
	for _, agent := range []string{"claude", "codex"} {
		dir, err := Register(home, agent, "work")
		require.NoError(t, err)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Empty(t, entries, "registration wrote into a %s account directory", agent)
	}
}

// TestRegisterKeepsAnAuthTypeTheAccountAlreadyChose covers the case that makes
// this safe to run on every login: an account that answered the picker itself
// keeps its answer, whatever it was. Silently re-pointing an account at a
// different identity provider is a worse outcome than the prompt this removes.
func TestRegisterKeepsAnAuthTypeTheAccountAlreadyChose(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)

	chosen := `{"security":{"auth":{"selectedType":"vertex-ai"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gemini", "settings.json"), []byte(chosen), 0o600))

	again, err := Register(home, "gemini", "work")
	require.NoError(t, err)
	require.Equal(t, dir, again)
	require.JSONEq(t, chosen, geminiAccountFile(t, dir, "settings.json"))
}

// TestRegisterPreservesTheRestOfTheAccountsGeminiSettings is the merge. The
// account's settings file is the agent's, not af's: it carries model choices,
// MCP servers and hooks, and a registration that rewrote it wholesale would take
// them out.
func TestRegisterPreservesTheRestOfTheAccountsGeminiSettings(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)

	existing := `{"ui":{"theme":"Dracula"},"security":{"folderTrust":{"enabled":true}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gemini", "settings.json"), []byte(existing), 0o600))

	_, err = Register(home, "gemini", "work")
	require.NoError(t, err)
	require.JSONEq(t,
		`{"ui":{"theme":"Dracula"},"security":{"folderTrust":{"enabled":true},"auth":{"selectedType":"oauth-personal"}}}`,
		geminiAccountFile(t, dir, "settings.json"))
}

// TestRegisterAddsToExistingTrustRulesRatherThanReplacingThem covers the same
// property for the other file, whose entries are other directories the operator
// has decided about.
func TestRegisterAddsToExistingTrustRulesRatherThanReplacingThem(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)

	existing := `{"/srv/repo":"TRUST_FOLDER","/tmp/scratch":"DO_NOT_TRUST"}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gemini", "trustedFolders.json"), []byte(existing), 0o600))

	_, err = Register(home, "gemini", "work")
	require.NoError(t, err)
	require.JSONEq(t,
		`{"/srv/repo":"TRUST_FOLDER","/tmp/scratch":"DO_NOT_TRUST",`+jsonString(dir)+`:"TRUST_FOLDER"}`,
		geminiAccountFile(t, dir, "trustedFolders.json"))
}

// TestRegisterDoesNotOverrideAnExistingTrustDecision is the sharp edge of that
// merge. gemini takes the LONGEST matching rule path, so af's exact-path entry
// would outrank a DO_NOT_TRUST an operator had placed on an ancestor — af would
// silently re-trust a directory they deliberately distrusted.
func TestRegisterDoesNotOverrideAnExistingTrustDecision(t *testing.T) {
	for name, rule := range map[string]func(dir string) string{
		"the account directory itself": func(dir string) string {
			return `{` + jsonString(dir) + `:"DO_NOT_TRUST"}`
		},
		"an ancestor of it": func(dir string) string {
			return `{` + jsonString(filepath.Dir(filepath.Dir(dir))) + `:"DO_NOT_TRUST"}`
		},
		"an ancestor reached through TRUST_PARENT": func(dir string) string {
			return `{` + jsonString(filepath.Join(filepath.Dir(dir), "sibling")) + `:"TRUST_PARENT"}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			dir, err := Register(home, "gemini", "work")
			require.NoError(t, err)
			existing := rule(dir)
			require.NoError(t, os.WriteFile(
				filepath.Join(dir, ".gemini", "trustedFolders.json"), []byte(existing), 0o600))

			_, err = Register(home, "gemini", "work")
			require.NoError(t, err)
			require.JSONEq(t, existing, geminiAccountFile(t, dir, "trustedFolders.json"))
		})
	}
}

// TestRegisterLeavesADocumentItCannotParseAlone covers the file af must not
// touch. gemini reads both of these through strip-json-comments, so a commented
// document is VALID to the CLI and unparseable to encoding/json; rewriting it
// would delete the operator's comments, and refusing the registration would
// break an account over a file that works.
func TestRegisterLeavesADocumentItCannotParseAlone(t *testing.T) {
	for _, file := range []string{"settings.json", "trustedFolders.json"} {
		t.Run(file, func(t *testing.T) {
			home := t.TempDir()
			dir, err := Register(home, "gemini", "work")
			require.NoError(t, err)
			commented := "// the operator's own note\n{}\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".gemini", file), []byte(commented), 0o600))

			_, err = Register(home, "gemini", "work")
			require.NoError(t, err, "a document af cannot parse must not fail the registration")
			require.Equal(t, commented, geminiAccountFile(t, dir, file))
		})
	}
}

// TestRegisterLeavesATrustDocumentWithAnUnknownLevelAlone covers the same rule
// for a document that parses but that gemini itself would reject: a value
// outside its TrustLevel enum makes the CLI raise a FatalConfigError, so this is
// either already broken or newer than af — and neither is af's to rewrite.
func TestRegisterLeavesATrustDocumentWithAnUnknownLevelAlone(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)
	existing := `{"/srv/repo":"TRUST_EVERYTHING_FOREVER"}`
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".gemini", "trustedFolders.json"), []byte(existing), 0o600))

	_, err = Register(home, "gemini", "work")
	require.NoError(t, err)
	require.JSONEq(t, existing, geminiAccountFile(t, dir, "trustedFolders.json"))
}

// TestRegisterIsIdempotentForGemini keeps the write off the repeat path.
// Register runs on every `af accounts login`, and a settings file rewritten each
// time would race a gemini that is reading it.
func TestRegisterIsIdempotentForGemini(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)

	before := map[string]os.FileInfo{}
	for _, name := range []string{"settings.json", "trustedFolders.json"} {
		info, err := os.Stat(filepath.Join(dir, ".gemini", name))
		require.NoError(t, err)
		before[name] = info
	}
	// A distinguishable mtime, so "unchanged" is a real observation rather than
	// two stats inside one filesystem timestamp tick.
	stale := before["settings.json"].ModTime().Add(-time.Hour)
	for name := range before {
		require.NoError(t, os.Chtimes(filepath.Join(dir, ".gemini", name), stale, stale))
	}

	_, err = Register(home, "gemini", "work")
	require.NoError(t, err)
	for name := range before {
		info, err := os.Stat(filepath.Join(dir, ".gemini", name))
		require.NoError(t, err)
		require.True(t, info.ModTime().Equal(stale), "%s was rewritten by a repeat registration", name)
	}
}

// TestRegisterWritesTheSettingsOwnerOnly keeps af's own posture: the account
// directory is 0700 because af decided where an agent's credentials live, and
// the files af puts inside it are written the same way rather than through
// whatever umask the operator's shell had.
func TestRegisterWritesTheSettingsOwnerOnly(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)
	for _, name := range []string{"settings.json", "trustedFolders.json"} {
		info, err := os.Stat(filepath.Join(dir, ".gemini", name))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "%s is not owner-only", name)
	}
}

// TestLoginPreconditionsNameTheSettingsAfWrote is the public half of this
// change. af writing into an agent's own settings is defensible only if the
// operator can see that it happened and check the files themselves, so the
// notice names both paths.
func TestLoginPreconditionsNameTheSettingsAfWrote(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "gemini", "work")
	require.NoError(t, err)
	notices, err := CheckLoginPreconditions("gemini", dir)
	require.NoError(t, err)
	joined := strings.Join(notices, "\n")
	require.Contains(t, joined, filepath.Join(dir, ".gemini", "settings.json"))
	require.Contains(t, joined, filepath.Join(dir, ".gemini", "trustedFolders.json"))

	// And only for the agent af writes them for: a notice about files that do not
	// exist is worse than none.
	for _, agent := range []string{"claude", "codex"} {
		other, err := Register(home, agent, "work")
		require.NoError(t, err)
		otherNotices, err := CheckLoginPreconditions(agent, other)
		require.NoError(t, err)
		require.NotContains(t, strings.Join(otherNotices, "\n"), "trustedFolders.json")
	}
}
