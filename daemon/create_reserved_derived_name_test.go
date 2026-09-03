package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// fakeTmuxOnPath puts a tmux that answers "no such session" ahead of the real
// one, so admission's existence probe is decided rather than wedged and no test
// here can touch a live tmux server.
func fakeTmuxOnPath(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newTitleAdmissionManager() *Manager {
	return &Manager{
		cfg:               config.DefaultConfig(),
		instances:         make(map[string]*session.Instance),
		reservedTitles:    make(map[string]struct{}),
		reservedTmuxNames: make(map[string]string),
	}
}

// TestValidateTitleRefusesReservedDerivedName is the #3732 red. The reserved
// guard asked IsReservedTitle, which only TRIMS whitespace, while toTmuxName
// DELETES it — so "ro ot" was creatable and derived the identical tmux session
// name as the reserved "root", giving the daemon two sessions it keys as one.
// Nothing needs to exist for this refusal: the reserved name is reserved even
// on a repo whose root agent has not been created yet, which is exactly the
// window the record scan cannot cover.
func TestValidateTitleRefusesReservedDerivedName(t *testing.T) {
	fakeTmuxOnPath(t)
	const repoID = "repo-id"
	repoPath := t.TempDir()

	for _, title := range []string{"ro ot", "r o o t", "  ro ot  "} {
		t.Run(title, func(t *testing.T) {
			m := newTitleAdmissionManager()
			err := m.validateTitleAvailableLocked(repoID, repoPath, title, "claude", runtimeNamespaceLocalTmux, false, nil)
			if err == nil {
				t.Fatalf("create admitted %q, which derives the reserved tmux session name %q", title, tmux.SanitizedNameForRepo(session.RootSessionTitle, repoPath))
			}
			// Actionable: names the title asked for, the reserved title it
			// collides with, and the remedy. "ro ot" and "root" look nothing
			// alike on a sidebar row, so a bare "reserved" reads as a bug.
			for _, want := range []string{title, session.RootSessionTitle, "reserved", "tmux", "root_agents"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
			// The pre-rename path must refuse identically, or an archived-name
			// reuse would mutate records for a create that is doomed anyway
			// (#2415) — which is why the check sits in the SHAPE half.
			if err := m.validateTitleClaimableLocked(repoID, repoPath, title, "claude", runtimeNamespaceLocalTmux, false, nil, nil); err == nil {
				t.Fatalf("the record-independent half admitted %q", title)
			}
		})
	}
}

// TestValidateTitleRefusesReservedSpelling keeps the older half of the rule
// pinned beside the new one: "Root " and "ROOT" derive DIFFERENT tmux names
// (toTmuxName preserves case), so only the case-folded spelling rule catches
// them, and it must keep catching them.
func TestValidateTitleRefusesReservedSpelling(t *testing.T) {
	fakeTmuxOnPath(t)
	const repoID = "repo-id"
	repoPath := t.TempDir()

	for _, title := range []string{"Root ", " ROOT", "root"} {
		t.Run(title, func(t *testing.T) {
			m := newTitleAdmissionManager()
			err := m.validateTitleAvailableLocked(repoID, repoPath, title, "claude", runtimeNamespaceLocalTmux, false, nil)
			if err == nil {
				t.Fatalf("create admitted the reserved spelling %q", title)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("refusal %q does not say the title is reserved", err)
			}
		})
	}
}

// TestReservedNameRuleLeavesTheEnsureLoopAlone is the counterweight: the
// daemon's own root-agent create passes allowReserved and must still be able to
// claim the title it reserves, next to an unrelated title that merely looks
// close. A rule that refused either would take the root agent offline.
func TestReservedNameRuleLeavesTheEnsureLoopAlone(t *testing.T) {
	fakeTmuxOnPath(t)
	const repoID = "repo-id"
	repoPath := t.TempDir()

	m := newTitleAdmissionManager()
	if err := m.validateTitleAvailableLocked(repoID, repoPath, session.RootSessionTitle, "claude", runtimeNamespaceLocalTmux, true, nil); err != nil {
		t.Fatalf("the ensure loop was refused its own reserved title: %v", err)
	}
	// A live root does not make every nearby title unavailable — only the ones
	// that derive its name. "ro-ot" keeps its own branch AND its own tmux name.
	m.instances[daemonInstanceKey(repoID, session.RootSessionTitle)] = &session.Instance{Title: session.RootSessionTitle}
	if err := m.validateTitleAvailableLocked(repoID, repoPath, "ro-ot", "claude", runtimeNamespaceLocalTmux, false, nil); err != nil {
		t.Fatalf("a title with a distinct derived name was refused beside the root agent: %v", err)
	}
}

// TestValidateTitleRefusesWhitespaceDerivedCollisionWithLiveSession covers the
// general half of #3732 — two ORDINARY titles that derive one tmux name. The
// record scan already compared derived names for punctuation variants ("a/b" vs
// "a_b"); this pins the whitespace flavour, which is the one that reaches a pair
// whose git branches stay distinct ("a b" -> "a-b", "ab" -> "ab").
func TestValidateTitleRefusesWhitespaceDerivedCollisionWithLiveSession(t *testing.T) {
	fakeTmuxOnPath(t)
	const (
		repoID    = "repo-id"
		existing  = "a b"
		candidate = "ab"
	)
	repoPath := t.TempDir()

	m := newTitleAdmissionManager()
	if got, want := tmux.SanitizedNameForRepo(existing, repoPath), tmux.SanitizedNameForRepo(candidate, repoPath); got != want {
		t.Fatalf("premise gone: %q derives %q, %q derives %q", existing, got, candidate, want)
	}
	m.instances[daemonInstanceKey(repoID, existing)] = &session.Instance{Title: existing}

	err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, "claude", runtimeNamespaceLocalTmux, false, nil)
	if err == nil {
		t.Fatalf("create admitted %q beside live session %q; both derive tmux session %q", candidate, existing, tmux.SanitizedNameForRepo(candidate, repoPath))
	}
	for _, want := range []string{existing, candidate, "tmux"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("collision error %q does not name %q", err, want)
		}
	}
}

// TestReserveCreateRefusesReservedDerivedName proves the wiring, not just the
// validator: the refusal has to be reachable from the real create entry point,
// under its own lock, for every caller that funnels through it.
func TestReserveCreateRefusesReservedDerivedName(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	fakeTmuxOnPath(t)
	repoPath := setupControlRepo(t)

	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, _, release, _, err := m.reserveCreate(CreateSessionRequest{
		Title: "ro ot", RepoPath: repoPath, Program: tmux.ProgramClaude,
	})
	if err == nil {
		if release != nil {
			release()
		}
		t.Fatal("reserveCreate admitted a title deriving the reserved root tmux name")
	}
	for _, want := range []string{"ro ot", session.RootSessionTitle, "reserved"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("reserveCreate refusal %q does not name %q", err, want)
		}
	}
}
