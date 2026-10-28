package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGetent installs a getent that answers only for nssOnlyName, the way an NSS
// module (mDNS, LDAP) answers for a name no DNS server knows (#5750).
func fakeGetent(t *testing.T, nssOnlyName, output string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "getent")
	script := "#!/bin/sh\n[ \"$1\" = ahosts ] && [ \"$2\" = " + nssOnlyName + " ] || exit 2\ncat <<'EOT'\n" + output + "EOT\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	restore := SetLookPathForTest(func(string) (string, error) { return bin, nil })
	t.Cleanup(restore)
}

func staticResolverMode(t *testing.T) {
	prev := dialUsesSystemResolver
	dialUsesSystemResolver = false
	t.Cleanup(func() { dialUsesSystemResolver = prev })
}

func listenLoopback(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// A name only NSS can answer is resolved through the system resolver, not by Go.
func TestStaticBinaryPinsANameOnlyNSSCanAnswer(t *testing.T) {
	staticResolverMode(t)
	port := listenLoopback(t)
	// The first address refuses on this port, so the try-each fallback is covered.
	fakeGetent(t, "nss-only.local", "::1   STREAM nss-only.local\n127.0.0.1   STREAM nss-only.local\n127.0.0.1   DGRAM\n")

	assert.Equal(t, "127.0.0.1", resolvePinnedSSHDialAddress("nss-only.local", port))
}

// With no system resolver to ask, the name is left to ssh: no pin, never a Go lookup.
func TestStaticBinaryWithoutGetentDoesNotPin(t *testing.T) {
	staticResolverMode(t)
	restore := SetLookPathForTest(func(string) (string, error) { return "", os.ErrNotExist })
	defer restore()
	port := listenLoopback(t)
	// "localhost" is resolvable by Go; the pin must still be empty.
	assert.Empty(t, resolvePinnedSSHDialAddress("localhost", port))
}

// An IP literal needs no resolution, so it still pins in a static build.
func TestStaticBinaryStillPinsAnIPLiteral(t *testing.T) {
	staticResolverMode(t)
	port := listenLoopback(t)
	assert.Equal(t, "127.0.0.1", resolvePinnedSSHDialAddress("127.0.0.1", port))
}
