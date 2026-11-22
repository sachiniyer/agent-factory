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
// choose script runs: it prefers the first per-uid root that verifies —
// $HOME/.af-path, then a uid-owned 0700 $XDG_RUNTIME_DIR/.af-path (#7787) —
// falls back to an unpredictable 0700 mktemp -d root outside them when none
// qualify (#7770 R4), refuses only when no colon-free spelling exists
// anywhere, verifies every root AFTER establishing it, and commits nothing
// until the leaf is PROVEN — a symlink whose readlink equals the session dir
// byte for byte inside a root that still verifies (#7770 R1).
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
	assert.Contains(t, script, `XDG_RUNTIME_DIR`, "a uid-owned 0700 XDG runtime dir is preferred over /tmp (#7787)")
	assert.Contains(t, script, `[ -O "$XDG_RUNTIME_DIR" ]`, "the XDG dir qualifies only when the session uid owns it")
	assert.Contains(t, script, `mktemp -d`, "a host with no per-uid root is handled by a mktemp root, never refused")
	assert.Contains(t, script, `base="${TMPDIR:-/tmp}"`, "the mktemp base defaults to /tmp")
	assert.Contains(t, script, `case "$base" in *:*) base=/tmp`, "a ':'-bearing TMPDIR falls back to the literal /tmp")
	assert.Contains(t, script, `case "$root" in *:*)`, "the root mktemp prints is itself re-checked for ':'")
	assert.Contains(t, script, `chmod 700`, "the indirection root is private to the session user")
	assert.Contains(t, script, `ln -sfn "$sess" "$link"`, "the link points at the real session dir")
	assert.Contains(t, script, `[ -d "$HOME" ]`, "$HOME is used only when it is a real directory")
	assert.Contains(t, script, `[ -e "$r" ] || { mkdir "$r" 2>/dev/null && chmod 700 "$r"`,
		"chmod runs only on a root af's own mkdir just created — never on a name another uid can swap (#7787 R1)")
	assert.NotContains(t, script, `chmod 700 "$r" 2>/dev/null; [`, "no second chmod may follow a `[ ! -L ]` check")
	assert.Contains(t, script, `[ -e "$r/$leaf" ] && [ ! -L "$r/$leaf" ]`,
		"a root whose leaf exists as a non-symlink is skipped before `ln` — implementations differ on ln -sfn into a real directory")
	assert.Contains(t, script, `loginctl show-user -p Linger`, "XDG qualifies only when linger keeps /run/user alive (#7787 R2)")
	assert.Contains(t, script, `/var/lib/systemd/linger/`, "the linger marker file is the first linger proof")
	assert.Contains(t, script, `[ -O "$r" ]`, "a root in a writable-by-others dir is only reused after ownership verifies (#7787)")
	assert.Contains(t, script, `ls -ld "$r"`, "the root's mode is verified where `stat` is not portable")
	assert.Contains(t, script, `[ "$(readlink "$link")" = "$sess" ]`,
		"the PATH entry is committed only after the link PROVES its target byte for byte")
	assert.Contains(t, script, `"$sess"/`+panePathMarkerName,
		"the committed link is recorded inside the session dir for the orphan sweep (#7787)")
	assert.False(t, combined, "the link path is parsed from stdout only — stderr stays out of the stream (#7770 R3)")
}

// A reattach carries the session's recorded link path — the persisted launch
// names it — so the link is re-created AT it whenever that root still
// verifies. When it does not (#7787: squatted, repointed, wrongly owned or
// moded), the same script mints a fresh root and reports the NEW link path —
// the recorded name is abandoned rather than refused on or written inside a
// stranger's dir — so mktemp is legitimately present as the fallback arm.
// A malformed record is still refused before any remote call.
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
	assert.Contains(t, script, `link=`+shellQuote("/tmp/af-path.aBcDeF/sess-x"),
		"the recorded link path is what the ensure first re-establishes")
	assert.Contains(t, script, `root="${link%/*}"`,
		"the recorded root is re-established, then verified — never trusted by name alone")
	assert.Contains(t, script, `mktemp -d`,
		"an unverifiable recorded root mints a fresh root instead of refusing (#7787)")
	assert.Contains(t, script, `AF_PANE_LINK=`,
		"the script reports the link it actually committed — the minted one when it had to remint")

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

