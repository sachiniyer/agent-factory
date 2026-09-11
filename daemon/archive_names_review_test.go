package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

type archiveScopeBackend struct {
	readyFakeBackend
	kind string
}

func (b archiveScopeBackend) Type() string { return b.kind }
func (b archiveScopeBackend) Capabilities() session.Capabilities {
	caps := b.readyFakeBackend.Capabilities()
	if b.kind != "local" {
		caps.Workspace = session.WorkspaceRemote
	}
	return caps
}

// Exercise the real create admission boundary without provisioning off-box runtimes.
func TestArchiveDirectoryBackendScope(t *testing.T) {
	for _, pair := range [][2]string{{"docker", "docker"}, {"ssh", "ssh"}, {"local", "docker"}, {"docker", "local"}, {"local", "local"}} {
		for _, source := range []string{"reserved", "persisted"} {
			t.Run(pair[0]+"_then_"+pair[1]+"_"+source, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				repoPath := setupControlRepo(t)
				m, err := NewManager(config.DefaultConfig())
				require.NoError(t, err)
				repo, _, release, _, err := m.reserveCreate(CreateSessionRequest{Title: "feature/login", RepoPath: repoPath, Program: "claude", Backend: pair[0]})
				require.NoError(t, err)
				if source == "persisted" {
					release()
					require.NoError(t, appendInstanceData(repo.ID, session.InstanceData{Title: "feature/login", Path: repoPath, Status: session.Archived, BackendType: pair[0]}))
				} else {
					defer release()
				}
				_, _, releaseSecond, _, err := m.reserveCreate(CreateSessionRequest{Title: "feature-login", RepoPath: repoPath, Program: "claude", Backend: pair[1]})
				if releaseSecond != nil {
					defer releaseSecond()
				}
				if pair == [2]string{"local", "local"} {
					require.ErrorContains(t, err, "archive directory")
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestArchiveDirectoryPortableComparison(t *testing.T) {
	for _, pair := range [][2]string{{"Caf\u00e9", "Cafe\u0301"}, {"Feature", "feature"}, {"x/Σz", "x-ςz"}, {"x/Σz", "x-σz"}, {"x/\u017f\u0301z", "x-\u015az"}, {"Feature", "unrelated"}} {
		for _, source := range []string{"live", "disk", "reserved"} {
			t.Run(pair[0]+"_"+pair[1]+"_"+source, func(t *testing.T) {
				m := &Manager{instances: make(map[string]*session.Instance), reservedTitles: make(map[string]struct{}), reservedArchiveTitles: make(map[string]struct{})}
				var disk []session.InstanceData
				switch source {
				case "live":
					m.instances[daemonInstanceKey("repo", pair[0])] = &session.Instance{Title: pair[0]}
				case "disk":
					disk = []session.InstanceData{{Title: pair[0], Status: session.Archived}}
				case "reserved":
					m.reservedTitles[daemonInstanceKey("repo", pair[0])] = struct{}{}
					m.reservedArchiveTitles[daemonInstanceKey("repo", pair[0])] = struct{}{}
				}
				err := m.validateArchiveTitleLocked("repo", pair[1], disk, nil, false)
				if pair[1] == "unrelated" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "archive directory")
				}
			})
		}
	}
}

func TestArchiveDirectoryIgnoresRemoteClaims(t *testing.T) {
	for _, source := range []string{"live", "disk", "reserved"} {
		for _, kind := range []string{"docker", "ssh", "remote"} {
			t.Run(source+"_"+kind, func(t *testing.T) {
				m := &Manager{instances: make(map[string]*session.Instance), reservedTitles: make(map[string]struct{})}
				var disk []session.InstanceData
				const title = "feature/login"
				switch source {
				case "live":
					inst := &session.Instance{Title: title}
					inst.SetBackend(archiveScopeBackend{readyFakeBackend{session.NewFakeBackend()}, kind})
					m.instances[daemonInstanceKey("repo", title)] = inst
				case "disk":
					disk = []session.InstanceData{{Title: title, BackendType: kind, Status: session.Archived}}
				case "reserved":
					m.reservedTitles[daemonInstanceKey("repo", title)] = struct{}{}
				}
				require.NoError(t, m.validateArchiveTitleLocked("repo", "feature-login", disk, nil, false))
			})
		}
	}
}

func TestArchiveTitleKeyNormalizesFoldOutput(t *testing.T) {
	// Long s plus acute folds to a decomposed s plus acute; capital s with
	// acute folds to a precomposed small s with acute. Both keys must be NFC.
	require.Equal(t, archiveTitleKey("x/\u017f\u0301z"), archiveTitleKey("x-\u015az"))
	require.Equal(t, "x-\u015bz", archiveTitleKey("x/\u017f\u0301z"))
}

// TestSanitizeArchiveTitleBoundsLongTitle is the archive-side regression for the
// long-title class that #2528 bounded on the worktree/branch/slug paths: a title
// creatable via the CLI/RPC/HTTP (no TUI 32-char cap, since the authoritative
// validateTitleShapeLocked checks only shape) must not derive an archive
// directory leaf that overruns NAME_MAX and fails with "file name too long".
// Against the unbounded sanitizer these leaves are 400–600 bytes.
func TestSanitizeArchiveTitleBoundsLongTitle(t *testing.T) {
	const nameMax = 255
	for _, tc := range []struct {
		name, title string
	}{
		{"ascii_500", strings.Repeat("a", 500)},
		{"exactly_255", strings.Repeat("a", 255)},
		{"exactly_256", strings.Repeat("a", 256)},
		{"with_slashes", strings.Repeat("a/", 200)},
		{"with_dotdot", strings.Repeat("ab..", 160)},
		{"leading_separators", "---" + strings.Repeat("a", 300)},
		{"trailing_dashes", strings.Repeat("a", 254) + "--"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeArchiveTitle(tc.title)
			require.NotEmpty(t, got, "leaf must never be empty")
			require.LessOrEqualf(t, len(got), nameMax, "leaf %d bytes over NAME_MAX %d: %q", len(got), nameMax, got)
		})
	}

	// The bound runs AFTER the leading-only trim and preserves trailing
	// separators — the report's reason for TrimLeft rather than Trim. A 256-byte
	// "a…a--" leaf cuts to 255 ending in a single '-' that must survive.
	require.Equal(t, strings.Repeat("a", 254)+"-", sanitizeArchiveTitle(strings.Repeat("a", 254)+"--"))

	// Short / empty / separator-only titles keep their pre-fix behavior.
	require.Equal(t, "MyApp", sanitizeArchiveTitle("MyApp"))
	require.Equal(t, "session", sanitizeArchiveTitle(""))
	require.Equal(t, "session", sanitizeArchiveTitle("---.."))
}

// TestSanitizeArchiveTitleTruncationRuneSafe verifies the NAME_MAX cut lands on a
// rune boundary so a multi-byte title whose limit falls inside a 4-byte rune does
// not leave invalid UTF-8 in the archive directory name — the same rune-safety
// class truncateDetail (session/git/exec_detail.go) enforces. A byte cut would
// split the rune and emit a U+FFFD replacement on display/normalization.
func TestSanitizeArchiveTitleTruncationRuneSafe(t *testing.T) {
	// 252 ASCII bytes followed by a 4-byte rune (U+10348 𐍈), then more ASCII. The
	// 255th byte sits inside the rune, so the rune-boundary cut backs up to 252.
	const emoji = "𐍈"
	title := strings.Repeat("a", 252) + emoji + strings.Repeat("b", 10)
	got := sanitizeArchiveTitle(title)
	require.LessOrEqual(t, len(got), archiveLeafNameMax, "leaf over NAME_MAX")
	require.True(t, utf8.ValidString(got), "truncated leaf is not valid UTF-8: %q", got)
	require.Equal(t, strings.Repeat("a", 252), got, "rune-boundary cut must drop the split rune and keep the 252 leading 'a's")
}

// TestArchivedWorktreePathBoundsLongTitle feeds a long title through the full
// archivedWorktreePath derivation (the call site archive.go uses unmodified) and
// asserts the leaf segment stays within NAME_MAX — the archive-side mirror of
// session/git/util_test.go TestBranchAndWorktreeNamesBoundLongTitles.
func TestArchivedWorktreePathBoundsLongTitle(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoID = "repo-id-xyz"
	dest, err := archivedWorktreePath(repoID, strings.Repeat("a", 500))
	require.NoError(t, err)
	leaf := filepath.Base(dest)
	require.NotEmpty(t, leaf)
	require.LessOrEqualf(t, len(leaf), archiveLeafNameMax, "archive leaf %d bytes over NAME_MAX %d", len(leaf), archiveLeafNameMax)
	// The repoID namespace is intact around the bounded leaf.
	require.Equal(t, repoID, filepath.Base(filepath.Dir(dest)))
}
