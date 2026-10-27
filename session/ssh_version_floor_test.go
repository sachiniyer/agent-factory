package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The three regressions #3061 shipped, each pinned by the property that would have
// caught it (#3092).
//
// The connecting theme is that a composed command is a COMPATIBILITY SURFACE. Every
// option costs a minimum OpenSSH version, and every option that changes how a host
// is identified costs a class of host-key configuration. Neither cost is visible at
// the call site, which is why they are asserted here instead.

// EVERY option the command emits, so a new one cannot be added without a reviewer
// pricing its version cost. Deliberately an ALLOWLIST rather than a ban on the one
// option that broke: `KnownHostsCommand=none` was itself defensive, and the next
// too-new option will not be that one.
//
// The floor these keep is OpenSSH 7.6 (2017) — `StrictHostKeyChecking=accept-new`,
// and only under that posture. Ubuntu 20.04 ships 8.2 and Debian 11 ships 8.4, both
// comfortably inside; `KnownHostsCommand` (8.5, March 2021) put them outside.
func TestSSHCommandEmitsOnlyLongEstablishedOptions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Options af may emit, each with the OpenSSH release that introduced it.
	allowed := map[string]string{
		"User":                  "1.2",
		"StrictHostKeyChecking": "1.2 (accept-new: 7.6)",
		"UserKnownHostsFile":    "1.2",
		"GlobalKnownHostsFile":  "1.2",
		"BatchMode":             "1.2",
		"ExitOnForwardFailure":  "4.4",
		// #3086's pin. Priced by reading readconf.c at the earliest release-era tag
		// that has it (V_1_2_2_P1), where `proxycommand` sits in the same keyword
		// table as `user` and `batchmode` — the options already priced 1.2 above.
		// The `%%` escape the value relies on is in percent_expand at the 7.6 floor.
		"ProxyCommand": "1.2",
	}

	for _, posture := range []string{
		config.SSHHostKeyStrict, config.SSHHostKeyAcceptNew, config.SSHHostKeyInsecure, "unknown-future",
	} {
		// BOTH shapes. A pinned session emits an option an unpinned one does not, so
		// checking only the unpinned command would leave the allowlist blind at
		// exactly the place the newest option lives.
		for _, dialAddr := range []string{"", "198.51.100.8"} {
			cmd, err := sshCommandPinnedTo(config.SSHConfig{Host: "h.example.com", Port: 2222}, posture, dialAddr, 0)
			require.NoError(t, err)
			fields := strings.Fields(cmd)
			for i, f := range fields {
				if f != "-o" || i+1 >= len(fields) {
					continue
				}
				name, _, _ := strings.Cut(strings.Trim(fields[i+1], "'"), "=")
				_, ok := allowed[name]
				assert.True(t, ok,
					"posture %q (pin %q) emits -o %s=…, which is not in the priced allowlist. Every option costs a "+
						"MINIMUM OpenSSH VERSION: `KnownHostsCommand` (8.5) made backend=ssh unusable on Ubuntu 20.04 "+
						"and Debian 11 because an unrecognised -o aborts option parsing rather than warning (#3092). "+
						"Add it here with the release that introduced it, or drop it.", posture, dialAddr, name)
			}
		}
	}
}

// The option that actually broke it, named so the regression cannot come back
// under the allowlist's more general wording.
func TestSSHCommandOmitsThePost85HostKeyHelperOption(t *testing.T) {
	// NOT named after the option, and asserted as "-o <name>" rather than the bare
	// word: t.TempDir() embeds the TEST NAME in its path, and that path is
	// interpolated into UserKnownHostsFile — so a test called …KnownHostsCommand
	// matches its own directory and fails against correct code. Measured.
	t.Setenv("HOME", t.TempDir())
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyStrict)
	assert.NotContains(t, cmd, "-o KnownHostsCommand",
		"OpenSSH 8.5+, and redundant: -F none means no config file is read, so nothing can install "+
			"such a helper. It cost every <8.5 user the whole backend (#3092)")
}

