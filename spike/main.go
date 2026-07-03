// Command spike-embedded-terminal is a throwaway proof-of-concept for issue
// #1089: render a LIVE, INTERACTIVE tmux session inside a non-fullscreen
// bubbletea/lipgloss pane while a left "instances rail" stays visible.
//
// Architecture A: PTY running `tmux -L <socket> attach -t <session>` →
// charmbracelet/x/vt emulator (cell grid) → ANSI string rendered inside a
// fixed lipgloss rectangle. Focused keystrokes are translated to vt key
// events; the emulator encodes them (honouring DECCKM etc.) and they flow
// back down the PTY. Ctrl-] detaches (tmux session stays alive), Tab
// switches host focus between the rail and the terminal pane.
//
// This is spike code: single file, no tests, not wired into the real TUI.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

var (
	socketName  = flag.String("socket", "spike1089", "tmux -L socket name (isolated server)")
	sessionName = flag.String("session", "work", "tmux session to attach to")
	railWidth   = flag.Int("rail", 26, "width of the fake instances rail")
)

const detachKey = tea.KeyCtrlCloseBracket // Ctrl-]

// ---- messages ----

type outputMsg struct{}    // PTY produced output; repaint
type ptyClosedMsg struct{} // attach client exited (detach or session death)

// ---- attached terminal state ----

// attachment owns one PTY + emulator pair. A fresh one is created per
// attach; detach tears it down and the frozen emulator keeps the last frame.
type attachment struct {
	cmd     *exec.Cmd
	ptmx    *os.File
	emu     *vt.SafeEmulator
	outCh   chan struct{} // coalesced "new output" signal (cap 1)
	doneCh  chan struct{} // closed when the PTY reader exits
	bytesIn atomic.Int64
	reads   atomic.Int64
}

func newAttachment(w, h int) (*attachment, error) {
	cmd := exec.Command("tmux", "-L", *socketName, "attach-session", "-t", *sessionName)
	// The demo itself usually runs inside another tmux (the test driver);
	// strip $TMUX so the inner client doesn't refuse to nest, and pin TERM
	// to what the vt emulator actually implements.
	env := []string{"TERM=xterm-256color"}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") || strings.HasPrefix(e, "TERM=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
	if err != nil {
		return nil, err
	}

	a := &attachment{
		cmd:    cmd,
		ptmx:   ptmx,
		emu:    vt.NewSafeEmulator(w, h),
		outCh:  make(chan struct{}, 1),
		doneCh: make(chan struct{}),
	}

	// PTY → emulator. Signal the UI (coalesced) after each chunk.
	go func() {
		defer close(a.doneCh)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				a.bytesIn.Add(int64(n))
				a.reads.Add(1)
				_, _ = a.emu.Write(buf[:n])
				select {
				case a.outCh <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Emulator → PTY: responses to terminal queries (DA, DSR, ...) and the
	// encoded key events injected via SendKey/SendText/Paste.
	go func() { _, _ = io.Copy(ptmx, a.emu) }()

	return a, nil
}

// stop kills the attach client and closes the PTY. The tmux session itself
// keeps running server-side — that's the whole point.
func (a *attachment) stop() {
	if a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
	_ = a.ptmx.Close()
	go func() { _ = a.cmd.Wait() }()
}

func (a *attachment) resize(w, h int) {
	_ = pty.Setsize(a.ptmx, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
	a.emu.Resize(w, h)
}

// waitOutput blocks until the PTY yields output (or dies), with a small
// sleep+drain so fast streams coalesce to ~60fps instead of a repaint per
// read.
func (a *attachment) waitOutput() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-a.outCh:
			time.Sleep(15 * time.Millisecond)
			select {
			case <-a.outCh:
			default:
			}
			return outputMsg{}
		case <-a.doneCh:
			return ptyClosedMsg{}
		}
	}
}

// ---- model ----

type focusArea int

const (
	focusRail focusArea = iota
	focusTerm
)

type model struct {
	width, height int
	termW, termH  int
	focus         focusArea
	att           *attachment // nil when detached
	frozen        *vt.SafeEmulator
	railSel       int
	frames        int64
	status        string
}

var fakeInstances = []string{"api-refactor", "fix-flaky-tests", "docs-sweep", "spike-1089"}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) paneSizes() {
	// rail | 1 gap | bordered terminal pane; 1 status line at the bottom.
	m.termW = m.width - *railWidth - 1 - 2 // 2 = border
	m.termH = m.height - 2 - 1             // border + status bar
	if m.termW < 10 {
		m.termW = 10
	}
	if m.termH < 3 {
		m.termH = 3
	}
}

