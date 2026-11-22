package session

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The indirection's leaf name is derived from the session dir alone — no
// stored field — so provision, teardown, a restored cleanup handle and the
// orphan sweep all name the same link. It must be colon-free itself (it sits
// inside the PATH entry) and distinct per session dir so two sessions can
// never collide.
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
// remote round trip may be spent proving it.
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

// A ':'-bearing session dir gets the indirection: a symlink to it under the
// remote login's $HOME, created race-safely (af-owned, chmod 700, a planted
// non-symlink at the leaf refused before `ln` — busybox's ln -sfn would write
// inside a real directory), and the launch's PATH entry becomes the link's
// expanded, colon-free spelling.
func TestEnsurePanePathDirBuildsTheIndirection(t *testing.T) {
	var script string
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, s string, _ io.Reader, _ bool) ([]byte, error) {
		script = s
		return []byte("AF_PANE_LINK=/home/u/.af-path/" + panePathLinkName("/srv/af:sessions/work.abc123") + "\n"), nil
	}}
	dir := "/srv/af:sessions/work.abc123"
	w := &sandboxWorkspace{shell: p, SessionDir: dir}
	require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))

	name := panePathLinkName(dir)
	assert.Equal(t, "/home/u/.af-path/"+name, w.panePathDir,
		"the PATH entry becomes the indirection's colon-free spelling, not the session dir")
	assert.Contains(t, script, ".af-path", "the indirection lives under an af-owned dir")
	assert.Contains(t, script, "chmod 700", "the indirection root is private to the session user")
	assert.Contains(t, script, `ln -sfn `+shellQuote(dir), "the link points at the real session dir")
	assert.Contains(t, script, "-L ", "a symlinked root is refused rather than written inside")
	assert.Contains(t, script, `[ -e "$link" ] && [ ! -L "$link" ]`,
		"an existing non-symlink at the leaf is refused before `ln` — implementations differ on ln -sfn into a real directory")
	assert.NotContains(t, script, "ln -sfn $", "the target is quoted literally, never re-expanded")
}

// The script itself fail-closes when the remote cannot supply a colon-free
// root (a ':'-bearing $HOME): the provision error is the remote's own
// diagnostic plus the dir it was solving for, and no PATH dir is committed.
func TestEnsurePanePathDirFailsWhenTheRemoteCannotIndirection(t *testing.T) {
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, _ string, _ io.Reader, _ bool) ([]byte, error) {
		return []byte("af: the remote login's $HOME contains ':', so no colon-free pane PATH indirection can be built under it"),
			errors.New("exit status 92")
	}}
	w := &sandboxWorkspace{shell: p, SessionDir: "/srv/af:sessions/work.abc123"}
	err := w.ensurePanePathDir(sshShortStepTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/srv/af:sessions/work.abc123")
	assert.Contains(t, err.Error(), "$HOME contains ':'")
	assert.Empty(t, w.panePathDir)
}

// A remote that answers garbage must not be trusted: only the marker line af
// printed is read as the link path, and it is itself verified to be a single
// PATH entry before it is committed. Transport noise — a wrapper's stderr
// mixed into the combined stream — can never masquerade as the answer, and
// must never refuse an otherwise working host (#7770 review).
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
				require.NoError(t, err, "wrapper noise on the combined stream must not refuse a link that was built")
				assert.Equal(t, "/home/u/.af-path/sess-x", w.panePathDir)
				return
			}
			require.Error(t, err)
			assert.Empty(t, w.panePathDir)
		})
	}
}

