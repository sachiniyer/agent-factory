package agentaccount

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// EXCLUSIVITY, not presence. Two accounts must genuinely resolve to different
// credential directories and a session must receive exactly one — the assertion
// a passthrough implementation fails, since it would hand every session the
// daemon's single ambient value while each looked correct in isolation (#3051).
func TestSelected_TwoAccountsSeeDifferentDirectories(t *testing.T) {
	home := t.TempDir()
	dirA, err := Register(home, "codex", "work")
	require.NoError(t, err)
	dirB, err := Register(home, "codex", "personal")
	require.NoError(t, err)
	require.NotEqual(t, dirA, dirB)

	ambient := []string{
		"CODEX_HOME=/home/op/.codex",
		"OPENAI_API_KEY=sk-ambient",
		"PATH=/usr/bin",
	}

	accountA, err := Selected(home, "codex", "work", "")
	require.NoError(t, err)
	scopedA, err := sessionenv.ApplyAccount(ambient, "codex", accountA)
	require.NoError(t, err)

	accountB, err := Selected(home, "codex", "personal", "")
	require.NoError(t, err)
	scopedB, err := sessionenv.ApplyAccount(ambient, "codex", accountB)
	require.NoError(t, err)

	homeA := envValue(t, scopedA, "CODEX_HOME")
	homeB := envValue(t, scopedB, "CODEX_HOME")
	require.NotEqual(t, homeA, homeB,
		"two concurrent sessions on different accounts must see different credential roots")
	require.Equal(t, dirA, homeA)
	require.Equal(t, dirB, homeB)

	// SUBTRACTION, the load-bearing half: an ambient key outranks the config
	// directory, so if it survived, the selection would be silently ignored while
	// every visible signal said it worked.
	for _, scoped := range [][]string{scopedA, scopedB} {
		require.NotContains(t, strings.Join(scoped, "\n"), "OPENAI_API_KEY=",
			"an ambient credential must not reach a session that selected an account")
	}

	// A session on one account cannot reach the other's directory: they are
	// separate paths, and only one is ever named.
	require.NotContains(t, strings.Join(scopedA, "\n"), dirB)
	require.NotContains(t, strings.Join(scopedB, "\n"), dirA)
}

// No account selected must leave the session exactly as it was before this
// feature existed — never a silent fallback onto some registered account.
func TestSelected_NoSelectionIsNotAnAccount(t *testing.T) {
	home := t.TempDir()
	_, err := Register(home, "codex", "work")
	require.NoError(t, err)

	account, err := Selected(home, "codex", "", "")
	require.NoError(t, err)
	require.Equal(t, sessionenv.Account{}, account,
		"an unselected session must carry no account, not the first registered one")
}

// An unregistered name REFUSES rather than being created on demand. An empty
// directory has no credentials in it, so launching against it would start an
// unauthenticated agent while the UI reported the selected account.
func TestSelected_RefusesAnUnregisteredAccount(t *testing.T) {
	home := t.TempDir()
	_, err := Selected(home, "codex", "never-registered", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not registered")
	require.Contains(t, err.Error(), "af accounts add", "the error must say how to fix it")
}

// Agents whose credential relocation was never verified refuse, rather than
// accepting a selection that would do nothing.
//
// gemini left this list in #3387 on measured evidence (GEMINI_CLI_HOME moves the
// keychain, verified under strace); the rest stay because their agent-specific
// variables move settings or config while the credentials follow a generic
// XDG/HOME path — amp and opencode were both measured doing exactly that.
func TestDir_RefusesAnUnverifiedAgent(t *testing.T) {
	for _, agent := range []string{"amp", "aider", "opencode", "devin", "unknown"} {
		_, err := Dir(t.TempDir(), agent, "work")
		require.ErrorIs(t, err, ErrUnsupportedAgent, "agent %q", agent)
	}
}

// A name must be one path component. "." and ".." resolve to the account root
// and the AF home, so a create there would scatter agent config over af's state.
func TestValidateName_RejectsTraversalAndSeparators(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "../escape", "/abs", "-leading", strings.Repeat("x", 65)} {
		require.Error(t, ValidateName(name), "name %q must be refused", name)
	}
	for _, name := range []string{"work", "personal-2", "team.eu", "a_b", "A1"} {
		require.NoError(t, ValidateName(name), "name %q is ordinary and must be accepted", name)
	}
}

