package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// afSkillDirName is the directory- and skill-name every agent that auto-discovers
// a SKILL.md (amp, codex, gemini) reads under its per-agent skills base. It is
// deliberately "agent-factory", NOT "af": those skills dirs are the user's GLOBAL
// per-agent config, shared with skills they install by hand, and "af" is exactly
// the short name a user or another tool would pick for their own af helper skill.
// Namespacing to "agent-factory" makes the directory unambiguously af-owned so we
// never collide with a user's "af" skill (#1585 review).
const afSkillDirName = "agent-factory"

// AfSkillName is afSkillDirName, exported for the plugin generator
// (commands/plugins_gen.go), which names the skill directory and the plugin
// itself after it so the installed artifact matches what af writes at runtime.
const AfSkillName = afSkillDirName

// AfSkillDescription is the one-line description every surface presents for the
// af skill — the SKILL.md frontmatter for amp/codex/gemini, the Claude Code
// slash command (plugin.go), and the generated plugin artifacts. It is what each
// agent surfaces for lazy activation, so there is exactly one of it.
const AfSkillDescription = "Manage Agent Factory (af) sessions, tabs, scheduled tasks, and the daemon via the af CLI"

// afSkillMarker stamps every file we write so a later launch can tell a file it
// owns (safe to regenerate) from a user's/tool's own file that happens to sit at
// our path (never clobber). See writeAfMarkedFile.
const afSkillMarker = "managed by agent-factory (af): regenerated on each af session launch; do not edit"

// afSkillDoc is the SKILL.md served to every agent that auto-discovers a
// name+description skill (amp, codex, gemini). Its body is afUsageReference
// (systemprompt.go) — the SAME text Claude receives via the plugin skill and
// aider via --read — so the agents' af knowledge cannot drift (#1043). All three
// CLIs require only name + description frontmatter (verified against `amp skill
// list`, the codex skill-creator, and the gemini skills docs); the description is
// what each surfaces for lazy activation, so it mirrors the Claude plugin skill's
// description. The afSkillMarker rides in an HTML comment just below the
// frontmatter — invisible in rendered markdown, and the token writeAfMarkedFile
// checks for ownership.
var afSkillDoc = "---\n" +
	"name: " + afSkillDirName + "\n" +
	"description: " + AfSkillDescription + "\n" +
	"---\n" +
	"<!-- " + afSkillMarker + " -->\n" +
	"\n" +
	afUsageReference + "\n"

// writeAfMarkedFile writes content to path NON-DESTRUCTIVELY and reports whether it
// wrote. It only (over)writes a file that carries afSkillMarker (a file we own); a
// file already at path WITHOUT the marker belongs to the user (or another tool)
// and is left untouched (wrote=false, no error) — we never silently destroy it.
// The parent directory is created as needed. An af-owned file is rewritten
// unconditionally, so edits to afUsageReference or the description propagate on the
// next session launch.
func writeAfMarkedFile(path, content string) (bool, error) {
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Contains(existing, []byte(afSkillMarker)) {
			return false, nil
		}
	} else if !os.IsNotExist(readErr) {
		return false, readErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	// Refuses a symlinked path (#3672). The marker check above is what decides
	// whether af may overwrite this file at all, and it reads THROUGH a link: a
	// marked target behind a link would authorize replacing the link itself,
	// which is the arrangement the check exists to protect. Refusing keeps the
	// ownership answer and the write pointed at the same inode.
	if err := config.AtomicWriteFileRefusingLink(path, []byte(content), 0644); err != nil {
		return false, err
	}
	return true, nil
}

// globalSkillConsent is what af knows about the user's answer to "may af manage
// your global agent config?". It has THREE values on purpose (#1933's rule): a
// config af could not read is UNKNOWN, not "no" — collapsing it to "no" would
// let an unreadable config authorize deleting a file, which is the
// fabricated-negative shape this repo keeps getting bitten by.
type globalSkillConsent int

const (
	globalSkillUnknown globalSkillConsent = iota // config unreadable: write nothing, delete nothing
	globalSkillDeclined
	globalSkillGranted
)

// globalAgentSkillsConsent reads the global_agent_skills opt-in.
//
// The key is global-only and defaults FALSE, so on a default install af writes
// nothing into any agent's global config. Enabling it is the user stating that
// af may manage that directory.
func globalAgentSkillsConsent() globalSkillConsent {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.WarningLog.Printf("af skill: could not read the af config (%v); leaving the global agent skills directories exactly as they are", err)
		return globalSkillUnknown
	}
	if cfg.GlobalAgentSkills {
		return globalSkillGranted
	}
	return globalSkillDeclined
}

