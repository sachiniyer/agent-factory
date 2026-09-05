
# Interface style guide

This is the staged [interface design](interface-design.md), not a restyled application.
Specimens consume the generated CSS; the TUI specimens show the equivalent cell layout
and semantic colours in HTML, not a running Bubble Tea application. The Go theme emits
lipgloss adaptive colours and component styles for P5. Controls below are inert examples,
except for native disclosure menus. Both themes remain visible regardless of the docs theme.

The adjacent stills are the current product, preserved without recolouring. Web stills
come from the [demo recorder](../dev/demo-assets.md). The TUI has one recorder poster
and legacy Sessions/Tasks SVG stills, not a full theme/screen matrix. Missing captures are
explicitly labelled; P1/P5 must add those recorder beats before visual changes to those
screens can claim pixel regression coverage. Empty/error examples are specifications,
not fabricated recordings. Click a still to inspect the full screen.

## Tokens in both themes

Every colour, metric and glyph below is enumerated from `design/tokens.json`.
CSS names use the `--af-` prefix. The TUI column gives cells, rows or flags as specified
by the role; font size and pixels are not converted mechanically into terminal cells.
Text pairs are checked at 4.5:1 against canvas, surface, raised and selection backgrounds;
focus and control borders are checked at 3:1. Agent-owned ANSI output is outside that check.


