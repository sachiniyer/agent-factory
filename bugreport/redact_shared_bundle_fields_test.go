package bugreport

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// The fields #3588 tracked: everything the #3548 field-coverage guard exposed as
// still reaching a publicly shared bundle verbatim, minus the ones later fixes
// closed. Each test here is the guard's finding turned into an assertion about
// the REAL pipeline (redactInstancesJSON → redactInstanceData → scrub), so a
// regression fails on the bundle a user would actually attach rather than on an
// allowlist entry.

// redactOneInstance runs a single record through the production instances path
// and returns the redacted JSON. Going through redactInstancesJSON rather than
// redactInstanceData is deliberate: the field policy and the closing text scrub
// are one pipeline, and #3588 is a register of fields that survived BOTH.
func redactOneInstance(t *testing.T, r *redactor, d session.InstanceData) string {
	t.Helper()
	raw, err := json.Marshal([]session.InstanceData{d})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(r.redactInstancesJSON(raw))
}

// TestRedactInstancesJSONRedactsTabNames covers register item "Tabs[].Name /
// PendingTabs[].Name". A tab name is user-chosen (`af sessions tab-create --name
// <tab>`) and nothing in the pipeline reached it: it is not a session title, so
// scrubSessionTitles cannot know it, and it is not a path, so the $HOME collapse
// does not apply. A tab named after a customer shipped verbatim.
func TestRedactInstancesJSONRedactsTabNames(t *testing.T) {
	r := &redactor{}
	out := redactOneInstance(t, r, session.InstanceData{
		ID: "abc123",
		Tabs: []session.TabData{
			{ID: "tab-1", Name: "AcmeCorp migration"},
			{ID: "tab-2", Name: "shell"},
		},
		PendingTabs: []session.TabData{{ID: "tab-3", Name: "AcmeCorp staging"}},
	})

	for _, name := range []string{"AcmeCorp migration", "AcmeCorp staging"} {
		if strings.Contains(out, name) {
			t.Errorf("user-chosen tab name %q reached the bundle:\n%s", name, out)
		}
	}
	// "a tab existed" is the triage-relevant fact, and the minted id plus the
	// kind carry it — so the ids must survive the name redaction.
	for _, id := range []string{"tab-1", "tab-2", "tab-3"} {
		if !strings.Contains(out, id) {
			t.Errorf("minted tab id %q should survive redaction:\n%s", id, out)
		}
	}
}

// TestRedactInstancesJSONRedactsAccountLabel covers register item "Account". The
// credential-account label is free text a user picks (`--account work`) and may
// name an employer or a client. Redacting to the marker keeps the triage-relevant
// fact — an account WAS set — and drops the label.
func TestRedactInstancesJSONRedactsAccountLabel(t *testing.T) {
	r := &redactor{}
	out := redactOneInstance(t, r, session.InstanceData{ID: "abc123", Account: "AcmeCorp-prod"})
	if strings.Contains(out, "AcmeCorp-prod") {
		t.Errorf("user-chosen account label reached the bundle:\n%s", out)
	}
	if !strings.Contains(out, `"account": "`+redactedMarker+`"`) {
		t.Errorf("an account that WAS set must still be visible as one:\n%s", out)
	}

	// A session with no account must not grow one: "an account was in play" is a
	// fact the redaction preserves, never one it invents.
	unset := redactOneInstance(t, &redactor{}, session.InstanceData{ID: "abc123"})
	if strings.Contains(unset, `"account"`) {
		t.Errorf("a session with no account gained one through redaction:\n%s", unset)
	}
}

// TestRedactInstancesJSONReducesProgramToItsAgent covers register item
// "Program". It is the session-level analogue of TabData.Command, which
// redactTabData already drops wholesale, and it can be an arbitrary command line
// with flags. Reducing it to the agent the command detects keeps the one
// triage-relevant thing — which agent ran — and drops the path and every flag
// value.
func TestRedactInstancesJSONReducesProgramToItsAgent(t *testing.T) {
	out := redactOneInstance(t, &redactor{}, session.InstanceData{
		ID:      "abc123",
		Program: "/opt/AcmeCorp/bin/claude --dangerously-skip-permissions --token sk-notreallyasecret",
	})
	if strings.Contains(out, "AcmeCorp") || strings.Contains(out, "dangerously-skip-permissions") {
		t.Errorf("the program command line reached the bundle:\n%s", out)
	}
	if !strings.Contains(out, `"program": "claude"`) {
		t.Errorf("the detected agent should survive so triage knows what ran:\n%s", out)
	}

	// No agent token in the command: there is nothing safe to keep, so the whole
	// value goes — the same trade Command already makes.
	opaque := redactOneInstance(t, &redactor{}, session.InstanceData{
		ID:      "abc123",
		Program: "/opt/AcmeCorp/bin/inhouse-wrapper --client AcmeCorp",
	})
	if strings.Contains(opaque, "AcmeCorp") || strings.Contains(opaque, "inhouse-wrapper") {
		t.Errorf("an undetectable program command line reached the bundle:\n%s", opaque)
	}
	if !strings.Contains(opaque, `"program": "`+redactedMarker+`"`) {
		t.Errorf("an undetectable program must collapse to the marker:\n%s", opaque)
	}
}

