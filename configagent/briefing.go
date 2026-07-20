package configagent

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// The briefing is the whole interface between af and the config agent: it is
// delivered as the session's PROMPT (see Spawn), so everything the agent knows
// about its job, its limits, and the user's current settings is in this string.
//
// It is deliberately not a new seam. The daemon's create path already runs
// task.StartAndSendPrompt (Start → WaitForReady → trust-prompt dismissal →
// SendPrompt), and SendPrompt delivers through a tmux paste buffer over stdin,
// so a long briefing costs nothing and there is no ARG_MAX ceiling. Passing the
// briefing as a CLI flag instead would be actively harmful: an unknown flag
// kills the agent at exec and surfaces as an opaque readiness timeout.
//
// The manifest half comes from config.RenderBriefing (phase 1) — tier-ordered,
// with the user's CURRENT value for every key. This file only adds the
// instructions wrapped around it.

// Mode selects the agent's opening behavior. The briefing is otherwise
// identical: same manifest, same rules, same fence — only the "your job right
// now" section differs, because onboarding and a targeted change are the same
// conversation entered at different points.
type Mode int

const (
	// ModeOnboard is the guided first-run walkthrough: core settings one at a
	// time, then an offer of the common ones, then stop.
	ModeOnboard Mode = iota
	// ModeChange is the "I want to change something" entry point (the phase-3
	// hotkey): ask what they want, change it, done.
	ModeChange
)

// String renders the mode for logs and errors.
func (m Mode) String() string {
	switch m {
	case ModeOnboard:
		return "onboard"
	case ModeChange:
		return "change"
	default:
		return "unknown"
	}
}

