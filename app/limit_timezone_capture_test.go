package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// TestLimitTimezoneCapture renders the sidebar at 80x24 so the reset badge
// remains visible in the #4039 evidence. Run only in the testbox container.
func TestLimitTimezoneCapture(t *testing.T) {
	// time.Local is read by runtime timer goroutines through time.Now, so even
	// sequential tests cannot safely replace it. Configure TZ before the child
	// process starts; the same test binary keeps race instrumentation enabled.
	const childEnv = "AF_LIMIT_TIMEZONE_CAPTURE_CHILD"
	if os.Getenv(childEnv) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLimitTimezoneCapture$", "-test.count=1", "-test.v")
		child.Env = append(os.Environ(), childEnv+"=1", "TZ=America/Los_Angeles")
		out, err := child.CombinedOutput()
		fmt.Print(string(out))
		require.NoError(t, err, "timezone capture subprocess failed")
		return
	}
	require.Equal(t, "America/Los_Angeles", time.Local.String())
	configureDesignStillsOutput(t)
	loc, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, loc)
	h, inst := newDesignDriverSceneHome(t, "dark", nil)
	h.sidebar.SetNowForTest(func() time.Time { return now })
	h.termWidth, h.termHeight = 80, 24
	banner := "Claude usage limit reached. Your limit will reset at 5pm (UTC)"
	hit, reset, parsed := task.NewLimitDetector(nil).Check(banner, "claude", now)
	require.True(t, hit)
	require.True(t, parsed)
	inst.SetLimitReached(reset)
	h.relayout()
	h.sidebar.SetSize(80, 24)
	frame := xansi.Strip(h.sidebar.View())
	fmt.Printf("CAPTURE START\n%s\nCAPTURE END\n", frame)
	require.Contains(t, frame, "[limit] resets 10am")
}
