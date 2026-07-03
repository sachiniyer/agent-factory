# Spike report: embedded interactive terminal panes (#1089)

**Branch:** `spike/1089-embedded-terminal` · **Demo:** `spike/main.go` (~430 LOC, standalone) · **Date:** 2026-07-02

**TL;DR — KEEP architecture A.** A PTY running `tmux attach` piped through
`github.com/charmbracelet/x/vt` renders vim, less, htop, and fast streaming
output correctly inside a fixed lipgloss rectangle, with the instances rail
visible the whole time. Interactive fidelity is high (arrows, search, insert
mode, CJK/emoji, bracketed paste, Ctrl-C, F-keys). Sustained streaming costs
~0.6% of one core at a capped ~62 fps. Ctrl-] detaches cleanly and the tmux
session survives; re-attach repaints current state. The genuinely fiddly parts
are input edge cases (modified arrows, mouse) and the reserved-key design —
not rendering, which is the part we feared.

---

## 0. What was built

Single-file bubbletea program (`spike/main.go`): left rail (fake instance
list, always visible) + bordered terminal pane + status bar with live perf
counters. The pane:

1. spawns `tmux -L spike1089 attach-session -t work` on a PTY
   (`creack/pty` — already a direct dep of this repo);
2. feeds PTY output into a `vt.SafeEmulator` cell grid;
3. copies the emulator's *read side* back into the PTY — this carries both
   the emulator's replies to terminal queries (DA, DSR, …) **and** the
   encoded keystrokes injected via `SendKey`;
4. renders the grid to an ANSI string each frame (`CellAt` + `Style.Diff`,
   ~40 LOC, same technique as `vt.Render()` but with a reverse-video cursor
   overlay) and places it inside a lipgloss border in `View()`;
5. translates focused `tea.KeyMsg` → `uv.KeyPressEvent` → `emulator.SendKey`,
   so the emulator encodes keys **according to the terminal's current modes**
   (application cursor keys, bracketed paste) instead of us hand-writing
   escape sequences;
6. propagates pane geometry changes via `pty.Setsize` + `emulator.Resize`;
   tmux reflows on the SIGWINCH.

Reserved host keys per the spike spec: `Ctrl-]` = detach, `Tab` = focus
switch. Everything else forwards when the pane is focused.

All testing ran on isolated tmux servers (`-L spike1089` for the inner
sessions, `-L spike1089outer` to host the demo headlessly and drive it with
`send-keys` / `capture-pane`). No contact with the default server, the real
daemon, or `~/.config/agent-factory`. Both servers torn down after.

## 1. Does it work?

**Yes. Every program tested rendered and interacted correctly. Nothing broke.**

| Program / behavior | Result |
|---|---|
| zsh prompt (powerline glyphs, emoji `👋🌍`) | ✅ renders, wraps at pane width |
| ANSI colors (16-color SGR through full chain) | ✅ verified escape-for-escape in `capture-pane -e` |
| **vim**: alt-screen entry/exit, insert mode, `:wq` | ✅ file written with exact content; shell screen restored on exit |
| vim with CJK + emoji (`日本語テスト 🚀✨`) | ✅ wide chars render, ruler column tracking correct |
| **less -N** on a 50,000-line file | ✅ `Down`(×3) 1→4, `PgDn` →35, `G` →49970, `?search` →25000, `q` clean |
| **htop**: meters, live refresh, process table, `q` | ✅ paints and updates correctly in-pane |
| **yes** fast-streaming for 10 s | ✅ no tearing, no lag, UI stayed responsive |
| Ctrl-C forwarded into pane | ✅ killed `cat`/`yes`/ticker inside session |
| Bracketed paste (`paste-buffer -p` → demo → shell) | ✅ pasted text lands intact at prompt |
| F1 / Home encodings (verified with `cat -v`) | ✅ `^[OP`, `^[[1~` |
| **Ctrl-] detach → re-attach** | ✅ ticker ran 3→10 across the gap; session alive; repaint on re-attach |
| **Resize** 120×36 → 100×28 outer | ✅ pane 91×33→71×25; inner `tput cols/lines` = 71/24; prompt rewrapped |

Evidence frames (plain-text `capture-pane`; rail on the left is the host TUI,
box content is the live embedded tmux client):

vim, alt-screen, unicode, inside the pane:

```
  INSTANCES                ╭──────────────────────────────────────────────────────────────────────────────╮
                           │Hello from embedded vim! CJK: 日本語テスト emoji: 🚀✨                         │
 > api-refactor            │second line for cursor test                                                   │
   fix-flaky-tests         │~                                                                             │
   docs-sweep              │~                                                                             │
                           │-- INSERT --                                                     2,28     All │
```

htop live, rail still visible, perf counters in the host status bar:

