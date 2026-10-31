package session

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Write-back is a versioned compare-and-swap from a bounded stream (#5864, #5865).
// These tables run the interleavings two sessions on ONE registry account can
// produce, against a real filesystem.

type sharedAccount struct {
	t        *testing.T
	registry string // the registry's credential file
	a, b     *sandboxWorkspace
}

func newSharedAccount(t *testing.T) *sharedAccount {
	t.Helper()
	acct := writeAccount(t, "claude", "v0")
	s := &sharedAccount{t: t, registry: filepath.Join(acct.Dir, ".credentials.json"),
		a: workspaceOn(t, acct, "box1"), b: workspaceOn(t, acct, "box1")}
	require.NoError(t, s.a.syncAccountIn(time.Minute))
	require.NoError(t, s.b.syncAccountIn(time.Minute))
	return s
}

// refresh is the agent on the host rotating its token inside its own scope.
func (s *sharedAccount) refresh(w *sandboxWorkspace, token string) {
	s.t.Helper()
	require.NoError(s.t, os.WriteFile(filepath.Join(w.AccountDir(), ".credentials.json"), []byte(token), 0o600))
}

func (s *sharedAccount) reg() string {
	s.t.Helper()
	b, err := os.ReadFile(s.registry)
	require.NoError(s.t, err)
	return string(b)
}

func (s *sharedAccount) writeBack(w *sandboxWorkspace) accountWriteBack {
	s.t.Helper()
	res, err := w.writeBackAccount(time.Minute)
	require.NoError(s.t, err)
	return res
}

func TestWriteBackInterleavingsNeverLetAStaleSnapshotWin(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, s *sharedAccount)
	}{
		{"A refreshes, then B (unchanged, stale) tears down", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			assert.Equal(t, 1, s.writeBack(s.a).Written)
			res := s.writeBack(s.b) // B's host copy is still v0: not a refresh
			assert.Zero(t, res.Written)
			assert.Empty(t, res.Skipped, "an unchanged file is not news, so it is not even a conflict")
			assert.Equal(t, "A1", s.reg(), "B's stale snapshot must not roll A's refresh back (#5864)")
		}},
		{"B tears down first (unchanged), then A refreshes", func(t *testing.T, s *sharedAccount) {
			assert.Zero(t, s.writeBack(s.b).Written)
			assert.Equal(t, "v0", s.reg())
			s.refresh(s.a, "A1")
			assert.Equal(t, 1, s.writeBack(s.a).Written)
			assert.Equal(t, "A1", s.reg())
		}},
		{"both refreshed: the second writer loses, visibly", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			s.refresh(s.b, "B1")
			assert.Equal(t, 1, s.writeBack(s.a).Written)
			res := s.writeBack(s.b)
			assert.Zero(t, res.Written)
			assert.Equal(t, []string{".credentials.json"}, res.Skipped, "the skip is reported, not silent")
			assert.Equal(t, "A1", s.reg(), "a fresher registry is never overwritten by an older baseline")
		}},
		{"the same, in the other order", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			s.refresh(s.b, "B1")
			assert.Equal(t, 1, s.writeBack(s.b).Written)
			assert.Equal(t, []string{".credentials.json"}, s.writeBack(s.a).Skipped)
			assert.Equal(t, "B1", s.reg())
		}},
		{"a session keeps writing back from what it last wrote", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			assert.Equal(t, 1, s.writeBack(s.a).Written)
			s.refresh(s.a, "A2")
			assert.Equal(t, 1, s.writeBack(s.a).Written, "the baseline advanced with the write")
			assert.Equal(t, "A2", s.reg())
			assert.Zero(t, s.writeBack(s.a).Written, "and an unchanged file after that is not a refresh")
		}},
		{"the account was removed: never resurrected", func(t *testing.T, s *sharedAccount) {
			require.NoError(t, os.Remove(s.registry))
			s.refresh(s.a, "A1")
			res := s.writeBack(s.a)
			assert.Zero(t, res.Written)
			assert.Equal(t, []string{".credentials.json"}, res.Skipped)
			assert.NoFileExists(t, s.registry)
		}},
		{"a process with no sync-in baseline writes nothing back", func(t *testing.T, s *sharedAccount) {
			s.a.syncedVersion = nil
			s.refresh(s.a, "A1")
			res := s.writeBack(s.a)
			assert.Zero(t, res.Written)
			assert.Equal(t, []string{".credentials.json"}, res.Skipped)
			assert.Equal(t, "v0", s.reg())
		}},
		{"teardown goes through the same rule", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			require.NoError(t, s.a.teardownAccount(time.Minute))
			require.NoError(t, s.b.teardownAccount(time.Minute))
			assert.Equal(t, "A1", s.reg())
			assert.NoDirExists(t, s.a.AccountDir())
			assert.NoDirExists(t, s.b.AccountDir())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newSharedAccount(t)) })
	}
}