<section class="sg-theme" data-af-theme="light">
<h3>Light tokens</h3>
<div class="sg-token-grid">
<div class="sg-swatch" style="border-left-color:var(--af-accent)"><strong>accent</strong><code>#2d6271</code><span>Primary action and selected navigation</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-archived)"><strong>archived</strong><code>#4c566a</code><span>Retained history; restore to resume</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-border)"><strong>border</strong><code>#657084</code><span>Control outlines; avoid framing every content row</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-canvas)"><strong>canvas</strong><code>#d8dee9</code><span>Page background; terminal default background</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-danger)"><strong>danger</strong><code>#883b43</code><span>Failure or destructive confirmation; never ordinary selection</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-dead)"><strong>dead</strong><code>#883b43</code><span>Process exited; inspect before restarting</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-focus)"><strong>focus</strong><code>#2d6271</code><span>Keyboard owner outline; also show a text cue</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-ink)"><strong>ink</strong><code>#2e3440</code><span>Names, body text and labels</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-limit-reached)"><strong>limit-reached</strong><code>#73436b</code><span>Usage limit; wait or choose an eligible account</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-lost)"><strong>lost</strong><code>#705014</code><span>Connection or process whereabouts unknown</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-muted)"><strong>muted</strong><code>#4c566a</code><span>Secondary information; never placeholder-only labels</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-on-accent)"><strong>on-accent</strong><code>#eceff4</code><span>Text on accent fill</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-raised)"><strong>raised</strong><code>#eceff4</code><span>Dialogs and menus</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-ready)"><strong>ready</strong><code>#405430</code><span>Ready for input; the only green liveness indicator</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-running)"><strong>running</strong><code>#4c566a</code><span>Running label only; no indicator</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-selection)"><strong>selection</strong><code>#c4d4de</code><span>Selected row background; pair with ink</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-surface)"><strong>surface</strong><code>#e3e7ef</code><span>Rail and header grouping</span></div>
</div>
<table><thead><tr><th>Metric</th><th>Web value</th><th>TUI</th><th>Role</th></tr></thead><tbody>
<tr><td>focus-width</td><td><code>2px</code></td><td>1</td><td>Web focus ring; one-cell TUI frame plus keyboard label</td></tr>
<tr><td>font-mono</td><td><code>ui-monospace, monospace</code></td><td>0</td><td>Code, paths, terminal samples; TUI inherits terminal font</td></tr>
<tr><td>font-ui</td><td><code>system-ui, sans-serif</code></td><td>0</td><td>Web labels; TUI inherits the user&#39;s terminal font</td></tr>
<tr><td>line-height</td><td><code>1.5</code></td><td>1</td><td>Web prose rhythm; terminal uses one row</td></tr>
<tr><td>motion-none</td><td><code>0ms</code></td><td>0</td><td>All indicators and reduced-motion transitions</td></tr>
<tr><td>motion-transition</td><td><code>120ms</code></td><td>0</td><td>Only user-caused web disclosure; TUI changes immediately</td></tr>
<tr><td>radius-control</td><td><code>4px</code></td><td>0</td><td>Inputs and buttons; square TUI border</td></tr>
<tr><td>radius-dialog</td><td><code>8px</code></td><td>1</td><td>Dialogs; rounded TUI border</td></tr>
<tr><td>radius-none</td><td><code>0px</code></td><td>0</td><td>Terminal chrome and structural dividers; square border</td></tr>
<tr><td>space-0</td><td><code>0px</code></td><td>0</td><td>No inset</td></tr>
<tr><td>space-1</td><td><code>4px</code></td><td>1</td><td>Glyph gap; one horizontal cell</td></tr>
<tr><td>space-2</td><td><code>8px</code></td><td>1</td><td>Related controls; one horizontal cell</td></tr>
<tr><td>space-3</td><td><code>12px</code></td><td>1</td><td>Compact row inset; one horizontal cell</td></tr>
<tr><td>space-4</td><td><code>16px</code></td><td>2</td><td>Panel inset; two horizontal cells</td></tr>
<tr><td>space-6</td><td><code>24px</code></td><td>1</td><td>Section gap; one vertical blank row</td></tr>
<tr><td>space-8</td><td><code>32px</code></td><td>2</td><td>Major section gap; two vertical blank rows</td></tr>
<tr><td>target-min</td><td><code>44px</code></td><td>1</td><td>Web touch target; one terminal row</td></tr>
<tr><td>type-body</td><td><code>0.875rem</code></td><td>1</td><td>14px controls and body; one terminal row</td></tr>
<tr><td>type-caption</td><td><code>0.75rem</code></td><td>1</td><td>12px metadata; one terminal row</td></tr>
<tr><td>type-display</td><td><code>1.25rem</code></td><td>1</td><td>20px dialog or empty-state title; bold TUI heading</td></tr>
<tr><td>type-title</td><td><code>1rem</code></td><td>1</td><td>16px section title; bold one-row TUI heading</td></tr>
<tr><td>weight-normal</td><td><code>400</code></td><td>0</td><td>Ordinary text; TUI bold false</td></tr>
<tr><td>weight-strong</td><td><code>600</code></td><td>1</td><td>Selected names and headings; TUI bold true</td></tr>
</tbody></table>
<div class="sg-bar"><span style="color:var(--af-running)"><span aria-hidden="true"></span> Running · No glyph<code>--af-glyph-running</code></span><span style="color:var(--af-ready)"><span aria-hidden="true">●</span> Ready<code>--af-glyph-ready</code></span><span style="color:var(--af-lost)"><span aria-hidden="true">◌</span> Lost<code>--af-glyph-lost</code></span><span style="color:var(--af-dead)"><span aria-hidden="true">○</span> Dead<code>--af-glyph-dead</code></span><span style="color:var(--af-archived)"><span aria-hidden="true">▧</span> Archived<code>--af-glyph-archived</code></span><span style="color:var(--af-limit-reached)"><span aria-hidden="true">◆</span> Limit reached<code>--af-glyph-limit-reached</code></span></div>
<p class="sg-caption">Caption · Last activity 2m ago</p><p class="sg-body">Body · Choose a session</p><p class="sg-title">Title · Sessions</p><p class="sg-display">Display · New session</p>
<div class="sg-bar"><span class="sg-square">Square frame</span><button type="button">Control radius</button><span class="sg-dialog">Dialog radius</span><span class="sg-focus">Keyboard focus</span></div>
<p>Spacing samples use the horizontal web grid; see the metric roles for terminal axes.</p>
<div class="sg-bar"><span>space-0 <i class="sg-space" style="width:var(--af-space-0)"></i></span><span>space-1 <i class="sg-space" style="width:var(--af-space-1)"></i></span><span>space-2 <i class="sg-space" style="width:var(--af-space-2)"></i></span><span>space-3 <i class="sg-space" style="width:var(--af-space-3)"></i></span><span>space-4 <i class="sg-space" style="width:var(--af-space-4)"></i></span><span>space-6 <i class="sg-space" style="width:var(--af-space-6)"></i></span><span>space-8 <i class="sg-space" style="width:var(--af-space-8)"></i></span></div>
<p>Motion · Static indicators · User-caused disclosure ≤120ms · Reduced motion 0ms · TUI 0ms</p>
</section>

