package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A provision failure returns no session and therefore no teardown closure. The
// per-session host-key directory must leave with that failed attempt, including
// failures added below the point where the pin is created in the future.
func TestHookProvisionFailureRemovesKnownHostsDirectory(t *testing.T) {
	newProvisioner := func(t *testing.T) (*hookProvisioner, string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		record := `{"host":"10.0.0.7","host_key":"` + provisionKey + `"}`
		h := newHookState(t, "printf '%s' '"+record+"'\n", "")
		p := newHookProvisioner(h, t.Name())
		p.hooks.LaunchCmd = ""
		p.hooks.ProvisionCmd = h.launch
		return p, filepath.Join(home, "hook-hosts", p.slug)
	}

	t.Run("known_hosts write fails", func(t *testing.T) {
		p, dir := newProvisioner(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "known_hosts"), 0o700))

		_, err := p.provisionHost()
		require.ErrorContains(t, err, "cannot write the per-session known_hosts")
		assert.NoDirExists(t, dir, "the directory created before the write failed leaked")
	})

	t.Run("af binary lookup fails", func(t *testing.T) {
		p, dir := newProvisioner(t)
		lookupErr := errors.New("binary lookup failed")
		previous := sshSelfBinary
		sshSelfBinary = func() (string, error) { return "", lookupErr }
		t.Cleanup(func() { sshSelfBinary = previous })

		_, err := p.provisionHost()
		require.Error(t, err)
		assert.ErrorIs(t, err, lookupErr)
		assert.NoDirExists(t, dir, "the unused pin leaked after binary lookup failed")
	})

	t.Run("partial sandbox provisioning is reaped", func(t *testing.T) {
		p, dir := newProvisioner(t)
		provisionErr := errors.New("remote git configuration failed")
		previous := newHookSandboxProvisioner
		calls := 0
		newHookSandboxProvisioner = func(spec ProvisionSpec, sshCmd, afBin, program string) *sandboxProvisioner {
			sp := previous(spec, sshCmd, afBin, program)
			sp.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
				assert.FileExists(t, filepath.Join(dir, "known_hosts"),
					"sandbox provisioning and its failure reap still need the pin")
				calls++
				switch calls {
				case 1:
					return []byte("/remote/session\n"), nil
				case 2:
					return nil, provisionErr
				default:
					// reap requires the remote challenge transformed to uppercase.
					return []byte(strings.ToUpper(script)), nil
				}
			}
			return sp
		}
		t.Cleanup(func() { newHookSandboxProvisioner = previous })

		_, err := p.provisionHost()
		require.Error(t, err)
		assert.ErrorIs(t, err, provisionErr)
		assert.Equal(t, 3, calls, "the partial workspace must be reaped before returning")
		assert.NoDirExists(t, dir, "the pin leaked after the transport stopped and reaped")
	})
}
