package designtokens

import (
	"bytes"
	"html/template"
	"os"
	"path/filepath"
	"strings"
)

type component struct {
	Name, Spec, WebStill, TUIStill, TUINote string
	Web                                     template.HTML // Authored static specimens, never external content.
	TUI                                     string
}

func components(t Tokens) []component {
	items := []component{
		{"Rail", "Use surface behind the rail, ink for names and ink-muted only for branch/time metadata. Selected rows use surface-raised plus an accent marker and bold name; the state keeps its own colour. Body type, space-2 row insets and space-1 glyph gaps are fixed. Show one label per liveness state; disclose mechanical detail on selection.", "dashboard", "sessions-dense", "App-model driver · sessions dense", "", ""},
		{"Header", "Use surface with ink context and heading type; no raised toolbar. Mark the active view with an accent underline, and use ink-muted only for secondary connection detail. Use space-3 insets and space-2 between controls. Theme choice is exactly Light / Dark / System.", "dashboard", "single-project", "App-model driver · single project", `<div class="sg-bar"><strong>todo-cli</strong><span class="sg-active">Sessions</span><span>Tasks</span><span>Config</span><span>Connected</span></div>`, "todo-cli · Sessions · Connected\nKeyboard: navigation"},
		{"Terminal chrome", "Use surface with ink title, heading type and border for an unfocused frame. Keyboard ownership adds the accent outline and the word Keyboard, never ready green. Use space-2 chrome insets; terminal content gets no decorative padding. Preserve agent ANSI output and ctrl+] exit.", "agent-tab", "keyboard", "App-model driver · keyboard", `<div class="sg-focus"><strong>tidy-tests · Agent · Keyboard</strong><pre>$ ./test.sh
2 tests passed</pre><span>ctrl+] · Return to sessions</span></div>`, "┌ tidy-tests · Agent · Keyboard ┐\n│ $ ./test.sh                  │\n│ 2 tests passed               │\n└ ctrl+] · Return to sessions ─┘"},
		{"Tabs and review", "Use surface and body-sized ink labels. Only the active tab gets an accent underline and bold text. Review branch changes in a process tab beside the agent tab. Use space-2 between tabs, no pill radii. TUI selection is a raised row and cursor. Preserve keyboard routes to close, switch and split.", "review", "pane", "App-model driver · pane", `<div class="sg-bar"><span>Agent</span><strong class="sg-active">diff</strong></div><pre>$ git diff --stat
2 files changed</pre>`, "  1 · Agent\n› 2 · diff\n  2 files changed"},
		{"Dialogs and overlays", "Use surface-raised, border and radius-dialog with space-3 insets. The title uses type-title; body and field labels use ink at body size. Group fields with space-2 and separate the footer with space-4. One accent-filled primary button; inline failure uses dead. Preserve input and restore focus on close.", "new-session", "prompt", "App-model driver · prompt", `<div class="sg-dialog"><strong>New session</strong><label>Title<input value="tidy-tests" readonly></label><label>Prompt<textarea readonly>Cover appending a second item…</textarea></label><div class="sg-bar"><button type="button">Cancel</button><button type="button" class="sg-primary">Create</button></div></div>`, "╭ New session ───────────────╮\n│ Title: tidy-tests          │\n│ Prompt: Cover appending…   │\n│ enter create · esc cancel  │\n╰───────────────────────────╯"},
		{"Tasks", "Use surface; task names, next run and action labels use body-sized ink. Only raw trigger/time metadata uses caption-sized ink-muted. Selected tasks use surface-raised plus the accent marker. Use space-2 row insets, space-4 between groups. Failures use dead and stay visible. Edit is primary; other actions are disclosed.", "tasks", "tasks", "App-model driver · tasks", `<div class="sg-selected"><strong>nightly-tests · Enabled</strong><br><span>Next run · Tomorrow at 12:00 UTC</span><br><small>cron · 0 12 * * *</small><br><button type="button" class="sg-primary">Edit task</button></div>`, "Tasks\n› nightly-tests · Enabled\n  Next run · Tomorrow at 12:00 UTC\n  enter edit · r run now · esc back"},
		{"Config and accounts", "Use surface with heading-sized ink section titles, body-sized ink keys, labels and values, and caption-sized ink-muted paths. Inputs use surface-raised, border and radius-control. Use space-3 panel insets and space-4 between Config and Accounts. Appearance offers Light / Dark / System only. No palette editor, presets or colour keys.", "config-accounts", "appearance", "App-model driver · appearance", `<strong>Config · Local daemon</strong><br><small>/work/config.toml</small><label>Appearance<select disabled><option>System</option><option>Light</option><option>Dark</option></select></label><label>Editor binary<span>Program used by editor tabs</span><input value="code-server" readonly></label><span>Saved · Applies to new tabs</span><hr><strong>Accounts</strong><p>work · Logged in</p><button type="button">Add account</button>`, "Config · Local daemon\n  Appearance: System · Light · Dark\n› Editor binary: code-server\n  Saved · Applies to new tabs\nAccounts\n  work · Logged in"},
		{"Buttons, fields and menus", "Primary: accent fill with surface text. Secondary: surface-raised fill with ink text. All controls use border, body type, radius-control and space-2 padding; focus always adds the accent outline. Disabled controls keep ink and a dashed outline. Destructive confirmation uses dead text. Menus use surface-raised and radius-dialog. Never use ink-muted for action or field labels.", "new-session", "project-picker", "App-model driver · project picker", `<label>Title<input class="sg-focus" value="tidy-tests" readonly></label><div class="sg-bar"><button class="sg-primary" type="button">Create</button><button type="button">Cancel</button><button disabled>Creating…</button><button class="sg-error" type="button">Delete session</button></div><details><summary>Project · todo-cli</summary><p>todo-cli · Selected</p><p>Register project…</p></details>`, "Project\n› todo-cli · Selected\n  Register project…\nenter select · esc cancel\nCreating… · Please wait"},
		{"Empty states", "Use surface, display-sized ink for the condition and body-sized ink for the next step. Use space-4 between explanation and the single primary action. No illustration, card or extra colour. Distinguish zero sessions from no project; empty TUI sections do not reserve permanent rows.", "dashboard", "zero-sessions", "App-model driver · zero sessions", `<strong class="sg-empty-title">No sessions yet</strong><p>Create a session in todo-cli.</p><button class="sg-primary" type="button">New session</button><hr><strong class="sg-empty-title">No project selected</strong><p>Choose a project to see its sessions.</p><button type="button">Choose project</button>`, "No sessions yet\nPress n to create a session.\n\nNo project selected\nChoose a project to continue."},
		{"Errors and notices", "Use surface and body-sized ink for consequence and recovery instructions; only the failure heading uses dead. A full unavailable screen may use display type. Use space-2 inside the message and space-4 before the recovery action. Save/restart notices use ink, never ready green. Wrap actionable text and preserve entered input.", "config-accounts", "no-daemon", "App-model driver · no daemon", `<strong class="sg-error sg-empty-title">Cannot reach the daemon</strong><p>Sessions could not be loaded. Check the daemon, then retry.</p><button type="button">Retry</button><hr><strong>Login expired</strong><p>Sign in again to reconnect.</p><button type="button">Sign in</button>`, "Cannot reach the daemon\nSessions could not be loaded.\nCheck the daemon, then retry.\n\nLogin expired · Sign in again"},
		{"Help and status bar", "Use surface and body-sized ink for shortcuts and exit instructions; only supplemental annotations use caption-sized ink-muted. Use space-2 between fragments, no boxes or state colours. Show valid shortcuts for the current keyboard owner; the exit route survives narrow widths. Connection labels are static.", "agent-tab", "help", "App-model driver · help", `<div class="sg-bar"><span>ctrl+] · Return to sessions</span><span>? · Help</span></div><p>Connecting…</p>`, "ctrl+] return · ? help\nConnecting…"},
	}
	var web strings.Builder
	var tui []string
	names := []string{"tidy-tests", "add-json-export", "remote-build", "fix-empty-add", "document-cli", "nightly-review"}
	for i, state := range t.States {
		marker := "  "
		class := "sg-state-row"
		if state.Name == "ready" {
			marker = "› "
			class += " sg-selected"
		}
		glyph := ""
		if state.Glyph != "" {
			glyph = `<span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-` + state.Color + `)">` + template.HTMLEscapeString(state.Glyph) + `</span>`
		}
		web.WriteString(`<div class="` + class + `">` + glyph + `<span class="sg-state-name">` + names[i] + `</span><span class="sg-state-label" style="color:var(--af-` + state.Color + `)">` + template.HTMLEscapeString(state.Label) + `</span></div>`)
		cell := state.Glyph
		if cell == "" {
			cell = " "
		}
		tui = append(tui, marker+cell+" "+names[i]+" · "+state.Label)
	}
	items[0].Web = template.HTML(web.String())
	items[0].TUI = strings.Join(tui, "\n")
	return items
}

