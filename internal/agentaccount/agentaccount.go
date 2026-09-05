// Package agentaccount owns where an agent account's credentials live and how a
// session's account is chosen. It never reads, writes, or transports the
// credential itself (#3051).
package agentaccount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/afhome"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// DirName is the account root under the AF home.
const DirName = "accounts"

// dirMode is owner-only. These directories hold agent credentials — af does not
// read them, but it does decide where they sit, and a world-readable credential
// store would be af's fault regardless of who wrote the bytes.
const dirMode = 0o700

// nameRule keeps an account name usable as a single path component. It is
// deliberately stricter than "not a path traversal": a name reaches a shell
// message, a config value and a directory, and one that needs quoting in any of
// those is a bug waiting to happen rather than a convenience.
var nameRule = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// loginCommands is the agent's OWN login invocation, verified against the
// installed CLIs rather than assumed from the binary name.
//
// They do not share a shape, which is the whole reason this is a table. Appending
// "login" universally printed `claude login`, which is not a command at all —
// Claude Code puts it under an `auth` subcommand. A registration flow whose
// printed next step does not run is worse than no guidance: the operator
// concludes the account is broken, when the only thing wrong was af's sentence.
//
// THE FLOW HAS TO BE BROWSER-FREE, which is what shapes codex's entry (#3854).
// af runs these in a tmux pane on the DAEMON's host, which is headless and
// usually remote: a browser-callback login there either opens a browser nobody
// is sitting in front of, or waits for an OAuth redirect to the daemon's own
// localhost that the operator's machine can never reach. The device-code shape —
// print a URL and a short code, poll while the human signs in from any device —
// is the one that fits. Where the agent expresses that as a FLAG it belongs
// here; where it expresses it as an environment variable it belongs in
// sessionenv.AccountLoginEnvironment, which is why gemini and claude add nothing
// to their words.
//
// Verified 2026-08-07, re-verified 2026-09-04 against the installed CLIs before
// af started RUNNING these rather than printing them (#3384), and again for
// #3854:
//   - claude 2.1.261: `claude auth --help` lists `login  Sign in to your
//     Anthropic account`, and `login` is absent from `claude --help`. Its
//     `login` takes --claudeai/--console/--email/--sso and NO browser-free flag,
//     so the lever is environmental — see AccountLoginEnvironment.
//   - codex-cli 0.153.2: `codex --help` lists `login  Manage login` at the top
//     level, and `codex login --help` lists `--device-auth`. (--with-api-key and
//     --with-access-token read a secret from stdin and are deliberately out of
//     scope: af never handles credential material.)
//   - gemini 0.51.0: there is NO login or auth subcommand — `gemini --help`
//     lists only mcp, extensions, skills, hooks, gemma and the default query
//     command. Its sign-in is the interactive picker the bare CLI raises on a
//     home with no credentials, which is why its entry is EMPTY rather than
//     absent: empty means "run the agent itself", absent means "af knows of no
//     flow", and the two must not collapse. `af accounts add gemini` has printed
//     exactly this invocation since #3609.
//
// An agent added to accountConfigVars without an entry here gets no printed
// command rather than a guessed one (#3057 review).
var loginCommands = map[string][]string{
	"claude": {"auth", "login"},
	"codex":  {"login", "--device-auth"},
	"gemini": {},
}

// LoginCommand returns the agent's login invocation words, and whether one is
// known. A false result means "say nothing", never "guess".
func LoginCommand(agent string) ([]string, bool) {
	words, ok := loginCommands[agent]
	return words, ok
}

// ErrUnsupportedAgent reports an agent whose credential relocation was never
// verified. It is a distinct error because the answer for the operator is
// "this agent cannot do accounts", not "you typed the name wrong".
var ErrUnsupportedAgent = errors.New("agent does not support multiple accounts")

// ValidateName rejects a name that cannot be one path component.
//
// Traversal is the obvious case and not the only one: "." and ".." resolve to
// the account ROOT and to the AF home, so a create there would scatter an
// agent's config over af's own state directory.
func ValidateName(name string) error {
	if !nameRule.MatchString(name) {
		return fmt.Errorf("account name %q must be 1-64 characters of letters, digits, dot, dash or underscore, starting with a letter or digit", name)
	}
	if name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("account name %q must be a single path component", name)
	}
	return nil
}

