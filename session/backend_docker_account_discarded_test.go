package session

// Tests for the account-path half of the overridden-env notice (#7668): an
// operator-set value a gemini account session discards is named once per
// session, on the credential-mount warning's own channel and shape.
// Restorable keep-if-set names carry the session_env_passthrough remedy;
// pins and selectors are named under "account sessions always ignore
// these", because forwarding cannot change their outcome. The pins below
// are the privacy property the whole notice exists under: variable NAMES
// only, never a value — a value can be a credential or an internal proxy
// address.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func accountWarnDockerEnv(t *testing.T, imageEnv string) {
	t.Helper()
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "inspect "+dockerCreatedID):
			return []byte("sha256:resolved\n"), nil
		case strings.HasPrefix(joined, "image inspect sha256:resolved"):
			return []byte(imageEnv), nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}))
}

// TestWarnDiscardedAccountEnvNames_NamesOnly pins the privacy property:
// sentinel values planted in the image environment and in run_args —
// shaped like a credential and an internal proxy address — reach the
// warning only as names. The values themselves must never appear.
func TestWarnDiscardedAccountEnvNames_NamesOnly(t *testing.T) {
	const (
		secretConfigDir = "/home/op/.config/s3cr3t-gcloud-internal-do-not-log"
		internalProxy   = "http://s3cr3t.corp.internal:8888"
		internalProject = "s3cr3t-internal-proj-do-not-log"
	)
	accountWarnDockerEnv(t, `["CLOUDSDK_CONFIG=`+secretConfigDir+`","GOOGLE_CLOUD_PROJECT=`+internalProject+`","GEMINI_API_KEY=`+secretConfigDir+`","PATH=/bin"]`)

	p := &dockerProvisioner{
		program:     tmux.ProgramGemini,
		image:       "example.invalid/agent:latest",
		containerID: dockerCreatedID,
		runArgs:     []string{"-e", "HTTP_PROXY=" + internalProxy},
		spec: ProvisionSpec{
			Program: "gemini", Title: "t",
			Account: sessionenv.Account{Agent: "gemini", Name: "work", Dir: "/acct"},
		},
	}
	buf := captureWarningLog(t)
	p.warnBlankedCredentialNames()
	out := buf.String()

	require.Contains(t, out, "session_env_passthrough",
		"restorable names carry the remedy")
	require.Contains(t, out, "account sessions always ignore",
		"pins and selectors are named without a remedy that cannot help")
	for _, name := range []string{"CLOUDSDK_CONFIG", "GOOGLE_CLOUD_PROJECT", "HTTP_PROXY"} {
		require.Contains(t, out, name, "a set-and-discarded name must be named")
	}
	for _, value := range []string{secretConfigDir, internalProxy, internalProject} {
		require.NotContains(t, out, value,
			"a discarded value must never be logged — names only")
	}
	require.NotContains(t, out, "PATH", "an admitted name is not a discard")
}

// TestWarnDiscardedAccountEnvNames_ReseatedTwinIsNotADiscard is R1/R4a on
// the docker path: an unforwarded lowercase proxy spelling set in the
// image is reseated to its FORWARDED twin's daemon-env value at the
// boundary — an operator value is delivered, so it must not be reported.
func TestWarnDiscardedAccountEnvNames_ReseatedTwinIsNotADiscard(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://daemon-side:3128")
	accountWarnDockerEnv(t, `["https_proxy=http://image-low:8080","NO_PROXY=localhost"]`)

	p := &dockerProvisioner{
		program:     tmux.ProgramGemini,
		image:       "example.invalid/agent:latest",
		containerID: dockerCreatedID,
		spec: ProvisionSpec{
			Program: "gemini", Title: "t",
			Account:               sessionenv.Account{Agent: "gemini", Name: "work", Dir: "/acct"},
			SessionEnvPassthrough: []string{"HTTPS_PROXY"},
		},
	}
	buf := captureWarningLog(t)
	p.warnBlankedCredentialNames()
	out := buf.String()
	require.NotContains(t, out, "https_proxy",
		"reseated to the forwarded twin's value — an operator value arrives")
	require.Contains(t, out, "NO_PROXY", "its unforwarded, unreseated twin is still a discard")
	require.NotContains(t, out, "image-low", "names only, never a value")
}

// TestWarnDiscardedAccountEnvNames_SkipsForwardedAndAmbient pins the
// complement: a keep-if-set name the operator forwarded through
// session_env_passthrough is the claim channel working, never a discard;
// and a non-account session emits nothing.
func TestWarnDiscardedAccountEnvNames_SkipsForwardedAndAmbient(t *testing.T) {
	accountWarnDockerEnv(t, `["HTTPS_PROXY=http://corp:3128","NO_PROXY=localhost"]`)

	p := &dockerProvisioner{
		program:     tmux.ProgramGemini,
		image:       "example.invalid/agent:latest",
		containerID: dockerCreatedID,
		spec: ProvisionSpec{
			Program: "gemini", Title: "t",
			Account:               sessionenv.Account{Agent: "gemini", Name: "work", Dir: "/acct"},
			SessionEnvPassthrough: []string{"HTTPS_PROXY"},
		},
	}
	buf := captureWarningLog(t)
	p.warnBlankedCredentialNames()
	out := buf.String()
	require.NotContains(t, out, "HTTPS_PROXY",
		"a forwarded keep-if-set name is claimed — warning for it would be a false positive")
	require.Contains(t, out, "NO_PROXY", "its unforwarded twin is still a discard")

	// A non-gemini account session emits nothing — the notice is
	// gemini-scoped (#7668).
	claude := &dockerProvisioner{
		program:     tmux.ProgramClaude,
		image:       "example.invalid/agent:latest",
		containerID: dockerCreatedID,
		spec: ProvisionSpec{
			Program: "claude", Title: "t",
			Account: sessionenv.Account{Agent: "claude", Name: "work", Dir: "/acct"},
		},
	}
	buf2 := captureWarningLog(t)
	claude.warnBlankedCredentialNames()
	require.Empty(t, buf2.String(), "claude account sessions are out of #7668's scope")

	// No account → the hook emits nothing at all.
	p2 := &dockerProvisioner{
		program:     tmux.ProgramGemini,
		image:       "example.invalid/agent:latest",
		containerID: dockerCreatedID,
		spec:        ProvisionSpec{Program: "gemini", Title: "t"},
	}
	buf3 := captureWarningLog(t)
	p2.warnBlankedCredentialNames()
	require.Empty(t, buf3.String())
}
