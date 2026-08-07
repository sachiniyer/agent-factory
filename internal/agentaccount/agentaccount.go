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
			ErrUnsupportedAgent, agent, strings.Join(sessionenv.AccountAgents(), ", "))
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
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("create account directory %s: %w", dir, err)
	}
	// MkdirAll honours the umask, so a 077 umask would have produced 0700 anyway
	// and a 022 one would not. Set it explicitly rather than inherit whatever the
	// operator's shell had.
	if err := os.Chmod(dir, dirMode); err != nil {
		return "", fmt.Errorf("secure account directory %s: %w", dir, err)
	}
	return dir, nil
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
	entries, err := os.ReadDir(filepath.Join(home, DirName, agent))
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
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return sessionenv.Account{}, fmt.Errorf(
			"account %q is not registered for %s; run `af accounts add %s %s` and log in with that account first",
			name, agent, agent, name)
	}
	return sessionenv.Account{
		Agent: agent, Name: name, Dir: dir, TrustedWrapper: trustedWrapper,
	}, nil
}