// The credential directory is owner-only whatever the operator's umask. af does
// not read these bytes, but it chose where they sit.
func TestRegister_CreatesAnOwnerOnlyDirectory(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "claude", "work")
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	require.Equal(t, filepath.Join(home, DirName, "claude", "work"), dir)

	// Idempotent: registering again is not an error, because af only makes the
	// place and the agent's own login fills it.
	again, err := Register(home, "claude", "work")
	require.NoError(t, err)
	require.Equal(t, dir, again)
}

func TestList_EmptyOnAFreshHomeAndSortedAfterRegistration(t *testing.T) {
	home := t.TempDir()
	names, err := List(home, "codex")
	require.NoError(t, err, "no accounts registered is an ordinary state, not a failure")
	require.Empty(t, names)

	for _, name := range []string{"zulu", "alpha"} {
		_, err := Register(home, "codex", name)
		require.NoError(t, err)
	}
	names, err = List(home, "codex")
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "zulu"}, names)
}

// Same precedence as every other key, and an empty result means "none".
func TestResolve_PrefersExplicitThenProjectThenGlobal(t *testing.T) {
	require.Equal(t, "flag", Resolve("flag", "project", "global"))
	require.Equal(t, "project", Resolve("", "project", "global"))
	require.Equal(t, "global", Resolve("", "  ", "global"))
	require.Equal(t, "", Resolve("", "", ""))
}

func envValue(t *testing.T, env []string, name string) string {
	t.Helper()
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	t.Fatalf("%s not present in the scoped environment", name)
	return ""
}

// A case-only variant must be refused. On the default macOS/Windows filesystem
// `work` and `Work` are ONE directory, so registering both would give two named
// accounts a single credential store: the second login overwrites the first, and
// both selections then authenticate as the same person while the UI reports two
// accounts. That is the silent-wrong-identity failure #2983 exists to prevent,
// reached through the filesystem rather than through the environment.
//
// Asserted on every platform, because the rule is uniform on every platform: an
// account name travels in shared project config, so one that means two accounts
// on Linux and one on a colleague's Mac is broken wherever it is written.
func TestRegister_RefusesANameThatDiffersOnlyByCase(t *testing.T) {
	home := t.TempDir()
	if _, err := Register(home, "codex", "work"); err != nil {
		t.Fatalf("register work: %v", err)
	}

	_, err := Register(home, "codex", "Work")
	if err == nil {
		t.Fatal("registering a case-only variant must be refused: on a case-insensitive " +
			"filesystem both names address one directory and share one identity")
	}
	if !strings.Contains(err.Error(), "collides with existing account") {
		t.Fatalf("the refusal must name the collision, got: %v", err)
	}
	// And it must not have been created under either spelling.
	names, err := List(home, "codex")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 1 || names[0] != "work" {
		t.Fatalf("the refused registration must leave the registry untouched, got %v", names)
	}

	// The exact same name is still idempotent — the collision rule must not break
	// re-registration, which is the ordinary case.
	if _, err := Register(home, "codex", "work"); err != nil {
		t.Fatalf("re-registering the same name must stay idempotent: %v", err)
	}
}

// A symlinked account path must be refused rather than followed.
//
// Two distinct harms, both silent. MkdirAll succeeds on an existing
// symlink-to-directory without saying it followed one, so the Chmod behind it
// would re-permission an arbitrary target to 0700. And the readers disagree
// about whether such an account exists at all: List skips it (DirEntry.IsDir is
// false for a symlink) while a Stat-based Selected would accept it, giving an
// account that `af accounts list` does not show and a launch happily uses.
func TestRegister_RefusesASymlinkedAccountPath(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Chmod(elsewhere, 0o755); err != nil {
		t.Fatalf("chmod target: %v", err)
	}

	linkPath := filepath.Join(home, DirName, "codex", "linked")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.Symlink(elsewhere, linkPath); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	if _, err := Register(home, "codex", "linked"); err == nil {
		t.Fatal("a symlinked account path must be refused, not followed")
	}

	// The link target's permissions must be untouched — the whole point.
	info, err := os.Stat(elsewhere)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("the refused registration re-permissioned the symlink target to %#o; "+
			"chmod followed the link", perm)
	}

	// And Selected must agree with List that it is not an account, rather than
	// authenticating through a path the registry never created.
	if _, err := Selected(home, "codex", "linked", ""); err == nil {
		t.Fatal("Selected must refuse a symlinked account, matching List which cannot see it")
	}
}

