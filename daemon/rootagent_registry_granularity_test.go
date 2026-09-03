package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
)

// These tests pin the #3297 granularity rule: a registry failure's blast
// radius must match what that failure can hide. A stray file hides nothing; a
// record that cannot be read hides only its own project; only a failed
// ENUMERATION — where the record list itself is unknown — fails the machine
// closed. #3247 shipped the conservative machine-wide latch for every shape;
// the owner's correction (#3297) is that fail-closed at the wrong scope is
// its own outage: one stray bitflip must not suppress every root agent on the
// box.

// corruptRegistryRecord overwrites a registered project's project.json with
// garbage, probe-proving the strict listing now fails and the detailed
// listing isolates exactly that record.
func corruptRegistryRecord(t *testing.T, projectID string) {
	t.Helper()
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, projectID, "project.json"), []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("corrupt record: %v", err)
	}
	if _, err := config.ListProjects(); err == nil {
		t.Fatalf("fixture failed: strict ListProjects still succeeds with a corrupt record")
	}
}

// TestEnsureRootAgentsIgnoresStrayRegistryFiles: a stray non-record file can
// hide no project's disable — enumeration succeeded and every real record was
// read — so it must suppress nothing: legacy roots still start, and a
// registered personal disable still applies (proving the records around the
// stray were genuinely read, not skipped wholesale).
func TestEnsureRootAgentsIgnoresStrayRegistryFiles(t *testing.T) {
	cases := []struct {
		name        string
		disable     bool
		wantCreates int
	}{
		{name: "legacy root still starts", disable: false, wantCreates: 1},
		{name: "registered personal disable still applies", disable: true, wantCreates: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			seen := installOptionsRecordingBackend(t)
			repoPath := setupControlRepo(t)
			if tc.disable {
				project := registerTestProject(t, repoPath)
				writePersonalRootAgent(t, project.ID, "enabled = false")
			}
			dir, err := config.ProjectRegistryDir()
			if err != nil {
				t.Fatalf("ProjectRegistryDir: %v", err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir registry: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("not a record"), 0o644); err != nil {
				t.Fatalf("write stray: %v", err)
			}

			manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			manager.ensureRootAgentsAndWait()

			if len(*seen) != tc.wantCreates {
				t.Fatalf("want %d creates with a stray registry file, got %d — a stray hides nothing and must not change any root-agent outcome", tc.wantCreates, len(*seen))
			}
			wantReason := rootAgentWillMaterialize
			if tc.disable {
				wantReason = rootAgentDisabled
			}
			if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != wantReason {
				t.Fatalf("verdict reason = %d, want %d — a stray file must not read as an unenumerable registry", got, wantReason)
			}
		})
	}
}

// TestEnsureRootAgentsSuppressesOnlyTheCorruptRecord: a record that cannot be
// read suppresses exactly ITS project — a sibling project's singleton root
// still starts, and the snapshot names the offending record directory. The
// legacy row pins the ACCEPTED RESIDUE of the granularity rule: the corrupt
// record's own personal disable is unreachable (its root path lives inside
// the unreadable record), so a legacy entry for that repo — an opt-in in its
// own right — still starts the root. The wrong-scope alternative, a
// machine-wide outage for one bad record, is what #3297 removes.
func TestEnsureRootAgentsSuppressesOnlyTheCorruptRecord(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	corruptPath := setupControlRepo(t)
	healthyPath := setupControlRepo(t)
	corruptProject := registerTestProject(t, corruptPath)
	healthyProject := registerTestProject(t, healthyPath)
	writePersonalRootAgent(t, corruptProject.ID, "enabled = true")
	writePersonalRootAgent(t, healthyProject.ID, "enabled = true\nprogram = \"/opt/healthy\"")
	corruptRegistryRecord(t, corruptProject.ID)

	var warnings bytes.Buffer
	prevWarning := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(prevWarning) })

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("want exactly the healthy project's root, got %d creates — one corrupt record must suppress only its own project", len(*seen))
	}
	if (*seen)[0].Program != "/opt/healthy" {
		t.Fatalf("the surviving create must be the healthy project's, got program %q", (*seen)[0].Program)
	}
	if got := warnings.String(); !strings.Contains(got, corruptProject.ID) || !strings.Contains(got, "only that project is affected") {
		t.Fatalf("the snapshot must name the corrupt record and its blast radius; warnings were:\n%s", got)
	}
}

// TestEnsureRootAgentsCorruptRecordKeepsLegacyOptIn pins the accepted residue
// explicitly: legacy entry + corrupt record for the same repo → the legacy
// opt-in still starts the root, even though the (unreachable) personal config
// holds a disable. Named residue, chosen over the machine-wide alternative.
func TestEnsureRootAgentsCorruptRecordKeepsLegacyOptIn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	corruptRegistryRecord(t, project.ID)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("the legacy opt-in must survive its project's corrupt record (the accepted #3297 residue), got %d creates", len(*seen))
	}
}

// TestVerdictNamesUnreadableRecordsForUnattributedRepos pins the #3316
// round-2 review: with a corrupt record present, a repo no readable config
// covers must not answer "not configured — add a root_agents entry" (advice
// that walks the user into the fail-open residue); it names the unreadable
// record directories that may be its own, and the delete preflight treats the
// state as blocking.
func TestVerdictNamesUnreadableRecordsForUnattributedRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	corruptRegistryRecord(t, project.ID)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rid := repoID(t, repoPath)

	verdict := manager.rootAgentMaterializeVerdictFor(rid)
	if verdict.reason != rootAgentRecordsUnreadable {
		t.Fatalf("verdict reason = %d, want rootAgentRecordsUnreadable — an unattributed repo with corrupt records present is not simply unconfigured", verdict.reason)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, project.ID) {
		t.Fatalf("the refusal must name the unreadable record directory, got: %s", detail)
	}
	if strings.Contains(detail, "add a root_agents entry") {
		t.Fatalf("the refusal must not steer toward the fail-open residue, got: %s", detail)
	}
}
