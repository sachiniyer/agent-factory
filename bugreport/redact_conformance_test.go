package bugreport

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactx/redactxtest"
)

// TestSharedStageConformance runs the shared (encoding × carrier) matrix
// against this package's scrub entry points. The log-carrier secrets include
// the registered values only the log/diagnostic policy matches bare — a
// session title — while the document carriers run only the kinds config
// scalars' generic policy covers (credentials, roots). That split is itself
// policy: a bare title inside a config value is deliberately not a secret
// there, and the matrix must not turn one consumer's policy into another's.
func TestSharedStageConformance(t *testing.T) {
	r := &redactor{}
	r.noteTitle("SecretSyncTitle")
	r.noteRepoRoot("/srv/ConfidentialClient/repo")

	logSecrets := []string{
		"ghp_0123456789abcdefghij",
		"SecretSyncTitle",
		"/srv/ConfidentialClient/repo",
	}
	docSecrets := []string{
		"ghp_0123456789abcdefghij",
		"/srv/ConfidentialClient/repo",
	}

	redactxtest.Run(t, "bugreport", "log", r.scrubLog, logSecrets)
	redactxtest.Run(t, "bugreport", "url", r.scrubLog, logSecrets)
	redactxtest.Run(t, "bugreport", "text", r.scrubLog, logSecrets)
	redactxtest.Run(t, "bugreport", "json", r.scrubJSON, docSecrets)
	redactxtest.Run(t, "bugreport", "toml", func(s string) string {
		return r.scrubConfigText(s, redactionTextConfigTOML)
	}, docSecrets)
}
