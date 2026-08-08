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
	a, aEnv := accountMountAndEnv(sessionenv.Account{Agent: "codex", Name: "work", Dir: "/home/op/.agent-factory/accounts/codex/work"})
	b, bEnv := accountMountAndEnv(sessionenv.Account{Agent: "codex", Name: "personal", Dir: "/home/op/.agent-factory/accounts/codex/personal"})

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
	if !strings.HasSuffix(a[1], ":"+dockerAccountHome) || !strings.HasSuffix(b[1], ":"+dockerAccountHome) {
		t.Errorf("both accounts must mount at the fixed container path %q; got %q and %q", dockerAccountHome, a[1], b[1])
	}
	want := "CODEX_HOME=" + dockerAccountHome
	if len(aEnv) != 2 || aEnv[1] != want {
		t.Errorf("the agent must be pointed at the mounted account home: want %q, got %v", want, aEnv)
	}
	if aEnv[1] != bEnv[1] {
		t.Errorf("the container-side config var must not depend on which account it is; got %q and %q", aEnv[1], bEnv[1])
	}
}

// claude uses a different variable, and getting it wrong would silently leave the
// agent on the container's default home — present, wrong, and quiet.
func TestAccountMountAndEnv_UsesTheAgentsOwnConfigVar(t *testing.T) {
	_, env := accountMountAndEnv(sessionenv.Account{Agent: "claude", Name: "work", Dir: "/acct/claude/work"})
	if want := "CLAUDE_CONFIG_DIR=" + dockerAccountHome; len(env) != 2 || env[1] != want {
		t.Fatalf("want %q, got %v", want, env)
	}
	// An agent with no account support produces nothing rather than a mount the
	// agent would never read.
	if mount, env := accountMountAndEnv(sessionenv.Account{Agent: "aider", Name: "work", Dir: "/acct/aider/work"}); mount != nil || env != nil {
		t.Errorf("an agent without account support must produce no mount; got %v %v", mount, env)
	}
	// A zero account is unscoped, not an empty mount spec.
	if mount, env := accountMountAndEnv(sessionenv.Account{}); mount != nil || env != nil {
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
		if !kind.CarriesAccount() {
			t.Errorf("%s places the account through the shared sandboxWorkspace, so it must carry one", kind)
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
		if err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: kind}); err != nil {
			t.Errorf("%s places the account now, so it must not be refused: %v", kind, err)
		}
	}
	// Unscoped creates are untouched on every kind.
	for _, kind := range []BackendKind{BackendLocal, BackendDocker, BackendSSH, BackendHook} {
		if err := refuseOffBoxAccount(InstanceOptions{Backend: kind}); err != nil {
			t.Errorf("an unscoped create on %s must not be refused: %v", kind, err)
		}
	}
}