// TestRedactInstancesJSONCollapsesRootsOutsideHome is the six-path half of the
// register. scrub replaces $HOME and the username tokens and NOTHING else, so a
// repo or worktree that does not sit under $HOME shipped its directory names
// verbatim — and a directory name is exactly as revealing as the file names
// #3541 was about.
//
// The fix collapses ROOTS rather than blanking paths: triage needs the layout —
// whether a worktree sits under the AF home, whether a relocation alternate is
// its sibling — and a marker destroys that.
func TestRedactInstancesJSONCollapsesRootsOutsideHome(t *testing.T) {
	const (
		afHome    = "/srv/ConfidentialClient/af"
		repo      = "/srv/ConfidentialClient/repo"
		worktree  = afHome + "/worktrees/kingfisher"
		alternate = afHome + "/worktrees/kingfisher-relocating"
		tree      = worktree + "/.af-source-0123456789abcdef0123456789abcdef"
	)
	r := &redactor{}
	r.noteAFHome(afHome)
	out := redactOneInstance(t, r, session.InstanceData{
		ID:   "abc123",
		Path: worktree,
		Worktree: session.GitWorktreeData{
			RepoPath:     repo,
			WorktreePath: worktree,
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				AlternatePath: alternate,
			},
		},
		ArchiveReport: &sessiongit.ArchiveReport{
			RetainedTrees: []sessiongit.ArchiveRetainedTree{{Path: tree}},
			RollbackFence: &sessiongit.ArchiveRollbackFence{
				OriginalRelocationRecovery: &sessiongit.ArchiveRollbackRelocationRecovery{
					AlternatePath: alternate,
				},
			},
		},
	})

	if strings.Contains(out, "ConfidentialClient") {
		t.Errorf("a directory name from outside $HOME reached the bundle:\n%s", out)
	}
	// The shape survives: each field still says which root it hangs off, and the
	// retained tree is still recognizably inside the worktree.
	for _, want := range []string{
		`"path": "[worktree:1]"`,
		`"repo_path": "[repo:1]"`,
		`"worktree_path": "[worktree:1]"`,
		`"alternate_path": "[af-home]/worktrees/kingfisher-relocating"`,
		`"path": "[worktree:1]/.af-source-0123456789abcdef0123456789abcdef"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("collapsed path %s is missing:\n%s", want, out)
		}
	}
}

// TestRedactInstancesJSONSharesOneRootToken pins the numbering: a token is per
// distinct ROOT, not per session, so two sessions in one repo read as being in
// one repo. A per-record token would make the bundle look like two projects.
func TestRedactInstancesJSONSharesOneRootToken(t *testing.T) {
	const repo = "/srv/ConfidentialClient/repo"
	rows, err := json.Marshal([]session.InstanceData{
		{ID: "one", Worktree: session.GitWorktreeData{RepoPath: repo, WorktreePath: repo + "/wt-one"}},
		{ID: "two", Worktree: session.GitWorktreeData{RepoPath: repo, WorktreePath: repo + "/wt-two"}},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	out := string((&redactor{}).redactInstancesJSON(rows))

	if strings.Contains(out, "ConfidentialClient") {
		t.Fatalf("repo directory name reached the bundle:\n%s", out)
	}
	if strings.Count(out, `"repo_path": "[repo:1]"`) != 2 {
		t.Errorf("both sessions must share one repo token:\n%s", out)
	}
	if !strings.Contains(out, `"worktree_path": "[worktree:1]"`) ||
		!strings.Contains(out, `"worktree_path": "[worktree:2]"`) {
		t.Errorf("two distinct worktrees must get two distinct tokens:\n%s", out)
	}
}

// TestRedactInstancesJSONBlanksAPathUnderNoKnownRoot is the other half of the
// policy. A path under neither a registered root nor $HOME is still an unknown
// directory name, so it goes to the marker — the alternative is the leak this
// whole register is about.
func TestRedactInstancesJSONBlanksAPathUnderNoKnownRoot(t *testing.T) {
	out := redactOneInstance(t, &redactor{}, session.InstanceData{
		ID:   "abc123",
		Path: "/mnt/ConfidentialClient/scratch/session",
	})
	if strings.Contains(out, "ConfidentialClient") {
		t.Errorf("an unknown absolute path reached the bundle verbatim:\n%s", out)
	}
	if !strings.Contains(out, `"path": "`+redactedMarker+`"`) {
		t.Errorf("an unknown absolute path must collapse to the marker:\n%s", out)
	}
}

// TestRedactInstancesJSONRemovesATitleFromACollapsedPath closes the residue the
// root tokens leave: an AF-home path names the session directory after the
// session TITLE, so collapsing the root alone would still ship the title that
// every other field drops.
func TestRedactInstancesJSONRemovesATitleFromACollapsedPath(t *testing.T) {
	const afHome = "/srv/ConfidentialClient/af"
	r := &redactor{}
	r.noteAFHome(afHome)
	out := redactOneInstance(t, r, session.InstanceData{
		ID:    "abc123",
		Title: "ProjectKingfisher",
		Path:  afHome + "/archived/0f8fc14cb4d0/ProjectKingfisher",
	})
	if strings.Contains(out, "ProjectKingfisher") {
		t.Errorf("the session title reached the bundle inside a collapsed path:\n%s", out)
	}
	if !strings.Contains(out, "[af-home]/archived/0f8fc14cb4d0/") {
		t.Errorf("the AF-home layout should survive the title redaction:\n%s", out)
	}
}

// TestScrubLogCollapsesKnownRootsOutsideHome is the second consequence of
// registering roots: the daemon log tail prints those same paths, and it is a
// fourth way they reach a bundle.
func TestScrubLogCollapsesKnownRootsOutsideHome(t *testing.T) {
	r := &redactor{}
	d := session.InstanceData{Worktree: session.GitWorktreeData{RepoPath: "/srv/ConfidentialClient/repo"}}
	r.noteSession(&d)

	got := r.scrubLog("worktree add failed in /srv/ConfidentialClient/repo/.git: exit status 128")
	if strings.Contains(got, "ConfidentialClient") {
		t.Errorf("the repo root reached the bundled log verbatim: %q", got)
	}
	if !strings.Contains(got, "[repo:1]/.git") {
		t.Errorf("the log should keep the layout under the collapsed root: %q", got)
	}
}

// TestNewRedactorRegistersTheAFHome pins where the AF-home token comes from in
// production: AGENT_FACTORY_HOME, resolved by the same config helper the rest of
// af uses, so an AF home deliberately kept outside $HOME is named rather than
// shipped.
func TestNewRedactorRegistersTheAFHome(t *testing.T) {
	afHome := filepath.Join(t.TempDir(), "ConfidentialClient-af")
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	got := newRedactor().scrub("daemon socket at " + afHome + "/daemon.sock")
	if strings.Contains(got, "ConfidentialClient-af") {
		t.Errorf("the AF home reached the bundle verbatim: %q", got)
	}
	if !strings.Contains(got, "[af-home]/daemon.sock") {
		t.Errorf("the AF home should collapse to its token: %q", got)
	}
}

// TestRedactInstancesJSONRedactsArchiveWarningFileNames covers register item
// "ArchiveWarning". #3554 closed the LOG path for exactly this text, but
// scrubArchiveWarningPaths is reached only from scrubLog while redactInstancesJSON
// applies plain scrub — so the bounded warning field still carried the
// user-chosen skipped file names into the JSON section of the bundle.
//
// The input is built from the REAL renderer, never a hand-written fixture, so a
// format change cannot leave this test passing while the bundle leaks.
func TestRedactInstancesJSONRedactsArchiveWarningFileNames(t *testing.T) {
	report := archiveReportWithSkipped("/srv/ConfidentialClient/af/worktrees/kingfisher",
		"credential", ".env.production", "customer-ssns.csv")
	out := redactOneInstance(t, &redactor{}, session.InstanceData{
		ID:             "abc123",
		ArchiveWarning: report.Warning("archive"),
	})

	for _, name := range []string{"credential", ".env.production", "customer-ssns.csv", "ConfidentialClient"} {
		if strings.Contains(out, name) {
			t.Errorf("the archive warning carried %q into the bundle:\n%s", name, out)
		}
	}
	// The warning's SHAPE is what triage reads, and it survives: the count, the
	// noun, and the reason beside each dropped name.
	for _, want := range []string{"af skipped 3 unreadable files", "permission denied"} {
		if !strings.Contains(out, want) {
			t.Errorf("the archive warning lost its triage text %q:\n%s", want, out)
		}
	}
}

// TestRedactInstancesJSONBlanksAnUnrecognizedArchiveWarning is the fail-safe.
// The field is the bounded projection of ONE renderer, which always writes the
// retained-at clause; text that does not carry it did not come from that
// renderer, so nothing in it can be told apart and it is dropped whole.
func TestRedactInstancesJSONBlanksAnUnrecognizedArchiveWarning(t *testing.T) {
	out := redactOneInstance(t, &redactor{}, session.InstanceData{
		ID:             "abc123",
		ArchiveWarning: `archive incomplete: could not read "customer-ssns.csv"`,
	})
	if strings.Contains(out, "customer-ssns.csv") {
		t.Errorf("an unparseable archive warning shipped its quoted names:\n%s", out)
	}
	if !strings.Contains(out, `"archive_warning": "`+redactedMarker+`"`) {
		t.Errorf("an unparseable archive warning must collapse to the marker:\n%s", out)
	}
}

// TestRedactInstancesJSONScrubsLostRestoreFailureError covers register item
// "LostRestoreFailure.Error". It is af-authored — daemon/lostrestore.go stores
// the restore loop's terminal error — but it QUOTES whatever tmux and git
// returned, and those name the session's tmux session (derived from the title)
// and its worktree root. Blanking it would cost triage the reason automatic
// recovery stopped, so it gets the log tail's treatment instead.
func TestRedactInstancesJSONScrubsLostRestoreFailureError(t *testing.T) {
	r := &redactor{}
	out := redactOneInstance(t, r, session.InstanceData{
		ID:       "abc123",
		Title:    "ProjectKingfisher",
		TmuxName: "af_0f8fc14c_ProjectKingfisher",
		Worktree: session.GitWorktreeData{
			RepoPath:     "/srv/ConfidentialClient/repo",
			WorktreePath: "/srv/ConfidentialClient/repo/wt",
		},
		LostRestoreFailure: &session.LostRestoreFailure{
			Attempts: 5,
			Error: "start tmux session af_0f8fc14c_ProjectKingfisher for \"ProjectKingfisher\" " +
				"in /srv/ConfidentialClient/repo/wt: exit status 1",
		},
	})

	for _, secret := range []string{"ProjectKingfisher", "ConfidentialClient"} {
		if strings.Contains(out, secret) {
			t.Errorf("the restore diagnostic carried %q into the bundle:\n%s", secret, out)
		}
	}
	// The af-authored diagnostic itself survives — that is the point of scrubbing
	// it rather than blanking it.
	for _, want := range []string{"start tmux session", "exit status 1", `"attempts": 5`} {
		if !strings.Contains(out, want) {
			t.Errorf("the restore diagnostic lost its triage text %q:\n%s", want, out)
		}
	}
}

// TestRedactTasksReducesProgramToItsAgent applies the Program and repo-root
// decisions to the task projection beside them. redactedTask.Program is the same
// arbitrary command line under a different owner, and its ProjectPath carried the
// same "the scrub collapses $HOME" assumption the six instance paths did.
func TestRedactTasksReducesProgramToItsAgent(t *testing.T) {
	r := &redactor{}
	out := r.redactTasks([]task.Task{{
		ID:          "t1",
		ProjectPath: "/srv/ConfidentialClient/repo",
		Program:     "/opt/AcmeCorp/bin/codex --profile acme",
	}})
	if len(out) != 1 {
		t.Fatalf("redactTasks returned %d rows, want 1", len(out))
	}
	if out[0].Program != "codex" {
		t.Errorf("task program = %q, want the detected agent %q", out[0].Program, "codex")
	}

	doc, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	scrubbed := r.scrubJSON(string(doc))
	if strings.Contains(scrubbed, "ConfidentialClient") || strings.Contains(scrubbed, "AcmeCorp") {
		t.Errorf("the task projection reached the bundle with a user directory name:\n%s", scrubbed)
	}
	if !strings.Contains(scrubbed, `"project_path":"[repo:1]"`) {
		t.Errorf("a task project path should collapse to its repo token:\n%s", scrubbed)
	}
}

// TestRedactInstancesFallbackDropsTheNewSensitiveKeys mirrors every typed
// redaction above onto the generic fallback, exactly as the existing entries in
// sensitiveJSONKeys mirror theirs. A record the typed decode rejects must not be
// LESS private than one it accepts.
func TestRedactInstancesFallbackDropsTheNewSensitiveKeys(t *testing.T) {
	r := &redactor{}
	// A string status forces the generic fallback.
	raw := json.RawMessage(`[{
		"status": "legacy-status",
		"account": "AcmeCorp-prod",
		"program": "/opt/AcmeCorp/bin/inhouse-wrapper --client AcmeCorp",
		"archive_warning": "archive completed with an incomplete archive: af skipped 1 unreadable file; complete original tree(s) were retained at \"/srv/ConfidentialClient/wt\"; skipped paths: \"customer-ssns.csv\" (permission denied)",
		"lost_restore_failure": {"attempts": 5, "error": "restore of ProjectKingfisher failed"},
		"tabs": [{"id": "tab-1", "name": "AcmeCorp migration", "account_scope": "AcmeCorp-retired-acct"}],
		"worktree": {"relocation_recovery": {"alternate_path": "/srv/ConfidentialClient/wt-relocating"}}
	}]`)

	out := string(r.redactInstancesJSON(raw))
	for _, secret := range []string{
		"AcmeCorp", "inhouse-wrapper", "customer-ssns.csv", "ConfidentialClient", "ProjectKingfisher",
		// account_scope mirrors redactTabData's AccountScope blanking (#4506).
		// The leak this entry pins only surfaces on the fallback arm, and only
		// when the label is absent from r.accounts (see
		// TestRedactInstancesFallbackRedactsAccountScope); here there is no
		// registry, so the structural walk is the only defence.
		"AcmeCorp-retired-acct",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("the generic fallback leaked %q:\n%s", secret, out)
		}
	}
}

// TestRedactInstancesFallbackRedactsAccountScope is the #4506 regression guard
// for the generic fallback. redactTabData blanks TabData.AccountScope on the
// typed path, so the fallback's structural walk must blank account_scope too:
// a record the typed decode rejects must never be less private than one it
// accepts.
//
// The closing text sweep (appendAccountLabelSpans) only matches labels STILL in
// r.accounts, so a tab authored under an account that was later renamed,
// retired, or deleted relies on the sensitiveJSONKeys entry exclusively. The two
// subtests pin the junction the report called out: a label retired from the
// registry (the leak, which the text sweep cannot rescue) and a label still in
// the registry (the common case the text sweep masks, here asserted to be
// blanked by the structural walk rather than left to the sweep alone).
func TestRedactInstancesFallbackRedactsAccountScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered string // a label still in the registry ("" registers none)
		scope      string // the account_scope value planted on the tab
	}{
		{
			// The leak the bug shipped: a tab authored under "acme", which was
			// later renamed/retired to "work". "acme" is no longer in r.accounts,
			// so the text sweep cannot reach it — only the structural walk can.
			name:       "label retired from the registry",
			registered: "work",
			scope:      "acme",
		},
		{
			// The common case the text sweep masks: the label is still
			// registered, so a missing sensitiveJSONKeys entry would be hidden
			// by the sweep blanking the value in place. The structural walk
			// must still own it, so the marker replaces the value regardless.
			name:       "label still in the registry",
			registered: "acme",
			scope:      "acme",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{}
			if tc.registered != "" {
				r.noteAccount(tc.registered)
			}
			// A string status forces the typed decode to fail, routing the
			// record to the generic fallback — the only path whose structural
			// walk reads sensitiveJSONKeys.
			raw := json.RawMessage(`[{
				"status": "legacy-status",
				"account": "work",
				"tabs": [{"id": "tab-1", "account_scope": "` + tc.scope + `"}]
			}]`)

			out := string(r.redactInstancesJSON(raw))

			if strings.Contains(out, tc.scope) {
				t.Errorf("fallback leaked account_scope %q:\n%s", tc.scope, out)
			}
			if !strings.Contains(out, `"account_scope": "`+redactedMarker+`"`) {
				t.Errorf("account_scope was not replaced by the redaction marker:\n%s", out)
			}
			// The structural triage field survives the fallback walk.
			if !strings.Contains(out, "tab-1") {
				t.Errorf("fallback dropped the safe tab id:\n%s", out)
			}
		})
	}
}

// TestCollapsePathFieldRequiresASeparatorBoundary is the prefix-boundary rule. A
// SIBLING of a known root is not inside it, and treating it as one would file a
// path under a token it has nothing to do with — while leaving the part of its
// own directory name that hangs past the prefix in the bundle.
func TestCollapsePathFieldRequiresASeparatorBoundary(t *testing.T) {
	r := &redactor{}
	r.noteWorktreeRoot("/srv/ConfidentialClient/repo")

	if got := r.collapsePathField("/srv/ConfidentialClient/repo-backup/wt"); got != redactedMarker {
		t.Errorf("collapsePathField(sibling of a known root) = %q, want the marker: a sibling is "+
			"not inside the root, and rewriting its prefix strands the rest of its name", got)
	}
	if got, want := r.collapsePathField("/srv/ConfidentialClient/repo/wt"), "[worktree:1]/wt"; got != want {
		t.Errorf("collapsePathField(inside a known root) = %q, want %q", got, want)
	}
}

// TestCollapsePathFieldKeepsTheTokenIntact pins which part of a collapsed path
// the title scrub may touch. Both neighbours of "repo" inside "[repo:1]" are
// non-word runes, so a session actually TITLED "repo" satisfies the bare-title
// boundary there — and rewriting the token would destroy the one thing the
// collapsed path is for.
func TestCollapsePathFieldKeepsTheTokenIntact(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot("/srv/ConfidentialClient/repo")
	r.noteTitle("repo")

	got := r.collapsePathField("/srv/ConfidentialClient/repo/worktrees/repo")
	if !strings.HasPrefix(got, "[repo:1]/") {
		t.Errorf("collapsePathField = %q: the root token was rewritten by the title scrub", got)
	}
	if strings.HasSuffix(got, "/repo") {
		t.Errorf("collapsePathField = %q: the title below the root survived", got)
	}
}

// TestCollapsePathFieldRedactsWorktreeSubdirectoryTitle closes the
// structured-field half of the leak class
// TestScrubbersRedactSanitizedTitleInSubdirectoryRecoveryPath closes for the
// text pass. A depth-1 leaf below <afHome>/worktrees carries a SANITIZED title
// segment (DerivedWorktreeSubdirectoryTitleSegment) that differs byte-for-byte
// from the raw title scrubSessionTitles matches, so the remainder scrub alone
// ships it verbatim. The downstream JSON text scrubber would redact it via
// appendWorktreeSubdirectoryTitleSpans, but that matcher keys its needle on the
// literal <afHome>; collapsePathField has just replaced that with the [af-home]
// token, so the needle can no longer match. The fix applies the same depth-1
// subdirectory scrub at the source, keyed on the token.
func TestCollapsePathFieldRedactsWorktreeSubdirectoryTitle(t *testing.T) {
	const (
		afHome       = "/srv/ConfidentialClient/af"
		repo         = "/srv/ConfidentialClient/repo"
		title        = "fix bug (urgent)"
		diskTitle    = "fix-bug-urgent" // == sessiongit.DerivedWorktreeSubdirectoryTitleSegment(title)
		archiveDest  = afHome + "/archived/0f8fc14cb4d0/fix bug (urgent)"
		depth1       = afHome + "/worktrees/" + diskTitle
		depth1Suffix = depth1 + "-2"
		depth2       = depth1 + "/subdir/file"
	)
	if got, want := sessiongit.DerivedWorktreeSubdirectoryTitleSegment(title), diskTitle; got != want {
		t.Fatalf("title derivation mismatch: got %q want %q (test fixture must match the registered segment)", got, want)
	}

	r := &redactor{}
	r.noteAFHome(afHome)
	r.noteSession(&session.InstanceData{
		Title: title,
		Worktree: session.GitWorktreeData{
			RepoPath:     repo,
			WorktreePath: archiveDest, // registered worktree root — collapses safely
			// AlternatePath (the old depth-1 leaf) is NOT registered as a root
			// anywhere — exactly the production state the bug asks for.
		},
	})

	// Depth-1 leaf: the segment must be redacted, not shipped verbatim. The
	// [af-home]/worktrees layout survives for triage.
	if got, want := r.collapsePathField(depth1), "[af-home]/worktrees/"+redactedMarker; got != want {
		t.Errorf("collapsePathField(depth-1 subdirectory leaf) = %q, want %q (segment must be redacted at the source)", got, want)
	}

	// firstFreeWorktreePath's "-N" collision suffix is AF-authored and survives,
	// matching the text-pass scrubber's trade.
	if got, want := r.collapsePathField(depth1Suffix), "[af-home]/worktrees/"+redactedMarker+"-2"; got != want {
		t.Errorf("collapsePathField(depth-1 leaf with collision suffix) = %q, want %q (collision suffix must survive)", got, want)
	}

	// A deeper path preserves the subdirectory layout; only the title-derived
	// depth-1 component is redacted, the rest of the path stays for triage.
	if got, want := r.collapsePathField(depth2), "[af-home]/worktrees/"+redactedMarker+"/subdir/file"; got != want {
		t.Errorf("collapsePathField(deeper subdirectory) = %q, want %q (only the title-derived segment should be redacted)", got, want)
	}

	// A depth-1 leaf that IS a registered worktree root still collapses to that
	// root's token. The subdirectory scrub must not perturb the worktree-root
	// collapse: in subdirectory mode the worktree's own WorktreePath is this
	// exact depth-1 shape, and the root collapse is what other fields
	// cross-reference via [worktree:N].
	rooted := &redactor{}
	rooted.noteAFHome(afHome)
	rooted.noteSession(&session.InstanceData{
		Title: title,
		Worktree: session.GitWorktreeData{
			RepoPath:     repo,
			WorktreePath: depth1,
		},
	})
	if got, want := rooted.collapsePathField(depth1), "[worktree:1]"; got != want {
		t.Errorf("collapsePathField(registered worktree root at the depth-1 leaf) = %q, want %q (subdirectory scrub must not shadow the worktree-root collapse)", got, want)
	}
}

// TestRedactInstancesJSONRedactsWorktreeSubdirectoryTitleFromAlternatePath is
// the end-to-end witness at the bundle level: it runs the production pipeline
// (redactInstancesJSON → redactInstanceData → collapsePathField → scrubJSON)
// against the exact state the bug report describes — an interrupted archive
// whose relocation_recovery.alternate_path still names the old depth-1
// subdirectory worktree directory. Both the relocation_recovery field and the
// archive_report.rollback_fence compatibility copy must redact the leaf,
// because both flow through collapsePathField.
func TestRedactInstancesJSONRedactsWorktreeSubdirectoryTitleFromAlternatePath(t *testing.T) {
	const (
		afHome       = "/srv/ConfidentialClient/af"
		repo         = "/srv/ConfidentialClient/repo"
		title        = "fix bug (urgent)"
		diskTitle    = "fix-bug-urgent"
		archiveDest  = afHome + "/archived/0f8fc14cb4d0/fix bug (urgent)"
		alternate    = afHome + "/worktrees/" + diskTitle
		retainedTree = alternate // the source is retained when the archive move is interrupted
	)
	r := &redactor{}
	r.noteAFHome(afHome)
	out := redactOneInstance(t, r, session.InstanceData{
		ID:    "instance-1",
		Title: title,
		Worktree: session.GitWorktreeData{
			RepoPath:     repo,
			WorktreePath: archiveDest,
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				AlternatePath: alternate,
			},
		},
		ArchiveReport: &sessiongit.ArchiveReport{
			RetainedTrees: []sessiongit.ArchiveRetainedTree{{Path: retainedTree}},
			RollbackFence: &sessiongit.ArchiveRollbackFence{
				OriginalRelocationRecovery: &sessiongit.ArchiveRollbackRelocationRecovery{
					AlternatePath: alternate,
				},
			},
		},
	})

	if strings.Contains(out, diskTitle) {
		t.Errorf("the title-derived subdirectory segment reached the bundle:\n%s", out)
	}
	for _, secret := range []string{title, "ConfidentialClient"} {
		if strings.Contains(out, secret) {
			t.Errorf("a directory name or raw title reached the bundle (%q):\n%s", secret, out)
		}
	}

	// The depth-1 layout below [af-home] survives for triage; the leaf is the
	// marker, not the segment. Both the relocation_recovery field and the
	// rollback-fence compatibility copy flow through collapsePathField, so
	// both must close — assert the redacted value appears at least twice.
	redactedAlternate := `"alternate_path": "[af-home]/worktrees/` + redactedMarker + `"`
	if got := strings.Count(out, redactedAlternate); got < 2 {
		t.Errorf("alternate_path redaction count = %d, want at least 2 (relocation_recovery + rollback_fence):\n%s", got, out)
	}
	// The retained tree at the same depth-1 source path also flows through
	// collapsePathField and must close for the same reason.
	if !strings.Contains(out, `"path": "[af-home]/worktrees/`+redactedMarker+`"`) {
		t.Errorf("retained_trees[].path at the depth-1 subdirectory leaf was not redacted:\n%s", out)
	}
	// The session's own worktree directory (the registered archive destination)
	// still collapses to its worktree-root token, unaffected by the new scrub.
	if !strings.Contains(out, `"worktree_path": "[worktree:1]"`) {
		t.Errorf("the registered worktree root should collapse to [worktree:1]:\n%s", out)
	}
}

// TestCollapsePathFieldScrubSubdirectoryLeafBeforeBareTitleRewrite pins the
// ordering of the two title scrubs in collapsePathField. A registered bare
// title ("bug") can be a whole token inside another session's subdirectory-title
// leaf ("fix-bug-urgent" from "fix bug urgent"): the '-' the sanitizer emits
// satisfies the bare-title token boundary on both sides of "bug", so a
// bare-title rewrite that ran FIRST would rewrite the leaf to
// "fix-[redacted]-urgent" and the depth-1 subdirectory scrub's needle could no
// longer match — leaving the rest of the title-derived text in the bundle. The
// subdirectory scrub must run against the ORIGINAL root-tokenized value, before
// the bare-title rewrite touches the remainder, so one redaction cannot hide the
// other's evidence.
func TestCollapsePathFieldScrubSubdirectoryLeafBeforeBareTitleRewrite(t *testing.T) {
	const (
		afHome    = "/srv/ConfidentialClient/af"
		bareTitle = "bug"
		longTitle = "fix bug urgent"
		segment   = "fix-bug-urgent" // == sessiongit.DerivedWorktreeSubdirectoryTitleSegment(longTitle)
		alternate = afHome + "/worktrees/" + segment
	)
	if got, want := sessiongit.DerivedWorktreeSubdirectoryTitleSegment(longTitle), segment; got != want {
		t.Fatalf("title derivation mismatch: got %q want %q (test fixture must match the registered segment)", got, want)
	}

	// Two registered sessions, mirroring noteSession's title registration: "bug"
	// (whose own subdirectory segment is "bug") and "fix bug urgent" (whose
	// subdirectory segment is "fix-bug-urgent"). "bug" is a whole token inside
	// "fix-bug-urgent" — its neighbours are '-' runes, which isWordRune rejects,
	// so appendTitleSpans' token boundary accepts it — and a bare-title rewrite
	// that ran first would rewrite the leaf to "fix-[redacted]-urgent", destroying
	// the needle the subdirectory scrub keys on.
	r := &redactor{}
	r.noteAFHome(afHome)
	r.noteTitle(bareTitle)
	r.noteWorktreeSubdirectoryTitle(bareTitle)
	r.noteTitle(longTitle)
	r.noteWorktreeSubdirectoryTitle(longTitle)

	got := r.collapsePathField(alternate)
	if want := "[af-home]/worktrees/" + redactedMarker; got != want {
		t.Errorf("collapsePathField(overlapping bare title + subdirectory title) = %q, want %q: the bare-title rewrite must not destroy the depth-1 segment the subdirectory scrub keys on", got, want)
	}
	if strings.Contains(got, segment) {
		t.Errorf("the title-derived segment reached the bundle verbatim: %q", got)
	}
}

// TestCollapsePathFieldRedactsWorktreeSubdirectoryTitleUnderNestedRoot closes
// the nested-root gap. When another session registers <afHome>/worktrees itself
// as a repository root (an in-place repo at that location), longest-root-first
// collapsing rewrites the whole parent to that root's token (e.g. "[repo:1]")
// before the depth-1 leaf scrub runs. <afHome>/worktrees is still the directory
// the sanitized title segments live below — only its token changed — so the
// scrub must anchor on that root token too, or the leaf ships verbatim as
// "[repo:1]/<segment>".
func TestCollapsePathFieldRedactsWorktreeSubdirectoryTitleUnderNestedRoot(t *testing.T) {
	const (
		afHome       = "/srv/ConfidentialClient/af"
		title        = "fix bug (urgent)"
		segment      = "fix-bug-urgent" // == sessiongit.DerivedWorktreeSubdirectoryTitleSegment(title)
		depth1       = afHome + "/worktrees/" + segment
		depth1Suffix = depth1 + "-2"
		depth2       = depth1 + "/subdir/file"
	)
	if got, want := sessiongit.DerivedWorktreeSubdirectoryTitleSegment(title), segment; got != want {
		t.Fatalf("title derivation mismatch: got %q want %q (test fixture must match the registered segment)", got, want)
	}

	r := &redactor{}
	r.noteAFHome(afHome)
	// Another session registers <afHome>/worktrees itself as a repo root, so a
	// path below it collapses to [repo:1]/... rather than [af-home]/worktrees/...
	// — the exact shape a [af-home]/worktrees-only scrub would miss.
	r.noteRepoRoot(afHome + "/worktrees")
	r.noteWorktreeSubdirectoryTitle(title)

	if got, want := r.collapsePathField(depth1), "[repo:1]/"+redactedMarker; got != want {
		t.Errorf("collapsePathField(nested-root depth-1 leaf) = %q, want %q (the [repo:1] token replaced <afHome>/worktrees; the leaf must still redact under it)", got, want)
	}
	// firstFreeWorktreePath's "-N" collision suffix is AF-authored and survives,
	// matching the text-pass scrubber and the non-nested [af-home]/worktrees case.
	if got, want := r.collapsePathField(depth1Suffix), "[repo:1]/"+redactedMarker+"-2"; got != want {
		t.Errorf("collapsePathField(nested-root depth-1 leaf with collision suffix) = %q, want %q (collision suffix must survive)", got, want)
	}
	// A deeper path preserves the subdirectory layout; only the title-derived
	// depth-1 component is redacted.
	if got, want := r.collapsePathField(depth2), "[repo:1]/"+redactedMarker+"/subdir/file"; got != want {
		t.Errorf("collapsePathField(nested-root deeper subdirectory) = %q, want %q (only the title-derived segment should be redacted)", got, want)
	}
	if got := r.collapsePathField(depth1); strings.Contains(got, segment) {
		t.Errorf("the title-derived segment reached the bundle verbatim: %q", got)
	}

	// Scrubbing the nested-root parent must not redact a "/worktrees" leaf that
	// belongs to an UNRELATED root: [repo:2]/worktrees/<segment> would be a
	// different directory and the sanitized segments are never placed there, so
	// the leaf stays for triage. (other-repo is registered first, so it is
	// [repo:1]; <afHome>/worktrees is [repo:2].)
	const unrelatedRepo = "/srv/ConfidentialClient/other-repo"
	unrelated := &redactor{}
	unrelated.noteRepoRoot(unrelatedRepo)
	unrelated.noteAFHome(afHome)
	unrelated.noteRepoRoot(afHome + "/worktrees")
	unrelated.noteWorktreeSubdirectoryTitle(title)
	if got, want := unrelated.collapsePathField(unrelatedRepo+"/worktrees/"+segment), "[repo:1]/worktrees/"+segment; got != want {
		t.Errorf("collapsePathField(unrelated repo /worktrees leaf) = %q, want %q (a /worktrees leaf below a different root is not the <afHome>/worktrees directory and must not be redacted)", got, want)
	}
}
