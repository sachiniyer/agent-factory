package config

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadRepoInstancesWithReader(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	for _, tc := range []struct {
		name, raw, want string
		err             error
		wantError       bool
	}{
		{name: "missing", err: os.ErrNotExist, want: "[]"},
		{name: "empty", raw: " \n", want: "[]"},
		{name: "legacy", raw: `[{"title":"old"}]`, want: `[{"title":"old"}]`},
		{name: "deadline", err: context.DeadlineExceeded, wantError: true},
		{name: "corrupt", raw: `{`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected, err := RepoInstancesPath("repo")
			require.NoError(t, err)
			calls := 0
			raw, err := LoadRepoInstancesWithReader("repo", func(path string) ([]byte, error) {
				calls++
				require.Equal(t, expected, path)
				return []byte(tc.raw), tc.err
			})
			require.Equal(t, 1, calls)
			if tc.wantError {
				require.Error(t, err)
				if tc.err != nil {
					require.ErrorIs(t, err, tc.err)
				}
				return
			}
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(raw))
		})
	}
	// An injected reader must still decode the current versioned envelope.
	want := []byte(`[{"title":"new"}]`)
	envelope, err := marshalInstancesEnvelope(want)
	require.NoError(t, err)
	raw, err := LoadRepoInstancesWithReader("repo", func(string) ([]byte, error) { return envelope, nil })
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(raw))
}