func (m *model) attach() tea.Cmd {
	a, err := newAttachment(m.termW, m.termH)
	if err != nil {
		m.status = "attach failed: " + err.Error()
		return nil
	}
	m.att = a
	m.frozen = nil
	m.focus = focusTerm
	m.status = "attached"
	return a.waitOutput()
}

func (m *model) detach() {
	if m.att == nil {
		return
	}
	m.frozen = m.att.emu
	m.att.stop()
	m.att = nil
	m.focus = focusRail
	m.status = "detached (tmux session still running) — Enter to re-attach"
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.paneSizes()
		if m.att != nil {
			m.att.resize(m.termW, m.termH)
		}
		return m, nil

	case outputMsg:
		if m.att == nil {
			return m, nil
		}
		return m, m.att.waitOutput()

	case ptyClosedMsg:
		m.detach()
		return m, nil

	case tea.KeyMsg:
		// Host-reserved keys, live in both focus states.
		switch msg.Type {
		case tea.KeyCtrlC:
			if m.focus != focusTerm { // forward Ctrl-C when terminal focused
				m.detach()
				return m, tea.Quit
			}
		case detachKey:
			m.detach()
			return m, nil
		case tea.KeyTab:
			if m.att != nil {
				if m.focus == focusRail {
					m.focus = focusTerm
				} else {
					m.focus = focusRail
				}
				return m, nil
			}
		}

		if m.focus == focusTerm && m.att != nil {
			forwardKey(m.att.emu, msg)
			return m, nil
		}

		// Rail-focused host keys.
		switch msg.String() {
		case "q", "ctrl+c":
			m.detach()
			return m, tea.Quit
		case "j", "down":
			m.railSel = (m.railSel + 1) % len(fakeInstances)
		case "k", "up":
			m.railSel = (m.railSel + len(fakeInstances) - 1) % len(fakeInstances)
		case "enter", "a":
			if m.att == nil {
				return m, m.attach()
			}
			m.focus = focusTerm
		}
		return m, nil
	}
	return m, nil
}

// forwardKey translates a bubbletea v1 KeyMsg into vt key events. The
// emulator encodes them according to the terminal's current modes (DECCKM,
// bracketed paste, ...) and emits the bytes on its Read side → down the PTY.
func forwardKey(emu *vt.SafeEmulator, msg tea.KeyMsg) {
	var mod uv.KeyMod
	if msg.Alt {
		mod |= uv.ModAlt
	}
	send := func(code rune) { emu.SendKey(uv.KeyPressEvent{Code: code, Mod: mod}) }

	switch msg.Type {
	case tea.KeyRunes:
		if msg.Paste {
			emu.Paste(string(msg.Runes))
			return
		}
		for _, r := range msg.Runes {
			emu.SendKey(uv.KeyPressEvent{Code: r, Text: string(r), Mod: mod})
		}
	case tea.KeySpace:
		emu.SendKey(uv.KeyPressEvent{Code: uv.KeySpace, Text: " ", Mod: mod})
	case tea.KeyEnter:
		send(uv.KeyEnter)
	case tea.KeyBackspace:
		send(uv.KeyBackspace)
	case tea.KeyDelete:
		send(uv.KeyDelete)
	case tea.KeyEsc:
		send(uv.KeyEscape)
	case tea.KeyUp:
		send(uv.KeyUp)
	case tea.KeyDown:
		send(uv.KeyDown)
	case tea.KeyLeft:
		send(uv.KeyLeft)
	case tea.KeyRight:
		send(uv.KeyRight)
	case tea.KeyShiftUp:
		mod |= uv.ModShift
		send(uv.KeyUp)
	case tea.KeyShiftDown:
		mod |= uv.ModShift
		send(uv.KeyDown)
	case tea.KeyCtrlUp:
		mod |= uv.ModCtrl
		send(uv.KeyUp)
	case tea.KeyCtrlDown:
		mod |= uv.ModCtrl
		send(uv.KeyDown)
	case tea.KeyHome:
		send(uv.KeyHome)
	case tea.KeyEnd:
		send(uv.KeyEnd)
	case tea.KeyPgUp:
		send(uv.KeyPgUp)
	case tea.KeyPgDown:
		send(uv.KeyPgDown)
	case tea.KeyInsert:
		send(uv.KeyInsert)
	case tea.KeyShiftTab:
		mod |= uv.ModShift
		send(uv.KeyTab)
	case tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4, tea.KeyF5, tea.KeyF6,
		tea.KeyF7, tea.KeyF8, tea.KeyF9, tea.KeyF10, tea.KeyF11, tea.KeyF12:
		send(uv.KeyF1 + rune(msg.Type-tea.KeyF1))
	default:
		// Ctrl+letter and friends: tea v1 KeyType for these IS the ASCII
		// control code (ctrl+a == 1 ... ctrl+z == 26, minus tab/enter which
		// have their own cases above).
		t := int(msg.Type)
		if t >= 1 && t <= 26 {
			emu.SendKey(uv.KeyPressEvent{Code: rune('a' + t - 1), Mod: mod | uv.ModCtrl})
		}
	}
}

