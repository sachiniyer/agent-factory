package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The completeness artifact (gemini_bundle_env.go) is what makes the "the
// bundle reads are classified" claim CHECKED rather than asserted — #7662's
// answer to the #7659 review's R3. These tests pin three invariants: the
// artifact is internally well-formed, every entry's class actually lands in
// the classification tables, and the reads af knows form multi-link fallback
// chains have every env link claimed.
//
// The list was measured against @google/gemini-cli 0.42.0 — `npm pack` into
// an empty directory, grep of the bundled chunks (the CLI was never
// executed). When the pinned version moves, re-measure the bundle and update
// the artifact and this version together.

// TestGeminiBundleEnvReads_VersionPin records the measured CLI version in a
// test, so a version bump that does not re-measure is a deliberate red diff
// here rather than a silent stale claim.
func TestGeminiBundleEnvReads_VersionPin(t *testing.T) {
	require.Equal(t, "0.42.0", GeminiBundleVersion,
		"the artifact was measured against @google/gemini-cli 0.42.0 — bumping it means re-measuring the bundle")
}

// TestGeminiBundleEnvReads_WellFormed: every entry names a real class, waived
// entries carry their one-line reason (and ONLY waived entries carry one),
// reassert entries carry the first-party value they install, and no name is
// listed twice.
func TestGeminiBundleEnvReads_WellFormed(t *testing.T) {
	seen := make(map[string]struct{}, len(GeminiBundleEnvReads))
	for _, r := range GeminiBundleEnvReads {
		require.NotEmpty(t, r.Name, "artifact entry with no name")
		_, dup := seen[r.Name]
		require.False(t, dup, "%s is listed twice", r.Name)
		seen[r.Name] = struct{}{}
		for _, class := range []GeminiBundleEnvClass{r.Mount, r.Account} {
			switch class {
			case GeminiBundleEnvPin, GeminiBundleEnvKeepIfSet, GeminiBundleEnvReassert,
				GeminiBundleEnvRoot, GeminiBundleEnvKept, GeminiBundleEnvNoPin,
				GeminiBundleEnvShared, GeminiBundleEnvWaived:
			default:
				t.Errorf("%s has an unknown class %q", r.Name, class)
			}
		}
		require.NotEmpty(t, r.Read, "%s must cite the measured read", r.Name)
		if r.Mount == GeminiBundleEnvWaived || r.Account == GeminiBundleEnvWaived {
			require.NotEmpty(t, r.Reason, "%s is waived without the one-line reason", r.Name)
		} else {
			require.Empty(t, r.Reason, "%s is claimed on both paths — a reason only belongs on a waiver", r.Name)
		}
		if r.Mount == GeminiBundleEnvReassert || r.Account == GeminiBundleEnvReassert {
			require.NotEmpty(t, r.Value, "%s is re-asserted without the first-party value", r.Name)
		}
	}
}

// TestGeminiBundleEnvReads_ClassificationsLand checks every claimed entry's
// account class reaches the table that delivers it — the derivation is the
// point of the artifact, so a class that names no table is a classification
// that silently does nothing.
func TestGeminiBundleEnvReads_ClassificationsLand(t *testing.T) {
	identity := sortedNamesToSetForTest(AccountIdentityNames("gemini"))
	latePins := accountLateEnvironmentNames["gemini"]
	lateValues := accountLateEnvironmentValues["gemini"]
	loader := accountLoaderFilledNames["gemini"]
	conditional := sortedNamesToSetForTest(AgentConditionalNames("gemini"))
	for _, r := range GeminiBundleEnvReads {
		switch r.Account {
		case GeminiBundleEnvPin:
			_, pinned := latePins[r.Name]
			require.True(t, pinned, "%s is pin-classified but absent from accountLateEnvironmentNames", r.Name)
			require.Contains(t, identity, r.Name, "%s must reach AccountIdentityNames so every path pins it", r.Name)
		case GeminiBundleEnvReassert:
			value, ok := lateValues[r.Name]
			require.True(t, ok, "%s is reassert-classified but absent from accountLateEnvironmentValues", r.Name)
			require.Equal(t, r.Value, value, "%s: artifact and table disagree on the re-asserted value", r.Name)
		case GeminiBundleEnvKeepIfSet:
			_, ok := loader[r.Name]
			require.True(t, ok, "%s is keep-if-set but absent from accountLoaderFilledNames", r.Name)
		case GeminiBundleEnvRoot:
			configVar, _ := SupportsAccounts("gemini")
			require.Equal(t, configVar, r.Name, "%s is the injected account root", r.Name)
		case GeminiBundleEnvKept, GeminiBundleEnvWaived, GeminiBundleEnvShared:
			// Kept names are ambient-admitted (or denied elsewhere) by
			// existing tables; waived names are deliberately unclaimed;
			// shared-boundary names are claimed by the agent-agnostic
			// mount tables, not the artifact-derived sets.
		}
		// A name reachable through the conditional cloud groups must be
		// classified — the conditional admission is what lets an operator's
		// cloud-mode value through, and the pin is what stops a repo's.
		if _, reachable := conditional[r.Name]; reachable {
			require.NotEqual(t, GeminiBundleEnvWaived, r.Account,
				"%s is conditionally admitted but waived — an unclaimed admitted name", r.Name)
			require.NotEqual(t, GeminiBundleEnvWaived, r.Mount,
				"%s is conditionally admitted but waived on the mount path", r.Name)
		}
	}
}