// #7770 R1 and #7787: run the REAL script under a local sh against a scratch
// HOME, TMPDIR and XDG_RUNTIME_DIR — the per-uid root preference, the
// verify-after-create + remint fallback and the readlink proof are
// load-bearing shell, so a string assertion is not evidence they work.
//
// runLocalSh satisfies sandboxShell by executing the script verbatim with a
// controlled HOME, TMPDIR, XDG_RUNTIME_DIR and linger evidence (each absent
// when empty); stubDir prepends a directory of command stubs to PATH (e.g. a
// fake loginctl). Combined output mirrors the production call.
type localSh struct {
	home       string
	tmpdir     string
	xdg        string
	lingerFile string
	stubDir    string
}

func (l *localSh) Run(_ time.Duration, script string, _ io.Reader, combined bool) ([]byte, error) {
	cmd := exec.Command("sh", "-c", script)
	path := os.Getenv("PATH")
	if l.stubDir != "" {
		path = l.stubDir + string(os.PathListSeparator) + path
	}
	cmd.Env = []string{"HOME=" + l.home, "TMPDIR=" + l.tmpdir, "PATH=" + path}
	if l.xdg != "" {
		cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+l.xdg)
	}
	if l.lingerFile != "" {
		cmd.Env = append(cmd.Env, "AF_LINGER_FILE="+l.lingerFile)
	}
	var out []byte
	var err error
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	return out, err
}

// readMarker is the in-dir record the link step leaves for the orphan sweep:
// the one path it committed outside the session dir (#7787).
func readMarker(t *testing.T, sess string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sess, panePathMarkerName))
	require.NoError(t, err, "every committed link must be recorded inside the session dir")
	return strings.TrimSpace(string(b))
}