<section class="sg-theme" data-af-theme="dark">
<h3>Dark tokens</h3>
<div class="sg-token-grid">
<div class="sg-swatch" style="border-left-color:var(--af-accent)"><strong>accent</strong><code>#90c4d3</code><span>Primary action and selected navigation</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-archived)"><strong>archived</strong><code>#d8dee9</code><span>Retained history; restore to resume</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-border)"><strong>border</strong><code>#a1aaba</code><span>Control outlines; avoid framing every content row</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-canvas)"><strong>canvas</strong><code>#2e3440</code><span>Page background; terminal default background</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-danger)"><strong>danger</strong><code>#e4c8cd</code><span>Failure or destructive confirmation; never ordinary selection</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-dead)"><strong>dead</strong><code>#e4c8cd</code><span>Process exited; inspect before restarting</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-focus)"><strong>focus</strong><code>#90c4d3</code><span>Keyboard owner outline; also show a text cue</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-ink)"><strong>ink</strong><code>#eceff4</code><span>Names, body text and labels</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-limit-reached)"><strong>limit-reached</strong><code>#dbb9d5</code><span>Usage limit; wait or choose an eligible account</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-lost)"><strong>lost</strong><code>#ebcb8b</code><span>Connection or process whereabouts unknown</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-muted)"><strong>muted</strong><code>#d8dee9</code><span>Secondary information; never placeholder-only labels</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-on-accent)"><strong>on-accent</strong><code>#2e3440</code><span>Text on accent fill</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-raised)"><strong>raised</strong><code>#434c5e</code><span>Dialogs and menus</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-ready)"><strong>ready</strong><code>#d5e2cc</code><span>Ready for input; the only green liveness indicator</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-running)"><strong>running</strong><code>#d8dee9</code><span>Running label only; no indicator</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-selection)"><strong>selection</strong><code>#3c505d</code><span>Selected row background; pair with ink</span></div>
<div class="sg-swatch" style="border-left-color:var(--af-surface)"><strong>surface</strong><code>#3b4252</code><span>Rail and header grouping</span></div>
</div>
<table><thead><tr><th>Metric</th><th>Web value</th><th>TUI</th><th>Role</th></tr></thead><tbody>
<tr><td>focus-width</td><td><code>2px</code></td><td>1</td><td>Web focus ring; one-cell TUI frame plus keyboard label</td></tr>
<tr><td>font-mono</td><td><code>ui-monospace, monospace</code></td><td>0</td><td>Code, paths, terminal samples; TUI inherits terminal font</td></tr>
<tr><td>font-ui</td><td><code>system-ui, sans-serif</code></td><td>0</td><td>Web labels; TUI inherits the user&#39;s terminal font</td></tr>
<tr><td>line-height</td><td><code>1.5</code></td><td>1</td><td>Web prose rhythm; terminal uses one row</td></tr>
<tr><td>motion-none</td><td><code>0ms</code></td><td>0</td><td>All indicators and reduced-motion transitions</td></tr>
<tr><td>motion-transition</td><td><code>120ms</code></td><td>0</td><td>Only user-caused web disclosure; TUI changes immediately</td></tr>
<tr><td>radius-control</td><td><code>4px</code></td><td>0</td><td>Inputs and buttons; square TUI border</td></tr>
<tr><td>radius-dialog</td><td><code>8px</code></td><td>1</td><td>Dialogs; rounded TUI border</td></tr>
<tr><td>radius-none</td><td><code>0px</code></td><td>0</td><td>Terminal chrome and structural dividers; square border</td></tr>
<tr><td>space-0</td><td><code>0px</code></td><td>0</td><td>No inset</td></tr>
<tr><td>space-1</td><td><code>4px</code></td><td>1</td><td>Glyph gap; one horizontal cell</td></tr>
<tr><td>space-2</td><td><code>8px</code></td><td>1</td><td>Related controls; one horizontal cell</td></tr>
<tr><td>space-3</td><td><code>12px</code></td><td>1</td><td>Compact row inset; one horizontal cell</td></tr>
<tr><td>space-4</td><td><code>16px</code></td><td>2</td><td>Panel inset; two horizontal cells</td></tr>
<tr><td>space-6</td><td><code>24px</code></td><td>1</td><td>Section gap; one vertical blank row</td></tr>
<tr><td>space-8</td><td><code>32px</code></td><td>2</td><td>Major section gap; two vertical blank rows</td></tr>
<tr><td>target-min</td><td><code>44px</code></td><td>1</td><td>Web touch target; one terminal row</td></tr>
<tr><td>type-body</td><td><code>0.875rem</code></td><td>1</td><td>14px controls and body; one terminal row</td></tr>
<tr><td>type-caption</td><td><code>0.75rem</code></td><td>1</td><td>12px metadata; one terminal row</td></tr>
<tr><td>type-display</td><td><code>1.25rem</code></td><td>1</td><td>20px dialog or empty-state title; bold TUI heading</td></tr>
<tr><td>type-title</td><td><code>1rem</code></td><td>1</td><td>16px section title; bold one-row TUI heading</td></tr>
<tr><td>weight-normal</td><td><code>400</code></td><td>0</td><td>Ordinary text; TUI bold false</td></tr>
<tr><td>weight-strong</td><td><code>600</code></td><td>1</td><td>Selected names and headings; TUI bold true</td></tr>
</tbody></table>
<div class="sg-bar"><span style="color:var(--af-running)"><span aria-hidden="true"></span> Running · No glyph<code>--af-glyph-running</code></span><span style="color:var(--af-ready)"><span aria-hidden="true">●</span> Ready<code>--af-glyph-ready</code></span><span style="color:var(--af-lost)"><span aria-hidden="true">◌</span> Lost<code>--af-glyph-lost</code></span><span style="color:var(--af-dead)"><span aria-hidden="true">○</span> Dead<code>--af-glyph-dead</code></span><span style="color:var(--af-archived)"><span aria-hidden="true">▧</span> Archived<code>--af-glyph-archived</code></span><span style="color:var(--af-limit-reached)"><span aria-hidden="true">◆</span> Limit reached<code>--af-glyph-limit-reached</code></span></div>
<p class="sg-caption">Caption · Last activity 2m ago</p><p class="sg-body">Body · Choose a session</p><p class="sg-title">Title · Sessions</p><p class="sg-display">Display · New session</p>
<div class="sg-bar"><span class="sg-square">Square frame</span><button type="button">Control radius</button><span class="sg-dialog">Dialog radius</span><span class="sg-focus">Keyboard focus</span></div>
<p>Spacing samples use the horizontal web grid; see the metric roles for terminal axes.</p>
<div class="sg-bar"><span>space-0 <i class="sg-space" style="width:var(--af-space-0)"></i></span><span>space-1 <i class="sg-space" style="width:var(--af-space-1)"></i></span><span>space-2 <i class="sg-space" style="width:var(--af-space-2)"></i></span><span>space-3 <i class="sg-space" style="width:var(--af-space-3)"></i></span><span>space-4 <i class="sg-space" style="width:var(--af-space-4)"></i></span><span>space-6 <i class="sg-space" style="width:var(--af-space-6)"></i></span><span>space-8 <i class="sg-space" style="width:var(--af-space-8)"></i></span></div>
<p>Motion · Static indicators · User-caused disclosure ≤120ms · Reduced motion 0ms · TUI 0ms</p>
</section>


