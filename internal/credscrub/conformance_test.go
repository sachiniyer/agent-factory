package credscrub

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactx/redactxtest"
)

// TestConformanceMatrix runs the shared (encoding × carrier) table against
// Scrub. Every secret here is a shape the flat matchers recognize in raw
// text, so each row asks the same question: does the secret still disappear
// when it sits under this encoding?
func TestConformanceMatrix(t *testing.T) {
	secrets := []string{
		"ghp_0123456789abcdefghij",            // PAT shape
		"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAA", // API-key shape
		"api_key=s3cr3tv4lue",                 // keyed value shape
	}
	for _, via := range []string{"log", "text", "url"} {
		redactxtest.Run(t, "credscrub", via, Scrub, secrets)
	}
}