```
  INSTANCES                ╭──────────────────────────────────────────────────────────────────────────────╮
                           │    0[||||||     30.8%]   4[|||||      21.6%]   8[||||||||||100.0%]           │
 > api-refactor            │    1[||||||||||100.0%]   5[||||||||||100.0%]   9[||||||||||100.0%]           │
   fix-flaky-tests         │  Mem[|||||||||||||||||||||||26.5G/126G] Tasks: 390, 1845 thr; 12 running     │
   docs-sweep              │  Swp[                          0K/0K] Load average: 10.11 10.35 11.29        │
  tab    focus term        │    PID USER       PRI  NI  VIRT   RES   SHR S  CPU%▽MEM%   TIME+  Command    │
  ctrl-] detach            │1059095 siyer       20   0 11944  7944  3732 R  93.5  0.0  0:00.75 htop       │
                           ╰──────────────────────────────────────────────────────────────────────────────╯
 focus=TERM  attached  in=11KB reads=141 frames=865  pane=91x33
```

less mid-file after backward search, detach/re-attach ticker, and resize
`tput` outputs are in the git history of this branch's test run; the three
one-line proofs:

```
--- after ?line 025000 ---      │  25000 line 025000 of the large test file │
--- after Ctrl-]  ---           focus=rail  detached (tmux session still running) — Enter to re-attach
--- after re-attach (~8s gap)   tick 3  →  tick 10       (ticker never stopped server-side)
```

## 2. Is it fast enough?

**Yes, with margin to spare.** 10 s of `yes 'fast token stream …'` inside the
embedded pane, measured via `/proc/<pid>/stat` deltas on the demo process:

- **CPU: 6 ticks over ~10.5 s ≈ 0.6% of one core** — on a box with load
  average ~10 (other tenants pinning cores), so this is a pessimistic
  environment, not a quiet-lab number.
- **Frame rate: 627 frames / 10 s ≈ 62 fps**, exactly at the demo's
  coalescing cap (~66 fps: signal channel of capacity 1 + 15 ms sleep-drain).
- **Ingest: only 436 KB reached the emulator in 10 s.** This is the key
  structural insight: **tmux is a natural flow-limiter.** The attach client
  receives screen *redraws*, not the raw firehose — `yes` produces tens of
  MB/s but tmux only ships what the visible pane looks like. Architecture A
  inherits tmux's own throttling for free.
- No tearing in any capture; keystroke→paint latency was consistently inside
  the 200–400 ms polling sleeps of the harness (subjectively immediate; not
  precision-measured).

A production TUI with 5–10 *attached* panes would multiply this, but even
10× 0.6% is nothing, and the obvious production design (only the
focused/visible pane holds a live attachment) makes it moot.

## 3. The genuinely hard / fiddly parts

Rendering — the part the RFC flagged as scary — turned out to be the easy
part. The real friction:

1. **Dependency ripple (one-time, resolved).** `charmbracelet/x/vt` is
   **untagged** (pseudo-version `v0.0.0-20260629…`) and lives in charm's
   v2/ultraviolet ecosystem. Adding it forced `x/ansi` v0.10.1→v0.11.7, which
   broke the build until `x/cellbuf` was bumped to v0.0.15, and dragged
   `go-runewidth` v0.0.16→v0.0.23 (East Asian width table changes) plus
   `ultraviolet` as a new indirect dep. **After the bumps: `go build ./...`
   clean and `go test` green across `ui`, `app`, `session`, `config`,
   `task`.** Risk to carry into production: pin the pseudo-version, and eyeball
   wide-char-adjacent UI tests since runewidth moved seven minor versions.
2. **Input fidelity has a long tail.** The happy 90% (runes, arrows, Enter,
   Esc, Ctrl+letter, F-keys, paste) is a ~100-LOC mapping table and works.
   Found gaps:
   - **Modified arrows (Ctrl-Up) never arrived** in the harness — swallowed
     somewhere in outer-tmux → bubbletea v1 → my mapping. Needs a
     real-terminal test; bubbletea v1's key model is lossy for some
     modifier combos, which may argue for waiting on/moving to bubbletea v2's
     key handling for the production build.
   - **Mouse is unimplemented** (emulator has `SendMouse`; wiring
     `tea.MouseMsg` through is straightforward but tmux mouse mode + host
     focus semantics need design: who owns the wheel — host scrollback or the
     inner app?).
   - The emulator's mode-aware `SendKey` encoding (DECCKM etc.) is a big
     win — we never hand-encode escape sequences — but it means input
     correctness is only as good as x/vt's encoder. It passed everything
     tested.
3. **Reserved-key design is a product decision, not a technical one.**
   The spec'd `Tab` = host-focus-switch is **wrong for production**: shells,
   vim, and every agent CLI need Tab (completion!). `Ctrl-]` for detach is
   good (already our `DetachKeyByte`). Recommend: forward *everything*
   including Tab while the pane is focused, and reserve only `Ctrl-]`
   (and maybe a prefix chord) as the escape hatch.