// Dir is where one account's credentials live. Deriving it in exactly one place
// is what keeps the writer (registration) and the reader (session launch) from
// disagreeing about the path — the class of bug where a feature works until two
// call sites drift.
func Dir(home, agent, name string) (string, error) {
	if _, ok := sessionenv.SupportsAccounts(agent); !ok {
		return "", fmt.Errorf("%w: %s (supported: %s)",
			ErrUnsupportedAgent, agent, sessionenv.AccountAgentsSummary())
	}
	if err := ValidateName(name); err != nil {
		return "", err
	}
	if strings.TrimSpace(home) == "" {
		return "", errors.New("cannot locate an agent account without an agent-factory home")
	}
	return filepath.Join(home, DirName, agent, name), nil
}

// Register creates an account's credential directory and returns it, so the
// operator can run the agent's own login flow against it.
//
// af does not perform the login and never sees the secret: that is the whole
// point of an account being a DIRECTORY. Registration is therefore idempotent —
// it makes a place, and the agent fills it.
func Register(home, agent, name string) (string, error) {
	dir, err := Dir(home, agent, name)
	if err != nil {
		return "", err
	}
	if err := refuseCaseCollision(home, agent, name); err != nil {
		return "", err
	}
	// BEFORE MkdirAll, which follows an ancestor symlink silently and would create
	// the account inside its target before any check below could object.
	if err := refuseSymlinkedAncestor(home, dir); err != nil {
		return "", err
	}
	// Parents with MkdirAll, the LEAF with a bare Mkdir — because the leaf's
	// creation is what serializes two concurrent registrations.
	//
	// refuseCaseCollision above is check-then-act: two processes registering
	// `work` and `Work` can both pass it before either creates anything, and both
	// then succeed. On a case-insensitive filesystem that is the P1 outcome
	// arriving through a race — one directory, two names, a shared identity.
	//
	// os.Mkdir is atomic and, on exactly the filesystems where the harm exists,
	// fails with EEXIST for a case-variant of an existing directory. So the
	// re-check below runs against COMMITTED state rather than a snapshot, and the
	// loser of the race is refused rather than silently joined to the winner's
	// credentials. MkdirAll cannot do this: it treats an existing directory as
	// success and reports nothing (#3057 review).
	//
	// The PARENT, though, is still a MkdirAll, and afhome.MkdirAll rather than
	// os.MkdirAll: this is <af-home>/accounts/<agent>, so
	// MkdirAll re-creates the whole home as an ancestor. Register is reachable
	// from the daemon's account routes (#3835), which is a daemon-owned write
	// that runs nowhere near the atomic-write seam (#3850). Not config's
	// re-export: this package deliberately depends on no heavy af package.
	if err := afhome.MkdirAll(filepath.Dir(dir), dirMode); err != nil {
		return "", fmt.Errorf("create account directory %s: %w", dir, err)
	}
	// The RESERVATION is the serialization point, not the account directory.
	//
	// os.Mkdir on the account directory only serializes where the filesystem folds
	// case, so on Linux both `work` and `Work` succeeded and the uniform rule this
	// package documents was not actually uniform. The reservation is named for the
	// CASE-FOLDED account, so the same O_EXCL create collides on every filesystem
	// (#3057 review).
	owner, err := reserveName(home, agent, name)
	if err != nil {
		return "", err
	}
	if owner != name {
		return "", fmt.Errorf(
			"account %q collides with existing account %q for %s: account names must differ by more than case, "+
				"because macOS and Windows filesystems treat them as one directory and both accounts would share "+
				"a single identity",
			name, owner, agent)
	}
	if err := os.Mkdir(dir, dirMode); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create account directory %s: %w", dir, err)
	}
	// And AFTER, because the pre-check can only inspect components that already
	// existed. This pass covers everything MkdirAll just created, plus anything
	// swapped in between the two.
	if err := refuseSymlinkedAncestor(home, dir); err != nil {
		return "", err
	}
	// Lstat AFTER MkdirAll, which succeeds on an existing symlink-to-directory
	// without reporting that it followed one. Chmod would then follow it too and
	// re-permission whatever it points at — an arbitrary path, chosen by whoever
	// made the link, silently set to 0700 by a registration command.
	//
	// Refusing also keeps the readers agreeing with each other. List skips a
	// symlink (DirEntry.IsDir is false for one) while Selected accepts it
	// (os.Stat follows), so a symlinked account is invisible to `af accounts
	// list` and usable by a launch — an account that does not appear to exist and
	// works anyway is the kind of state nobody debugs successfully (#3057 review).
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect account directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf(
			"account path %s is a symlink; accounts must be real directories so af never re-permissions or "+
				"authenticates through a path chosen elsewhere — remove the link, or point AGENT_FACTORY_HOME at the real location",
			dir)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("account path %s exists and is not a directory", dir)
	}
	// MkdirAll honours the umask, so a 077 umask would have produced 0700 anyway
	// and a 022 one would not. Set it explicitly rather than inherit whatever the
	// operator's shell had.
	if err := os.Chmod(dir, dirMode); err != nil {
		return "", fmt.Errorf("secure account directory %s: %w", dir, err)
	}
	return dir, nil
}