// The connection is NAME-based. Dialling a literal forces a HostKeyAlias, and no
// alias value is correct: measured against a real sshd whose host key is certified
// for the principal `real.example`, `HostKeyAlias=[real.example]:2202` is REJECTED
// (the alias is the principal) while a PLAIN known_hosts entry on a non-default
// port requires exactly that bracketed form. One string cannot be both (#3092).
func TestSSHCommandDialsTheConfiguredNameNotAnAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, cfg := range []config.SSHConfig{
		{Host: "h.example.com"},
		{Host: "h.example.com", Port: 2222},
	} {
		cmd := sshCmdFor(t, cfg, config.SSHHostKeyStrict)
		fields := strings.Fields(cmd)
		assert.Equal(t, "'h.example.com'", fields[len(fields)-1],
			"ssh must resolve the NAME — that is what keeps host certificates valid and keeps the "+
				"dialer's try-each-address fallback (#3092)")
		assert.NotContains(t, cmd, "HostKeyAlias",
			"an alias is only needed when dialling an address, and no alias value satisfies both a "+
				"plain [host]:port entry and a certificate principal")
	}
}

// A typo in ssh.identity_file must REFUSE, not silently authenticate as somebody
// else. ssh treats an unusable `-i` as a warning and falls through to
// agent/default keys, and IdentitiesOnly is deliberately off here.
//
// Asserted against verifySSHIdentityFile, NOT the composer: composition has to
// stay filesystem-pure because restoreRuntimeCleanup calls it while persisted
// handles are loading, and a check there captures a permanently dead cleanup the
// moment a key is briefly unavailable — the defect this file already fixed once
// for the accept-new store.
func TestSSHIdentityFileMustBeReadable(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "typo_id_ed25519")

	err := verifySSHIdentityFile(config.SSHConfig{Host: "h.example.com", IdentityFile: missing})
	require.Error(t, err, "an unreadable identity file must be refused")
	assert.Contains(t, err.Error(), missing, "the operator needs to see WHICH path")
	assert.Contains(t, err.Error(), "backend=ssh")

	// os.Stat SUCCEEDS for all three of these, and ssh can load none of them as a
	// key — so an existence check would pass them straight through to the silent
	// agent fallback this exists to prevent.
	asDir := filepath.Join(dir, "a-directory")
	require.NoError(t, os.Mkdir(asDir, 0o700))
	assert.Error(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: asDir}),
		"a directory is not a key, and os.Stat is happy with it")

	if os.Geteuid() != 0 {
		unreadable := filepath.Join(dir, "mode000")
		require.NoError(t, os.WriteFile(unreadable, []byte("k"), 0o000))
		assert.Error(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: unreadable}),
			"an existing but unreadable file passes os.Stat and still cannot be loaded")
	}

	// A NAMED PIPE is the one that decides HOW the check is written, not just what
	// it returns. os.Stat is happy with it and ssh cannot load it — but opening it
	// to find that out BLOCKS until some process opens the write end, and nothing
	// ever will. So this case fails an open-first implementation by HANGING rather
	// than by returning the wrong answer, and a bounded subtest is what turns that
	// into a red instead of a suite that never finishes.
	fifo := filepath.Join(dir, "a-fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	done := make(chan error, 1)
	go func() { done <- verifySSHIdentityFile(config.SSHConfig{IdentityFile: fifo}) }()
	select {
	case err := <-done:
		assert.Error(t, err, "a named pipe is not a key, and os.Stat is happy with it")
	case <-time.After(10 * time.Second):
		t.Fatal("verifySSHIdentityFile BLOCKED on a named pipe: it must settle the file KIND with a " +
			"stat before opening, because opening a FIFO waits for a writer that never comes — a hung " +
			"provision is worse than the silent fallback this check exists to prevent")
	}

	// A readable regular file passes. Its CONTENT is deliberately not validated —
	// see verifySSHIdentityFile: every way of checking it would reject some working
	// configuration (encrypted keys, certificates, PKCS#11, FIDO), which is a worse
	// failure than the one being prevented, and ssh does print a diagnostic for a
	// malformed key rather than failing silently the way a missing path did. This
	// fixture is non-key bytes ON PURPOSE, to pin that boundary rather than imply
	// af verified a key.
	present := filepath.Join(dir, "id_ed25519")
	require.NoError(t, os.WriteFile(present, []byte("not-validated-on-purpose"), 0o600))
	require.NoError(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: present}),
		"af checks that the PATH names something it can hand to ssh, not that the bytes are a key")

	// And an unset identity file is not an error: the agent is the intended path.
	require.NoError(t, verifySSHIdentityFile(config.SSHConfig{Host: "h.example.com"}))
}

