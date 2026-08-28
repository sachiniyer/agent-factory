package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A listener key that failed to rebind must not be reported as applied (#3030).
//
// Applied is populated by effect CLASS, before the rebind is attempted, so a key
// whose bind failed was still listed as live — the one field an operator reads to
// answer "did my change take effect" answering yes while the daemon serves the
// old address. Warnings and FailedListenerKeys carried the truth beside it, which
// makes it worse rather than better: a consumer rendering Applied renders a claim
// that contradicts the two fields next to it.
func TestWithoutKeys_DropsFailedListenerKeysFromApplied(t *testing.T) {
	applied := []string{"network.cors_allowed_origins", "network.listen_addr", "network.require_loopback_token"}

	kept := withoutKeys(applied, []string{"network.listen_addr"})

	assert.Equal(t, []string{"network.cors_allowed_origins", "network.require_loopback_token"}, kept,
		"a key that did not bind is not applied; the keys that took effect independently of the "+
			"rebind must survive, because a security tightening lands whether or not a socket moved")
}

func TestWithoutKeys_NoFailuresLeavesAppliedIntact(t *testing.T) {
	applied := []string{"network.listen_addr", "network.preview_listen_addr"}
	assert.Equal(t, applied, withoutKeys(applied, nil),
		"a successful reconcile must not disturb the applied set")
}
