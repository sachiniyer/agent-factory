package session

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// #5769: a first address that never answers must not consume the whole budget.
func TestStaticBinaryDeadFirstAddressDoesNotStarveTheRest(t *testing.T) {
	staticResolverMode(t)
	port := listenLoopback(t)
	fakeGetent(t, "dead-first.test", "198.51.100.9 STREAM dead-first.test\n127.0.0.1 STREAM dead-first.test\n")

	prevT := sshDialProbeTimeout
	sshDialProbeTimeout = 2 * time.Second
	defer func() { sshDialProbeTimeout = prevT }()

	var firstShare time.Duration
	prev := dialPinCandidate
	dialPinCandidate = func(addr string, timeout time.Duration) (net.Conn, error) {
		if strings.HasPrefix(addr, "198.51.100.9") {
			firstShare = timeout
			time.Sleep(timeout) // never answers
			return nil, os.ErrDeadlineExceeded
		}
		return net.DialTimeout("tcp", addr, timeout)
	}
	defer func() { dialPinCandidate = prev }()

	assert.Equal(t, "127.0.0.1", resolvePinnedSSHDialAddress("dead-first.test", port))
	assert.Less(t, firstShare, sshDialProbeTimeout, "the dead address must get a share, not the whole budget")
}