## Component inventory

Each component appears on both surfaces in both themes. Behaviour and cuts are defined
in the [component inventory](interface-design.md#component-inventory); these compact
specimens establish hierarchy, not final screen geometry or interaction tests.


<h3>Rail</h3>
<p>Name first, one state label second. Selected row uses selection fill; focus has an outline and keyboard cue. Keep names reachable by search and keyboard; disclose branch and churn details on selection.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-selected"><strong><span class="sg-ready">●</span> add-json-export</strong><br><small>Ready · Review changes</small></div><div>fix-empty-add<br><small>Running</small></div></section>
<figure><a href="../../assets/web/dashboard.png"><img loading="lazy" src="../../assets/web/dashboard.png" alt="Current web dashboard screen in light theme"></a><figcaption>Current recorder still · dashboard · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Sessions · 3
<span class="sg-selected">› <span style="color:var(--af-ready)">●</span> add-json-export · Ready</span>
    fix-empty-add · Running</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Rail evidence"></a><figcaption>Recorder poster: sessions rail. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-selected"><strong><span class="sg-ready">●</span> add-json-export</strong><br><small>Ready · Review changes</small></div><div>fix-empty-add<br><small>Running</small></div></section>
<figure><a href="../../assets/web/dashboard-dark.png"><img loading="lazy" src="../../assets/web/dashboard-dark.png" alt="Current web dashboard screen in dark theme"></a><figcaption>Current recorder still · dashboard · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Sessions · 3
<span class="sg-selected">› <span style="color:var(--af-ready)">●</span> add-json-export · Ready</span>
    fix-empty-add · Running</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Rail evidence"></a><figcaption>Recorder poster: sessions rail. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<h3>Header</h3>
<p>Project and active view provide context. Keep one primary create action. Connection status is separate from session liveness and always static.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-bar"><strong>todo-cli</strong><span class="sg-selected">Sessions</span><span>Tasks</span><span>Config</span><span>Connected</span></div></section>
<figure><a href="../../assets/web/dashboard.png"><img loading="lazy" src="../../assets/web/dashboard.png" alt="Current web dashboard screen in light theme"></a><figcaption>Current recorder still · dashboard · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>todo-cli · Sessions · Connected
Keyboard: navigation</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Header evidence"></a><figcaption>Recorder poster: rail header and project context. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-bar"><strong>todo-cli</strong><span class="sg-selected">Sessions</span><span>Tasks</span><span>Config</span><span>Connected</span></div></section>
<figure><a href="../../assets/web/dashboard-dark.png"><img loading="lazy" src="../../assets/web/dashboard-dark.png" alt="Current web dashboard screen in dark theme"></a><figcaption>Current recorder still · dashboard · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>todo-cli · Sessions · Connected
Keyboard: navigation</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Header evidence"></a><figcaption>Recorder poster: rail header and project context. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<h3>Terminal chrome</h3>
<p>Content owns the area. One title line names session, tab and keyboard owner. A focus outline does not imply ready or success. Preserve agent ANSI output and ctrl&#43;] exit.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-focus"><strong>tidy-tests · Agent · Keyboard</strong><pre>$ ./test.sh
2 tests passed</pre><small>ctrl+] · Return to sessions</small></div></section>
<figure><a href="../../assets/web/agent-tab.png"><img loading="lazy" src="../../assets/web/agent-tab.png" alt="Current web agent-tab screen in light theme"></a><figcaption>Current recorder still · agent-tab · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>┌ tidy-tests · Agent · Keyboard ┐
│ $ ./test.sh                  │
│ 2 tests passed               │
└ ctrl+] · Return to sessions ─┘</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Terminal chrome evidence"></a><figcaption>Recorder poster: preview frame; agent output is not recoloured by this spec. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-focus"><strong>tidy-tests · Agent · Keyboard</strong><pre>$ ./test.sh
2 tests passed</pre><small>ctrl+] · Return to sessions</small></div></section>
<figure><a href="../../assets/web/agent-tab-dark.png"><img loading="lazy" src="../../assets/web/agent-tab-dark.png" alt="Current web agent-tab screen in dark theme"></a><figcaption>Current recorder still · agent-tab · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>┌ tidy-tests · Agent · Keyboard ┐
│ $ ./test.sh                  │
│ 2 tests passed               │
└ ctrl+] · Return to sessions ─┘</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Terminal chrome evidence"></a><figcaption>Recorder poster: preview frame; agent output is not recoloured by this spec. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<h3>Tabs and review</h3>
<p>Use label and underline for the active web tab; retain tab numbering and a selected tree row in the TUI. PR link stays beside the review context. Closing and splitting remain keyboard reachable.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-bar"><span>Agent</span><strong class="sg-active">diff</strong><span>PR #128 · Open</span></div><pre>$ git diff --stat
2 files changed</pre></section>
<figure><a href="../../assets/web/review.png"><img loading="lazy" src="../../assets/web/review.png" alt="Current web review screen in light theme"></a><figcaption>Current recorder still · review · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>  1 · Agent
<span class="sg-selected">› 2 · diff · PR #128</span>
  2 files changed</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Tabs and review evidence"></a><figcaption>Recorder poster: selected child tab and review content. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-bar"><span>Agent</span><strong class="sg-active">diff</strong><span>PR #128 · Open</span></div><pre>$ git diff --stat
