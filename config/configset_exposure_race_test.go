package config

import (
	"os"
	"strings"
	"testing"
)

// TestListenerWriteRefusalJudgesTheFileInsideTheLock is #2412's property carried
// into the #5137 refusal: the judgment runs on the bytes about to be written,
// INSIDE the file lock, never on a config loaded before it.
//
// `af config set` re-reads config.toml inside the file lock to make its surgical
// edit; the exposure is a PAIRING — a non-loopback listen_addr together with
// require_token = false — so judging it needs the value of the key the caller is
// not setting. This test recreates the window directly rather than racing for
// it: it lets a competing writer change the OTHER half of the pairing on disk,
// and only then applies the write. A judgment taken from the stale pre-lock
// snapshot would see the safe half and let the write through; the refused write
// must see the file as it actually is.
//
// The stakes are the silent write. Both racers exit 0 with nothing printed, and
// the daemon is not a backstop — it emits its own notice only when it binds, so
// an already-running daemon says nothing until the next restart, while the
// config left on disk is one the NEXT start refuses to bind.
func TestListenerWriteRefusalJudgesTheFileInsideTheLock(t *testing.T) {
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
			// is refused yet, and this snapshot is what a stale judgment would be
			// computed from.
			preLock, err := LoadConfig()
			if err != nil {
				t.Fatalf("loading the seed config: %v", err)
			}
			if ListenerBindRefusal(preLock) != "" {
				t.Fatal("premise broken: the seed config is already refused, so this test cannot " +
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
			var applyErr error
			if err := WithFileLock(tomlPath, func() error {
				res, _, applyErr = write.apply(pinnedTestTarget(t, tomlPath), prettyHomePath(tomlPath))
				return applyErr
			}); err == nil {
				var warnings []string
				if res != nil {
					warnings = res.Warnings
				}
				t.Fatalf("applying %s=%q: succeeded with warnings=%v — the write landed on a "+
					"refused posture and MUST error", c.key, c.value, warnings)
			}

			// The judgment must have been made on the locked file — the competing
			// bytes — not the stale pre-lock snapshot.
			if !strings.Contains(applyErr.Error(), "refusing to write") {
				t.Fatalf("the refusal must name itself a refusal, got: %v", applyErr)
			}
			for _, want := range []string{
				"network.require_token true", "network.listen_addr 127.0.0.1:8443",
				"network.allow_unauthenticated_network true",
			} {
				if !strings.Contains(applyErr.Error(), want) {
					t.Errorf("the refusal must carry the %q fix, got: %v", want, applyErr)
				}
			}

			// And nothing was written: the file still holds exactly what the
			// competing writer left.
			after, err := os.ReadFile(tomlPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != c.competing {
				t.Errorf("a refused write must leave the file untouched.\ncompeting: %q\nafter: %q",
					c.competing, after)
			}
		})
	}
}

// TestExposureWarningStaysSilentWhenTheRaceLeavesItSafe is the other half: the
// judgment must not complain merely because it now looks at fresh bytes. A
// competing writer that makes the config SAFER — or one that leaves an exposure
// this write then closes — must still exit quiet, or the complaint becomes noise
// and gets ignored, which is the same outcome as not printing it. And an
// unrelated key on an already-refused file stays allowed AND quiet: it did not
// create the posture, and refusing it would strand every other config edit
// behind an unrelated fix (#5137).
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
			name:      "an unrelated key never speaks, even on a refused config",
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
				res, _, applyErr = write.apply(pinnedTestTarget(t, tomlPath), prettyHomePath(tomlPath))
				return applyErr
			}); err != nil {
				t.Fatalf("applying %s=%q: %v", c.key, c.value, err)
			}

			if len(res.Warnings) != 0 {
				t.Errorf("setting %s=%q warned, but the resulting config is not a serving "+
					"unauthenticated network listener: %v", c.key, c.value, res.Warnings)
			}
		})
	}
}

// TestSetGlobalConfigValueRefusesFromTheWrittenFile pins the wiring end to end:
// SetGlobalConfigValue's refusal must come from scalarWrite.apply's judgment of
// the file, so the two cannot drift apart while the unit test above keeps
// passing. It sets the second half of an exposure that is already half-present
// on disk — the case where reconstructing from a pre-lock snapshot and reading
// the file happen to agree, so this stays honest about what it covers: the
// plumbing, not the race.
func TestSetGlobalConfigValueRefusesFromTheWrittenFile(t *testing.T) {
	path := writeTempConfig(t, "default_program = 'claude'\nlisten_addr = '0.0.0.0:8443'\nrequire_token = true\n")

	_, err := SetGlobalConfigValue("require_token", "false")
	if err == nil {
		t.Fatal("turning the token off on a network-bound listener must be refused")
	}
	if !strings.Contains(err.Error(), "0.0.0.0:8443") {
		t.Errorf("the refusal must name the address it is protecting, got: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(after), "require_token = false") {
		t.Error("the refused write must not have landed the token-off value")
	}
}
