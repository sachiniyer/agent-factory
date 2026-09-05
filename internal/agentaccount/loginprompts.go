package agentaccount

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/afhome"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// answerLoginPrompts records, in an account's OWN credential home, the answers
// to the questions the agent's login flow would otherwise ask about af's
// directory (#3858).
//
// THE BOUNDARY THIS DOES NOT CROSS. af still never reads, writes, or forwards a
// credential — the rule #3051/#2983/#3384 are built on. What it writes is the
// agent's own non-secret configuration, about af's own 0700 directory, with the
// only value that directory can have: "af made this folder, and the identity in
// it is the one you are about to sign in to." A registered account is af's
// answer to "whose identity is this", so an ambient API key selected here would
// be the exact thing accounts exist to avoid, and a folder-trust question about
// a directory af created has one honest answer.
//
// It is deliberately a TABLE with one entry rather than a branch. Which agents
// af writes settings for is a question a reviewer must be able to answer by
// looking, the way loginCommands answers "which agents does af know a login
// invocation for" — and the absent agents are the claim: claude and codex reach
// their device-code prompt with an empty account directory, measured in #3857,
// so there is nothing for af to answer on their behalf.
var loginPromptAnswers = map[string]func(dir string) error{
	"gemini": answerGeminiLoginPrompts,
}

// answerLoginPrompts runs the agent's entry, if it has one. An agent with no
// entry is untouched — a registration must not leave files in an account
// directory for a flow that never asks anything.
func answerLoginPrompts(agent, dir string) error {
	answer, ok := loginPromptAnswers[agent]
	if !ok {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve account directory %s: %w", dir, err)
	}
	if err := answer(abs); err != nil {
		// Named, because the underlying error is about a file the operator has
		// never heard of. A registration that fails here failed to write inside a
		// directory af had just created 0700 in its own home, and the account
		// could not have held a credential either.
		return fmt.Errorf("record %s's start-up answers for this account: %w", agent, err)
	}
	return nil
}

// gemini 0.51.0's two prompts, and where each one's answer lives.
//
// Measured on this box against the installed CLI, and confirmed in its bundle
// (packages/core/src/config/storage.ts, packages/core/src/utils/paths.ts):
// homedir() is `process.env["GEMINI_CLI_HOME"] || os.homedir()`, and both files
// hang off <homedir()>/.gemini/ — so they are the ACCOUNT's, not the machine's,
// exactly as the credential is.
//
//   - the folder-trust dialog ("Do you trust the files in this folder?") →
//     trustedFolders.json, a flat map of absolute path to trust level.
//   - the auth picker ("How would you like to authenticate for this project?") →
//     settings.json, security.auth.selectedType.
const (
	geminiHomeDirName        = ".gemini"
	geminiSettingsFile       = "settings.json"
	geminiTrustedFoldersFile = "trustedFolders.json"
	// geminiGoogleLogin is the "Sign in with Google" option, which the bundle
	// spells `"oauth-personal" /* LOGIN_WITH_GOOGLE */`. Its siblings are
	// `gemini-api-key` (USE_GEMINI) and `vertex-ai` (USE_VERTEX_AI), and neither
	// is a thing af may choose for an operator: both are ambient identities read
	// from the environment, which is what an account exists to replace.
	geminiGoogleLogin = "oauth-personal"
	// geminiTrustFolder is the trust level for the folder itself, as opposed to
	// geminiTrustParent (its parent, which would trust every sibling account) and
	// DO_NOT_TRUST.
	geminiTrustFolder = "TRUST_FOLDER"
	// geminiTrustParent trusts a rule path's PARENT, so a rule carrying it decides
	// one directory up from where it is written.
	geminiTrustParent = "TRUST_PARENT"
)

// geminiTrustLevels is gemini's TrustLevel enum. A value outside it makes the
// CLI raise a FatalConfigError on startup rather than ignore the entry, which is
// why finding one means af leaves the whole document alone.
var geminiTrustLevels = map[string]bool{
	geminiTrustFolder: true,
	geminiTrustParent: true,
	"DO_NOT_TRUST":    true,
}

// answerGeminiLoginPrompts writes both answers, each only where the account has
// not already answered for itself.
func answerGeminiLoginPrompts(dir string) error {
	if err := selectGeminiGoogleLogin(dir); err != nil {
		return err
	}
	return trustGeminiAccountDir(dir)
}

// selectGeminiGoogleLogin sets security.auth.selectedType, which is what removes
// the auth picker.
//
// It is also the setting that does the most work: with it present, gemini runs
// the sign-in BEFORE it mounts its interactive UI, so the folder-trust dialog —
// which the UI raises — never gets a chance to appear ahead of the device-code
// prompt either. Measured both ways: with only this file the pane's first frame
// is "Please visit the following URL…", and with only the trust file it is the
// picker.
func selectGeminiGoogleLogin(dir string) error {
	path := filepath.Join(dir, geminiHomeDirName, geminiSettingsFile)
	doc, ours, err := readAgentJSON(path)
	if err != nil || !ours {
		return err
	}
	security, ok := objectAt(doc, "security")
	if !ok {
		return nil
	}
	auth, ok := objectAt(security, "auth")
	if !ok {
		return nil
	}
	if chosen, present := auth["selectedType"]; present {
		// ANY existing value stands, including one af would not have picked. The
		// operator (or gemini, after they answered the picker once) chose it, and
		// a registration that silently re-pointed an account at a different
		// identity provider is a worse bug than a prompt.
		if text, isString := chosen.(string); !isString || strings.TrimSpace(text) != "" {
			return nil
		}
	}
	auth["selectedType"] = geminiGoogleLogin
	return writeAgentJSON(path, doc)
}

