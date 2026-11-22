package session

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The indirection's leaf name carries a session-dir digest, so two sessions
// can never collide inside a shared root. It must be colon-free itself (it
// sits inside the PATH entry) and distinct per session dir.
func TestPanePathLinkNameIsColonFreeDeterministicAndDistinct(t *testing.T) {
	a := panePathLinkName("/srv/af:sessions/work.abc123")
	require.True(t, strings.HasPrefix(a, "sess-"), "the name must carry the session marker, got %q", a)
	assert.NotContains(t, a, ":", "the leaf name is part of a PATH entry and must never contain ':'")
	assert.Equal(t, a, panePathLinkName("/srv/af:sessions/work.abc123"),
		"the same session dir must name the same link every time")
	assert.NotEqual(t, a, panePathLinkName("/srv/af:sessions/work.def456"),
		"different session dirs must not collide on the link name")
	assert.NotEqual(t, a, panePathLinkName("/other:root/work.abc123"),
		"the whole dir feeds the name — the same basename under a second root must not collide")
}

// A colon-free session dir needs no indirection: it IS the PATH entry, and no
// remote round trip may be spent proving it (#7770 R5).
func TestEnsurePanePathDirKeepsAColonFreeSessionDir(t *testing.T) {
	ran := false
	p := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		ran = true
		return nil, nil
	}}
	for _, dir := range []string{
		"/home/u/.af-sessions/sess",
		"/srv/af sessions/sess", // whitespace is a legal PATH entry (#7770 review)
		"/srv/af-sessions/dir with\ttab",
	} {
		w := &sandboxWorkspace{shell: p, SessionDir: dir}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		assert.Equal(t, dir, w.panePathDir)
	}
	assert.False(t, ran, "a colon-free session dir must not spend a remote call")
}

// A session dir that is not absolute cannot be a single PATH entry either —
// refused before launch, naming the property violated.
func TestEnsurePanePathDirRefusesARelativeSessionDir(t *testing.T) {
	p := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return nil, errors.New("must not run")
	}}
	w := &sandboxWorkspace{shell: p, SessionDir: "relative/sess"}
	err := w.ensurePanePathDir(sshShortStepTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
	assert.Empty(t, w.panePathDir)
}

// A ':'-bearing session dir gets the indirection. With no recorded path the
// choose script runs: it prefers $HOME/.af-path when that spelling is usable,
// falls back to an unpredictable 0700 mktemp -d root outside $HOME when $HOME
// bears ':' (#7770 R4), refuses only when no colon-free spelling exists
// anywhere, guards every planted path, and commits nothing until the leaf is
// PROVEN — a symlink whose readlink equals the session dir byte for byte
// (#7770 R1).
func TestEnsurePanePathDirBuildsTheIndirection(t *testing.T) {
	var script string
	var combined bool
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, s string, _ io.Reader, c bool) ([]byte, error) {
		script, combined = s, c
		return []byte("AF_PANE_LINK=/home/u/.af-path/" + panePathLinkName("/srv/af:sessions/work.abc123") + "\n"), nil
	}}
	dir := "/srv/af:sessions/work.abc123"
	w := &sandboxWorkspace{shell: p, SessionDir: dir}
	require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))

	name := panePathLinkName(dir)
	assert.Equal(t, "/home/u/.af-path/"+name, w.panePathDir,
		"the PATH entry becomes the indirection's colon-free spelling, not the session dir")
	assert.Contains(t, script, `.af-path`, "the preferred root lives under the remote login's $HOME")
	assert.Contains(t, script, `mktemp -d`, "a ':'-bearing $HOME is handled by a mktemp root outside it, never refused")
	assert.Contains(t, script, `base="${TMPDIR:-/tmp}"`, "the mktemp base defaults to /tmp")
	assert.Contains(t, script, `case "$base" in *:*) base=/tmp`, "a ':'-bearing TMPDIR falls back to the literal /tmp")
	assert.Contains(t, script, `case "$root" in *:*)`, "the root mktemp prints is itself re-checked for ':'")
	assert.Contains(t, script, `chmod 700`, "the indirection root is private to the session user")
	assert.Contains(t, script, `ln -sfn "$sess" "$link"`, "the link points at the real session dir")
	assert.Contains(t, script, `[ -d "$HOME" ]`, "$HOME is used only when it is a real directory")
	assert.Contains(t, script, `[ -L "$link" ]`, "a symlinked root or leaf is refused rather than written inside")
	assert.Contains(t, script, `[ -e "$link" ] && [ ! -L "$link" ]`,
		"an existing non-symlink at the leaf is refused before `ln` — implementations differ on ln -sfn into a real directory")
	assert.Contains(t, script, `[ "$(readlink "$link")" = "$sess" ]`,
		"the PATH entry is committed only after the link PROVES its target byte for byte")
	assert.False(t, combined, "the link path is parsed from stdout only — stderr stays out of the stream (#7770 R3)")
}