4. **Perf throttling is necessary but trivial.** Naive repaint-per-PTY-read
   would redraw hundreds of times/s under streaming. A 1-slot signal channel
   + 15 ms drain (12 lines of code) caps it at ~60 fps with zero perceptible
   lag. Also render only rows the emulator touched if we ever care
   (`Emulator.Touched()` exists); at 0.6% CPU we don't.
5. **Resize works but has ordering subtleties.** PTY winsize and
   `emulator.Resize` must both happen (PTY→tmux reflow, emulator→grid);
   during the ~100 ms reflow window the grid briefly shows stale wrap. Fine in
   practice. Production must also decide the multi-client rule — tmux resizes
   a session to the smallest attached client, so an external `tmux attach` to
   the same session while a pane holds it will shrink both (same issue the
   full-screen attach has today).
6. **Cursor overlay + minor unknowns.** `vt.Render()` doesn't draw the
   cursor, so the demo re-implements the row loop (~40 LOC) to reverse-video
   the cursor cell. `IsAltScreen()` never reported true even with vim open —
   suspect the tmux client redraw path doesn't use mode 1049 the way I
   assumed; cosmetic here, but don't build "agent is in a TUI" detection on it
   without checking. htop's F5 tree-toggle was inconclusive in captures
   (F1's `^[OP` encoding verified raw, so F-keys do transmit).

## 4. Scope of the real production build

Rough shape if we proceed (estimates, not commitments):

- **New package `ui/termpane/`** (~500–700 LOC + tests):
  - `attachment.go` — PTY + emulator lifecycle (spawn, read pump, response
    pump, resize, kill); ~150 LOC, mostly transplantable from the spike.
  - `keys.go` — full tea→uv key translation incl. mouse; ~150–200 LOC.
  - `render.go` — grid→ANSI with cursor overlay, focused/unfocused styling;
    ~80 LOC.
  - `model.go` — bubbletea glue, focus state, throttle; ~120 LOC.
- **`session/tmux/` hooks — small.** `tmux.go` already runs
  `tmux attach-session` on a PTY via `ptyFactory` (`tmux.go:350`) with
  detach/kill plumbing (`killAttach`, `DetachKeyByte`). Production adds an
  attach mode that hands the ptmx to `termpane` instead of the current
  stdin/stdout copy + `monitorWindowSize` takeover (`tmux_unix.go`). Maybe
  ~50–100 LOC changed; the session-lifecycle machinery is untouched — the
  daemon still owns sessions, this is purely a TUI-side projection change,
  consistent with the #960 single-writer split.
- **`app/` wiring** — replace the full-screen attach state with an
  embedded-pane state in the #1024 layout; the two-pane split from epic PR 5
  is the natural place. Size depends on that code, guess ~200–300 LOC.
- **Deps:** `charmbracelet/x/vt` (+`ultraviolet` indirect), and the
  `x/ansi`/`x/cellbuf`/`go-runewidth` bumps described above. All green on
  build + tests already, on this branch.
- **Policy decisions needed:** attach-only-the-focused-pane vs all visible;
  mouse ownership; final reserved-key set; scrollback UX (tmux copy-mode
  forwarded, vs host-side using `emulator.Scrollback()`).

Total: **2–4 focused PRs, on the order of 1–1.5k LOC including tests.** The
risky unknown is not rendering (proven here) — it's input-edge-case QA across
real agent CLIs (Claude Code, Aider are themselves TUIs) and terminal
diversity.

## 5. Recommendation: KEEP architecture A. Don't build B.

**KEEP.** Architecture A works end-to-end on our exact stack (bubbletea
v1.3.5 + lipgloss v1 + tmux), performs far better than needed, and reuses
tmux for the two hard problems (persistence, flow-limiting). The demo is
~430 LOC and took one session to build and validate — the production version
is a normal-sized feature, not a moonshot.

**Architecture B (`tmux -CC` control mode) — don't.** Control mode is
designed for a client that *replaces* the entire tmux UI (iTerm2): you get
`%output` events per pane and must still run a VT emulator per pane AND
reimplement layout/attach semantics yourself. For "one live embedded pane
with the rail visible," it's strictly more work than A for no fidelity gain.
The one scenario where B (or one control-mode client feeding N emulators)
becomes interesting is *many simultaneously-live panes without N attach
clients* — file that as a future optimization; at 0.6% CPU per attachment
there's no pressure.

---

*Spike artifacts: `spike/main.go`, `spike/README.md` on branch
`spike/1089-embedded-terminal`. Not for merge. All testing on isolated tmux
servers (`-L spike1089*`), torn down after; production daemon and default
tmux server untouched.*
