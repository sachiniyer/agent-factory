package commands

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"

	"github.com/spf13/cobra"
)

// runToken invokes a token subcommand with a fresh output buffer, resetting the
// shared --json flag first so tests do not leak state into one another.
func runToken(t *testing.T, cmd *cobra.Command, jsonMode bool) string {
	t.Helper()
	tokenJSONFlag = jsonMode
	t.Cleanup(func() { tokenJSONFlag = false })

	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	if err := cmd.RunE(c, nil); err != nil {
		t.Fatalf("%s: %v", cmd.Use, err)
	}
	return out.String()
}

func TestTokenShowGeneratesToken(t *testing.T) {
	tempAFHome(t)

	out := runToken(t, tokenShowCmd, false)
	if !strings.Contains(out, "token:") {
		t.Fatalf("token show output missing token line:\n%s", out)
	}
	// HTTP-only: there is no TLS cert, so no fingerprint line is printed.
	if strings.Contains(out, "tls_fingerprint") {
		t.Fatalf("token show must not print a tls_fingerprint line (TLS was removed):\n%s", out)
	}
}

func TestTokenShowIdempotent(t *testing.T) {
	tempAFHome(t)

	first := parseTokenJSON(t, runToken(t, tokenShowCmd, true))
	second := parseTokenJSON(t, runToken(t, tokenShowCmd, true))

	if first.Token == "" {
		t.Fatal("empty token from show")
	}
	if first.Token != second.Token {
		t.Fatalf("token show not idempotent: %q != %q", first.Token, second.Token)
	}
}

func TestTokenRotateChangesToken(t *testing.T) {
	tempAFHome(t)

	before := parseTokenJSON(t, runToken(t, tokenShowCmd, true)).Token
	rotated := parseTokenJSON(t, runToken(t, tokenRotateCmd, true)).Token
	if rotated == "" {
		t.Fatal("empty token from rotate")
	}
	if rotated == before {
		t.Fatalf("rotate did not change the token: %q", rotated)
	}

	// A subsequent show reflects the rotated token.
	after := parseTokenJSON(t, runToken(t, tokenShowCmd, true)).Token
	if after != rotated {
		t.Fatalf("show after rotate = %q, want rotated %q", after, rotated)
	}
}

// tokenJSONPayload mirrors tokenShowResult/tokenRotateResult for decoding.
type tokenJSONPayload struct {
	Token string `json:"token"`
}

func parseTokenJSON(t *testing.T, out string) tokenJSONPayload {
	t.Helper()
	var env struct {
		Data  tokenJSONPayload        `json:"data"`
		Error *apiproto.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope: %v\noutput: %s", err, out)
	}
	if env.Error != nil {
		t.Fatalf("envelope carried an error: %s", env.Error.Message)
	}
	return env.Data
}