2 files changed</pre></section>
<figure><a href="../../assets/web/review-dark.png"><img loading="lazy" src="../../assets/web/review-dark.png" alt="Current web review screen in dark theme"></a><figcaption>Current recorder still · review · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>  1 · Agent
<span class="sg-selected">› 2 · diff · PR #128</span>
  2 files changed</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Tabs and review evidence"></a><figcaption>Recorder poster: selected child tab and review content. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<h3>Dialogs and overlays</h3>
<p>One title, labelled fields, inline error and one primary action. Busy copy is static. Preserve entered text after failure, trap web focus and return it on close. TUI pickers share the same frame and esc return.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-dialog"><strong>New session</strong><label>Title<input value="tidy-tests" readonly></label><label>Prompt<textarea readonly>Cover appending a second item…</textarea></label><div class="sg-bar"><button type="button">Cancel</button><button type="button" class="sg-primary">Create</button></div></div></section>
<figure><a href="../../assets/web/new-session.png"><img loading="lazy" src="../../assets/web/new-session.png" alt="Current web new-session screen in light theme"></a><figcaption>Current recorder still · new-session · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>╭ New session ───────────────╮
│ Title: tidy-tests          │
│ Prompt: Cover appending…   │
│ enter create · esc cancel  │
╰───────────────────────────╯</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><figcaption>No recorder still of a TUI creation overlay is committed; web still documents the corresponding workflow.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-dialog"><strong>New session</strong><label>Title<input value="tidy-tests" readonly></label><label>Prompt<textarea readonly>Cover appending a second item…</textarea></label><div class="sg-bar"><button type="button">Cancel</button><button type="button" class="sg-primary">Create</button></div></div></section>
<figure><a href="../../assets/web/new-session-dark.png"><img loading="lazy" src="../../assets/web/new-session-dark.png" alt="Current web new-session screen in dark theme"></a><figcaption>Current recorder still · new-session · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>╭ New session ───────────────╮
│ Title: tidy-tests          │
│ Prompt: Cover appending…   │
│ enter create · esc cancel  │
╰───────────────────────────╯</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><figcaption>No recorder still of a TUI creation overlay is committed; web still documents the corresponding workflow.</figcaption></figure>
</div>