// refuseCaseCollision rejects a name that differs from an existing account only
// by case.
//
// macOS and Windows default to case-INSENSITIVE filesystems, where `work` and
// `Work` are one directory. The registry would treat them as two accounts, so a
// second `codex login` would overwrite the first account's credentials and both
// selections would then authenticate as the same person — while the UI reported
// two distinct accounts. That is the exact silent-wrong-identity outcome this
// feature exists to prevent, arrived at from the filesystem instead of from the
// environment (#3057 review).
//
// It refuses on EVERY platform rather than probing the filesystem's behaviour.
// An account name is portable configuration — a project config can name one, and
// that config is shared across machines — so a name that means two accounts on
// Linux and one on a colleague's Mac is a footgun wherever it is written. The
// uniform rule costs a user one rename and behaves identically everywhere.
//
// Refusing rather than lowercasing is deliberate too: silently folding `Work`
// into `work` would hand back an EXISTING account's directory from a command the
// operator issued to create a new one.
// refuseSymlinkedAncestor rejects a symlink anywhere between the AF home and the
// account directory, exclusive of the home itself.
//
// Checking only the final component was not enough, and the gap is the whole
// point of the guard: if `accounts/codex` is a symlink, MkdirAll follows it and
// creates `work` inside the target, where `work` is a perfectly ordinary
// directory. A final-component Lstat sees nothing wrong and the registration is
// accepted, so the boundary is bypassed one level up — af then chmods and
// authenticates through a location chosen by whoever made the link, and both
// List and Selected report it as a valid account (#3057 review).
//
// The AF home itself is deliberately NOT checked. An operator symlinking their
// own ~/.agent-factory somewhere is an ordinary, deliberate arrangement, and
// af's other state trees already live behind it; refusing that would reject a
// working install for a property this package has no business policing. What is
// policed is everything af creates BELOW it.
//
// A component that does not exist yet is fine — it is about to be created as a
// real directory. Only what is actually there is inspected.
// reservationDir holds one file per case-folded account name, recording the
// spelling that owns it. It is a dot-directory so ValidateName rejects it and
// List therefore skips it without needing a special case.
const reservationDir = ".names"

// reserveName claims an account name's CASE-FOLDED form and returns the spelling
// that owns it — this call's own name when the claim succeeded, or the existing
// owner's when it did not.
//
// This is what makes the rule uniform. Creating the account directory only
// collides where the filesystem folds case; creating a file named for the folded
// form collides everywhere, so `work` and `Work` contend for one O_EXCL create
// on Linux exactly as they do on macOS.
//
// The reservation is never removed. It is the record of which spelling owns the
// name, and deleting it on unregister would let a differently-cased account take
// the same directory later — the collision the reservation exists to prevent,
// merely deferred.
func reserveName(home, agent, name string) (string, error) {
	dir := filepath.Join(home, DirName, agent, reservationDir)
	// Through the home guard for the same reason Register's own create is: this
	// is <af-home>/accounts/<agent>/.names (#3850).
	if err := afhome.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("create account reservation directory %s: %w", dir, err)
	}
	// The reservation path gets the SAME ancestor check as the account directory.
	// It was added after that guard existed and was not routed through it, so a
	// symlinked `.names` had MkdirAll follow it and the reservation land outside
	// the AF home — the identity record for every account of this agent, written
	// somewhere chosen by whoever made the link, while registration reported
	// success. A guard is only worth what it covers (#3057 review).
	if err := refuseSymlinkedAncestor(home, dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, strings.ToLower(name))

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		if _, err := file.WriteString(name); err != nil {
			// Remove the file THIS call created. Left behind, it poisons the name
			// permanently: a partial write reads as a different owner and reports a
			// case collision forever, and an empty one reports an ownerless
			// reservation forever. Either way the account is unusable until someone
			// finds and deletes a dot-directory entry they were never told about —
			// a failed registration must not cost the name (#3057 review).
			_ = file.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("record account reservation %s: %w", path, err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("record account reservation %s: %w", path, err)
		}
		return name, nil
	}
	if !os.IsExist(err) {
		return "", fmt.Errorf("reserve account name %s: %w", path, err)
	}

	// Someone else holds it. Read the owning spelling — retrying briefly, because
	// the winner creates the file before writing to it, so a loser arriving inside
	// that window sees an empty file rather than the owner's name. Treating empty
	// as "no owner" would let both registrations through, which is the race this
	// whole mechanism exists to close.
	for attempt := 0; attempt < 50; attempt++ {
		recorded, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read account reservation %s: %w", path, err)
		}
		if owner := strings.TrimSpace(string(recorded)); owner != "" {
			return owner, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return "", fmt.Errorf(
		"account reservation %s was created but never recorded an owner; remove it if no account of that name exists",
		path)
}