// The early refusal: a session dir that will carry ':' AND a remote login
// whose $HOME carries ':' leave af no colon-free spelling for the
// indirection. The refusal names the field and the character, runs before
// anything is staged, and leaves the attempt reapable as pre-launch.
func TestCheckPanePathFeasibility(t *testing.T) {
	probe := func(home string, err error, ran *bool) func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
			*ran = true
			assert.Contains(t, script, "$HOME", "the probe reads the remote login's $HOME")
			if home == "" {
				return nil, err
			}
			return []byte("AF_PANE_HOME=" + home + "\n"), err
		}
	}

	t.Run("colon-free workspace_root never probes", func(t *testing.T) {
		ran := false
		w := &sandboxWorkspace{
			shell: &sandboxProvisioner{runCommandFn: probe("/h:me", nil, &ran)},
			spec:  ProvisionSpec{WorkspaceRoot: "/srv/af-sessions"},
		}
		require.NoError(t, w.checkPanePathFeasibility(sshShortStepTimeout))
		assert.False(t, ran, "a colon-free root needs no $HOME probe")
	})

	t.Run("colon-bearing workspace_root with clean HOME proceeds", func(t *testing.T) {
		ran := false
		w := &sandboxWorkspace{
			shell: &sandboxProvisioner{runCommandFn: probe("/home/u", nil, &ran)},
			spec:  ProvisionSpec{WorkspaceRoot: "/srv/af:sessions"},
		}
		require.NoError(t, w.checkPanePathFeasibility(sshShortStepTimeout))
		assert.True(t, ran)
	})

	t.Run("HOME under a colon is refused before staging, naming field and character", func(t *testing.T) {
		ran := false
		w := &sandboxWorkspace{
			shell: &sandboxProvisioner{runCommandFn: probe("/h:me", nil, &ran)},
			spec:  ProvisionSpec{},
		}
		err := w.checkPanePathFeasibility(sshShortStepTimeout)
		require.Error(t, err)
		assert.True(t, ran)
		assert.Contains(t, err.Error(), "$HOME")
		assert.Contains(t, err.Error(), "':'")
		assert.Contains(t, err.Error(), "workspace_root", "the actionable field must be named")
	})

	t.Run("colon workspace_root plus colon HOME is refused, naming workspace_root", func(t *testing.T) {
		w := &sandboxWorkspace{
			shell: &sandboxProvisioner{runCommandFn: probe("/h:me", nil, new(bool))},
			spec:  ProvisionSpec{WorkspaceRoot: "/srv/af:sessions"},
		}
		err := w.checkPanePathFeasibility(sshShortStepTimeout)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "workspace_root")
		assert.Contains(t, err.Error(), "/srv/af:sessions")
		assert.Contains(t, err.Error(), "':'")
	})

	t.Run("a colon-bearing preserved dir plus colon HOME is refused", func(t *testing.T) {
		ran := false
		w := &sandboxWorkspace{
			shell: &sandboxProvisioner{runCommandFn: probe("/h:me", nil, &ran)},
			spec:  ProvisionSpec{Adopt: &AdoptSpec{SessionDir: "/preserved/a:b"}},
		}
		err := w.checkPanePathFeasibility(sshShortStepTimeout)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/preserved/a:b")
		assert.Contains(t, err.Error(), "':'")
	})

	t.Run("a colon-free preserved dir never probes", func(t *testing.T) {
		ran := false
		w := &sandboxWorkspace{
			shell: &sandboxProvisioner{runCommandFn: probe("/h:me", nil, &ran)},
			spec:  ProvisionSpec{Adopt: &AdoptSpec{SessionDir: "/preserved/ab"}},
		}
		require.NoError(t, w.checkPanePathFeasibility(sshShortStepTimeout))
		assert.False(t, ran)
	})

	t.Run("an unanswered probe must not refuse a host that may be fine", func(t *testing.T) {
		ran := false
		w := &sandboxWorkspace{
			shell: &sandboxProvisioner{runCommandFn: probe("", errors.New("ssh: connection refused"), &ran)},
			spec:  ProvisionSpec{WorkspaceRoot: "/srv/af:sessions"},
		}
		require.NoError(t, w.checkPanePathFeasibility(sshShortStepTimeout),
			"a transport failure here would refuse every working host; the link step fail-closes instead")
		assert.True(t, ran)
	})
}