// ensureAfSkillDir writes the af-managed "agent-factory" skill into the given
// per-agent skills base directory and returns the skill directory path. The agent
// auto-discovers skills there with NO command-line flag, so this injects
// afUsageReference WITHOUT touching the launch command — the spawn stays
// byte-identical to the working no-injection launch. That is the whole point of a
// file seam over a flag: an unknown flag would kill the spawn as an opaque
// readiness timeout (the #1043/#1116/#1131 class). A write failure just means the
// agent loses the af guidance for this launch; callers log and carry on. The write
// is non-destructive (writeAfMarkedFile): a user's own un-marked SKILL.md at our
// path is left untouched and logged.
//
// Every base passed here is the USER'S GLOBAL per-agent config directory
// (codex's $CODEX_HOME/skills, gemini's ~/.gemini/skills, amp's
// ~/.config/amp/skills), which is why the consent gate lives at this one choke
// point rather than in each caller (#1977). A file written there reaches outside
// af and outlives it: it survives archiving the session, survives uninstalling
// af, and is still loaded when the user runs that agent by hand in an unrelated
// directory tomorrow. Creating a session is not consent to edit the user's
// global tool configuration, so af does none of it unless global_agent_skills
// is on. An empty returned path means "not injected"; every caller discards the
// path and only checks the error, so this reads as a clean skip.
//
// The af-owned seams are unaffected and stay unconditional: claude
// (--plugin-dir), aider (--read) and opencode (OPENCODE_CONFIG) all point at
// files under af's OWN config dir, so they vanish with af and are invisible to
// an agent af did not launch. That is the pattern this gate exists to converge
// on; these three agents expose no equivalent per-launch pointer (codex's
// CODEX_HOME and gemini's GEMINI_CLI_HOME relocate the agent's whole home,
// auth and history included, and amp's --settings-file would have af own
// settings the user also sets), so consent is the honest seam until one does.
// ensureAfSkillDir writes — or, when consent is declined, removes — af's skill at
// base, and additionally cleans every legacy base.
//
// legacy exists because the WRITE target moved. A scoped session's skill belongs
// under its account root (#3645), but every scoped launch before that fix wrote
// into the agent's ambient root, and the declined-consent cleanup is what makes
// #1977's promise true: af's edits to the user's global config must not outlive
// the decision not to make them. Cleaning only the new target would quietly narrow
// that promise for an operator who launches nothing but scoped sessions — the
// stale ambient file would then survive forever (#3645 review).
//
// Nothing is ever WRITTEN to a legacy base. It is a cleanup list, not a second
// destination.
func ensureAfSkillDir(base string, legacy ...string) (string, error) {
	skillDir := filepath.Join(base, afSkillDirName)
	path := filepath.Join(skillDir, "SKILL.md")

	switch globalAgentSkillsConsent() {
	case globalSkillGranted:
	case globalSkillDeclined:
		// Also clean up what an EARLIER af version wrote here before this key
		// existed, so af's edits to the user's global config do not outlive the
		// decision not to make them (#1977's first objection).
		removeAfSkillDir(skillDir, path)
		for _, other := range legacy {
			if other == "" || other == base {
				continue
			}
			legacyDir := filepath.Join(other, afSkillDirName)
			removeAfSkillDir(legacyDir, filepath.Join(legacyDir, "SKILL.md"))
		}
		// INFO, not WARNING (#2166): global_agent_skills defaults false, so this
		// fires on every codex/gemini/amp session start on a DEFAULT install. af
		// is honoring the documented default, which is not a defect.
		log.InfoLog.Printf("af skill: not writing %s — af does not manage global agent config directories unless global_agent_skills = true is set in the af config (af guidance not injected for this agent)", path)
		return "", nil
	default: // globalSkillUnknown
		return "", nil
	}

	wrote, err := writeAfMarkedFile(path, afSkillDoc)
	if err != nil {
		return "", err
	}
	if !wrote {
		// INFO, not WARNING (#2166): the user owns a file at our path and af is
		// deliberately not overwriting it. That is the designed outcome of the
		// marker guard, repeated on every session start, not something to fix.
		log.InfoLog.Printf("af skill: %s exists but is not af-managed; leaving it untouched (af guidance not injected)", path)
	}
	return skillDir, nil
}

