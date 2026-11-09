package keys

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin what Bubble Tea's real input parser delivers for keys that
// pile up in one terminal read: the TUI's key routing is built on these
// shapes, and #7015 came from the gap between them and the key map. They drive
// the same parser the TUI reads its keyboard with, so a Bubble Tea upgrade that
// changes either shape fails here, by name, before it changes behavior.

// keyRecorder is a model that records every KeyMsg it receives until the
// terminator (ctrl+d) arrives.
type keyRecorder struct{ keys []tea.KeyMsg }

func (r *keyRecorder) Init() tea.Cmd { return nil }

func (r *keyRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return r, nil
	}
	if k.Type == tea.KeyCtrlD {
		return r, tea.Quit
	}
	r.keys = append(r.keys, k)
	return r, nil
}

func (r *keyRecorder) View() string { return "" }

// readKeys runs raw terminal bytes through a real Bubble Tea program and returns
// the KeyMsgs it delivers, in order. All of input arrives in ONE Read — the shape
// of keys that queued in the TTY while the event loop was busy. A ctrl+d byte
// terminates the run; it is a control byte, so it never joins a rune burst.
func readKeys(t *testing.T, input string) []tea.KeyMsg {
	t.Helper()
	rec := &keyRecorder{}
	p := tea.NewProgram(rec,
		tea.WithInput(strings.NewReader(input+"\x04")),
		tea.WithOutput(io.Discard),
		tea.WithoutRenderer(),
		tea.WithoutSignalHandler(),
	)
	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		p.Kill()
		<-done
		t.Fatalf("the program never delivered the ctrl+d terminator; keys so far: %v", rec.keys)
	}
	return rec.keys
}

// TestBubbleTeaDeliversARuneBurstAsOneKey pins the #7015 premise: printable
// bytes that arrive in one read become ONE KeyRunes message, and its String()
// is the whole burst, so it matches no single-key binding. Two `k` presses that
// queued behind a slow frame reach the TUI as "kk", not as `k`, `k`.
func TestBubbleTeaDeliversARuneBurstAsOneKey(t *testing.T) {
	got := readKeys(t, "kk")

	require.Len(t, got, 1, "the burst must arrive as a single message")
	assert.Equal(t, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("kk")}, got[0])
	assert.Equal(t, "kk", got[0].String())
	_, bound := GlobalKeyStringsMap[got[0].String()]
	assert.False(t, bound, "the burst's String() misses the key map, even though each rune is bound")
	_, bound = GlobalKeyStringsMap["k"]
	assert.True(t, bound, "control: a lone k is the bound up key")
}

// TestBubbleTeaAttachesAltToOneRuneOnly pins the Alt shape a burst splitter must
// respect. An ESC marks only the ONE rune right after it as Alt-modified
// (Bubble Tea's detectOneMsg stops collecting after that rune), and the runes
// that follow in the same read arrive as their own plain burst. So a multi-rune
// message never carries Alt, and splitting the plain burst never needs to give
// any rune an Alt it did not have, or take one away.
func TestBubbleTeaAttachesAltToOneRuneOnly(t *testing.T) {
	got := readKeys(t, "\x1bkkk")

	require.Len(t, got, 2, "ESC+k is one Alt key, and the remaining runes are a separate burst")
	assert.Equal(t, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k"), Alt: true}, got[0])
	assert.Equal(t, "alt+k", got[0].String())
	assert.Equal(t, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("kk")}, got[1],
		"the runes after the Alt key carry no Alt")
}
