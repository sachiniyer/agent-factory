package agentproto

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactx/redactxtest"
)

// TestSharedStageConformance runs the matrix rows that name agentproto. Its
// match policy is key-aware only — an access_token VALUE is a secret solely
// under its key, so the secrets here are deliberately not credential shapes;
// a bare-token row would assert a redaction this package correctly never
// performs. The keyed rows are the ones that carry a value under
// access_token=, plus the residue rows every consumer shares.
func TestSharedStageConformance(t *testing.T) {
	secrets := []string{
		"T0K3Nv4lu3.d3ad-b33f",
		"zz_opaque_token_9x",
	}
	redactxtest.Run(t, "agentproto", "url", RedactAccessTokenURL, secrets)
	redactxtest.Run(t, "agentproto", "text", RedactAccessTokenText, secrets)
}