func guidePage(root string, t Tokens) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(root, "design/style-guide.tmpl"))
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("guide").Funcs(template.FuncMap{
		"colorKeys": func() []string { return keys(t.Colors) },
		"valueKeys": func() []string { return keys(t.Values) },
		"themeLabel": func(mode string) string {
			if mode == "light" {
				return "Light"
			}
			return "Dark"
		},
		"tuiSample": func(sample string) template.HTML {
			lines := strings.Split(sample, "\n")
			for i, line := range lines {
				escaped := template.HTMLEscapeString(line)
				for _, state := range t.States {
					if state.Glyph != "" {
						escaped = strings.ReplaceAll(escaped, state.Glyph, `<span style="color:var(--af-`+state.Color+`)">`+state.Glyph+`</span>`)
					}
					escaped = strings.ReplaceAll(escaped, state.Label, `<span style="color:var(--af-`+state.Color+`)">`+template.HTMLEscapeString(state.Label)+`</span>`)
				}
				if strings.HasPrefix(line, "›") {
					escaped = `<span class="sg-selected">` + escaped + `</span>`
				}
				lines[i] = escaped
			}
			return template.HTML(strings.Join(lines, "\n"))
		},
	}).Parse(string(raw))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		Tokens     Tokens
		Themes     []string
		Components []component
	}{t, []string{"light", "dark"}, components(t)})
	return b.Bytes(), err
}