// trustGeminiAccountDir adds the account directory to gemini's trusted folders,
// which is what removes the folder-trust dialog from the pane that runs after
// the sign-in — and from a re-login against an account that already holds a
// credential, where gemini goes straight to its interactive UI and no
// device-code prompt covers for it.
//
// The blast radius is one directory, and it is af's. Trusting a folder lets
// gemini load PROJECT-scope configuration from it — GEMINI.md, commands, hooks,
// MCP servers — and the only pane whose working directory is the account
// directory is the login pane. A session runs with the same GEMINI_CLI_HOME but
// its own worktree as the working directory, so this entry says nothing about
// any repository and the session's own trust dialog is untouched.
//
// The alternatives were both wider. `GEMINI_CLI_TRUST_WORKSPACE=true` in the
// pane environment trusts whatever directory the process happens to be in, and
// `security.folderTrust.enabled: false` in the account's settings would disable
// the check for every session that account ever runs. A path-scoped rule is the
// narrowest lever gemini offers.
func trustGeminiAccountDir(dir string) error {
	path := filepath.Join(dir, geminiHomeDirName, geminiTrustedFoldersFile)
	doc, ours, err := readAgentJSON(path)
	if err != nil || !ours {
		return err
	}
	covered, ok := geminiTrustRuleCovers(doc, dir)
	if !ok || covered {
		return nil
	}
	doc[dir] = geminiTrustFolder
	return writeAgentJSON(path, doc)
}

// geminiTrustRuleCovers reports whether some existing rule already decides this
// directory, and whether the document is one af understands well enough to add
// to at all.
//
// Asking about COVERAGE rather than about the exact key is what keeps af from
// overriding a decision the operator made. gemini resolves both sides through
// realpath and then takes the LONGEST matching rule path, so af's exact-path
// entry would outrank a DO_NOT_TRUST an operator had put on an ancestor — af
// would quietly re-trust a directory they had deliberately distrusted. A rule
// that already reaches this directory, whatever it says, is left to stand.
func geminiTrustRuleCovers(doc map[string]any, dir string) (covered, understood bool) {
	// Every value is validated BEFORE any coverage is decided, so the answer does
	// not depend on Go's randomized map order: a document holding both a covering
	// rule and an unreadable one must give the same verdict every time.
	for _, level := range doc {
		text, isString := level.(string)
		if !isString || !geminiTrustLevels[text] {
			// gemini fatals on a value outside its enum, so this document is
			// already broken or newer than af. Neither is af's to rewrite.
			return false, false
		}
	}
	target := pathutil.ResolveForCompare(dir)
	for rulePath, level := range doc {
		effective := rulePath
		if level == geminiTrustParent {
			effective = filepath.Dir(rulePath)
		}
		if pathutil.IsAtOrInside(target, pathutil.ResolveForCompare(effective)) {
			return true, true
		}
	}
	return false, true
}

// objectAt returns the nested object at key, creating an empty one when the key
// is absent.
//
// The false result is "the key holds something that is not an object", and it
// means af stops: a caller that overwrote it would delete whatever the operator
// (or a newer CLI) put there. Creating on absence is safe because nothing is
// written until the caller reaches its assignment; an abandoned branch is
// discarded with the document.
func objectAt(doc map[string]any, key string) (map[string]any, bool) {
	existing, present := doc[key]
	if !present {
		child := map[string]any{}
		doc[key] = child
		return child, true
	}
	child, ok := existing.(map[string]any)
	return child, ok
}

// readAgentJSON loads one of an agent's own JSON settings documents.
//
// The second result is whether this is a document af may ADD TO. A missing file
// is (an empty document, true): the ordinary case for an account af just made.
// A file af cannot parse is (nil, false) with NO error, and that is a decision
// rather than an oversight — gemini reads its settings through
// strip-json-comments, so a document with comments in it is valid to the CLI and
// unparseable to encoding/json. Reporting it as a failure would refuse a
// registration over a file that works, and rewriting it would delete the
// operator's comments and any key af does not model. Leaving it alone costs the
// account nothing but the prompt it had before.
//
// An I/O failure IS returned. af has just created this directory 0700 inside its
// own home; not being able to read inside it is not a formatting opinion.
func readAgentJSON(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false, nil
	}
	if doc == nil {
		// Literal `null` parses without error into a nil map. Writing to it would
		// panic, and it is not a document af has any business replacing.
		return nil, false, nil
	}
	return doc, true, nil
}

// writeAgentJSON persists a settings document beside its neighbours.
//
// Through a temp file and a rename, for the reason every other af write is: a
// short write or a crash mid-write leaves a truncated settings.json, and gemini
// treats a trustedFolders.json it cannot parse as a FATAL config error — af
// would have bricked the account it was smoothing. The rename also means the
// write never follows a symlink at the destination.
//
// It hand-rolls the write rather than calling config.AtomicWriteFile for the
// reason Register hand-rolls its own directory creation: this package
// deliberately depends on no heavy af package. The AF-home latch is still
// honoured, through afhome.MkdirAll.
func writeAgentJSON(path string, doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := afhome.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".af-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Explicitly, not on CreateTemp's word: this is af's own posture for anything
	// it puts in an account directory, and it should be visible here rather than
	// inherited from another package's documented default.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(staged, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}