// A reattach carries the session's recorded link path — the persisted launch
// names it — so the link is re-created AT it (the ensure-at script), never a
// fresh spelling. A malformed record is refused before any remote call.
func TestEnsurePanePathDirReensuresTheRecordedPath(t *testing.T) {
	var script string
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, s string, _ io.Reader, _ bool) ([]byte, error) {
		script = s
		return []byte("AF_PANE_LINK=/tmp/af-path.aBcDeF/sess-x\n"), nil
	}}
	w := &sandboxWorkspace{
		shell:       p,
		SessionDir:  "/srv/af:sessions/work.abc123",
		panePathDir: "/tmp/af-path.aBcDeF/sess-x",
	}
	require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
	assert.Equal(t, "/tmp/af-path.aBcDeF/sess-x", w.panePathDir)
	assert.Contains(t, script, `link=/tmp/af-path.aBcDeF/sess-x`,
		"the recorded link path is re-created verbatim")
	assert.NotContains(t, script, "mktemp", "a recorded path is never re-chosen")

	for _, bad := range []string{"/tmp/a:b/x", "tmp/af-path.x/sess"} {
		ran := false
		p := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
			ran = true
			return nil, nil
		}}
		w := &sandboxWorkspace{shell: p, SessionDir: "/srv/af:sessions/s", panePathDir: bad}
		require.Error(t, w.ensurePanePathDir(sshShortStepTimeout), bad)
		assert.False(t, ran, "a malformed recorded path is refused before any remote call")
	}
}

// The script itself fail-closes when the remote cannot supply a colon-free
// root: the provision error is the remote's own diagnostic plus the dir it
// was solving for, and no PATH dir is committed.
func TestEnsurePanePathDirFailsWhenTheRemoteCannotIndirection(t *testing.T) {
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, _ string, _ io.Reader, _ bool) ([]byte, error) {
		return []byte("af: no colon-free pane PATH indirection can be built: the remote login's $HOME contains ':' and the mktemp root /x:y/af-path.z contains ':' too"),
			errors.New("exit status 98")
	}}
	w := &sandboxWorkspace{shell: p, SessionDir: "/srv/af:sessions/work.abc123"}
	err := w.ensurePanePathDir(sshShortStepTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/srv/af:sessions/work.abc123")
	assert.Contains(t, err.Error(), "colon-free")
	assert.Empty(t, w.panePathDir)
}

// A remote that answers garbage must not be trusted: the link path is read
// from stdout only (#7770 R3 — a wrapper's stderr can never masquerade as
// the answer), and inside stdout only the marker line af printed is read,
// itself verified to be a single PATH entry before it is committed. Noise
// inside stdout must never refuse an otherwise working host either.
func TestEnsurePanePathDirVerifiesTheReportedLink(t *testing.T) {
	for label, out := range map[string]string{
		"relative":              "AF_PANE_LINK=home/u/.af-path/sess-x\n",
		"colon-bearing":         "AF_PANE_LINK=/home/u:a/.af-path/sess-x\n",
		"empty marker":          "AF_PANE_LINK=\n",
		"no marker":             "warning: ssh wrote a banner\n/home/u/.af-path/sess-x\n",
		"noise around a marker": "noise: traced argv\nAF_PANE_LINK=/home/u/.af-path/sess-x\nmore noise\n",
	} {
		t.Run(label, func(t *testing.T) {
			p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, _ string, _ io.Reader, _ bool) ([]byte, error) {
				return []byte(out), nil
			}}
			w := &sandboxWorkspace{shell: p, SessionDir: "/srv/af:sessions/work.abc123"}
			err := w.ensurePanePathDir(sshShortStepTimeout)
			if label == "noise around a marker" {
				require.NoError(t, err, "wrapper noise on stdout must not refuse a link that was built")
				assert.Equal(t, "/home/u/.af-path/sess-x", w.panePathDir)
				return
			}
			require.Error(t, err)
			assert.Empty(t, w.panePathDir)
		})
	}
}

