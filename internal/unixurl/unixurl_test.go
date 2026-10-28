package unixurl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The client recovers EXACTLY the socket path the provisioner created, for any
// byte the filesystem allows (#5870).
func TestBuildParseRoundTripsAnyPath(t *testing.T) {
	for _, path := range []string{
		"/srv/af-sessions/s.AAA/af-callback.sock",
		"/srv/af#work/s.AAA/af-callback.sock",
		"/srv/af?x=1/s.AAA/af-callback.sock",
		"/srv/100%/s.AAA/af-callback.sock",
		"/srv/%2F/%23/%25/af-callback.sock",
		"/srv/my home/s.AAA/af-callback.sock",
		"/home/ユーザー/ñandú/s.AAA/af-callback.sock",
		"/srv/a+b;c=d&e@f:g/s.AAA/af-callback.sock",
		"/srv/\xff\xfe-not-utf8/s.AAA/af-callback.sock",
		"/srv/tab\there/s.sock",
	} {
		t.Run(path, func(t *testing.T) {
			got, ok, err := Parse(Build(path))
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, path, got)
		})
	}
}

// The raw concatenation this replaces used to parse "/srv/af#work/s.sock" as the
// path "/srv/af" plus a fragment, silently dialling a socket that does not exist.
// A fragment or query is now refused loudly instead.
func TestRawConcatenationIsRefusedNotSilentlyTruncated(t *testing.T) {
	_, ok, err := Parse("unix://" + "/srv/af#work/s.sock")
	assert.True(t, ok)
	assert.Error(t, err)
}

func TestParseRefusesWhatIsNotAnAbsoluteCleanSocketPath(t *testing.T) {
	for _, target := range []string{
		"unix://host/path.sock", "unix:relative.sock", "unix:///a/../b.sock", "unix:///a//b.sock",
		"unix:///srv/s.sock#frag", "unix:///srv/s.sock?q=1",
	} {
		_, ok, err := Parse(target)
		assert.True(t, ok, target)
		assert.Error(t, err, target)
	}
	_, ok, err := Parse("http://127.0.0.1:1")
	assert.False(t, ok)
	assert.NoError(t, err)
}
