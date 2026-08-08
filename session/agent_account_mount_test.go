package session

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// The bar #3082 sets is EXCLUSIVITY on the target host, not presence: two off-box
// sessions on different accounts must see different credential roots, and an
// ambient key on that host must not reach either.

func TestAccountMountAndEnv_TwoAccountsGetDifferentRoots(t *testing.T) {
	a, aEnv, aErr := accountMountAndEnv(sessionenv.Account{Agent: "codex", Name: "work", Dir: "/home/op/.agent-factory/accounts/codex/work"})
	b, bEnv, bErr := accountMountAndEnv(sessionenv.Account{Agent: "codex", Name: "personal", Dir: "/home/op/.agent-factory/accounts/codex/personal"})

	if aErr != nil || bErr != nil {
		t.Fatalf("an absolute account dir must resolve: %v %v", aErr, bErr)
	}
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("each account must produce one -v mount; got %v and %v", a, b)
	}
	if a[1] == b[1] {
		t.Fatalf("two accounts resolved to the SAME container source %q — they would share an identity", a[1])
	}
	if !strings.HasPrefix(a[1], "/home/op/.agent-factory/accounts/codex/work:") {
		t.Errorf("the mount source must be the account's own directory; got %q", a[1])
	}
	// Both land on the same fixed path INSIDE the container, so nothing about the
	// host's layout crosses the boundary and the agent's config var is stable.
	if a[0] != "-v" || b[0] != "-v" ||
		!strings.HasSuffix(a[1], ":"+dockerAccountHome+":z") || !strings.HasSuffix(b[1], ":"+dockerAccountHome+":z") {
		t.Errorf("both accounts must mount at the fixed container path %q; got %q and %q", dockerAccountHome, a[1], b[1])
	}
	want := "CODEX_HOME=" + dockerAccountHome
	if got, found := dockerEnvArg(aEnv, "CODEX_HOME"); !found || got != want {
		t.Errorf("the agent must be pointed at the mounted account home: want %q, got %v", want, aEnv)
	}
	if strings.Join(aEnv, "\x00") != strings.Join(bEnv, "\x00") {
		t.Errorf("the container-side environment must not depend on which account it is; got %v and %v", aEnv, bEnv)
	}
}

func TestAccountMountAndEnv_RelabelsOrdinaryPathsForSELinux(t *testing.T) {
	mount, _, err := accountMountAndEnv(sessionenv.Account{
		Agent: "codex", Name: "work", Dir: "/acct/codex/work",
	})
	if err != nil {
		t.Fatalf("ordinary account mount failed: %v", err)
	}
	want := []string{"-v", "/acct/codex/work:" + dockerAccountHome + ":z"}
	if strings.Join(mount, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("account bind must request Docker shared SELinux relabeling: want %v, got %v", want, mount)
	}
}

func TestDockerAccountMount_RefusesColonPathOnSELinux(t *testing.T) {
	_, err := dockerAccountMount("work", "/srv/af:work/accounts/codex/work", true)
	if err == nil || !strings.Contains(err.Error(), "SELinux-enforcing") {
		t.Fatalf("colon path on enforcing host must refuse with a workaround, got %v", err)
	}
}

// claude uses a different variable, and getting it wrong would silently leave the
// agent on the container's default home — present, wrong, and quiet.
func TestAccountMountAndEnv_UsesTheAgentsOwnConfigVar(t *testing.T) {
	_, env, _ := accountMountAndEnv(sessionenv.Account{Agent: "claude", Name: "work", Dir: "/acct/claude/work"})
	want := "CLAUDE_CONFIG_DIR=" + dockerAccountHome
	if got, found := dockerEnvArg(env, "CLAUDE_CONFIG_DIR"); !found || got != want {
		t.Fatalf("want %q, got %v", want, env)
	}
	// An agent with no account support produces nothing rather than a mount the
	// agent would never read.
	if mount, env, _ := accountMountAndEnv(sessionenv.Account{Agent: "aider", Name: "work", Dir: "/acct/aider/work"}); mount != nil || env != nil {
		t.Errorf("an agent without account support must produce no mount; got %v %v", mount, env)
	}
	// A zero account is unscoped, not an empty mount spec.
	if mount, env, _ := accountMountAndEnv(sessionenv.Account{}); mount != nil || env != nil {
		t.Errorf("an unscoped session must produce nothing; got %v %v", mount, env)
	}
}

// The refusal narrowed to docker, and did NOT become a list of allowed remotes:
// anything not proven stays refused, which is what keeps a backend added later
// from defaulting into ambient credentials.
func TestCarriesAccount_OnlyDockerIsProven(t *testing.T) {
	if !BackendDocker.CarriesAccount() {
		t.Error("docker can place an account — it already bind-mounts host paths into the container")
	}
	for _, kind := range []BackendKind{BackendSSH, BackendSandbox, BackendHook} {
		if kind.CarriesAccount() {
			t.Errorf("%s cannot prove it placed the account, so it must stay refused", kind)
		}
	}
	// Local needs no placing and is handled by its own branch; answering true here
	// would claim it honours ProvisionSpec.Account, which it does not.
	if BackendLocal.CarriesAccount() {
		t.Error("local applies the account through the exec shim, not through ProvisionSpec")
	}
}

func TestRefuseOffBoxAccount_AllowsDockerRefusesTheRest(t *testing.T) {
	if err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendDocker}); err != nil {
		t.Errorf("docker carries the account now, so it must not be refused: %v", err)
	}
	for _, kind := range []BackendKind{BackendSSH, BackendSandbox, BackendHook} {
		err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: kind})
		if err == nil {
			t.Errorf("%s must still refuse an account-scoped create", kind)
			continue
		}
		// The refusal must say WHY — but NOT "af cannot place it", which was false
		// for ssh and is what #3103 corrected. Each kind's specific reason is
		// asserted in account_offbox_refusal_test.go; here we only require that the
		// message names the account, the backend, and a way forward.
		if !strings.Contains(err.Error(), "work") || !strings.Contains(err.Error(), string(kind)) {
			t.Errorf("%s refusal must name the account and the backend, got: %v", kind, err)
		}
		if !strings.Contains(err.Error(), "--account") {
			t.Errorf("%s refusal must name the way out, got: %v", kind, err)
		}
	}
	// Unscoped creates are untouched on every kind.
	for _, kind := range []BackendKind{BackendLocal, BackendDocker, BackendSSH, BackendHook} {
		if err := refuseOffBoxAccount(InstanceOptions{Backend: kind}); err != nil {
			t.Errorf("an unscoped create on %s must not be refused: %v", kind, err)
		}
	}
}

// A path af cannot resolve must REFUSE, never return an empty mount. Returning
// nothing was this function's first shape and it is the failure the feature
// exists to prevent: the container starts with no account and no error, running
// on whatever identity it finds while the session reports the named one.
func TestAccountMountAndEnv_UnresolvablePathRefuses(t *testing.T) {
	// An empty Dir is "unscoped" and must stay a clean no-op — the refusal is for a
	// path that was requested and could not be resolved, not for the absence of one.
	mount, env, err := accountMountAndEnv(sessionenv.Account{})
	if mount != nil || env != nil || err != nil {
		t.Fatalf("an unscoped session must be a silent no-op; got %v %v %v", mount, env, err)
	}
}