// #7770 R1: run the REAL script under a local sh against a scratch HOME — the
// guards, the mktemp fallback, and the readlink proof are load-bearing shell,
// so a string assertion is not evidence they work.
//
// runLocalSh satisfies sandboxShell by executing the script verbatim with a
// controlled HOME and TMPDIR; combined output mirrors the production call.
type localSh struct {
	home   string
	tmpdir string
}

func (l *localSh) Run(_ time.Duration, script string, _ io.Reader, combined bool) ([]byte, error) {
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"HOME=" + l.home, "TMPDIR=" + l.tmpdir, "PATH=" + os.Getenv("PATH")}
	var out []byte
	var err error
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	return out, err
}

func TestEnsurePanePathDirRealScript(t *testing.T) {
	sess := filepath.Join(t.TempDir(), "af:sess", "work") // a ':'-bearing session dir that really exists
	require.NoError(t, os.MkdirAll(sess, 0o755))

	t.Run("colon-free HOME prefers the shared root", func(t *testing.T) {
		home := t.TempDir()
		w := &sandboxWorkspace{shell: &localSh{home: home}, SessionDir: sess}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))

		link := filepath.Join(home, ".af-path", panePathLinkName(sess))
		assert.Equal(t, link, w.panePathDir)
		target, err := os.Readlink(link)
		require.NoError(t, err, "the leaf must be a symlink")
		assert.Equal(t, sess, target, "readlink equals the session dir byte for byte")
		st, err := os.Stat(filepath.Dir(link))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), st.Mode().Perm(), "the shared root is private")
	})

	t.Run("a colon-bearing HOME falls back to mktemp outside it", func(t *testing.T) {
		tmp := t.TempDir()
		w := &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp}, SessionDir: sess}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))

		require.True(t, strings.HasPrefix(w.panePathDir, tmp+string(filepath.Separator)+"af-path."),
			"the root is an unpredictable mktemp dir under TMPDIR, got %q", w.panePathDir)
		target, err := os.Readlink(w.panePathDir)
		require.NoError(t, err)
		assert.Equal(t, sess, target)
		st, err := os.Stat(filepath.Dir(w.panePathDir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), st.Mode().Perm(), "the mktemp root is 0700")

		// And a second attempt must not collide with the first: mktemp gives a
		// distinct root even though the leaf name is the same digest.
		w2 := &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp}, SessionDir: sess}
		require.NoError(t, w2.ensurePanePathDir(sshShortStepTimeout))
		assert.NotEqual(t, w.panePathDir, w2.panePathDir,
			"two provisions for the same session dir never share a mktemp root")
		assert.Equal(t, filepath.Base(w.panePathDir), filepath.Base(w2.panePathDir),
			"the leaf name is the session-dir digest — the root, not the leaf, is the unique part")
	})

	t.Run("a planted directory at the leaf is refused, not written inside", func(t *testing.T) {
		home := t.TempDir()
		leaf := filepath.Join(home, ".af-path", panePathLinkName(sess))
		require.NoError(t, os.MkdirAll(leaf, 0o755)) // the planted directory

		w := &sandboxWorkspace{shell: &localSh{home: home}, SessionDir: sess}
		err := w.ensurePanePathDir(sshShortStepTimeout)
		require.Error(t, err)
		assert.Empty(t, w.panePathDir)
		assert.Empty(t, func() []string {
			e, _ := os.ReadDir(leaf)
			names := make([]string, len(e))
			for i, f := range e {
				names[i] = f.Name()
			}
			return names
		}(), "nothing was created inside the planted directory")
	})

	t.Run("a symlinked root is refused", func(t *testing.T) {
		home := t.TempDir()
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(home, ".af-path")))

		w := &sandboxWorkspace{shell: &localSh{home: home}, SessionDir: sess}
		require.Error(t, w.ensurePanePathDir(sshShortStepTimeout))
		assert.Empty(t, w.panePathDir)
	})

	t.Run("the ensure-at script recreates the recorded path", func(t *testing.T) {
		tmp := t.TempDir()
		link := filepath.Join(tmp, "af-path.Xyz", "sess-x")
		w := &sandboxWorkspace{shell: &localSh{home: t.TempDir()}, SessionDir: sess, panePathDir: link}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		target, err := os.Readlink(link)
		require.NoError(t, err)
		assert.Equal(t, sess, target)
	})
}