// Two teardowns racing, both holding a refresh: exactly one wins, whole — the
// registry is never a mix and never lost.
func TestConcurrentTeardownsPickExactlyOneWinner(t *testing.T) {
	for i := 0; i < 25; i++ {
		s := newSharedAccount(t)
		s.refresh(s.a, "A1")
		s.refresh(s.b, "B1")
		var wg sync.WaitGroup
		results := make([]accountWriteBack, 2)
		for n, w := range []*sandboxWorkspace{s.a, s.b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := w.writeBackAccount(time.Minute)
				assert.NoError(t, err)
				results[n] = res
			}()
		}
		wg.Wait()
		assert.Equal(t, 1, results[0].Written+results[1].Written, "exactly one write-back lands")
		assert.Equal(t, 1, len(results[0].Skipped)+len(results[1].Skipped), "the other is a reported skip")
		assert.Contains(t, []string{"A1", "B1"}, s.reg())
	}
}

// --- the bound ---

// archiveOf builds a tar of the named files.
func archiveOf(t *testing.T, files map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range order {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(files[name])), Typeflag: tar.TypeReg}))
		_, err := tw.Write(files[name])
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// A host that sends an oversized or truncated archive is refused whole, and the
// registry is left exactly as it was — nothing is written until every entry parsed.
func TestWriteBackRefusesAnOversizedOrTruncatedArchive(t *testing.T) {
	acct := writeAccount(t, "gemini", "g0")
	creds := map[string][]byte{}
	rels := accountCredentialRel["gemini"]
	for _, rel := range rels {
		creds[rel] = []byte("refreshed-" + filepath.Base(rel))
	}
	good := archiveOf(t, creds, rels)

	for _, tc := range []struct {
		name string
		out  []byte
		want string
	}{
		{"oversized: more bytes than the bound", bytes.Repeat([]byte("x"), int(accountArchiveLimit("gemini"))+10), "bound"},
		{"truncated mid-entry", good[:len(good)-700], "unreadable archive"},
		{"truncated after the first entry", good[:512+512+512], ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &sandboxWorkspace{shell: fixedOutputShell{out: tc.out}, SessionDir: "/s",
				spec:          ProvisionSpec{Account: acct, AccountSyncHost: "box1"},
				syncedVersion: map[string]string{}}
			for _, rel := range rels {
				b, _ := os.ReadFile(filepath.Join(acct.Dir, filepath.FromSlash(rel)))
				if b != nil {
					w.syncedVersion[rel] = accountVersion(b)
				}
			}
			before, _ := os.ReadFile(filepath.Join(acct.Dir, rels[0]))
			_, err := w.writeBackAccount(time.Second)
			require.Error(t, err)
			if tc.want != "" {
				assert.Contains(t, err.Error(), tc.want)
			}
			after, _ := os.ReadFile(filepath.Join(acct.Dir, rels[0]))
			assert.Equal(t, string(before), string(after), "a refused archive writes nothing, not even its valid entries")
		})
	}
}

// The bound holds WHILE STREAMING: past the limit the writer refuses and cancels,
// and never holds more than the limit.
func TestLimitWriterNeverHoldsPastItsLimit(t *testing.T) {
	cancelled := false
	lw := &limitWriter{limit: 100, cancel: func() { cancelled = true }}
	n, err := lw.Write(bytes.Repeat([]byte("a"), 60))
	require.NoError(t, err)
	assert.Equal(t, 60, n)
	n, err = lw.Write(bytes.Repeat([]byte("b"), 60))
	require.ErrorIs(t, err, ErrOutputOverLimit)
	assert.Equal(t, 40, n, "only the room that was left is accepted")
	assert.True(t, cancelled, "the command is cancelled the moment the limit is crossed")
	assert.LessOrEqual(t, lw.buf.Len(), 100)
	_, err = lw.Write([]byte("more"))
	require.ErrorIs(t, err, ErrOutputOverLimit)
	assert.LessOrEqual(t, lw.buf.Len(), 100)
}

// The production path: a command that would stream forever is killed at the bound,
// through the real command composition, and returns promptly with the bound error.
func TestRunLimitedKillsAnEndlessStreamAtTheBound(t *testing.T) {
	p := &sandboxProvisioner{sshCmd: "sh -c"} // runs the script locally, same composition as ssh
	start := time.Now()
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = p.RunLimited(30*time.Second, "yes x", 4096)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("RunLimited did not return: the endless stream was not killed at the bound")
	}
	require.ErrorIs(t, err, ErrOutputOverLimit)
	assert.Nil(t, out, "nothing past the bound is returned")
	assert.Less(t, time.Since(start), 15*time.Second)

	// Within the bound it behaves like Run.
	ok, err := p.RunLimited(30*time.Second, "printf hello", 4096)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(ok))
}

