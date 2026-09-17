package testguard

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// afSkillDirName mirrors session.afSkillDirName. This package cannot import
// session: session/tmux's tests import testguard, and session imports
// session/tmux.
const afSkillDirName = "agent-factory"

// ambientAgentStoreFiles resolves the files af writes into an agent's own config
// root, using the ambient (pre-sandbox) environment. The paths are a hand-kept
// copy of session's skill bases, and FencedAgentStoreFiles' drift test is what
// holds them to production. That is the SKILL.md that
// ensureAfSkillDir writes under global_agent_skills and removes when that
// setting is off (session/agentskill.go, session/ampskill.go). A sandboxed
// package runs with global_agent_skills off, so a test that reaches a real root
// removes the developer's skill instead of writing one.
//
// Codex and Gemini list both the override-derived root and the HOME-derived
// one. A test that clears the override, or a package that never calls
// SandboxHome, reaches the HOME-derived root. Returns nil when neither HOME nor
// an override resolves, as ConfigTripwire does.
func ambientAgentStoreFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	var bases []string
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		bases = append(bases, filepath.Join(codexHome, "skills"))
	}
	if geminiHome := os.Getenv("GEMINI_CLI_HOME"); geminiHome != "" {
		bases = append(bases, filepath.Join(geminiHome, ".gemini", "skills"))
	}
	if home != "" {
		bases = append(bases,
			filepath.Join(home, ".codex", "skills"),
			filepath.Join(home, ".gemini", "skills"),
			filepath.Join(home, ".config", "amp", "skills"),
			filepath.Join(home, ".config", "devin", "skills"),
		)
	}
	seen := make(map[string]bool, len(bases))
	var files []string
	for _, base := range bases {
		file := filepath.Join(filepath.Clean(base), afSkillDirName, "SKILL.md")
		if !seen[file] {
			seen[file] = true
			files = append(files, file)
		}
	}
	return files
}

// FencedAgentStoreFiles is the list the agent-store tripwire guards, resolved
// from the current environment. It is exported for one consumer: the session
// package's drift test (TestAgentStoreFenceMatchesWhereLaunchesWriteSkills). That
// test launches every supported agent through the real create path and fails if
// af writes a skill file this list does not name, or if the list names a file af
// no longer writes. This package cannot import session, so that test is what
// keeps the hand-kept paths above in step with session/agentskill.go and
// session/ampskill.go.
func FencedAgentStoreFiles() []string {
	return ambientAgentStoreFiles()
}

// agentStoreTripwire snapshots ambientAgentStoreFiles and returns a verify
// func, with the same CREATED/MODIFIED/DELETED rules as the config files.
//
// A snapshot catches writes only. A test that polls the real Codex store
// changes nothing on disk, so no before/after comparison can see it, and this
// check does not claim to. SandboxHome is what keeps reads off the real store.
// This check catches a test that got past the sandbox and wrote or deleted
// something there.
//
// A test that sets its own CODEX_HOME, GEMINI_CLI_HOME or HOME cannot trip this
// check. The roots are resolved once, before the sandbox and before any test
// runs. Later environment changes do not move them. AF_DISABLE_AGENT_STORE_TRIPWIRE=1
// turns it off, for a box where a live af install rewrites its skills during
// the run.
func agentStoreTripwire() func() error {
	if os.Getenv("AF_DISABLE_AGENT_STORE_TRIPWIRE") == "1" {
		return func() error { return nil }
	}
	type snapshot struct {
		path    string
		before  []byte
		existed bool
	}
	var snaps []snapshot
	for _, path := range ambientAgentStoreFiles() {
		before, existed, err := hashFile(path)
		if err != nil {
			continue // unreadable: nothing to guard for this file
		}
		snaps = append(snaps, snapshot{path: path, before: before, existed: existed})
	}
	return func() error {
		for _, snap := range snaps {
			after, exists, err := hashFile(snap.path)
			if err != nil {
				return fmt.Errorf("agent store tripwire: cannot re-read %s after the test run: %w", snap.path, err)
			}
			switch {
			case snap.existed && !exists:
				return fmt.Errorf("agent store tripwire: %s was DELETED during this package's test run — a test escaped its HOME sandbox and removed af's skill from a real agent config root (#4469)", snap.path)
			case snap.existed && !bytes.Equal(snap.before, after):
				return fmt.Errorf("agent store tripwire: %s was MODIFIED during this package's test run — a test escaped its HOME sandbox and rewrote af's skill in a real agent config root (#4469)", snap.path)
			case !snap.existed && exists:
				return fmt.Errorf("agent store tripwire: %s was CREATED during this package's test run — a test escaped its HOME sandbox and wrote af's skill into a real agent config root (#4469)", snap.path)
			}
		}
		return nil
	}
}