// A symlink ANYWHERE below the AF home must be refused, not just the final
// component. Checking only the leaf left the boundary bypassable one level up:
// with `accounts/codex` a symlink, MkdirAll follows it and creates `work` inside
// the target as a perfectly ordinary directory, so a leaf-only Lstat sees
// nothing wrong and accepts the registration (#3057 review).
func TestRegister_RefusesASymlinkedAncestor(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()

	// accounts/codex is a symlink; the account name below it would be real.
	agentLink := filepath.Join(home, DirName, "codex")
	if err := os.MkdirAll(filepath.Dir(agentLink), 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	if err := os.Symlink(elsewhere, agentLink); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	if _, err := Register(home, "codex", "work"); err == nil {
		t.Fatal("a symlinked ANCESTOR must be refused: MkdirAll follows it and the " +
			"account lands in the link target while the leaf looks like a real directory")
	}

	// Nothing may have been created inside the link target — the refusal has to
	// happen before MkdirAll, not merely be reported after it.
	if _, err := os.Stat(filepath.Join(elsewhere, "work")); err == nil {
		t.Fatal("the refused registration created the account inside the symlink target")
	}
}

// The ancestor guard must run where the DECISION is made, not only where the
// account was created. Register's check cannot protect an account registered
// earlier whose `accounts/<agent>` component was replaced with a symlink since:
// the leaf stays a real directory, so a leaf-only check accepts it and the
// session authenticates through a path outside the registry (#3057 review).
func TestSelectedAndList_RefuseAnAncestorSwappedAfterRegistration(t *testing.T) {
	home := t.TempDir()
	if _, err := Register(home, "codex", "work"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := Selected(home, "codex", "work", ""); err != nil {
		t.Fatalf("precondition: a freshly registered account must select cleanly: %v", err)
	}

	// Swap accounts/codex for a symlink to a directory that DOES contain a
	// real "work" directory — so the leaf check alone would still pass.
	agentDir := filepath.Join(home, DirName, "codex")
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "work"), 0o700); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}
	if err := os.RemoveAll(agentDir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(elsewhere, agentDir); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	if _, err := Selected(home, "codex", "work", ""); err == nil {
		t.Fatal("Selected accepted an account whose ancestor is now a symlink: the " +
			"session would authenticate through a path outside the registry")
	}
	if _, err := List(home, "codex"); err == nil {
		t.Fatal("List enumerated through a symlinked ancestor, reporting an arbitrary " +
			"directory's contents as registered accounts")
	}
}

// The login guidance must be the agent's REAL invocation. Appending "login"
// universally printed `claude login`, which Claude Code does not have — it lives
// under `auth`. Verified against the installed CLIs on 2026-08-07.
func TestLoginCommand_IsPerAgentAndNotGuessed(t *testing.T) {
	claude, ok := LoginCommand("claude")
	require.True(t, ok)
	require.Equal(t, []string{"auth", "login"}, claude,
		"claude puts login under `auth`; `claude login` is not a command")

	codex, ok := LoginCommand("codex")
	require.True(t, ok)
	require.Equal(t, []string{"login"}, codex)

	// An unknown agent yields nothing rather than a guess.
	_, ok = LoginCommand("gemini")
	require.False(t, ok, "an agent with no verified login command must report none, not a guess")
}