// A literal '%' in ssh.identity_file must be REFUSED even when the path names a
// real, readable, regular key — because ssh percent-token-expands -i/IdentityFile
// values (ssh_config(5): %%, %C, %d, %h, %i, %j, %k, %L, %l, %n, %p, %r, %u), so ssh
// would load a DIFFERENT file than the one this guard proves readable. The literal
// path can exist and be a perfectly good key while the expanded path does not, and
// ssh's startup pre-stat of the literal -i value SUCCEEDS (no "Warning: Identity
// file … not accessible"), so the divergence surfaces only at key-load time — or
// never, when an agent key authenticates instead. That is the one input class the
// stat/open guard above CANNOT see: silent wrong-identity auth, the exact failure
// verifySSHIdentityFile exists to prevent.
//
// REFUSED, NOT ESCAPED: doubling % to %% (the way escapeSSHPercent does for
// ProxyCommand) is measured to be WORSE for -i — ssh's startup pre-stat then sees
// the literal %%d path (nonexistent), prints the warning and drops the identity,
// falling back to defaults. So the guard refuses, and the composition stays
// literal (see TestSSHCommandCompositionDoesNotEscapePercentInIdentity).
func TestSSHIdentityFileRefusesAPercentTokenExpandedPath(t *testing.T) {
	dir := t.TempDir()

	// A directory whose name is literally %d — a real, stat-able, open-able path.
	// %d is ssh's home-directory token, so a working key here is what ssh never gets
	// to load: ssh rewrites the path before reading it.
	pctDir := filepath.Join(dir, "%d")
	require.NoError(t, os.Mkdir(pctDir, 0o700))
	percentKey := filepath.Join(pctDir, "client_key")
	require.NoError(t, os.WriteFile(percentKey, []byte("a-real-readable-file"), 0o600))

	// PRE-FIX THIS WOULD PASS: the literal file exists, is regular and readable —
	// every check verifySSHIdentityFile performs succeeds on the literal path —
	// and then ssh would silently load a different identity. So the assertion is
	// the REFUSAL, not the file existing.
	err := verifySSHIdentityFile(config.SSHConfig{Host: "h.example.com", IdentityFile: percentKey})
	require.Error(t, err, "an identity path containing % must be refused: ssh would expand it to a "+
		"different path than af verified and silently authenticate as someone else")
	assert.Contains(t, err.Error(), percentKey, "the operator needs to see WHICH path")
	assert.Contains(t, err.Error(), "backend=ssh")
	assert.Contains(t, err.Error(), "%",
		"the message must say WHY — that ssh expands % as a token")
	assert.NotContains(t, err.Error(), "not accessible",
		"this is the percent-token refusal, not a missing-path/directory/mode refusal")

	// Every representative token is refused — not just %d. %q is measured to make ssh
	// die loudly-but-late (after connect); the others silently diverge. The guard
	// cannot know which token ssh supports, so it refuses any %.
	tokenDir := filepath.Join(dir, "tokens")
	require.NoError(t, os.Mkdir(tokenDir, 0o700))
	for _, token := range []string{"%d", "%h", "%p", "%C", "%q", "key%name", "100%key"} {
		// The FILENAME keeps its % — a real, stat-able, readable regular file whose
		// path contains the token, so the refusal is the % check, not a stat failure.
		p := filepath.Join(tokenDir, token)
		require.NoError(t, os.WriteFile(p, []byte("k"), 0o600))
		assert.Error(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: p}),
			"token %q in an identity path must be refused", token)
	}

	// A '%' that arrives via expandUserPath (a HOME containing %) is caught too,
	// because the check runs on the EXPANDED value — the exact string af hands ssh.
	// Pin that by pointing ~ at a home whose own path contains %.
	pctHome := filepath.Join(dir, "home%dir")
	require.NoError(t, os.MkdirAll(filepath.Join(pctHome, ".ssh"), 0o700))
	homeKey := filepath.Join(pctHome, ".ssh", "id_ed25519")
	require.NoError(t, os.WriteFile(homeKey, []byte("k"), 0o600))
	t.Setenv("HOME", pctHome)
	// os.UserHomeDir honours $HOME, so expandUserPath("~/...") lands inside pctHome.
	assert.Error(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: "~/.ssh/id_ed25519"}),
		"a % that arrived via HOME expansion must be refused too — af hands ssh the expanded path")

	// NO REGRESSION: a readable key with no % anywhere still passes. This is the
	// overwhelming majority of real paths, and they must behave identically.
	plain := filepath.Join(dir, "plain_id_ed25519")
	require.NoError(t, os.WriteFile(plain, []byte("k"), 0o600))
	require.NoError(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: plain}),
		"a % free readable key must keep passing — nothing about the guard changed for it")
}