// Reaching the bound through the shell interface is the same refusal.
type overLimitShell struct{}

func (overLimitShell) Run(time.Duration, string, io.Reader, bool) ([]byte, error) {
	return nil, errors.New("unused")
}
func (overLimitShell) RunLimited(time.Duration, string, int64) ([]byte, error) {
	return nil, ErrOutputOverLimit
}

func TestWriteBackUsesTheBoundedStreamWhenTheShellOffersIt(t *testing.T) {
	acct := writeAccount(t, "claude", "v0")
	w := &sandboxWorkspace{shell: overLimitShell{}, SessionDir: "/s", spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1"}}
	_, err := w.writeBackAccount(time.Second)
	require.ErrorIs(t, err, ErrOutputOverLimit)
	_ = context.Background()
}

// --- #5928 (go/zipslip): the archive is untrusted, and no archive-controlled name
// ever reaches a path. Every hostile shape is refused WHOLE — including when a valid
// entry precedes it — and nothing is written, inside the registry or outside it.

func hostileArchive(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		require.NoError(t, tw.WriteHeader(h))
		if h.Size > 0 {
			_, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func TestReadAccountArchiveRefusesEveryHostileNameWhole(t *testing.T) {
	valid := &tar.Header{Name: ".credentials.json", Mode: 0o600, Size: 4, Typeflag: tar.TypeReg}
	for name, hostile := range map[string]*tar.Header{
		"dot-dot":                {Name: "../x", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"deep dot-dot":           {Name: "../../etc/x", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"dot-slash dotdot":       {Name: "./../x", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"absolute":               {Name: "/etc/x", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"nested absolute":        {Name: "/home/u/.claude/.credentials.json", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"unlisted":               {Name: "settings.json", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"symlink":                {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"symlink by listed name": {Name: ".credentials.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hardlink":               {Name: "h", Typeflag: tar.TypeLink, Linkname: ".credentials.json"},
		"directory":              {Name: "d/", Typeflag: tar.TypeDir, Mode: 0o700},
	} {
		t.Run(name, func(t *testing.T) {
			for _, order := range [][]*tar.Header{{hostile}, {valid, hostile}, {hostile, valid}} {
				_, err := readAccountArchive(hostileArchive(t, order...), accountCredentialRel["claude"])
				require.Error(t, err, "the archive is rejected whole, whatever order the hostile entry is in")
			}
		})
	}
	// Control: the valid entry alone is accepted, keyed by the allowlisted name.
	got, err := readAccountArchive(hostileArchive(t, valid), accountCredentialRel["claude"])
	require.NoError(t, err)
	assert.Contains(t, got, ".credentials.json")
	for key := range got {
		assert.True(t, filepath.IsLocal(key))
	}
}

func TestHostileArchiveWritesNothingAnywhere(t *testing.T) {
	outside := t.TempDir()
	acct := writeAccount(t, "claude", "v0")
	w := &sandboxWorkspace{SessionDir: "/s", spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1"},
		syncedVersion: map[string]string{".credentials.json": accountVersion([]byte("v0"))}}
	for _, name := range []string{"../" + filepath.Base(outside) + "/pwned", outside + "/pwned"} {
		w.shell = fixedOutputShell{out: hostileArchive(t,
			&tar.Header{Name: ".credentials.json", Mode: 0o600, Size: 5, Typeflag: tar.TypeReg},
			&tar.Header{Name: name, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})}
		_, err := w.writeBackAccount(time.Second)
		require.Error(t, err)
		got, _ := os.ReadFile(filepath.Join(acct.Dir, ".credentials.json"))
		assert.Equal(t, "v0", string(got), "not even the valid first entry is written")
		entries, _ := os.ReadDir(outside)
		assert.Empty(t, entries, "nothing is created outside the registry")
	}
}

// --- #5902: the registry is replaced ONLY from a fully validated archive.

// failingTar stands in for the host's tar: it writes a complete, valid archive and
// then exits non-zero, as GNU tar does for "File shrank … padding with zeros".
func failingTar(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar on this machine")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + real + " \"$@\"\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tar"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWriteBackKeepsCredentialsWhenTarFails(t *testing.T) {
	s := newSharedAccount(t)
	s.refresh(s.a, "A1-refreshed")
	failingTar(t)
	_, err := s.a.writeBackAccount(time.Minute)
	require.Error(t, err, "tar's failure must fail the read — no limiter in the pipeline may mask it")
	assert.Equal(t, "v0", s.reg(), "the existing credential is kept")
}

func TestWriteBackRefusesCorruptedContentWhateverTheTransportSaid(t *testing.T) {
	for name, content := range map[string][]byte{
		"zero-padded (a file that shrank under tar)": append([]byte("{\"t\":1}"), make([]byte, 64)...),
		"empty (truncated to nothing)":               {},
		"NUL in the middle":                          []byte("ab\x00cd"),
	} {
		t.Run(name, func(t *testing.T) {
			acct := writeAccount(t, "claude", "v0")
			w := &sandboxWorkspace{SessionDir: "/s", spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1"},
				syncedVersion: map[string]string{".credentials.json": accountVersion([]byte("v0"))},
				shell:         fixedOutputShell{out: archiveOf(t, map[string][]byte{".credentials.json": content}, []string{".credentials.json"})}}
			_, err := w.writeBackAccount(time.Second)
			require.Error(t, err)
			got, _ := os.ReadFile(filepath.Join(acct.Dir, ".credentials.json"))
			assert.Equal(t, "v0", string(got), "a valid credential is never replaced by corrupted content")
		})
	}
}

func TestWriteBackRefusesDuplicateAndUnterminatedArchives(t *testing.T) {
	acct := writeAccount(t, "claude", "v0")
	mk := func(out []byte) *sandboxWorkspace {
		return &sandboxWorkspace{SessionDir: "/s", spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1"},
			syncedVersion: map[string]string{".credentials.json": accountVersion([]byte("v0"))}, shell: fixedOutputShell{out: out}}
	}
	one := map[string][]byte{".credentials.json": []byte("fresh")}
	good := archiveOf(t, one, []string{".credentials.json"})

	_, err := mk(archiveOf(t, one, []string{".credentials.json", ".credentials.json"})).writeBackAccount(time.Second)
	require.ErrorContains(t, err, "twice")

	// Cut at a block boundary after a whole entry: no end-of-archive marker.
	_, err = mk(good[:1024]).writeBackAccount(time.Second)
	require.ErrorContains(t, err, "end-of-archive")

	got, _ := os.ReadFile(filepath.Join(acct.Dir, ".credentials.json"))
	assert.Equal(t, "v0", string(got))
	_, err = mk(good).writeBackAccount(time.Second)
	require.NoError(t, err, "control: the intact archive is accepted")
}

// --- #5901: the bound is tar's own arithmetic, so a credential AT the cap fits.

func TestAccountArchiveLimitIsTarsRealArithmetic(t *testing.T) {
	// One max-size file under GNU tar's default 10,240-byte record: 512 header +
	// 1 MiB data + 1,024 trailer = 1,050,112, padded up to 1,054,720.
	assert.Equal(t, int64(1054720), tarArchiveSize([]int64{accountSyncMaxFileBytes}, tarRecord))
	assert.Equal(t, int64(1050112), tarArchiveSize([]int64{accountSyncMaxFileBytes}, tarBlock), "with -b 1 the padding is a block")
	assert.GreaterOrEqual(t, accountArchiveLimit("claude"), int64(1054720))

	// And the real thing: a credential of EXACTLY the cap, archived by this
	// machine's tar with its defaults, fits under the bound.
	real, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar on this machine")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"), bytes.Repeat([]byte("a"), accountSyncMaxFileBytes), 0o600))
	cmd := exec.Command(real, "-cf", "-", "-C", dir, "auth.json")
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.LessOrEqual(t, int64(len(out)), accountArchiveLimit("codex"), "a max-size credential must not be refused for tar's own padding")
}

func TestWriteBackAcceptsACredentialExactlyAtTheCap(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar on this machine")
	}
	big := func(b byte) []byte { return bytes.Repeat([]byte{b}, accountSyncMaxFileBytes) }
	acct := writeAccount(t, "claude", string(big('a')))
	w := workspaceOn(t, acct, "box1")
	require.NoError(t, w.syncAccountIn(time.Minute), "sync-in admits a credential of exactly the cap")
	require.NoError(t, os.WriteFile(filepath.Join(w.AccountDir(), ".credentials.json"), big('b'), 0o600))

	res, err := w.writeBackAccount(time.Minute)
	require.NoError(t, err, "write-back must not refuse what sync-in admitted")
	assert.Equal(t, 1, res.Written)
	got, _ := os.ReadFile(filepath.Join(acct.Dir, ".credentials.json"))
	assert.Equal(t, big('b'), got)
}
