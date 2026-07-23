package config

import (
	"os"
	"strings"
	"testing"
)

// TestExposureWarningJudgesTheFileInsideTheLock is #2412.
//
// `af config set` re-reads config.toml inside the file lock to make its surgical
// edit, but it used to judge the exposure warning from a *Config loaded BEFORE
// the lock. The exposure is a PAIRING — a non-loopback listen_addr together with
// require_token = false — so judging it needs the value of the key the caller is
// not setting, and that value came from the stale pre-lock snapshot.
//
// This test recreates the window directly rather than racing for it: it captures
// what a pre-lock LoadConfig returns, lets a competing writer change the OTHER
// half of the pairing on disk, and only then applies the write. The old code
// answered from the captured snapshot and stayed silent; the fix answers from
// the bytes it is about to write.
//
// The stakes are that silence. Both racers can exit 0 with nothing printed, and
// the daemon is not a backstop — it emits its own notice only when it binds, so
// an already-running daemon says nothing until the next restart. The config left
// on disk serves DeliverPrompt, which runs instructions through the user's
// agents, to anyone who can route to it.
func TestExposureWarningJudgesTheFileInsideTheLock(t *testing.T) {
	cases := []struct {
		name string
		// seed is the config both processes start from: safe, loopback-bound,
		// token required.
		seed string
		// competing is what the other process leaves on disk while this one is
		// between its pre-lock load and its locked read.
		competing string
		// key/value is what this process then writes — the other half of the
		// exposure.
		key, value string
	}{
		{
			name:      "listen_addr write lands on a token that was turned off",
			seed:      "default_program = 'claude'\nlisten_addr = '127.0.0.1:8443'\nrequire_token = true\n",
			competing: "default_program = 'claude'\nlisten_addr = '127.0.0.1:8443'\nrequire_token = false\n",
			key:       "listen_addr",
			value:     "0.0.0.0:8443",
		},
		{
			name:      "require_token write lands on a listener that moved to the network",
			seed:      "default_program = 'claude'\nlisten_addr = '127.0.0.1:8443'\nrequire_token = true\n",
			competing: "default_program = 'claude'\nlisten_addr = '0.0.0.0:8443'\nrequire_token = true\n",
			key:       "require_token",
			value:     "false",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tomlPath := writeTempConfig(t, c.seed)

			// What SetGlobalConfigValue loads before it takes the lock. Nothing
			// is exposed yet, and this snapshot is what the pre-#2412 warning was
			// computed from.
			preLock, err := LoadConfig()
			if err != nil {
				t.Fatalf("loading the seed config: %v", err)
			}
			if ListenerServesUnauthenticatedNetwork(preLock.ListenAddr, preLock.RequireToken) {
				t.Fatal("premise broken: the seed config is already exposed, so this test cannot " +
					"distinguish a stale judgment from a fresh one")
			}

			// The other process wins the window and commits its half of the
			// pairing. In production it held the same lock to do this; here the
			// lock is uncontended because we are standing in for the moment just
			// after it released.
			if err := os.WriteFile(tomlPath, []byte(c.competing), 0644); err != nil {
				t.Fatal(err)
			}

			section, leaf, spec, ok := resolveSettable(c.key)
			if !ok {
				t.Fatalf("premise broken: %q is not settable", c.key)
			}
			canonical, encoded, err := canonicalizeScalar(spec.kind, c.value)
			if err != nil {
				t.Fatalf("canonicalizing %s=%q: %v", c.key, c.value, err)
			}
			write := scalarWrite{key: c.key, section: section, leaf: leaf, canonical: canonical, encoded: encoded}

			var res *SetResult
			if err := WithFileLock(tomlPath, func() error {
				var applyErr error
				res, applyErr = write.apply(tomlPath, prettyHomePath(tomlPath))
				return applyErr
			}); err != nil {
				t.Fatalf("applying %s=%q: %v", c.key, c.value, err)
			}

			// The write must have produced a genuinely exposed config — otherwise
			// there would be nothing to warn about and the assertion below would
			// pass for the wrong reason.
			after, err := LoadConfig()
			if err != nil {
				t.Fatalf("reloading after the write: %v", err)
			}
			if !ListenerServesUnauthenticatedNetwork(after.ListenAddr, after.RequireToken) {
				t.Fatalf("premise broken: after setting %s=%q the config is listen_addr=%q "+
					"require_token=%v, which is not exposed", c.key, c.value, after.ListenAddr, after.RequireToken)
			}

			if len(res.Warnings) == 0 {
				t.Fatalf("setting %s=%q made the control plane unauthenticated on the network "+
					"(listen_addr=%q, require_token=%v) and printed NOTHING.\n\n"+
					"The warning was judged from the config loaded before the file lock "+
					"(listen_addr=%q, require_token=%v), not from the file this write actually "+
					"landed on. Judge the exposure from the bytes being written (#2412).",
					c.key, c.value, after.ListenAddr, after.RequireToken,
					preLock.ListenAddr, preLock.RequireToken)
			}
			if w := res.Warnings[0]; !strings.Contains(w, "af config set require_token true") {
				t.Errorf("the warning must say how to fix it, got: %s", w)
			}
		})
	}
}