// writeStub drops an executable command stub into dir — a fake loginctl, say —
// so the real script answers a dictated system state through a stubbed PATH.
func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755))
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
		assert.Equal(t, link, readMarker(t, sess), "the marker names the committed link")
	})

	t.Run("a colon-bearing HOME prefers XDG_RUNTIME_DIR over /tmp only with linger", func(t *testing.T) {
		tmp, xdg := t.TempDir(), t.TempDir()
		require.NoError(t, os.Chmod(xdg, 0o700))
		lingerOn := filepath.Join(t.TempDir(), "linger-marker")
		require.NoError(t, os.WriteFile(lingerOn, []byte(""), 0o600))
		lingerOff := filepath.Join(t.TempDir(), "no-such-marker")

		// Linger proven by the marker file: /run/user survives the ssh
		// command's own logind session, so the link does too (#7787 R2).
		w := &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp, xdg: xdg, lingerFile: lingerOn}, SessionDir: sess}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))

		link := filepath.Join(xdg, ".af-path", panePathLinkName(sess))
		assert.Equal(t, link, w.panePathDir,
			"a uid-owned 0700 XDG runtime dir beats a /tmp mktemp root once linger is proven")
		target, err := os.Readlink(link)
		require.NoError(t, err)
		assert.Equal(t, sess, target)
		st, err := os.Stat(filepath.Dir(link))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), st.Mode().Perm(), "the XDG-rooted .af-path is private")
		assert.Equal(t, link, readMarker(t, sess))

		// Linger proven by loginctl instead of the marker file: a stub answers
		// "Linger=yes" and the root still lands under XDG.
		stubYes := t.TempDir()
		writeStub(t, stubYes, "loginctl", "#!/bin/sh\nif [ \"$1\" = \"show-user\" ]; then echo 'Linger=yes'; else exit 1; fi\n")
		require.NoError(t, os.RemoveAll(filepath.Join(xdg, ".af-path")))
		require.NoError(t, os.Remove(filepath.Join(sess, panePathMarkerName)))
		w = &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp, xdg: xdg, lingerFile: lingerOff, stubDir: stubYes}, SessionDir: sess}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		assert.Equal(t, link, w.panePathDir, "loginctl's Linger=yes is equivalent linger evidence")

		// Without linger the runtime dir is deleted under the session's own
		// logind session: the verified /tmp mktemp root, which survives, wins.
		for label, stub := range map[string]string{
			"loginctl says no": "#!/bin/sh\nif [ \"$1\" = \"show-user\" ]; then echo 'Linger=no'; else exit 1; fi\n",
			"loginctl fails":   "#!/bin/sh\nexit 1\n",
		} {
			stubDir := t.TempDir()
			writeStub(t, stubDir, "loginctl", stub)
			require.NoError(t, os.RemoveAll(filepath.Join(xdg, ".af-path")))
			require.NoError(t, os.Remove(filepath.Join(sess, panePathMarkerName)))
			w = &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp, xdg: xdg, lingerFile: lingerOff, stubDir: stubDir}, SessionDir: sess}
			require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout), label)
			assert.True(t, strings.HasPrefix(w.panePathDir, tmp+string(filepath.Separator)+"af-path."),
				label+": no linger proof means the durable /tmp root, got %q", w.panePathDir)
		}
	})

	t.Run("an unusable XDG_RUNTIME_DIR falls back to mktemp", func(t *testing.T) {
		tmp := t.TempDir()
		lingerOn := filepath.Join(t.TempDir(), "linger-marker")
		require.NoError(t, os.WriteFile(lingerOn, []byte(""), 0o600))
		for label, xdg := range map[string]func() string{
			"unset":         func() string { return "" },
			"relative":      func() string { return "run/user" },
			"colon-bearing": func() string { return "/run/user:x" },
			"nonexistent":   func() string { return filepath.Join(tmp, "no-such-xdg") },
			"mode 0755":     func() string { d := t.TempDir(); require.NoError(t, os.Chmod(d, 0o755)); return d },
			"a regular file": func() string {
				f := filepath.Join(tmp, "xdg-file")
				require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
				return f
			},
		} {
			t.Run(label, func(t *testing.T) {
				os.Remove(filepath.Join(sess, panePathMarkerName))
				w := &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp, xdg: xdg(), lingerFile: lingerOn}, SessionDir: sess}
				require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))

				require.True(t, strings.HasPrefix(w.panePathDir, tmp+string(filepath.Separator)+"af-path."),
					"no per-uid option means the unpredictable mktemp root under TMPDIR, got %q", w.panePathDir)
				target, err := os.Readlink(w.panePathDir)
				require.NoError(t, err)
				assert.Equal(t, sess, target)
				st, err := os.Stat(filepath.Dir(w.panePathDir))
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o700), st.Mode().Perm(), "the mktemp root is 0700")
				assert.Equal(t, w.panePathDir, readMarker(t, sess))
			})
		}
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
		assert.Equal(t, w.panePathDir, readMarker(t, sess), "the mktemp link is marker-recorded too")

		// And a second attempt must not collide with the first: mktemp gives a
		// distinct root even though the leaf name is the same digest.
		w2 := &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp}, SessionDir: sess}
		require.NoError(t, w2.ensurePanePathDir(sshShortStepTimeout))
		assert.NotEqual(t, w.panePathDir, w2.panePathDir,
			"two provisions for the same session dir never share a mktemp root")
		assert.Equal(t, filepath.Base(w.panePathDir), filepath.Base(w2.panePathDir),
			"the leaf name is the session-dir digest — the root, not the leaf, is the unique part")
	})

	t.Run("a planted directory at the leaf is skipped, not written inside", func(t *testing.T) {
		home, tmp := t.TempDir(), t.TempDir()
		leaf := filepath.Join(home, ".af-path", panePathLinkName(sess))
		require.NoError(t, os.MkdirAll(leaf, 0o755)) // the planted directory

		w := &sandboxWorkspace{shell: &localSh{home: home, tmpdir: tmp}, SessionDir: sess}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout),
			"a root af cannot commit into is skipped for a fresh one, never a refusal")
		assert.True(t, strings.HasPrefix(w.panePathDir, tmp+string(filepath.Separator)+"af-path."),
			"the link is committed under a different root, got %q", w.panePathDir)
		assert.Empty(t, func() []string {
			e, _ := os.ReadDir(leaf)
			names := make([]string, len(e))
			for i, f := range e {
				names[i] = f.Name()
			}
			return names
		}(), "nothing was created inside the planted directory")
	})

	t.Run("a symlinked root is skipped, never written through", func(t *testing.T) {
		home, tmp, target := t.TempDir(), t.TempDir(), t.TempDir()
		require.NoError(t, os.Symlink(target, filepath.Join(home, ".af-path")))

		w := &sandboxWorkspace{shell: &localSh{home: home, tmpdir: tmp}, SessionDir: sess}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		assert.True(t, strings.HasPrefix(w.panePathDir, tmp+string(filepath.Separator)+"af-path."),
			"the link went under a mktemp root, got %q", w.panePathDir)
		assert.Empty(t, func() []string {
			e, _ := os.ReadDir(target)
			names := make([]string, len(e))
			for i, f := range e {
				names[i] = f.Name()
			}
			return names
		}(), "nothing was written through the squatting symlink")
	})
}

