package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
)

// checkProgramCommand resolves command the way the session launch will,
// anchored to repoWorkspace when the doctor runs inside a git repository,
// mirroring the daemon-side #5779 fix (preflight.LocalSessionPrereqsAt takes
// repo.WorkspacePath()): a relative program_overrides path resolves against the
// session's launch directory, never the doctor's incidental cwd. The returned
// outsideRepo flag is true only when the doctor is genuinely outside any git
// repository (config.ErrNotGitRepository) AND the failed command's resolution
// depends on the working directory: there the launch directory cannot be
// determined, so today's preflight.CheckCommand (resolved against the doctor's
// own cwd) is kept and the caller reports any failure with an honest message
// instead of install or reinstall guidance (#6487). A cwd-independent command
// (a bare name resolved through PATH, or an absolute path) resolves the same
// way regardless of where the doctor was run, so a failure there is a genuine
// install/permission problem and keeps today's classified ProgramError
// remediation — the honest message is reserved for the relative paths the
// doctor can actually misresolve. A failed repo probe for any other reason
// (git missing from PATH, an unreadable .git, …) is not "outside a repo" — the
// doctor may be inside the session's repo but could not prove it, so today's
// CheckCommand is kept with its normal classified remediation.
func checkProgramCommand(repoWorkspace string, repoErr error, command string) (*preflight.ProgramCheck, error, bool) {
	if repoErr == nil {
		check, err := preflight.CheckCommandAt(command, repoWorkspace)
		return check, err, false
	}
	check, err := preflight.CheckCommand(command)
	if errors.Is(repoErr, config.ErrNotGitRepository) && commandDependsOnCwd(check, command) {
		return check, err, true
	}
	return check, err, false
}

// commandDependsOnCwd reports whether command's resolution depends on the
// working directory the doctor happened to be run from. It gates on the
// executables preflight actually resolved — check.Executable and
// check.ShellExecutable, plus their resolution's working-dir dependence —
// rather than re-parsing the command, so env -i/-u/-C forms, relative PATH=
// entries, opaque wrappers, and quoted paths all land on the right answer
// (#6792 item 3); commandToken's coarse split is the fallback only for
// commands that failed before any executable resolved. Only a relative
// executable — a path containing a separator that is not absolute, e.g.
// "bin/claude" — or a lookup that walked a relative PATH entry is
// cwd-dependent: a bare name resolved through absolute PATH entries and an
// absolute path resolve the same way regardless of cwd. Outside a repo the
// doctor can only misresolve the cwd-dependent case, so the honest no-anchor
// message is reserved for it (#6487).
func commandDependsOnCwd(check *preflight.ProgramCheck, command string) bool {
	if check != nil {
		if check.ShellExecutable != "" && check.ShellPath == "" {
			// The wrapper's own resolution failed before the agent's ran, so
			// the agent's via flag was never produced — token relativity is
			// all that is known about either executable.
			return isRelativeExecutable(check.ShellExecutable) || isRelativeExecutable(check.Executable)
		}
		if check.Executable != "" || check.ShellExecutable != "" {
			// The resolution ran: the via flags already say whether its (and
			// the wrapper's) lookup depended on the directory. They are the
			// answer even when the token itself is relative — under an
			// absolute env -C the token joins a fixed literal dir, not the
			// launch dir.
			return check.PathViaWorkingDir || check.ShellViaWorkingDir
		}
	}
	return isRelativeExecutable(commandToken(command))
}

