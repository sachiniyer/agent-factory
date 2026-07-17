package configagent

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// The briefing IS the feature: it is the only thing the config agent is told
// about its job, its limits, and the user's settings. A missing rule here is not
// a cosmetic defect — it is an agent that batches changes, or repeats the
// restart note, or leaves a network listener unauthenticated, or edits the
// user's repo. So each rule the owner specified gets a test.

// briefingConfig returns a config whose values all differ from the defaults, so
// a renderer that accidentally printed defaults instead of the live config would
// fail rather than coincidentally pass.
func briefingConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = "codex"
	cfg.ListenAddr = "0.0.0.0:9443"
	cfg.RequireToken = true
	cfg.DaemonPollInterval = 4321
	cfg.UpdateChannel = config.UpdateChannelPreview
	return cfg
}

// TestBriefingCarriesTheManifestInTierOrder pins that the briefing actually
// contains phase 1's manifest, every key, in tier order. Without this the agent
// would be walking a list it invented.
func TestBriefingCarriesTheManifestInTierOrder(t *testing.T) {
	out := BuildBriefing(ModeOnboard, briefingConfig(), "/tmp/af/config.toml")

	core := strings.Index(out, "## Core settings")
	common := strings.Index(out, "## Common settings")
	advanced := strings.Index(out, "## Advanced settings")
	if core < 0 || common < 0 || advanced < 0 {
		t.Fatalf("briefing is missing a tier section: core=%d common=%d advanced=%d", core, common, advanced)
	}
	if !(core < common && common < advanced) {
		t.Errorf("tier sections are out of order: core=%d common=%d advanced=%d", core, common, advanced)
	}

	for _, e := range config.Manifest() {
		if !strings.Contains(out, "### `"+e.Key+"`") {
			t.Errorf("briefing does not mention config key %q — the agent cannot offer what it cannot see", e.Key)
		}
	}
}

// TestBriefingCarriesCurrentValues is why BuildBriefing takes a config at all:
// an agent recommending changes has to know what is set now, or it will offer to
// "turn on" something already on.
func TestBriefingCarriesCurrentValues(t *testing.T) {
	out := BuildBriefing(ModeOnboard, briefingConfig(), "/tmp/af/config.toml")

	for _, want := range []string{
		"current: codex",
		"current: 0.0.0.0:9443",
		"current: 4321",
		"current: preview",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("briefing is missing current value %q", want)
		}
	}
}

// TestBriefingStatesTheScopeFence is the load-bearing safety test. Because the
// config agent rides the in-place seam, its cwd IS the user's real working tree
// — nothing sandboxes it. The fence is instruction-only, so if this wording goes
// missing there is no second line of defense.
func TestBriefingStatesTheScopeFence(t *testing.T) {
	for _, mode := range []Mode{ModeOnboard, ModeChange} {
		out := BuildBriefing(mode, briefingConfig(), "/tmp/af/config.toml")

		for _, want := range []string{
			"real working tree",
			"Do not read, create, edit, move or delete any file in this repository.",
			"Do not run git.",
			"Do not build, test, lint or run the project.",
			"Do not create sessions, tabs or tasks.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("mode %s: briefing is missing scope-fence rule %q", mode, want)
			}
		}
	}
}

// TestBriefingCouplesListenAddrToRequireToken pins the security rule the owner
// called the single most consequential thing the walkthrough does: opening af to
// the network must never leave the token off. If this text drifts, the walkthrough
// can talk a user into an unauthenticated control plane on their LAN.
func TestBriefingCouplesListenAddrToRequireToken(t *testing.T) {
	for _, mode := range []Mode{ModeOnboard, ModeChange} {
		out := BuildBriefing(mode, briefingConfig(), "/tmp/af/config.toml")

		for _, want := range []string{
			"af config set require_token true",
			"Never leave the user with a non-loopback",
			"require_token = false",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("mode %s: briefing is missing the listen_addr/require_token coupling text %q", mode, want)
			}
		}
		// The rule is worthless if it does not say what "non-loopback" means.
		if !strings.Contains(out, "127.0.0.1") || !strings.Contains(out, "localhost") {
			t.Errorf("mode %s: the security rule must say which addresses count as local", mode)
		}
	}
}