<h3>Tasks</h3>
<p>Task name, enabled state and next occurrence lead; full trigger and delivery details expand on selection. Errors stay visible. Edit is primary; destructive actions belong in the selected task&#39;s actions.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-selected"><strong>nightly-tests · Enabled</strong><br><small>Next run · Tomorrow at 12:00 UTC</small><br><button type="button">Edit task</button></div></section>
<figure><a href="../../assets/web/tasks.png"><img loading="lazy" src="../../assets/web/tasks.png" alt="Current web tasks screen in light theme"></a><figcaption>Current recorder still · tasks · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Tasks
<span class="sg-selected">› nightly-tests · Enabled</span>
  Next run · Tomorrow at 12:00 UTC
  enter edit · r run now · esc back</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/tui-tasks.svg"><img loading="lazy" src="../../assets/tui/tui-tasks.svg" alt="Current TUI Tasks evidence"></a><figcaption>Existing Tasks SVG still; not a light/dark recorder pair. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-selected"><strong>nightly-tests · Enabled</strong><br><small>Next run · Tomorrow at 12:00 UTC</small><br><button type="button">Edit task</button></div></section>
<figure><a href="../../assets/web/tasks-dark.png"><img loading="lazy" src="../../assets/web/tasks-dark.png" alt="Current web tasks screen in dark theme"></a><figcaption>Current recorder still · tasks · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Tasks
<span class="sg-selected">› nightly-tests · Enabled</span>
  Next run · Tomorrow at 12:00 UTC
  enter edit · r run now · esc back</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/tui-tasks.svg"><img loading="lazy" src="../../assets/tui/tui-tasks.svg" alt="Current TUI Tasks evidence"></a><figcaption>Existing Tasks SVG still; not a light/dark recorder pair. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<h3>Config and accounts</h3>
<p>Key, purpose and value form one group. Save feedback stays with that key. Accounts have their own section; login state is text, not a second session-liveness dot. Show daemon host before a truncatable path.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><strong>Config · Local daemon</strong><label>Editor binary<small>Program used by editor tabs</small><input value="code-server" readonly></label><small>Saved · Applies to new tabs</small><hr><strong>Accounts</strong><p>work · Logged in</p><button type="button">Add account</button></section>
<figure><a href="../../assets/web/config-accounts.png"><img loading="lazy" src="../../assets/web/config-accounts.png" alt="Current web config-accounts screen in light theme"></a><figcaption>Current recorder still · config-accounts · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Config · Local daemon
<span class="sg-selected">› Editor binary: code-server</span>
  Saved · Applies to new tabs