// isRelativeExecutable reports whether token is a cwd-dependent executable:
// a path containing a separator that is not absolute.
func isRelativeExecutable(token string) bool {
	return token != "" && !filepath.IsAbs(token) && strings.ContainsAny(token, `/\`)
}

// commandToken returns the executable token command launches — the first shell
// word after any leading VAR=value assignments and an optional `env` wrapper,
// which may itself be followed by further assignments. It is a deliberately
// coarse split on whitespace, used only when preflight could not report a
// resolved executable at all (commandDependsOnCwd's fallback): the real gate
// reads check.Executable and check.ShellExecutable, which the full
// env-aware model fills in (#6792 item 3).
func commandToken(command string) string {
	fields := strings.Fields(command)
	i := 0
	for i < len(fields) && isShellAssignment(fields[i]) {
		i++
	}
	if i < len(fields) && filepath.Base(fields[i]) == "env" {
		i++
		for i < len(fields) && isShellAssignment(fields[i]) {
			i++
		}
	}
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

// isShellAssignment reports whether word is a leading VAR=value shell
// assignment, the same shape preflight.command_parse skips before the
// executable. Mirrored here so doctor's advisory check stays self-contained.
func isShellAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for pos, r := range name {
		if pos == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// noAnchorMessage is the honest failure message for a program check the doctor
// ran from outside any git repository: af doctor resolved the command against
// its own working directory, which is not the directory any session launches in,
// so the result is not authoritative for a relative program_overrides path. It
// points the user at the session's repo rather than at installing or
// reinstalling a binary (#6487).
func noAnchorMessage(command string) string {
	return fmt.Sprintf("%q did not resolve from this directory — `af doctor` is not inside a git repository, "+
		"so it checked the command against its own working directory, which is not where a session launches. "+
		"A relative program_overrides path resolves in each session's workspace; run `af doctor` from inside "+
		"the session's repo to verify it against the directory the session will launch in.", command)
}

// overrideAbsentFromSessionBase reports (detail, remediation) for a resolved
// executable a fresh session worktree would not provide as a runnable file,
// or ("","") when the command's resolved executables are all unaffected. A
// session launches in a worktree created from the tree it branches from —
// origin's default branch when the repo has one, else HEAD
// (session/git/worktree_ops.go) — so a resolution that depended on the launch
// directory and landed inside this workspace is honest only when that base
// tree holds a runnable file at the same repo-relative path (#6792 items
// 1–3). The candidates are what preflight actually resolved — the detected
// program plus the shell wrapper it launches through — gated on each
// resolution's working-dir dependence rather than a re-parse of the command,
// so env -i/-u/-C forms, relative PATH= entries, opaque wrappers, and quoted
// paths are covered the same way. An absolute token or an absolute PATH entry
// resolves the same file from any directory and is not the worktree's
// problem. Probe failures are conservative: a git error never downgrades a
// PASS on its own.
func overrideAbsentFromSessionBase(workspace string, check *preflight.ProgramCheck) (string, string) {
	if workspace == "" || check == nil {
		return "", ""
	}
	var ref string
	refOK, refResolved := false, false
	baseRef := func() (string, bool) {
		if !refResolved {
			ref, refOK = sessionBaseRef(workspace)
			refResolved = true
		}
		return ref, refOK
	}
	// probeCandidate answers "would the session resolve a runnable file at
	// this candidate": launch-dir-independent answers (an absolute token or
	// PATH entry, an absolute env -C's dir) name the same file at launch, so
	// a runnable stat now is the launch answer; launch-dir answers must map
	// inside this workspace to a runnable entry in the base tree — they are
	// the worktree's contents, not this checkout's. provable=false means the
	// probe cannot answer (path outside the repo, no base ref) and the
	// candidate proves nothing either way.
	probeCandidate := func(cand preflight.ExecutableCandidate) (verdict baseTreeEntry, display string, provable bool) {
		if !cand.ViaWorkingDir {
			if runnableFile(cand.Path) {
				return baseTreeRunnable, cand.Path, true
			}
			return baseTreeAbsent, cand.Path, true
		}
		rel, err := filepath.Rel(workspace, cand.Path)
		if err != nil || rel == "" || rel == "." || filepath.IsAbs(rel) ||
			rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return baseTreeUnknown, "", false // resolved outside the repo; cannot map to the base tree
		}
		r, ok := baseRef()
		if !ok {
			return baseTreeUnknown, "", false
		}
		// The display for a worktree-bound candidate is its repo-relative
		// path — that IS the path the session resolves, under a different
		// root.
		return baseTreeProbe(workspace, r, rel), rel, true
	}
	for _, grp := range []struct {
		token, path string
		via         bool
		candidates  []preflight.ExecutableCandidate
		wrapper     bool
		// proven is false when an opaque prefix leaves the modeled launch
		// dir unverified (preflight's LaunchDirKnown) — the launch answer
		// is then reported unverified instead of probing the wrong dir.
		proven bool
	}{
		{check.Executable, check.Path, check.PathViaWorkingDir, check.PathCandidates, false, check.LaunchDirKnown},
		// The wrapper token itself is resolved by the session's shell in the
		// session's launch dir — always proven, unlike the agent's post-
		// wrapper cwd which an opaque prefix can move (LaunchDirKnown).
		{check.ShellExecutable, check.ShellPath, check.ShellViaWorkingDir, check.ShellCandidates, true, true},
	} {
		candidates := grp.candidates
		if len(candidates) == 0 {
			if grp.path == "" {
				continue
			}
			// A path-form executable is a single-point lookup — no fallback
			// candidates beyond the resolved file itself.
			candidates = []preflight.ExecutableCandidate{{Path: grp.path, ViaWorkingDir: grp.via}}
		}
		// selIdx is the location this checkout's lookup selected — the first
		// runnable-now candidate, which is not necessarily the first entry:
		// earlier locations may hold a file the checkout cannot run but a
		// clean worktree can (committed-but-locally-deleted).
		selIdx := -1
		for i, cand := range candidates {
			if cand.Path == grp.path {
				selIdx = i
				break
			}
		}
		if selIdx < 0 {
			candidates = append([]preflight.ExecutableCandidate{{Path: grp.path, ViaWorkingDir: grp.via}}, candidates...)
			selIdx = 0
		}
		// Only candidates that can precede or supply the launch winner make
		// the lookup launch-dir-dependent: trailing relative entries after
		// an absolute selection are unreachable at launch and must not gate.
		launchDependent := grp.via
		for i := 0; i < selIdx && !launchDependent; i++ {
			launchDependent = candidates[i].ViaWorkingDir
		}
		if !grp.proven && launchDependent {
			// The modeled launch dir is unverified — an opaque prefix may
			// change it — so the tree probe answers the wrong question
			// entirely. Report the launch resolution unverified outright
			// rather than probing a dir the session may not run in.
			r, _ := baseRef()
			return unprovenLaunchDirDetail(check.Command, grp.token, grp.path, grp.wrapper, r),
				unprovenLaunchDirRemediation()
		}
		// Walk the lookup in order and simulate the launch: each location's
		// launch answer is its own verdict, so the session's resolution is
		// the FIRST provably-runnable location — which may sit before the
		// checkout's selection (a committed file missing locally) or after
		// it (an installed fallback past an untracked one).
		type probe struct {
			verdict  baseTreeEntry
			display  string
			provable bool
		}
		results := make([]probe, len(candidates))
		launchIdx, launchDisplay := -1, ""
		for i, cand := range candidates {
			results[i].verdict, results[i].display, results[i].provable = probeCandidate(cand)
			if results[i].provable && results[i].verdict == baseTreeRunnable {
				// The first launch-runnable location IS the session's answer —
				// probing trailing entries cannot change it, and each probe can
				// cost a gitProbeTimeout when the object store stalls.
				launchIdx, launchDisplay = i, results[i].display
				break
			}
		}
		// The verdict is established only when every location that could
		// affect it was proven: an unproven location before the apparent
		// winner could hold a committed file the session resolves instead —
		// any unknown before the outcome means the outcome is unverified.
		// Locations after a proven winner are unreachable at launch, and
		// when nothing proved runnable every location's answer counts.
		firstUncertain := -1
		limit := len(results)
		if launchIdx >= 0 {
			limit = launchIdx
		}
		for i := 0; i < limit; i++ {
			if !results[i].provable || results[i].verdict == baseTreeUnknown {
				firstUncertain = i
				break
			}
		}
		if launchIdx == selIdx && firstUncertain < 0 {
			continue // the session resolves the same file — the PASS is honest
		}
		r, _ := baseRef()
		var detail, remediation string
		sel := results[selIdx]
		switch {
		case firstUncertain >= 0:
			// An unproven location precedes the apparent winner (or nothing
			// proved runnable at all) — the session's resolution is not
			// established. Fail closed: report it unverified, never an
			// assertion or an unqualified PASS.
			display := results[firstUncertain].display
			if display == "" {
				display = candidates[firstUncertain].Path
			}
			detail, remediation = unverifiableLookupDetail(check.Command, grp.token, grp.path, display, launchDisplay, grp.wrapper, r),
				unverifiableInBaseRemediation()
		case launchIdx >= 0 && launchIdx < selIdx:
			// An earlier location shadows the selection: the session
			// resolves a different file regardless of what the selection's
			// own verdict is.
			detail, remediation = shadowedAtLaunchDetail(check.Command, grp.token, grp.path, launchDisplay, grp.wrapper),
				shadowedAtLaunchRemediation()
		case launchIdx > selIdx:
			// The session skips the proven-missing selection for a rescue.
			detail, remediation = fallsBackDetail(check.Command, grp.token, grp.path, launchDisplay, grp.wrapper, sel.verdict, r),
				fallsBackRemediation(r)
		default: // launchIdx < 0 — the selected is proven non-runnable and nothing rescues
			switch sel.verdict {
			case baseTreeAbsent:
				detail, remediation = absentFromBaseDetail(check.Command, grp.token, grp.path, grp.wrapper, r),
					absentFromBaseRemediation(r)
			case baseTreeNotExecutable:
				detail, remediation = notExecutableInBaseDetail(check.Command, grp.token, grp.path, grp.wrapper, r),
					notExecutableInBaseRemediation(r)
			default:
				detail, remediation = notAFileInBaseDetail(check.Command, grp.token, grp.path, grp.wrapper, r),
					notAFileInBaseRemediation()
			}
		}
		return detail, remediation
	}
	return "", ""
}

// runnableFile reports whether path is a runnable file — the same bar
// resolveExecutable applies (exists, not a directory, an execute bit set).
func runnableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

// gitProbeTimeout bounds each read-only git invocation: a wedged object
// store or a lazy-fetch attempt must never hang af doctor.
const gitProbeTimeout = 10 * time.Second

// gitTreeRead runs a read-only git probe against workspace.
// GIT_NO_LAZY_FETCH makes object access fail instead of promisor-fetching in
// a partial clone — the no-network/no-mutation contract covers the object
// store too, and an unreadable promised object answers "unknown", never
// "absent". (On a full clone, or a git old enough to ignore the variable,
// nothing fetches: ls-tree reads only tree objects, which blob:none filters
// keep local.) Tests stub this to observe the commands each probe issues.
var gitTreeRead = func(workspace string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitProbeTimeout)
	defer cancel()
	return gitTreeCmd(ctx, workspace, args...).Output()
}

func gitTreeCmd(ctx context.Context, workspace string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workspace}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	return cmd
}

// sessionBaseRef returns the git ref whose tree a new session's worktree
// checks out, mirroring session/git/worktree_ops.go resolveOriginHead —
// origin/HEAD, then origin/main, then origin/master — with HEAD as the
// fallback setupNewWorktree uses when no origin ref resolves. af doctor MUST
// NOT fetch: session creation fetches first, but the question here is "would
// a session created NOW find this program", which the locally cached refs
// answer without network access, ref writes, or any repository mutation. ok
// is false when no ref resolves (unborn HEAD with no usable origin) — there
// is then no base tree to check against, so callers stay conservative.
func sessionBaseRef(workspace string) (ref string, ok bool) {
	for _, candidate := range []string{"origin/HEAD", "origin/main", "origin/master", "HEAD"} {
		if _, err := gitTreeRead(workspace, "rev-parse", "--verify", "--quiet", candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// baseTreeEntry is how a repo-relative path appears in the base ref's tree.
type baseTreeEntry int

const (
	baseTreeRunnable      baseTreeEntry = iota // runnable at launch — an executable blob or a symlink resolving to one
	baseTreeAbsent                             // not in the tree — the clean worktree will not contain it
	baseTreeNotExecutable                      // committed 100644 — materializes in the worktree but is not runnable
	baseTreeNotAFile                           // a tree (040000) or gitlink (160000) entry — present, but not a file
	baseTreeUnknown                            // the probe could not answer — inconclusive, reported as unverified
)

// baseTreeProbe reports how rel appears in ref's tree via `git ls-tree -z
// <ref> -- <rel>` — the issue's prescribed "is the path in the base commit's
// tree" answered against the tree object alone: presence is a missing entry,
// and the entry's mode says whether the worktree materializes something
// runnable — a file committed 100644 is found at launch but not runnable,
// and a directory or submodule entry is not a runnable file either. A
// symlink entry is answered by dereferencing its stored target — repo-
// relative targets recurse into the tree (bounded), absolute targets name
// the same host file at launch — since a dangling link stays dangling in
// the worktree and must not shadow the real resolution. ls-tree never
// dereferences the entry's blob, so unlike `git cat-file -e <ref>:<rel>` it
// cannot promisor-fetch a blob:none-filtered object; the one blob read —
// a symlink's tiny target blob — runs under GIT_NO_LAZY_FETCH and answers
// "unknown" rather than fetch. The probe reads the base ref's tree — never
// the current index or working tree.
func baseTreeProbe(workspace, ref, rel string) baseTreeEntry {
	return baseTreeEntryAt(workspace, ref, rel, 0)
}

// treeEntryMode returns the git mode of rel's entry in ref's tree via
// `git ls-tree -z <ref> -- <rel>` — ("", true, false) when absent,
// ("", _, false) on probe error.
func treeEntryMode(workspace, ref, rel string) (mode string, found bool, ok bool) {
	out, err := gitTreeRead(workspace, "ls-tree", "-z", ref, "--", rel)
	if err != nil {
		return "", false, false
	}
	for _, entry := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		meta, path, isEntry := strings.Cut(entry, "\t")
		if !isEntry || path != rel {
			continue
		}
		mode, _, _ = strings.Cut(meta, " ")
		return mode, true, true
	}
	return "", false, true
}

// readTreeLink returns a committed symlink's stored target — the blob's tiny
// content — under GIT_NO_LAZY_FETCH, so a blob:none-filtered object answers
// "unreadable" rather than promisor-fetching it.
func readTreeLink(workspace, ref, rel string) (string, bool) {
	out, err := gitTreeRead(workspace, "cat-file", "-p", ref+":"+rel)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// treeLinkDepth bounds chained symlink resolution through the tree — each hop
// is a bounded read; the cap keeps pathological chains finite.
const treeLinkDepth = 8

// linkTargetUnresolvable reports whether a committed symlink target contains
// a ".." component. POSIX resolves such a target component-wise — an
// intermediate ".." only works when the preceding real directory exists —
// which Clean would collapse wrongly (e.g. "missing/../realbin" cleans to
// "realbin" even when "missing" is absent and the link dangles). af does not
// model that traversal, so the target is unverifiable, never runnable.
func linkTargetUnresolvable(target string) bool {
	for _, part := range strings.Split(target, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func baseTreeEntryAt(workspace, ref, rel string, depth int) baseTreeEntry {
	if depth >= treeLinkDepth {
		return baseTreeUnknown
	}
	mode, found, ok := treeEntryMode(workspace, ref, rel)
	if !ok {
		return baseTreeUnknown
	}
	if !found {
		// Not a literal tree path — an intermediate component may still be a
		// committed symlink redirecting the lookup elsewhere in the tree,
		// exactly as the worktree's filesystem will resolve it.
		resolved, verdict := resolveTreeRel(workspace, ref, rel, depth+1)
		if resolved == "" {
			return verdict
		}
		return baseTreeEntryAt(workspace, ref, resolved, depth+1)
	}
	switch mode {
	case "100755": // executable blob — runnable
		return baseTreeRunnable
	case "120000":
		return baseTreeLinkAt(workspace, ref, rel, depth+1)
	case "100644":
		return baseTreeNotExecutable
	default:
		return baseTreeNotAFile
	}
}

// baseTreeLinkAt answers whether the symlink materialized at rel resolves to
// a runnable file in the session's worktree: an absolute target names the
// same host file at launch; a repo-relative target is answered by the tree
// entry IT lands on — followed through any further links or symlinked
// directories. An escaping target, an unreadable blob, or an over-deep chain
// is unverifiable — never "runnable" by default: a dangling committed link
// stays dangling in the worktree and must not shadow the real resolution.
func baseTreeLinkAt(workspace, ref, rel string, depth int) baseTreeEntry {
	if depth >= treeLinkDepth {
		return baseTreeUnknown
	}
	target, ok := readTreeLink(workspace, ref, rel)
	if !ok || target == "" {
		return baseTreeUnknown
	}
	if filepath.IsAbs(target) {
		if runnableFile(target) {
			return baseTreeRunnable
		}
		return baseTreeAbsent
	}
	if linkTargetUnresolvable(target) {
		return baseTreeUnknown // ".." traversal is unverifiable inside the tree
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(rel), target))
	if resolved == ".." || filepath.IsAbs(resolved) ||
		strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return baseTreeUnknown // escapes the repo — the launch target is unverifiable
	}
	return baseTreeEntryAt(workspace, ref, resolved, depth)
}

// resolveTreeRel follows rel's intermediate components through ref's tree the
// way the worktree's filesystem resolves them: each component may itself be a
// symlink (`bin -> realbin`) redirecting the rest of the path — possibly
// through further links. Returns (resolved, _) when a link redirects the
// lookup to another in-tree path for the caller to classify, or ("", verdict)
// when resolution settles: Absent (a component is missing, a non-directory
// entry stands in the way, or every component is a real directory and the
// leaf simply is not there), Runnable/Absent for an absolute link's host
// path, or Unknown for anything unverifiable.
func resolveTreeRel(workspace, ref, rel string, depth int) (string, baseTreeEntry) {
	if depth >= treeLinkDepth {
		return "", baseTreeUnknown
	}
	parts := strings.Split(rel, "/")
	for i := 0; i < len(parts)-1; i++ {
		prefix := strings.Join(parts[:i+1], "/")
		mode, found, ok := treeEntryMode(workspace, ref, prefix)
		if !ok {
			return "", baseTreeUnknown
		}
		if !found {
			return "", baseTreeAbsent
		}
		switch mode {
		case "040000": // a real directory — descend
			continue
		case "120000": // a directory symlink — follow it as the worktree would
			target, ok := readTreeLink(workspace, ref, prefix)
			if !ok || target == "" {
				return "", baseTreeUnknown
			}
			if filepath.IsAbs(target) {
				joined := filepath.Join(append([]string{target}, parts[i+1:]...)...)
				if runnableFile(joined) {
					return "", baseTreeRunnable
				}
				return "", baseTreeAbsent
			}
			if linkTargetUnresolvable(target) {
				return "", baseTreeUnknown
			}
			resolved := filepath.Clean(filepath.Join(
				append([]string{filepath.Dir(prefix), target}, parts[i+1:]...)...))
			if resolved == ".." || filepath.IsAbs(resolved) ||
				strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return "", baseTreeUnknown // escapes the repo — unverifiable at launch
			}
			// The redirect lands on another in-tree path — hand it back so
			// the caller classifies its leaf (and walks ITS intermediates).
			return resolved, baseTreeAbsent
		default: // a blob or gitlink has no children — nothing lives under it
			return "", baseTreeAbsent
		}
	}
	return "", baseTreeAbsent // all intermediates are real directories — the leaf is simply absent
}

// baseRefLabel names the tree the worktree materializes — origin's default
// branch when the repo has a usable origin ref, else the HEAD fallback
// setupNewWorktree uses.
func baseRefLabel(ref string) string {
	if strings.HasPrefix(ref, "origin/") {
		return "origin's default branch (" + ref + ")"
	}
	return "HEAD (the repo has no usable origin ref, so sessions branch from HEAD)"
}

// baseAbsenceSubject phrases which resolved executable the warning is about:
// the command's own program, or the relative wrapper it launches through.
func baseAbsenceSubject(command, token, resolvedPath string, wrapper bool) string {
	if wrapper {
		return fmt.Sprintf("%q launches the wrapper %q, which resolves to %s in this checkout",
			command, token, resolvedPath)
	}
	return fmt.Sprintf("%q resolves to %s in this checkout", command, resolvedPath)
}

// absentFromBaseDetail is the advisory warning for a resolved executable
// absent from the base tree: session worktrees are clean checkouts of that
// tree, so the override is absent at launch unless it is provisioned into
// each session (via setup_commands / the setup overlay, which applies after
// `git worktree add`). It names the two supported remedies rather than
// claiming the command is broken — the file does run in this checkout, it
// just will not be in the worktree the session launches in unless one of
// those remedies provides it.
func absentFromBaseDetail(command, token, resolvedPath string, wrapper bool, ref string) string {
	return fmt.Sprintf("%s, but that path is not in %s — "+
		"session worktrees are clean checkouts of that tree and will not contain it "+
		"unless setup_commands provisions it there, so the override will be absent at launch "+
		"without that provisioning", baseAbsenceSubject(command, token, resolvedPath, wrapper), baseRefLabel(ref))
}

func absentFromBaseRemediation(ref string) string {
	if strings.HasPrefix(ref, "origin/") {
		return "commit the file and push it to origin's default branch so it is part of the tree the session checks out, " +
			"or provision it in each session via setup_commands or the setup overlay"
	}
	return "commit the file so it is part of the tree the session checks out, " +
		"or provision it in each session via setup_commands or the setup overlay"
}

// notExecutableInBaseDetail covers the file committed WITHOUT the executable
// bit: the worktree materializes it, but non-executable — the session finds
// the path and still cannot run it.
func notExecutableInBaseDetail(command, token, resolvedPath string, wrapper bool, ref string) string {
	return fmt.Sprintf("%s, but that file is committed without the executable bit in %s — "+
		"a clean checkout materializes it non-executable, so the override will not run at launch",
		baseAbsenceSubject(command, token, resolvedPath, wrapper), baseRefLabel(ref))
}

func notExecutableInBaseRemediation(ref string) string {
	remediation := "mark the file executable in git (chmod +x and commit, or git update-index --chmod=+x)"
	if strings.HasPrefix(ref, "origin/") {
		remediation += " and push it to origin's default branch"
	}
	return remediation + ", or provision it in each session via setup_commands or the setup overlay"
}

// notAFileInBaseDetail covers a path that exists in the base tree as a
// directory or submodule — present in the worktree, but never a runnable
// program.
func notAFileInBaseDetail(command, token, resolvedPath string, wrapper bool, ref string) string {
	return fmt.Sprintf("%s, but that path is not a regular file in %s — "+
		"session worktrees are clean checkouts of that tree and will not contain a runnable program there, "+
		"so the override will not run at launch",
		baseAbsenceSubject(command, token, resolvedPath, wrapper), baseRefLabel(ref))
}

func notAFileInBaseRemediation() string {
	return "point the override at a runnable file that is part of the tree the session checks out, " +
		"or provision the program in each session via setup_commands or the setup overlay"
}

// baseCausePhrase renders why the selected file does not run at launch, for
// the fallback detail: "is not in <ref>", "is committed without the
// executable bit in <ref>", or "is not a regular file in <ref>".
func baseCausePhrase(verdict baseTreeEntry, ref string) string {
	switch verdict {
	case baseTreeAbsent:
		return fmt.Sprintf("is not in %s", baseRefLabel(ref))
	case baseTreeNotExecutable:
		return fmt.Sprintf("is committed without the executable bit in %s", baseRefLabel(ref))
	default:
		return fmt.Sprintf("is not a regular file in %s", baseRefLabel(ref))
	}
}

// fallsBackDetail covers a bare-name lookup whose checkout-selected file does
// not run at launch BUT whose later PATH entries rescue the name: the session
// still runs a program — just a different file than the checkout picks. The
// warning is honest about both halves: the override's file is absent, and
// the session falls back to the displayed candidate (a repo-relative path
// for a worktree-bound rescue, an absolute path for an installed one).
func fallsBackDetail(command, token, resolvedPath, fallback string, wrapper bool, verdict baseTreeEntry, ref string) string {
	return fmt.Sprintf("%s, but that %s — "+
		"the session's lookup continues past it and falls back to %s, so the session runs that file "+
		"instead of the one this checkout selects",
		baseAbsenceSubject(command, token, resolvedPath, wrapper), baseCausePhrase(verdict, ref), fallback)
}

func fallsBackRemediation(ref string) string {
	if strings.HasPrefix(ref, "origin/") {
		return "commit the file and push it to origin's default branch so the session runs it instead of the fallback, " +
			"or provision it in each session via setup_commands or the setup overlay"
	}
	return "commit the file so the session runs it instead of the fallback, " +
		"or provision it in each session via setup_commands or the setup overlay"
}

// shadowedAtLaunchDetail covers an earlier PATH location that resolves at
// launch even though this checkout skips it — typically a committed file
// that is deleted or non-executable locally: the worktree still contains it,
// the session's lookup finds it first, and the file this checkout selected
// never runs.
func shadowedAtLaunchDetail(command, token, resolvedPath, launchDisplay string, wrapper bool) string {
	return fmt.Sprintf("%s, but the session's lookup resolves %s first — "+
		"the session runs that file, not the one this checkout selects",
		baseAbsenceSubject(command, token, resolvedPath, wrapper), launchDisplay)
}

func shadowedAtLaunchRemediation() string {
	return "ensure the intended file wins the lookup in the session's worktree — commit and push it, " +
		"or provision it via setup_commands — or accept that sessions resolve the checked-in file"
}

// unprovenLaunchDirDetail reports a launch-dir-relative resolution whose
// launch directory the model cannot prove — an opaque prefix may change it.
// Probing the modeled dir's tree would answer the wrong question, so the
// session's resolution is unverified outright.
func unprovenLaunchDirDetail(command, token, resolvedPath string, wrapper bool, ref string) string {
	return fmt.Sprintf("%s, but af cannot prove the directory the session launches in — "+
		"an opaque prefix may change it — so what the session resolves against %s is unverified",
		baseAbsenceSubject(command, token, resolvedPath, wrapper), baseRefLabel(ref))
}

func unprovenLaunchDirRemediation() string {
	return "pin the command down — an absolute path is launch-dir-independent — " +
		"or check by hand where the session actually lands"
}

// unverifiableLookupDetail reports a PATH location whose launch answer the
// base-tree probe could not establish — the ref resolves but reading its
// tree failed (a promised object that will not fetch under
// GIT_NO_LAZY_FETCH, a damaged object store), or the location resolves
// outside the repo where no tree question applies. Because it precedes the
// apparent winner, the session's resolution is not established: the check
// fails closed and reports the lookup unverified rather than asserting a
// conclusion or passing unqualified. A known later winner is hedged as a
// fall-through, not a fact.
func unverifiableLookupDetail(command, token, resolvedPath, uncertain, winner string, wrapper bool, ref string) string {
	detail := fmt.Sprintf("%s, but af could not verify what the session resolves — "+
		"the launch answer for %s could not be established against %s",
		baseAbsenceSubject(command, token, resolvedPath, wrapper), uncertain, baseRefLabel(ref))
	if winner != "" {
		detail += fmt.Sprintf("; if nothing earlier resolves, the lookup reaches %s", winner)
	}
	return detail
}

func unverifiableInBaseRemediation() string {
	return "check by hand that the file resolves in a clean checkout of the " +
		"base branch — `git worktree add` is the same tree a session gets — " +
		"or commit and push it so the session does not depend on the probe's answer"
}