// TestBriefingStatesTheApplyRules covers the four delivery rules: apply
// immediately, echo key = value, say the hand-editable line once, and do not
// parrot the restart note `af config set` already prints.
func TestBriefingStatesTheApplyRules(t *testing.T) {
	out := BuildBriefing(ModeOnboard, briefingConfig(), "/tmp/af/config.toml")

	for _, want := range []string{
		"`af config set <key> <value>`",        // apply directly
		"Never batch",                          // no batching
		"shall I apply that?",                  // no confirm-diff step
		"`key = value`",                        // echo shape
		"Do not repeat it after every change.", // no duplicated restart note
		// The undo story. Matched on a wrap-INDEPENDENT fragment: an earlier
		// version asserted "stays\n  hand-editable", which pinned the paragraph's
		// line breaks rather than the rule, and broke the first time the section
		// was rewrapped for readability.
		"hand-editable", // undo story
		"--json",        // machine-readable reads
	} {
		if !strings.Contains(out, want) {
			t.Errorf("briefing is missing apply rule %q", want)
		}
	}
}

// TestBriefingRefusesToSetTheme pins that the agent is told not to attempt the
// one key that would waste the user's time: 19 hex slots, not settable by CLI.
func TestBriefingRefusesToSetTheme(t *testing.T) {
	out := BuildBriefing(ModeOnboard, briefingConfig(), "/tmp/af/config.toml")
	if !strings.Contains(out, "cannot write it") || !strings.Contains(out, "do not offer to pick hex values") {
		t.Errorf("briefing must tell the agent not to try setting theme:\n%s", out)
	}
}

// TestBriefingNamesTheConfigPath checks the caller-supplied path is used (so a
// relocated AGENT_FACTORY_HOME is named honestly), and that an empty path falls
// back to the documented default rather than rendering a blank.
func TestBriefingNamesTheConfigPath(t *testing.T) {
	out := BuildBriefing(ModeOnboard, briefingConfig(), "/custom/home/config.toml")
	if !strings.Contains(out, "/custom/home/config.toml") {
		t.Error("briefing should name the real config path it was given")
	}

	fallback := BuildBriefing(ModeOnboard, briefingConfig(), "  ")
	if !strings.Contains(fallback, config.DefaultConfigPathLabel) {
		t.Errorf("an empty config path should fall back to %q", config.DefaultConfigPathLabel)
	}
}

// TestModesDifferOnlyInTheOpening is the two-modes requirement: same manifest,
// same rules, same fence — a different job section. It asserts both that the
// openings differ AND that the shared parts really are shared, since "different
// modes" would be worthless if ModeChange quietly dropped the security rule.
func TestModesDifferOnlyInTheOpening(t *testing.T) {
	cfg := briefingConfig()
	onboard := BuildBriefing(ModeOnboard, cfg, "/tmp/af/config.toml")
	change := BuildBriefing(ModeChange, cfg, "/tmp/af/config.toml")

	if onboard == change {
		t.Fatal("ModeOnboard and ModeChange must not produce the same briefing")
	}

	// ModeOnboard walks; ModeChange asks.
	if !strings.Contains(onboard, "Walk the user through setting af up, one thing at a time.") {
		t.Error("ModeOnboard should open with the guided walkthrough instruction")
	}
	if strings.Contains(onboard, "What do you want to change?") {
		t.Error("ModeOnboard must not open with the change prompt — it is a walkthrough")
	}
	if !strings.Contains(change, "What do you want to change?") {
		t.Error("ModeChange should open by asking what to change")
	}
	if !strings.Contains(change, "do not\nstart a setup tour") {
		t.Error("ModeChange must explicitly not walk the full list")
	}
	if strings.Contains(change, "Walk the user through setting af up") {
		t.Error("ModeChange must not carry the walkthrough instruction")
	}

	// The manifest and the non-negotiable rules are identical in both.
	for _, shared := range []string{
		"### `listen_addr`",
		"### `default_program`",
		"af config set require_token true",
		"Do not run git.",
		"current: codex",
	} {
		if !strings.Contains(onboard, shared) || !strings.Contains(change, shared) {
			t.Errorf("both modes must carry %q — only the opening may differ", shared)
		}
	}
}

// TestBriefingHandlesNilConfig pins that a briefing never panics on a nil config
// and never presents defaults as if they were the user's live settings.
func TestBriefingHandlesNilConfig(t *testing.T) {
	out := BuildBriefing(ModeOnboard, nil, "/tmp/af/config.toml")
	if !strings.Contains(out, "current: unknown") {
		t.Error("a nil config should render current values as unknown")
	}
}