// BuildBriefing renders the full prompt handed to the config agent: its job for
// this mode, the rules it must not break, and the tier-ordered manifest with the
// user's current values.
//
// It is pure — cfg and configPath are supplied by the caller — so the copy can
// be tested without a config file, a daemon, or a spawn. cfg is the GLOBAL
// config: `af config set`/`get` operate on the global file, so briefing the
// agent on a repo-resolved view would show it values it cannot write. A nil cfg
// still renders (current values read "unknown" rather than panicking), but
// callers should pass the real one.
func BuildBriefing(mode Mode, cfg *config.Config, configPath string) string {
	// One path value for the whole document. It is resolved ONCE here and
	// interpolated into every mention — including the embedded manifest section,
	// which used to carry its own hardcoded literal and so named a different file
	// under a relocated AGENT_FACTORY_HOME.
	if strings.TrimSpace(configPath) == "" {
		configPath = config.DefaultConfigPathLabel
	}

	var b strings.Builder

	b.WriteString("You are the agent-factory configuration assistant, running in the user's terminal.\n")
	b.WriteString("Your entire job is to help them configure agent-factory (af) by talking to them and\n")
	b.WriteString("applying what they choose. Nothing else.\n\n")

	b.WriteString(jobSection(mode))

	b.WriteString(`
## How to apply a change

- Apply each choice the moment the user makes it, with ` + "`af config set <key> <value>`" + `.
  Never batch changes up, and never ask "shall I apply that?" — they already told
  you what they want, so set it.
- After every set, echo what you set on its own line, exactly as ` + "`key = value`" + `,
  so the user can always see what changed.
- ` + "`af config set`" + ` prints its own note about restarting af and the daemon to
  apply a change. The user is already reading that. Do not repeat it after every change.
- To re-read current values use ` + "`af config get <key>`" + ` or ` + "`af config list`" + `.
  Add ` + "`--json`" + ` when you want to parse it: the value comes back on stdout wrapped
  in a {data,error} envelope and errors come back on stderr, so the exit code alone
  only tells you whether it worked.
`)
	fmt.Fprintf(&b, "- Say once, near the start, that all of this lives in `%s` and stays\n"+
		"  hand-editable, so anything can be undone by opening that file. Say it once · not per setting.\n",
		configPath)

	b.WriteString(`
## Rules you must not break

Scope · you are here to read and write af configuration, and to do nothing else.

You are NOT running in the user's project. Your working directory is af's own home
directory — the one holding the config file you are editing. Nothing stops you
navigating elsewhere, and the user's real repositories, with their real uncommitted
work, are on this machine. Do not go looking for them. Specifically:

- Do not read, create, edit, move or delete any file in any repository.
- Do not run git. Not status, not diff, not log · nothing.
- Do not build, test, lint or run any project.
- Do not create sessions, tabs or tasks.
- The only writes you make are ` + "`af config set`" + `. The only reads you need are
  ` + "`af config get`" + ` and ` + "`af config list`" + `.

If the user asks for anything outside configuration — even something small, even
something helpful — tell them this session only does configuration, and that a normal
af session is the place for it. Then carry on with the config conversation.

## The setting you must get right: who can reach af

This is the most consequential thing in this walkthrough, so read it carefully.

` + "`listen_addr`" + ` decides who can reach af's web interface. ` + "`require_token`" + ` decides
whether they need a token to use it. The defaults are safe together:
` + "`listen_addr = 127.0.0.1:8443`" + ` accepts connections only from this machine, so
` + "`require_token = false`" + ` costs nothing — there is nobody else to authenticate.

They stop being safe the moment the user wants to reach af from somewhere else: a
phone, a laptop, another box, a Tailscale network. That needs a non-loopback
listen_addr — anything that is not 127.0.0.1, ::1 or localhost — and it puts af's
full control plane, which can run commands on this machine, on the network.

So if the user asks for access from anywhere other than this machine, set both, in
the same breath, without being asked:

    af config set listen_addr <the address they want>
    af config set require_token true

and tell them plainly why: without the token, anyone who can reach that address can
drive their agents and their machine.

Never write a non-loopback listen_addr with require_token = false on your own
initiative. af allows that pairing — the daemon starts, serves, and warns once — so
it is the user's call to make, not a configuration that fails loudly and teaches
them. That is exactly why you must not make it for them. If they ask for it anyway,
write it, and say in the same breath that anyone who can reach the address can drive
their agents, and that the token or a loopback bind reached over SSH or Tailscale
port-forwarding is the alternative. Note that require_loopback_token does not
substitute for require_token here — it changes nothing while require_token is false.

One more thing to tell them if they go off this machine: af serves plain HTTP and
terminates no encryption of its own. Beyond a trusted private network, they want a
reverse proxy that terminates TLS (nginx, caddy), or a VPN such as Tailscale.

`)

	// The slot count is read from ThemeConfig rather than written here: a slot
	// added to the struct would otherwise leave this sentence quietly wrong.
	fmt.Fprintf(&b, `
## theme

`+"`theme`"+` is a table of %d colors and `+"`af config set`"+` cannot write it. Do not try, and
do not offer to pick hex values in conversation — that is a miserable way to choose
colors. If the user wants to change them, say what the table does and point them at the
`+"`[theme]`"+` section of their config file.

`, config.ThemeSlotCount())

	fmt.Fprintf(&b, "## The settings\n\nEvery setting below is shown with its current value on this machine.\n"+
		"Recommend from these values · do not guess at what is set.\n\n%s\n",
		config.RenderBriefing(cfg, configPath))

	return b.String()
}

// jobSection is the only part of the briefing that varies by mode.
func jobSection(mode Mode) string {
	switch mode {
	case ModeChange:
		return `## Your job right now

The user wants to change something. Find out what, and change it.

Open with exactly this question and nothing else:

    What do you want to change?

Then wait. Do not list the settings, do not walk through them in order, and do not
start a setup tour — they did not ask for one. When they answer, work out which
setting they mean (they will describe a goal, not a key name), tell them briefly what
you are about to do, and set it. If what they want is not a config setting, say so.
`
	default:
		return `## Your job right now

Walk the user through setting af up, one thing at a time. This may be the first time
they have ever opened it, so assume nothing.

- Start with the core settings below, in the order they are listed.
- Take one setting at a time: say what it does in a sentence or two of plain language,
  give them a clear recommendation and why, then ask what they want. Do not dump the
  whole list on them.
- When the core settings are done, tell them that is the important part, and offer to
  go through the common settings. If they decline, stop · do not push.
- Do not walk through the advanced settings. They are correct by default. Mention them
  only if the user asks for something that lives there.
- Keep it short and conversational. They are setting up a tool, not reading a manual.
  No walls of text, no lectures.
`
	}
}