Accounts
  work · Logged in</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><figcaption>No recorder still of TUI Config is committed; inspect ui/config_pane.go and ui/config_pane_accounts.go.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><strong>Config · Local daemon</strong><label>Editor binary<small>Program used by editor tabs</small><input value="code-server" readonly></label><small>Saved · Applies to new tabs</small><hr><strong>Accounts</strong><p>work · Logged in</p><button type="button">Add account</button></section>
<figure><a href="../../assets/web/config-accounts-dark.png"><img loading="lazy" src="../../assets/web/config-accounts-dark.png" alt="Current web config-accounts screen in dark theme"></a><figcaption>Current recorder still · config-accounts · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Config · Local daemon
<span class="sg-selected">› Editor binary: code-server</span>
  Saved · Applies to new tabs
Accounts
  work · Logged in</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><figcaption>No recorder still of TUI Config is committed; inspect ui/config_pane.go and ui/config_pane_accounts.go.</figcaption></figure>
</div>

<h3>Buttons, fields and menus</h3>
<p>A visible label survives placeholder removal. Primary, secondary, disabled and destructive states share geometry. Hover is optional; focus is visible. A disclosure offers project, filter or tab actions without a permanent toolbar.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><label>Title<input class="sg-focus" value="tidy-tests" readonly></label><div class="sg-bar"><button class="sg-primary" type="button">Create</button><button type="button">Cancel</button><button disabled>Creating…</button><button class="sg-danger" type="button">Delete session</button></div><details><summary>Project · todo-cli</summary><p>todo-cli · Selected</p><p>Register project…</p></details></section>
<figure><a href="../../assets/web/new-session.png"><img loading="lazy" src="../../assets/web/new-session.png" alt="Current web new-session screen in light theme"></a><figcaption>Current recorder still · new-session · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Project
<span class="sg-selected">› todo-cli · Selected</span>
  Register project…
enter select · esc cancel
Creating… · Please wait</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><figcaption>No recorder still of TUI picker or menu states is committed.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><label>Title<input class="sg-focus" value="tidy-tests" readonly></label><div class="sg-bar"><button class="sg-primary" type="button">Create</button><button type="button">Cancel</button><button disabled>Creating…</button><button class="sg-danger" type="button">Delete session</button></div><details><summary>Project · todo-cli</summary><p>todo-cli · Selected</p><p>Register project…</p></details></section>
<figure><a href="../../assets/web/new-session-dark.png"><img loading="lazy" src="../../assets/web/new-session-dark.png" alt="Current web new-session screen in dark theme"></a><figcaption>Current recorder still · new-session · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Project
<span class="sg-selected">› todo-cli · Selected</span>
  Register project…
enter select · esc cancel
Creating… · Please wait</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><figcaption>No recorder still of TUI picker or menu states is committed.</figcaption></figure>
</div>

<h3>Empty states</h3>
<p>Name what is absent and one next action. Distinguish zero sessions from no project. Keep navigation available. Empty sections do not reserve permanent rows in the TUI.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><strong>No sessions yet</strong><p>Create a session in todo-cli.</p><button class="sg-primary" type="button">New session</button><hr><strong>No project selected</strong><p>Choose a project to see its sessions.</p><button type="button">Choose project</button></section>
<figure><a href="../../assets/web/dashboard.png"><img loading="lazy" src="../../assets/web/dashboard.png" alt="Current web dashboard screen in light theme"></a><figcaption>Current recorder still · dashboard · light · Context only; this state has no committed recording</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>No sessions yet
Press n to create a session.

No project selected
Choose a project to continue.</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Empty states evidence"></a><figcaption>Poster shows empty Automations and the surrounding workspace; no zero-session recorder still is committed. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><strong>No sessions yet</strong><p>Create a session in todo-cli.</p><button class="sg-primary" type="button">New session</button><hr><strong>No project selected</strong><p>Choose a project to see its sessions.</p><button type="button">Choose project</button></section>
<figure><a href="../../assets/web/dashboard-dark.png"><img loading="lazy" src="../../assets/web/dashboard-dark.png" alt="Current web dashboard screen in dark theme"></a><figcaption>Current recorder still · dashboard · dark · Context only; this state has no committed recording</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>No sessions yet
Press n to create a session.