// removeAfSkillDir deletes the skill af ITSELF wrote into a global agent config
// directory, and nothing else.
//
// Every step is gated on a POSITIVE observed fact, never an inference:
//
//   - The file is removed only when it is present AND carries afSkillMarker. A
//     file at our path without the marker is the user's (or another tool's) and
//     is left alone, exactly as writeAfMarkedFile refuses to overwrite it.
//   - A read that FAILS for any reason other than not-exist (permissions, I/O)
//     leaves the file alone. "I could not look" is not "there is nothing of
//     value here" — that conflation is the #1969/#2011 class.
//   - A SYMLINK at our path is left alone, because the marker was read through
//     it: the fact observed belongs to the target, not to the link (#3672).
//   - The enclosing agent-factory/ directory is removed with os.Remove, not
//     RemoveAll, so the kernel refuses (ENOTEMPTY) if anything else is in there.
//     A user who dropped their own file beside ours keeps it, and we never have
//     to predict what else the directory holds.
func removeAfSkillDir(skillDir, path string) {
	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.WarningLog.Printf("af skill: could not read %s to check whether af wrote it (%v); leaving it in place", path, err)
		}
		return
	}
	if !bytes.Contains(existing, []byte(afSkillMarker)) {
		return
	}
	// The same refusal as writeAfMarkedFile, and for the sharper version of its
	// reason: os.ReadFile above followed the link to find the marker, so a plain
	// os.Remove would unlink the LINK on the strength of the TARGET's content —
	// deleting the user's arrangement while af's own file survives. That is the
	// #3672 asymmetry, pointed at the delete side.
	if err := config.RemoveFileRefusingLink(path); err != nil {
		log.WarningLog.Printf("af skill: could not remove the af-managed %s (%v); it stays until removed by hand", path, err)
		return
	}
	// Best-effort, and deliberately not RemoveAll: this succeeds only if the
	// directory is now empty.
	_ = os.Remove(skillDir)
	// INFO, not WARNING (#2166): this reports a cleanup that SUCCEEDED, taken
	// because the user's config says af may not manage that directory.
	log.InfoLog.Printf("af skill: removed the af-managed %s — af no longer writes into global agent config directories (set global_agent_skills = true to restore it)", path)
}

// codexSkillsBaseDir returns codex's skills-discovery base: $CODEX_HOME/skills, or
// $HOME/.codex/skills when CODEX_HOME is unset. Verified against codex-cli 0.144.1,
// whose skill-creator documents placing a skill in "$CODEX_HOME/skills (or
// ~/.codex/skills when CODEX_HOME is unset) so Codex can discover it
// automatically". This retires the #1043 wall: codex 0.144.1 auto-discovers user
// skills dropped here (its own built-ins live under a sibling .system/ dir), so af
// no longer needs to stuff afUsageReference into -c developer_instructions.
func codexSkillsBaseDir(target skillTarget) (string, error) {
	if target.unresolved {
		return "", errUnresolvedAccountSkillRoot
	}
	// The account root SUBSTITUTES for the variable, because the account boundary
	// sets that variable to exactly this directory. One rule serves both agents:
	// the skills base is a function of the agent's config-root VALUE, and scoping a
	// session changes that value (#3645).
	if codexHome := target.root; codexHome != "" {
		return filepath.Join(codexHome, "skills"), nil
	}
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "skills"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "skills"), nil
}

// ensureCodexSkillDir writes the af skill into codex's skills base. See
// ensureAfSkillDir and codexSkillsBaseDir.
func ensureCodexSkillDir(target skillTarget) (string, error) {
	base, err := codexSkillsBaseDir(target)
	if err != nil {
		return "", err
	}
	return ensureAfSkillDir(base, ambientSkillBase(codexSkillsBaseDir, target))
}

// geminiSkillsBaseDir returns gemini's USER-scope skills base:
// $GEMINI_CLI_HOME/.gemini/skills, or $HOME/.gemini/skills when GEMINI_CLI_HOME is
// unset. Verified against gemini-cli 0.42.0: user skills are discovered under
// ~/.gemini/skills, and GEMINI_CLI_HOME relocates the .gemini dir (it creates a
// .gemini folder inside the given path — enterprise docs). Gemini scans this dir at
// session start and ENABLES a dropped skill automatically (verified: `gemini skills
// list --all` reports agent-factory [Enabled]).
func geminiSkillsBaseDir(target skillTarget) (string, error) {
	if target.unresolved {
		return "", errUnresolvedAccountSkillRoot
	}
	// GEMINI_CLI_HOME is a HOME-like root — the CLI appends `.gemini/` itself — so
	// the account directory takes the variable's place and the skills land at
	// <account dir>/.gemini/skills. Same substitution as codex, different shape,
	// which is exactly the distinction #3387 measured and #3609 wrote down.
	if geminiHome := target.root; geminiHome != "" {
		return filepath.Join(geminiHome, ".gemini", "skills"), nil
	}
	if geminiHome := os.Getenv("GEMINI_CLI_HOME"); geminiHome != "" {
		return filepath.Join(geminiHome, ".gemini", "skills"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "skills"), nil
}

// ensureGeminiSkillDir writes the af skill into gemini's user-scope skills base.
// See ensureAfSkillDir and geminiSkillsBaseDir.
func ensureGeminiSkillDir(target skillTarget) (string, error) {
	base, err := geminiSkillsBaseDir(target)
	if err != nil {
		return "", err
	}
	return ensureAfSkillDir(base, ambientSkillBase(geminiSkillsBaseDir, target))
}

func ensureDevinSkillDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return ensureAfSkillDir(filepath.Join(home, ".config", "devin", "skills"))
}