// ---- view ----

var (
	railStyle       = lipgloss.NewStyle().Width(0).Padding(0, 1)
	selStyle        = lipgloss.NewStyle().Reverse(true)
	borderFocused   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("10"))
	borderUnfocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	dimStyle        = lipgloss.NewStyle().Faint(true)
)

func (m *model) View() string {
	if m.width == 0 {
		return "starting..."
	}
	m.frames++

	// Left rail: always visible — that's the point of #1089.
	var rail strings.Builder
	rail.WriteString(" INSTANCES\n\n")
	for i, name := range fakeInstances {
		line := "  " + name
		if i == m.railSel {
			line = selStyle.Render("> " + name)
		}
		rail.WriteString(line + "\n")
	}
	rail.WriteString("\n")
	rail.WriteString(dimStyle.Render(" tab    focus term\n ctrl-] detach\n enter  (re)attach\n q      quit\n"))
	railBox := railStyle.Width(*railWidth).Height(m.height - 1).Render(rail.String())

	// Terminal pane.
	var body string
	switch {
	case m.att != nil:
		body = renderScreen(m.att.emu, m.termW, m.termH, m.focus == focusTerm)
	case m.frozen != nil:
		body = renderScreen(m.frozen, m.termW, m.termH, false)
	default:
		body = lipgloss.Place(m.termW, m.termH, lipgloss.Center, lipgloss.Center, dimStyle.Render("no attachment — press Enter"))
	}
	border := borderUnfocused
	if m.focus == focusTerm && m.att != nil {
		border = borderFocused
	}
	termBox := border.Render(body)

	main := lipgloss.JoinHorizontal(lipgloss.Top, railBox, " ", termBox)

	// Status bar with live perf counters.
	var bytesIn, reads int64
	alt := ""
	if m.att != nil {
		bytesIn = m.att.bytesIn.Load()
		reads = m.att.reads.Load()
		if m.att.emu.IsAltScreen() {
			alt = " [altscreen]"
		}
	}
	focus := "rail"
	if m.focus == focusTerm {
		focus = "TERM"
	}
	status := fmt.Sprintf(" focus=%s%s  %s  in=%dKB reads=%d frames=%d  pane=%dx%d",
		focus, alt, m.status, bytesIn/1024, reads, m.frames, m.termW, m.termH)
	return main + "\n" + dimStyle.Render(status)
}

// renderScreen turns the emulator's cell grid into an ANSI string, drawing
// the cursor as a reverse-video cell when the pane has focus. Style.Diff
// keeps escape output minimal (same trick vt's own Render uses — we
// re-implement the row loop only to be able to inject the cursor overlay).
func renderScreen(emu *vt.SafeEmulator, w, h int, focused bool) string {
	cur := emu.CursorPosition()
	var sb strings.Builder
	sb.Grow(w * h * 2)
	for y := 0; y < h; y++ {
		if y > 0 {
			sb.WriteByte('\n')
		}
		prev := uv.Style{}
		for x := 0; x < w; {
			content, width, st := " ", 1, uv.Style{}
			if c := emu.CellAt(x, y); c != nil && c.Width > 0 && c.Content != "" {
				content, width, st = c.Content, c.Width, c.Style
			}
			if focused && x == cur.X && y == cur.Y {
				st.Attrs ^= uv.AttrReverse
			}
			sb.WriteString(st.Diff(&prev))
			prev = st
			sb.WriteString(content)
			x += width
		}
		if !prev.IsZero() {
			sb.WriteString("\x1b[m")
		}
	}
	return sb.String()
}

// ---- main ----

func main() {
	flag.Parse()

	// Make sure the isolated tmux server + target session exist.
	check := exec.Command("tmux", "-L", *socketName, "has-session", "-t", *sessionName)
	if err := check.Run(); err != nil {
		create := exec.Command("tmux", "-L", *socketName, "new-session", "-d", "-s", *sessionName, "-x", "80", "-y", "24")
		if out, err := create.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "cannot create tmux session: %v\n%s", err, out)
			os.Exit(1)
		}
	}

	m := &model{focus: focusRail, status: "detached — Enter to attach"}
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if m.att != nil {
		m.att.stop()
	}
}