// TestExposureWarningStaysSilentWhenTheRaceLeavesItSafe is the other half: the
// fix must not warn merely because it now looks at fresh bytes. A competing
// writer that makes the config SAFER — or one that leaves an exposure this write
// then closes — must still exit quiet, or the warning becomes noise and gets
// ignored, which is the same outcome as not printing it.
func TestExposureWarningStaysSilentWhenTheRaceLeavesItSafe(t *testing.T) {
	cases := []struct {
		name       string
		seed       string
		competing  string
		key, value string
	}{
		{
			name:      "competing writer turned the token back on",
			seed:      "default_program = 'claude'\nlisten_addr = '127.0.0.1:8443'\nrequire_token = false\n",
			competing: "default_program = 'claude'\nlisten_addr = '127.0.0.1:8443'\nrequire_token = true\n",
			key:       "listen_addr",
			value:     "0.0.0.0:8443",
		},
		{
			name:      "this write is the one that closes the exposure",
			seed:      "default_program = 'claude'\nlisten_addr = '127.0.0.1:8443'\nrequire_token = false\n",
			competing: "default_program = 'claude'\nlisten_addr = '0.0.0.0:8443'\nrequire_token = false\n",
			key:       "listen_addr",
			value:     "127.0.0.1:8443",
		},
		{
			name:      "an unrelated key never speaks, even on an exposed config",
			seed:      "default_program = 'claude'\nlisten_addr = '127.0.0.1:8443'\nrequire_token = true\n",
			competing: "default_program = 'claude'\nlisten_addr = '0.0.0.0:8443'\nrequire_token = false\n",
			key:       "auto_update",
			value:     "true",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tomlPath := writeTempConfig(t, c.seed)
			if _, err := LoadConfig(); err != nil {
				t.Fatalf("loading the seed config: %v", err)
			}
			if err := os.WriteFile(tomlPath, []byte(c.competing), 0644); err != nil {
				t.Fatal(err)
			}

			section, leaf, spec, ok := resolveSettable(c.key)
			if !ok {
				t.Fatalf("premise broken: %q is not settable", c.key)
			}
			canonical, encoded, err := canonicalizeScalar(spec.kind, c.value)
			if err != nil {
				t.Fatalf("canonicalizing %s=%q: %v", c.key, c.value, err)
			}
			write := scalarWrite{key: c.key, section: section, leaf: leaf, canonical: canonical, encoded: encoded}

			var res *SetResult
			if err := WithFileLock(tomlPath, func() error {
				var applyErr error
				res, applyErr = write.apply(tomlPath, prettyHomePath(tomlPath))
				return applyErr
			}); err != nil {
				t.Fatalf("applying %s=%q: %v", c.key, c.value, err)
			}

			if len(res.Warnings) != 0 {
				t.Errorf("setting %s=%q warned, but the resulting config is not an unauthenticated "+
					"network listener: %v", c.key, c.value, res.Warnings)
			}
		})
	}
}

// TestSetGlobalConfigValueWarnsFromTheWrittenFile pins the wiring end to end:
// SetGlobalConfigValue's warning must come from scalarWrite.apply's judgment of
// the file, so the two cannot drift apart while the unit test above keeps
// passing. It sets the second half of an exposure that is already half-present
// on disk — the case where reconstructing from a pre-lock snapshot and reading
// the file happen to agree, so this stays honest about what it covers: the
// plumbing, not the race.
func TestSetGlobalConfigValueWarnsFromTheWrittenFile(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\nlisten_addr = '0.0.0.0:8443'\nrequire_token = true\n")

	res, err := SetGlobalConfigValue("require_token", "false")
	if err != nil {
		t.Fatalf("set require_token=false: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("turning the token off on a network-bound listener must warn")
	}
	if w := res.Warnings[0]; !strings.Contains(w, "0.0.0.0:8443") {
		t.Errorf("the warning must name the address that is exposed, got: %s", w)
	}
}