func refuseSymlinkedAncestor(home, dir string) error {
	relative, err := filepath.Rel(home, dir)
	if err != nil {
		return fmt.Errorf("locate account directory %s under %s: %w", dir, home, err)
	}
	current := home
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			// Not created yet; MkdirAll will make a real directory here.
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect account path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf(
				"account path component %s is a symlink; accounts must live in real directories under the "+
					"agent-factory home so af never re-permissions or authenticates through a path chosen elsewhere",
				current)
		}
	}
	return nil
}

func refuseCaseCollision(home, agent, name string) error {
	existing, err := List(home, agent)
	if err != nil {
		return err
	}
	for _, other := range existing {
		if other != name && strings.EqualFold(other, name) {
			return fmt.Errorf(
				"account %q collides with existing account %q for %s: account names must differ by more than case, "+
					"because macOS and Windows filesystems treat them as one directory and both accounts would share "+
					"a single identity",
				name, other, agent)
		}
	}
	return nil
}

// List returns the registered account names for an agent, sorted.
//
// A missing directory is an empty list, not an error: "no accounts registered"
// is an ordinary state, and reporting it as a failure would make `af accounts
// list` fail on a fresh install.
func List(home, agent string) ([]string, error) {
	if _, ok := sessionenv.SupportsAccounts(agent); !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedAgent, agent)
	}
	root := filepath.Join(home, DirName, agent)
	// Same reason as Selected: ReadDir follows a symlinked ancestor, so a swapped
	// component would have `af accounts list` enumerate an arbitrary directory as
	// though those were registered accounts.
	if err := refuseSymlinkedAncestor(home, root); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && ValidateName(entry.Name()) == nil {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Resolve picks the account for a session: an explicit --account, else the
// project default, else the global one. Same precedence as every other key, so
// there is nothing new for an operator to learn.
//
// An empty result means "no account selected", which must leave the session on
// the ambient identity exactly as before this feature existed — never a silent
// fallback to some arbitrary registered account.
func Resolve(explicit, project, global string) string {
	for _, candidate := range []string{explicit, project, global} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// Selected builds the scope a launch applies, or the zero value when no account
// was selected.
//
// It REFUSES a name that was never registered rather than creating the directory
// on demand. An unregistered account has no credentials in it, so launching
// against it would start an unauthenticated agent while the UI reported the
// selected account — the silent-wrong-identity outcome this whole feature exists
// to prevent.
func Selected(home, agent, name, trustedWrapper string) (sessionenv.Account, error) {
	if strings.TrimSpace(name) == "" {
		return sessionenv.Account{}, nil
	}
	dir, err := Dir(home, agent, name)
	if err != nil {
		return sessionenv.Account{}, err
	}
	// Ancestors, not just the leaf — and on every SELECTION, not only at
	// registration. Register's check cannot protect an account registered last
	// week whose `accounts/<agent>` component was replaced with a symlink since:
	// the leaf is still a real directory, so a leaf-only check accepts it and the
	// session authenticates through a path outside the registry. The guard has to
	// run where the decision is made (#3057 review).
	if err := refuseSymlinkedAncestor(home, dir); err != nil {
		return sessionenv.Account{}, err
	}
	// Lstat, matching Register and List. os.Stat follows a symlink, so a launch
	// would authenticate through a path the registry never created and that `af
	// accounts list` does not show. All three readers must answer the same
	// question or an account exists for some of them and not others.
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return sessionenv.Account{}, fmt.Errorf(
			"account %q is not registered for %s; run `af accounts add %s %s` and log in with that account first",
			name, agent, agent, name)
	}
	return sessionenv.Account{
		Agent: agent, Name: name, Dir: dir, TrustedWrapper: trustedWrapper,
	}, nil
}
