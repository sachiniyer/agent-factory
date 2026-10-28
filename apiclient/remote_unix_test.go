package apiclient

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseUnixDaemonURL(t *testing.T) {
	sock, ok, err := parseUnixDaemonURL("unix:///home/u/.af-sessions/s.X/af-callback.sock")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "/home/u/.af-sessions/s.X/af-callback.sock", sock)

	_, ok, err = parseUnixDaemonURL("http://127.0.0.1:8443")
	require.NoError(t, err)
	assert.False(t, ok, "other schemes keep the http/ws path")

	for _, bad := range []string{"unix://host/x.sock", "unix://rel.sock", "unix:///a/../b.sock", "unix://"} {
		_, ok, err = parseUnixDaemonURL(bad)
		assert.Truef(t, ok, "%s is recognised as a unix target", bad)
		assert.Errorf(t, err, "%s must be refused", bad)
	}
}

// The unix target sends the bearer to the socket and ONLY the socket; a missing
// socket (the forward is mid-re-dial) is a failed call — there is nothing else to
// dial (#5799).
func TestNewRemoteUnixDialsOnlyTheSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "afcb")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "cb.sock")

	c, err := NewRemote("unix://"+sock, "tok-123")
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, c.httpBase+"/v1/ping", nil)
	require.NoError(t, err)
	_, err = c.httpClient.Do(req)
	require.Error(t, err, "no socket: the call fails")

	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	gotAuth := make(chan string, 1)
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth <- r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))
	}()

	req, err = http.NewRequest(http.MethodGet, c.httpBase+"/v1/ping", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, "Bearer tok-123", <-gotAuth)
}