// #7787: a recorded indirection root — for a mktemp root, a name inside a
// directory any local uid can write — is never trusted by name alone. The
// ensure-at script re-establishes it, PROVES it is a real uid-owned 0700 dir
// holding a free-or-symlink leaf, and mints a fresh root on ANY failure. Each
// squat shape below fails on master, which refused the run outright.
func TestEnsureAtRemintsARootItCannotVerify(t *testing.T) {
	sess := filepath.Join(t.TempDir(), "af:sess", "work")
	require.NoError(t, os.MkdirAll(sess, 0o755))
	leaf := panePathLinkName(sess)

	setup := func(t *testing.T) (tmp, link string, w *sandboxWorkspace) {
		tmp = t.TempDir()
		link = filepath.Join(tmp, "af-path.Rec", leaf)
		return tmp, link, &sandboxWorkspace{
			shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp}, SessionDir: sess, panePathDir: link}
	}

	// assertReminted is what every "the recorded root cannot be used" case must
	// produce: a NEW committed link under a different af-owned 0700 root, the
	// new path carried back to the caller, and the marker naming it.
	assertReminted := func(t *testing.T, tmp, link string, w *sandboxWorkspace) {
		t.Helper()
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		assert.NotEqual(t, link, w.panePathDir, "the squatted name is abandoned, not fought over")
		assert.True(t, strings.HasPrefix(w.panePathDir, tmp+string(filepath.Separator)+"af-path."),
			"the fresh root is a mktemp dir under TMPDIR, got %q", w.panePathDir)
		target, err := os.Readlink(w.panePathDir)
		require.NoError(t, err)
		assert.Equal(t, sess, target)
		st, err := os.Stat(filepath.Dir(w.panePathDir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), st.Mode().Perm())
		assert.Equal(t, w.panePathDir, readMarker(t, sess), "the record the sweep reads names the minted link")
	}

	t.Run("a squatter's symlink at the root", func(t *testing.T) {
		tmp, link, w := setup(t)
		target := t.TempDir()
		require.NoError(t, os.Chmod(target, 0o755))
		require.NoError(t, os.Symlink(target, filepath.Dir(link)))
		assertReminted(t, tmp, link, w)
		e, _ := os.ReadDir(target)
		assert.Empty(t, e, "nothing was written through the squatter's symlink")
		st, err := os.Stat(target)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), st.Mode().Perm(),
			"chmod never followed the swapped-in symlink to its target (#7787 R1)")
	})

	t.Run("a squatter's regular file at the root", func(t *testing.T) {
		tmp, link, w := setup(t)
		require.NoError(t, os.WriteFile(filepath.Dir(link), []byte("squat"), 0o600))
		assertReminted(t, tmp, link, w)
	})

	t.Run("a squatter's directory holding a planted leaf dir", func(t *testing.T) {
		tmp, link, w := setup(t)
		require.NoError(t, os.MkdirAll(link, 0o755)) // a real dir AT the leaf path
		assertReminted(t, tmp, link, w)
		e, _ := os.ReadDir(link)
		assert.Empty(t, e, "nothing was created inside the planted leaf")
	})

	t.Run("a squatter's directory holding a planted leaf file", func(t *testing.T) {
		tmp, link, w := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
		require.NoError(t, os.WriteFile(link, []byte("planted"), 0o600))
		assertReminted(t, tmp, link, w)
	})

	t.Run("the fresh root prefers a per-uid dir over /tmp when one exists", func(t *testing.T) {
		tmp, link, w := setup(t)
		xdg := t.TempDir()
		require.NoError(t, os.Chmod(xdg, 0o700))
		linger := filepath.Join(t.TempDir(), "me")
		require.NoError(t, os.WriteFile(linger, nil, 0o600))
		w.shell = &localSh{home: "/nonexistent:h:ome", tmpdir: tmp, xdg: xdg, lingerFile: linger}
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Dir(link))) // recorded root is squatted

		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		assert.Equal(t, filepath.Join(xdg, ".af-path", leaf), w.panePathDir,
			"a remint still prefers a private per-uid root, /tmp only when none qualifies")
	})
}