// aiderReadDoc is the read-only context file aider loads via --read. Aider has NO
// auto-discovered global skills/instructions directory (its only persistent
// global-instructions seams are the --read flag and the user-owned .aider.conf.yml
// "read:" list; verified against aider 0.86.2). So — unlike amp/codex/gemini — af
// injects the skill by pointing a --read flag at this af-owned file, exactly as
// claude uses --plugin-dir. It is plain markdown (aider adds it to the chat as a
// read-only file), not a SKILL.md, so it needs no frontmatter; the afSkillMarker
// rides in a leading HTML comment for ownership.
var aiderReadDoc = "<!-- " + afSkillMarker + " -->\n\n" + afUsageReference + "\n"

// aiderReadFilePath returns the path of the af-owned aider context file, under the
// af config dir (config.GetConfigDir()). It is NOT written into the user's worktree
// (that would pollute git status) nor into .aider.conf.yml (that is the user's
// file). The path is af-exclusive, so --read can point at it safely.
func aiderReadFilePath() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "aider", "af-skill.md"), nil
}

// ensureAiderReadFile writes aiderReadDoc to the af-owned aider context file and
// returns its path, non-destructively. An empty path (with nil error) means the
// file exists but is NOT af-managed — the caller must then skip injecting --read
// rather than point it at a file we do not own. A non-af file at our own path is
// nearly impossible (af owns the dir), but the marker guard is honored uniformly.
func ensureAiderReadFile() (string, error) {
	path, err := aiderReadFilePath()
	if err != nil {
		return "", err
	}
	wrote, err := writeAfMarkedFile(path, aiderReadDoc)
	if err != nil {
		return "", err
	}
	if !wrote {
		// INFO, not WARNING (#2166): same marker-guard skip as ensureAfSkillDir.
		log.InfoLog.Printf("af skill: %s exists but is not af-managed; not injecting --read for aider", path)
		return "", nil
	}
	return path, nil
}

// skillTarget says WHERE af's guidance skill belongs for one launch.
//
// The two agents that discover af's guidance from a config root — codex, from
// $CODEX_HOME/skills, and gemini, from <GEMINI_CLI_HOME>/.gemini/skills — read
// that root from an environment variable the ACCOUNT BOUNDARY sets per session.
// af writes the skill from the daemon, before that variable exists, so reading it
// out of the daemon's own environment described the daemon and not the session:
// an account-scoped session searched its account directory while af had written
// into the operator's ambient home, and the opted-in guidance was simply absent
// (#3645).
//
// claude is the contrast that names the defect. af hands claude its guidance with
// an explicit `--plugin-dir <af-owned path>` on the command line, so it does not
// depend on where CLAUDE_CONFIG_DIR points and account scoping cannot lose it.
// aider (--read) and opencode (an env seam at an af-owned path) are handed theirs
// the same way. Guidance passed BY PATH survives; guidance DISCOVERED from a
// config root does not. Preferring the explicit form was the first choice here and
// neither CLI offers one — gemini's `skills` subcommand only manages skills inside
// the already-discovered scope, and codex 0.152.1 has no skills subcommand at all
// (both checked against the installed binaries) — so the remaining option is to
// write into the account's own root.
//
// THREE states, not a bool. "unscoped" and "scoped but af cannot say where" are
// different answers, and collapsing them fails OPEN: the unresolved case would
// take the daemon's root, which is the exact defect this type exists to close.
type skillTarget struct {
	// root is the agent config root the account boundary will install for this
	// launch, or "" for an unscoped session, which reads the daemon's own.
	root string
	// unresolved is true when the session IS account-scoped and af could not
	// resolve which directory that is. Writing nothing is the honest answer — a
	// skill in the wrong root is invisible to the session AND edits a directory
	// the operator did not select — and the launch itself refuses moments later
	// for the same reason the resolution failed.
	unresolved bool
}