No project selected
Choose a project to continue.</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Empty states evidence"></a><figcaption>Poster shows empty Automations and the surrounding workspace; no zero-session recorder still is committed. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<h3>Errors and notices</h3>
<p>Cause, consequence and one next action. Daemon unavailable and expired login must not masquerade as an empty list. Wrap actionable error text; retain recoverable input. Details are a disclosure, never the only explanation.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-danger"><strong>Cannot reach the daemon</strong><p>Sessions could not be loaded. Check the daemon, then retry.</p></div><button type="button">Retry</button><hr><strong>Login expired</strong><p>Sign in again to reconnect.</p><button type="button">Sign in</button></section>
<figure><a href="../../assets/web/config-accounts.png"><img loading="lazy" src="../../assets/web/config-accounts.png" alt="Current web config-accounts screen in light theme"></a><figcaption>Current recorder still · config-accounts · light · Context only; this state has no committed recording</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Cannot reach the daemon
Sessions could not be loaded.
Check the daemon, then retry.

Login expired · Sign in again</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><figcaption>No recorder still of TUI errors is committed; inspect ui/err.go and app/home_view.go.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-danger"><strong>Cannot reach the daemon</strong><p>Sessions could not be loaded. Check the daemon, then retry.</p></div><button type="button">Retry</button><hr><strong>Login expired</strong><p>Sign in again to reconnect.</p><button type="button">Sign in</button></section>
<figure><a href="../../assets/web/config-accounts-dark.png"><img loading="lazy" src="../../assets/web/config-accounts-dark.png" alt="Current web config-accounts screen in dark theme"></a><figcaption>Current recorder still · config-accounts · dark · Context only; this state has no committed recording</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Cannot reach the daemon
Sessions could not be loaded.
Check the daemon, then retry.

Login expired · Sign in again</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><figcaption>No recorder still of TUI errors is committed; inspect ui/err.go and app/home_view.go.</figcaption></figure>
</div>

<h3>Help and status bar</h3>
<p>Show only shortcuts valid for the current keyboard owner; full help remains discoverable. Narrow layouts keep the exit route first. No blinking cursor or progress animation in af chrome.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-bar"><kbd>ctrl+]</kbd><span>Return to sessions</span><kbd>?</kbd><span>Help</span></div><p>Connecting…</p></section>
<figure><a href="../../assets/web/agent-tab.png"><img loading="lazy" src="../../assets/web/agent-tab.png" alt="Current web agent-tab screen in light theme"></a><figcaption>Current recorder still · agent-tab · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>ctrl+] return · ? help
Connecting…</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Help and status bar evidence"></a><figcaption>Recorder poster: bottom shortcut strip. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-bar"><kbd>ctrl+]</kbd><span>Return to sessions</span><kbd>?</kbd><span>Help</span></div><p>Connecting…</p></section>
<figure><a href="../../assets/web/agent-tab-dark.png"><img loading="lazy" src="../../assets/web/agent-tab-dark.png" alt="Current web agent-tab screen in dark theme"></a><figcaption>Current recorder still · agent-tab · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>ctrl+] return · ? help
Connecting…</pre><div class="sg-bar"><span style="color:var(--af-running)"> Running</span><span style="color:var(--af-ready)">● Ready</span><span style="color:var(--af-lost)">◌ Lost</span><span style="color:var(--af-dead)">○ Dead</span><span style="color:var(--af-archived)">▧ Archived</span><span style="color:var(--af-limit-reached)">◆ Limit reached</span></div></section>
<figure><a href="../../assets/tui/demo-poster.png"><img loading="lazy" src="../../assets/tui/demo-poster.png" alt="Current TUI Help and status bar evidence"></a><figcaption>Recorder poster: bottom shortcut strip. Original colours preserved beside both theme specimens.</figcaption></figure>
</div>


## Capture coverage

The [existing Sessions SVG](../assets/tui/tui-sessions.svg) supplements the recorder
poster and shows an older palette. The [Tasks SVG](../assets/tui/tui-tasks.svg) supplies
Tasks context. Neither establishes light/dark recorder coverage. P1 owns the baseline
matrix; P2/P5 own applying tokens and matching new stills; P6 refreshes the public tour.

## Regeneration

Run `make docs`, then `go run ./scripts/gen-design --check` and `mkdocs build --strict`.
The docs drift gate checks the web CSS, Go theme, docs CSS copy and this page together.
Edit the token source and specimen sources, never these generated outputs.
