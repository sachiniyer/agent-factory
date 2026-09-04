package session

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

// accountCredentialRoots is the HOME-relative directory each agent's account
// config variable REPLACES. It is the whole difference between the two
// credential tables this test holds together:
//
//	CLAUDE_CONFIG_DIR replaces ~/.claude · CODEX_HOME replaces ~/.codex ·
//	GEMINI_CLI_HOME replaces ~ itself (it is a HOME-like root — gemini's bundle
//	shadows homedir() and appends .gemini, #3387).
//
// So <home>/<root>/<x> and <account dir>/<x> are the same file for the same
// agent, and neither table gets to name a credential the other does not.
var accountCredentialRoots = map[string]string{
	"claude": ".claude",
	"codex":  ".codex",
	"gemini": "",
}

// accountCredentialArtifactExclusions are mounted credential paths that are
// deliberately NOT evidence of a completed login, with the reason each is out.
//
// Named rather than skipped: an entry that simply failed to match would make
// this test a list of two hand-written tables again, which is what it exists to
// prevent. A new mount entry has to be classified here or the test fails.
var accountCredentialArtifactExclusions = map[string]map[string]string{
	"gemini": {
		filepath.Join(".gemini", "google_accounts.json"): "records WHICH accounts the CLI knows, not that any " +
			"of them is authenticated, so its presence is not evidence of a completed login",
	},
}

// TestAccountCredentialArtifactsMatchTheMountedCredentials holds
// agentaccount's account-relative login artifacts against this package's
// home-relative mount table, in BOTH directions.
//
// The two lists are a mirror by necessity — this package imports agentaccount,
// so agentaccount cannot import this table — and #3384 turns that mirror
// load-bearing: `af accounts login` reports an account logged in from the
// artifact's presence, so a credential file this repo already knows about and
// that list forgets makes a real login report failure, while one only the login
// side knows about mounts nothing into a container. Deriving one from the other
// is what keeps a rename in either place from being a silent divergence
// (#3384; the same discipline as #2416's single trust-dismissal gate).
func TestAccountCredentialArtifactsMatchTheMountedCredentials(t *testing.T) {
	for _, agent := range agentaccount.LoginAgents() {
		root, ok := accountCredentialRoots[agent]
		if !ok {
			t.Fatalf("agent %q can be logged in but this test does not say which HOME-relative "+
				"directory its account variable replaces; add it to accountCredentialRoots", agent)
		}
		mounted, ok := agentCredentialFiles[agent]
		if !ok {
			t.Fatalf("agent %q can be logged in but has no entry in agentCredentialFiles, so a docker "+
				"session of that agent mounts no credential", agent)
		}

		artifacts := agentaccount.AccountCredentialArtifacts(agent)
		if len(artifacts) == 0 {
			t.Fatalf("agent %q can be logged in but names no credential artifact, so af could never "+
				"report the login succeeded", agent)
		}
		covered := make(map[string]struct{}, len(artifacts))
		for _, artifact := range artifacts {
			homeRelative := filepath.Join(root, artifact)
			if !slices.Contains(mounted, homeRelative) {
				t.Fatalf("agent %q reports a login from %q (%q relative to home), which agentCredentialFiles "+
					"does not list; the two tables name the same files and have drifted",
					agent, artifact, homeRelative)
			}
			covered[homeRelative] = struct{}{}
		}
		for _, homeRelative := range mounted {
			if _, ok := covered[homeRelative]; ok {
				continue
			}
			if _, excluded := accountCredentialArtifactExclusions[agent][homeRelative]; excluded {
				continue
			}
			t.Fatalf("agent %q mounts %q as a credential but a login into an account never looks for it; "+
				"add it to agentaccount's account artifacts, or record why it is not evidence of a login "+
				"in accountCredentialArtifactExclusions", agent, homeRelative)
		}
	}
}

// TestAccountCredentialArtifactExclusionsAreLive keeps the exclusion list from
// outliving the entries it excuses. A stale exclusion is a reason nobody can
// check, and it would silently absorb a future rename of the real credential.
func TestAccountCredentialArtifactExclusionsAreLive(t *testing.T) {
	for agent, exclusions := range accountCredentialArtifactExclusions {
		for path, reason := range exclusions {
			if !slices.Contains(agentCredentialFiles[agent], path) {
				t.Fatalf("agent %q excludes %q from login evidence (%q), but agentCredentialFiles no longer "+
					"lists it; remove the exclusion", agent, path, reason)
			}
		}
	}
}