// errUnresolvedAccountSkillRoot reports that af declined to place the skill
// because it could not name the account's config root. It is not a launch
// failure: injectSystemPrompt logs it and returns the command unchanged, exactly
// as it does for a skills directory it cannot write.
var errUnresolvedAccountSkillRoot = errors.New(
	"cannot place the af skill: the session is account-scoped but af could not resolve the account's directory")

// resolveSkillTarget answers skillTarget for one launch.
//
// It resolves the account HERE rather than taking a directory from the caller
// because the daemon is where injectSystemPrompt runs, and the local launch defers
// account resolution to the exec shim — so no caller on that path has the
// directory to hand. agentaccount.Selected is the same resolver the boundary uses,
// so the skill cannot land somewhere the session will not read.
//
// The agent is derived from the RESOLVED command, never from i.Program: an account
// validated in one agent's namespace with program_overrides pointing at another is
// refused later (#3082/#3108), and until then af must not write into the account
// directory of an agent this session is not running.
func resolveSkillTarget(i *Instance, program string) skillTarget {
	if i == nil {
		return skillTarget{}
	}
	agent := tmux.DetectAgentFromCommand(program)
	if agent == "" {
		// An opaque command names no agent, so there is no per-agent skills base to
		// choose and injectSystemPrompt will not write one either.
		return skillTarget{}
	}
	configVar, scopable := sessionenv.SupportsAccounts(agent)

	// A MOUNTED ACCOUNT ROOT belongs to the host, and this process may be inside
	// the container af mounted it into.
	//
	// The in-container agent-server builds its instance with NO account and the
	// local backend (daemon/agentserver_headless.go), while docker has pointed the
	// agent's config variable at /af-account — a writable bind mount of the HOST's
	// account directory. Without this the container reads that as its own ambient
	// root: with its own /af-home config, where global_agent_skills defaults false,
	// the declined-consent cleanup would DELETE the af-marked skill the host's
	// scoped launch had just written, and race that launch's discovery (#3645
	// review).
	//
	// Unresolved rather than unscoped, and that distinction is the point: the
	// container cannot know whether the host operator opted in, so it must neither
	// write nor clean. The host owns that directory.
	if scopable && underDockerAccountHome(os.Getenv(configVar)) {
		return skillTarget{unresolved: true}
	}

	name := strings.TrimSpace(i.Account)
	if name == "" {
		return skillTarget{}
	}
	if !scopable {
		// The session names an account for an agent that cannot be scoped. The
		// launch refuses this; until it does, af must write nothing rather than
		// guess a directory.
		return skillTarget{unresolved: true}
	}
	// The account name was validated in the namespace of the session's OWN agent,
	// and account namespaces are separate — the same name means a DIFFERENT
	// identity for a different agent (#3082/#3108). So a launch whose resolved
	// command is another agent must not resolve this name against that agent's
	// registry: a handoff to gemini, or a cross-agent program_overrides, would
	// otherwise write into a gemini account the operator never selected, moments
	// before the launch refuses for exactly that reason (#3645 review).
	if requested := sessionenv.AgentForCommand(i.AgentProgram()); requested != agent {
		return skillTarget{unresolved: true}
	}
	home, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("af skill: cannot locate the agent-factory home to resolve account %q for %s: %v", name, agent, err)
		return skillTarget{unresolved: true}
	}
	account, err := agentaccount.Selected(home, agent, name, "")
	if err != nil {
		log.WarningLog.Printf("af skill: cannot resolve account %q for %s: %v", name, agent, err)
		return skillTarget{unresolved: true}
	}
	if account.Dir == "" {
		return skillTarget{unresolved: true}
	}
	return skillTarget{root: account.Dir}
}

// underDockerAccountHome reports whether a config root is the account directory
// af bind-mounts into a container, or a path inside it. Compared as PATH
// SEGMENTS, never as a string prefix, so a sibling named /af-account-backup is
// not swept in with it.
func underDockerAccountHome(root string) bool {
	if root == "" {
		return false
	}
	clean := filepath.Clean(root)
	if clean == dockerAccountHome {
		return true
	}
	return strings.HasPrefix(clean, dockerAccountHome+string(filepath.Separator))
}

// ambientSkillBase is where this agent's skill would have gone before the account
// root became the target — the location a declined-consent launch must still
// clean. Empty for an unscoped launch, whose base already IS the ambient one, and
// empty when the ambient base cannot be resolved, since there is then no path to
// clean rather than a path to guess.
func ambientSkillBase(base func(skillTarget) (string, error), target skillTarget) string {
	if target.root == "" {
		return ""
	}
	ambient, err := base(skillTarget{})
	if err != nil {
		return ""
	}
	return ambient
}