// Concurrent registration of case-variant names must not produce two accounts.
//
// refuseCaseCollision is check-then-act, so two processes registering `work` and
// `Work` could both pass it before either created anything. On a
// case-insensitive filesystem that is the P1 outcome reached through a race: one
// directory, two names, a shared identity. The fix makes the LEAF's creation the
// serialization point — os.Mkdir is atomic and returns EEXIST for a case-variant
// there — so the loser re-checks against committed state (#3057 review).
//
// On a case-SENSITIVE filesystem the two names are genuinely distinct
// directories with distinct credentials, so there is no shared identity to
// prevent and both may legitimately succeed. This asserts the invariant that
// holds on BOTH: every accepted registration owns a directory no other accepted
// registration shares.
func TestRegister_ConcurrentCaseVariantsNeverShareADirectory(t *testing.T) {
	home := t.TempDir()

	const pairs = 64
	type result struct {
		dir string
		err error
	}
	results := make(chan result, pairs*2)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup

	for i := 0; i < pairs; i++ {
		for _, name := range []string{fmt.Sprintf("acct%d", i), fmt.Sprintf("ACCT%d", i)} {
			done.Add(1)
			go func(name string) {
				defer done.Done()
				start.Wait()
				dir, err := Register(home, "codex", name)
				results <- result{dir: dir, err: err}
			}(name)
		}
	}
	start.Done()
	done.Wait()
	close(results)

	// The invariant: no two ACCEPTED registrations resolved to the same directory.
	// A shared directory is precisely the shared-credential outcome.
	owners := map[string]int{}
	accepted := 0
	for r := range results {
		if r.err != nil {
			continue
		}
		accepted++
		owners[r.dir]++
	}
	if accepted == 0 {
		t.Fatal("every concurrent registration failed; the test proves nothing")
	}
	for dir, n := range owners {
		if n > 1 {
			t.Fatalf("%d accepted registrations share the credential directory %s: "+
				"those accounts authenticate as the same identity", n, dir)
		}
	}

	// And the registry must agree: one entry per real directory, never a
	// case-variant pair addressing one.
	names, err := List(home, "codex")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != accepted {
		t.Fatalf("registry holds %d accounts but %d registrations were accepted", len(names), accepted)
	}

	// No case-variant PAIR survives — asserted UNCONDITIONALLY now.
	//
	// This used to be gated on the filesystem folding case, because os.Mkdir only
	// serializes where it does, so on Linux both variants landed. That gate was
	// the test agreeing with a weaker implementation instead of holding it to the
	// rule refuseCaseCollision documents. The case-folded reservation collides on
	// every filesystem, so the rule is now genuinely uniform and the test says so
	// on every platform rather than only on macOS (#3057 review).
	seen := map[string]string{}
	for _, name := range names {
		key := strings.ToLower(name)
		if other, dup := seen[key]; dup {
			t.Fatalf("accepted case-variant accounts %q and %q for one agent: the rule is "+
				"documented as uniform across platforms and must hold under concurrency too", other, name)
		}
		seen[key] = name
	}
}

// The RESERVATION directory needs the same ancestor guard as the account
// directory. It was added after that guard existed and was not routed through
// it, so a symlinked `.names` had MkdirAll follow it and the reservation — the
// record of which spelling owns each account — land outside the AF home, while
// registration reported success (#3057 review).
func TestRegister_RefusesASymlinkedReservationDirectory(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()

	names := filepath.Join(home, DirName, "codex", reservationDir)
	if err := os.MkdirAll(filepath.Dir(names), 0o700); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.Symlink(elsewhere, names); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	if _, err := Register(home, "codex", "work"); err == nil {
		t.Fatal("a symlinked reservation directory must be refused: the reservation " +
			"would be written outside the AF home while registration reported success")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "work")); err == nil {
		t.Fatal("the refused registration wrote a reservation into the symlink target")
	}
}

// A reservation whose owner write fails must not poison the name.
//
// The O_EXCL create already happened, so a file left behind is permanent: a
// partial write reads as a different owner and reports a case collision forever,
// an empty one reports an ownerless reservation forever. Either way the account
// is unusable until someone finds and deletes a dot-directory entry they were
// never told about (#3057 review).
func TestReserveName_DoesNotPoisonTheNameWhenTheOwnerWriteFails(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, DirName, "codex", reservationDir)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	// Simulate the failed-write outcome exactly: the exclusive file exists and
	// carries no owner, which is the state a crashed or short write leaves.
	path := filepath.Join(dir, "work")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	_, err := Register(home, "codex", "work")
	require.Error(t, err, "an ownerless reservation must be reported, not silently adopted")

	// Now the rollback path: remove it as the fixed code does, and the name must
	// be reclaimable rather than permanently lost.
	require.NoError(t, os.Remove(path))
	dirPath, err := Register(home, "codex", "work")
	require.NoError(t, err, "once the poisoned reservation is gone the name must be usable again")
	require.DirExists(t, dirPath)

	owner, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "work", string(owner), "a successful registration must record its owner")
}
