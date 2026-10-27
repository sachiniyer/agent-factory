package session

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingShell struct {
	scripts []string
	stdin   []string
}

func (r *recordingShell) Run(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
	r.scripts = append(r.scripts, script)
	body := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	r.stdin = append(r.stdin, body)
	return nil, nil
}

func TestSeedFromBundle(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "x.bundle")
	require.NoError(t, os.WriteFile(bundle, []byte("BUNDLE"), 0o600))
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: "/s", spec: ProvisionSpec{
		SeedBundlePath: bundle, SeedRefs: []string{"refs/heads/feat", "refs/af-move/k/snapshot"},
	}}
	require.NoError(t, w.seedFromBundle(time.Second))
	require.Len(t, sh.scripts, 2)
	require.Equal(t, "BUNDLE", sh.stdin[0])
	require.Contains(t, sh.scripts[0], "/s/seed.bundle")
	fetch := sh.scripts[1]
	require.Contains(t, fetch, "git -C '/s/workspace' fetch '/s/seed.bundle'")
	require.Contains(t, fetch, "'refs/heads/feat:refs/heads/feat'")
	require.Contains(t, fetch, "'refs/af-move/k/snapshot:refs/af-move/k/snapshot'")
	require.True(t, strings.HasSuffix(fetch, "rm -f '/s/seed.bundle'"))
}

func TestSeedFromBundleNoopWithoutBundle(t *testing.T) {
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, spec: ProvisionSpec{}}
	require.NoError(t, w.seedFromBundle(time.Second))
	require.Empty(t, sh.scripts)
	w.spec.SeedBundlePath = "/nonexistent"
	require.Error(t, w.seedFromBundle(time.Second))
	w.spec.SeedBundlePath = os.DevNull
	require.ErrorContains(t, w.seedFromBundle(time.Second), "no refs")
}