// #7787: a recorded root af itself made — removed while the session was away,
// or left with a mode a reboot relaxed — is re-established at the recorded
// path: an af-owned 0700 dir repairs to a verified state, never mints.
func TestEnsureAtRestoresItsOwnRecordedRoot(t *testing.T) {
	sess := filepath.Join(t.TempDir(), "af:sess", "work")
	require.NoError(t, os.MkdirAll(sess, 0o755))
	leaf := panePathLinkName(sess)

	t.Run("a removed root is recreated at the recorded path", func(t *testing.T) {
		tmp := t.TempDir()
		link := filepath.Join(tmp, "af-path.Rec", leaf)
		w := &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp}, SessionDir: sess, panePathDir: link}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		assert.Equal(t, link, w.panePathDir, "the verified recorded path is kept — the launch still names it")
		target, err := os.Readlink(link)
		require.NoError(t, err)
		assert.Equal(t, sess, target)
		assert.Equal(t, link, readMarker(t, sess))

		require.NoError(t, os.RemoveAll(filepath.Dir(link)))
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		target, err = os.Readlink(link)
		require.NoError(t, err, "a root removed while the session was away is re-created")
		assert.Equal(t, sess, target)
	})

	t.Run("a root at the wrong mode is abandoned, never chmodded in place", func(t *testing.T) {
		tmp := t.TempDir()
		link := filepath.Join(tmp, "af-path.Rec", leaf)
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755)) // wrong mode, still ours
		w := &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp}, SessionDir: sess, panePathDir: link}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		assert.NotEqual(t, link, w.panePathDir,
			"an existing name in a shared dir is never mode-repaired — a fresh root is minted (#7787 R1)")
		st, err := os.Stat(filepath.Dir(link))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), st.Mode().Perm(),
			"and the mode of the existing name is left exactly as it was")
	})

	t.Run("a leaf repointed elsewhere is re-pinned to this session", func(t *testing.T) {
		tmp := t.TempDir()
		link := filepath.Join(tmp, "af-path.Rec", leaf)
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
		require.NoError(t, os.Symlink("/etc", link))
		w := &sandboxWorkspace{shell: &localSh{home: "/nonexistent:h:ome", tmpdir: tmp}, SessionDir: sess, panePathDir: link}
		require.NoError(t, w.ensurePanePathDir(sshShortStepTimeout))
		target, err := os.Readlink(link)
		require.NoError(t, err)
		assert.Equal(t, sess, target, "ln -sfn re-pins the leaf to THIS session dir")
	})
}