// Composition must not touch the filesystem, whatever the identity file says.
func TestSSHCommandCompositionIgnoresAnUnreadableIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "gone_id_ed25519")

	cmd, err := sshCommandPinnedTo(
		config.SSHConfig{Host: "h.example.com", IdentityFile: missing}, config.SSHHostKeyStrict, "", 0)

	require.NoError(t, err,
		"restoreRuntimeCleanup composes while loading persisted handles; refusing here would capture a "+
			"permanently dead teardown the moment a key is briefly unavailable")
	assert.Contains(t, cmd, "-i '"+missing+"'")
}

// A cleanup record that carries a dial address knows the exact machine its
// workspace is on. Dropping that would let a multi-address name re-resolve to a
// DIFFERENT machine, remove nothing, report success and retire the only
// tombstone — a permanent leak. So it is still decoded and honoured.
//
// Records exist from two eras and this covers both: #3090 wrote the field and
// dialled it as ssh's DESTINATION (with the HostKeyAlias that rejected host
// certificates), and #3086 writes it again but applies it as a ProxyCommand. Both
// decode to the same field and both must now reap through the current mechanism —
// there is no second code path for the older shape.
func TestPinnedCleanupRecordStillDialsItsRecordedAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer SetSSHRelayBinaryForTest("/opt/af/af")()

	raw, err := json.Marshal(&RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "many.example.com", Port: 2222},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		DialAddress:         "198.51.100.8",
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.NoError(t, err)
	var back RuntimeCleanupData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, "198.51.100.8", back.SSH.DialAddress, "the field must still DECODE")

	backend, _, err := restoreRuntimeCleanup("pinned-legacy", "ssh", &back)
	require.NoError(t, err)
	sb, ok := backend.(*sshBackend)
	require.True(t, ok)

	assert.Contains(t, proxyCommandValue(t, sb.provisioner.sshCmd), "ssh-relay '198.51.100.8' 2222",
		"the teardown must reach the machine the session actually ran on")
	fields := strings.Fields(sb.provisioner.sshCmd)
	assert.Equal(t, "'many.example.com'", fields[len(fields)-1],
		"and reach it WITHOUT making the address ssh's destination: that is what forced a HostKeyAlias, and "+
			"no alias value satisfies both a plain [name]:port entry and a certificate principal (#3090)")
	assert.NotContains(t, sb.provisioner.sshCmd, "HostKeyAlias")
}

// Every record with nothing to pin — written before #3090, between #3100 and
// #3086, or by a provision whose one-off resolution could not settle — dials the
// name exactly as it always has. A teardown that refused would leak the workspace
// it exists to remove.
func TestUnpinnedCleanupRecordDialsTheName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	backend, _, err := restoreRuntimeCleanup("ordinary", "ssh", &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "h.example.com", Port: 2222},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.NoError(t, err)
	sb, ok := backend.(*sshBackend)
	require.True(t, ok)

	fields := strings.Fields(sb.provisioner.sshCmd)
	assert.Equal(t, "'h.example.com'", fields[len(fields)-1])
	assert.NotContains(t, sb.provisioner.sshCmd, "HostKeyAlias")
	assert.NotContains(t, sb.provisioner.sshCmd, "-o ProxyCommand")
}

// A picker is a promise. ssh.identity_file is a local, side-effect-free fact, so
// a repo whose key path is a typo must be told at CHOOSE time rather than being
// offered a backend whose every create fails deterministically (#3092).
func TestBackendUnusableReasonSSHValidatesTheIdentityFile(t *testing.T) {
	repo := repoWithOriginForTest(t)
	restore := SetLookPathForTest(func(name string) (string, error) { return "/usr/bin/" + name, nil })
	defer restore()

	missing := filepath.Join(t.TempDir(), "typo_id_ed25519")
	err := BackendUnusableReason(BackendSSH,
		&config.ResolvedConfig{SSH: &config.SSHConfig{Host: "h.example.com", IdentityFile: missing}}, repo)
	require.Error(t, err, "the picker must not offer a backend whose every create fails on this path")
	assert.Contains(t, err.Error(), missing)

	// A readable key, and no key at all, both stay usable — so this moved only the
	// case that was already broken.
	good := filepath.Join(t.TempDir(), "id_ed25519")
	require.NoError(t, os.WriteFile(good, []byte("k"), 0o600))
	assert.NoError(t, BackendUnusableReason(BackendSSH,
		&config.ResolvedConfig{SSH: &config.SSHConfig{Host: "h.example.com", IdentityFile: good}}, repo))
	assert.NoError(t, BackendUnusableReason(BackendSSH,
		&config.ResolvedConfig{SSH: &config.SSHConfig{Host: "h.example.com"}}, repo))
}