// TestGeminiBundleEnvReads_EveryChainLinkClaimed pins the #7659-review R2
// property: an empty pin holds only when every environment link of the
// read's fallback chain is claimed. Each chain below is the exact ||-chain
// the bundle performs; a name added to the chain on a future version lands
// here when the artifact is re-measured.
func TestGeminiBundleEnvReads_EveryChainLinkClaimed(t *testing.T) {
	chains := map[string][]string{
		// google-auth-library :14547 — the project the ADC credential and
		// its quota bill against.
		"GCLOUD_PROJECT || GOOGLE_CLOUD_PROJECT || gcloud_project || google_cloud_project": {
			"GCLOUD_PROJECT", "GOOGLE_CLOUD_PROJECT", "gcloud_project", "google_cloud_project",
		},
		// google-auth-library :14312 — the application-default credential
		// file, upper- then lowercase spelling.
		"GOOGLE_APPLICATION_CREDENTIALS || google_application_credentials": {
			"GOOGLE_APPLICATION_CREDENTIALS", "google_application_credentials",
		},
		// grpc-js mapProxyName — the transport's proxy chain, head first.
		"grpc_proxy || https_proxy || http_proxy": {
			"grpc_proxy", "https_proxy", "http_proxy",
		},
		// The bypass list's own fallback.
		"no_grpc_proxy || no_proxy": {
			"no_grpc_proxy", "no_proxy",
		},
		// gcp-metadata — the endpoint the ADC probe and token fetch dial.
		// The third link is a compiled constant, not an env read.
		"GCE_METADATA_IP || GCE_METADATA_HOST || <compiled>": {
			"GCE_METADATA_IP", "GCE_METADATA_HOST",
		},
		// The npm-config proxy helper inside the bundle.
		"npm_config_proxy || http_proxy || https_proxy": {
			"npm_config_proxy", "http_proxy", "https_proxy",
		},
		"npm_config_no_proxy || no_proxy": {
			"npm_config_no_proxy", "no_proxy",
		},
		// The region label chain.
		"GOOGLE_CLOUD_REGION || FUNCTION_REGION || region": {
			"GOOGLE_CLOUD_REGION", "FUNCTION_REGION",
		},
		// The GCA project fallback.
		"GOOGLE_CLOUD_PROJECT || GOOGLE_CLOUD_PROJECT_ID": {
			"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_PROJECT_ID",
		},
	}
	byName := make(map[string]GeminiBundleEnvRead, len(GeminiBundleEnvReads))
	for _, r := range GeminiBundleEnvReads {
		byName[r.Name] = r
	}
	for chain, links := range chains {
		for _, link := range links {
			r, ok := byName[link]
			require.True(t, ok, "%s: link %s is not in the measured-read artifact at all", chain, link)
			require.NotEqual(t, GeminiBundleEnvWaived, r.Mount,
				"%s: link %s is waived on the mount path — the chain has an unclaimed link", chain, link)
			require.NotEqual(t, GeminiBundleEnvWaived, r.Account,
				"%s: link %s is waived on the account path — the chain has an unclaimed link", chain, link)
		}
	}
}

// TestGeminiBundleEnvReads_ProxyAndTrustRootsClassifiedOnBothPaths pins the
// #7659-review R1 property: every name that chooses the proxy or the trust
// roots of a transport carrying the credential — including its case and
// transport-specific twins — is claimed on BOTH paths.
func TestGeminiBundleEnvReads_ProxyAndTrustRootsClassifiedOnBothPaths(t *testing.T) {
	proxyAndTrust := []string{
		// grpc transport-specific spellings (#7659 R1).
		"grpc_proxy", "no_grpc_proxy",
		// Generic proxy spellings in both cases — grpc-js's own chain reads
		// the lowercase ones, and every other client mixes them.
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "no_proxy", "all_proxy",
		// The npm-config proxy helper's spellings.
		"npm_config_proxy", "npm_config_no_proxy",
		// Trust roots and TLS transport configuration.
		"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", "GRPC_SSL_CIPHER_SUITES",
		"GOOGLE_API_CERTIFICATE_CONFIG",
	}
	byName := make(map[string]GeminiBundleEnvRead, len(GeminiBundleEnvReads))
	for _, r := range GeminiBundleEnvReads {
		byName[r.Name] = r
	}
	for _, name := range proxyAndTrust {
		r, ok := byName[name]
		require.True(t, ok, "%s is a proxy/trust-root read missing from the artifact", name)
		require.NotEqual(t, GeminiBundleEnvWaived, r.Mount,
			"%s selects the proxy or trust roots of credential-bearing transport but is waived on the mount path", name)
		require.NotEqual(t, GeminiBundleEnvWaived, r.Account,
			"%s selects the proxy or trust roots of credential-bearing transport but is waived on the account path", name)
		require.NotEqual(t, GeminiBundleEnvKept, r.Account,
			"%s routes credential-bearing traffic — ambient keeping is not a claim", name)
	}
}

// The #7662 additions to the unconditional pin set: the lowercase credential
// twin and the auth-type selector are identities a late source must not
// restore — they pin defined-and-empty like their siblings.
func TestGeminiBundleEnvReads_IdentityPinsReachEveryPath(t *testing.T) {
	for _, name := range []string{"google_application_credentials", "GEMINI_DEFAULT_AUTH_TYPE"} {
		_, inLate := accountLateEnvironmentNames["gemini"][name]
		require.True(t, inLate, "%s must be an unconditional late-environment pin", name)
		require.Contains(t, AccountIdentityNames("gemini"), name,
			"%s must reach the shared identity list", name)
	}
	// The twin is an identity, exactly like its uppercase sibling.
	_, isCredential := accountCredentialNames["gemini"]["google_application_credentials"]
	require.True(t, isCredential, "the lowercase twin substitutes the account directory like GOOGLE_APPLICATION_CREDENTIALS")
}

// sortedNamesToSetForTest turns a sorted slice into a membership set —
// the local twin of the session package's helper.
func sortedNamesToSetForTest(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}
