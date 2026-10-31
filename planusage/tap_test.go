package planusage

import (
	"bytes"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const statusLine = `{"cwd":"%s","workspace":{"project_dir":"%s"},"rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":1761004800},"seven_day":{"used_percentage":41,"resets_at":"2026-10-26T09:00:00Z"}}}`

func env(home, userHome string, vars map[string]string) TapEnv {
	return TapEnv{Home: home, UserHome: userHome, Now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		Getenv: func(k string) string { return vars[k] }, Record: true}
}

func input(project string) string {
	return strings.ReplaceAll(statusLine, "%s", project)
}

func TestTapWritesReadingAndRunsUserStatusLine(t *testing.T) {
	h := home(t)
	userHome := t.TempDir()
	settings := filepath.Join(t.TempDir(), "real-settings.json")
	write(t, settings, `{"statusLine":{"type":"command","command":"cat >/dev/null; echo mine"}}`)
	if err := os.MkdirAll(filepath.Join(userHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(settings, filepath.Join(userHome, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	Tap(strings.NewReader(input(t.TempDir())), &out, env(h, userHome, nil))
	if out.String() != "mine\n" {
		t.Fatalf("output %q", out.String())
	}
	r, err := LoadReading(DefaultAccount)
	if err != nil || r == nil || r.FiveHour.UsedPercentage != 23.5 || r.FiveHour.ResetsAt != 1761004800 ||
		r.SevenDay.ResetsAt != time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC).Unix() || r.ReadAt.IsZero() {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestTapRunsOnlyTheUserLevelStatusLine(t *testing.T) {
	h := home(t)
	accountDir := filepath.Join(h, "accounts", "claude", "work")
	write(t, filepath.Join(accountDir, "settings.json"), `{"statusLine":{"command":"echo account"}}`)
	project := t.TempDir()
	var out bytes.Buffer
	Tap(strings.NewReader(input(project)), &out, env(h, t.TempDir(), map[string]string{"CLAUDE_CONFIG_DIR": accountDir}))
	if out.String() != "account\n" {
		t.Fatalf("output %q", out.String())
	}
	if r, _ := LoadReading("work"); r == nil {
		t.Fatal("no reading for the account")
	}
	if r, _ := LoadReading(DefaultAccount); r != nil {
		t.Fatal("reading went to the default account")
	}

	// A project's own status line is NOT run (Claude runs it only after the user
	// trusts the workspace, which the tap cannot see): the user-level one still is.
	for _, f := range []string{"settings.local.json", "settings.json"} {
		write(t, filepath.Join(project, ".claude", f), `{"statusLine":{"command":"echo project > `+filepath.Join(project, "ran")+`; echo project"}}`)
	}
	out.Reset()
	Tap(strings.NewReader(input(project)), &out, env(h, t.TempDir(), map[string]string{"CLAUDE_CONFIG_DIR": accountDir}))
	if out.String() != "account\n" {
		t.Fatalf("output %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(project, "ran")); err == nil {
		t.Fatal("the tap ran a project-level status line")
	}

	// With no user-level status line at all, a project one still does not run.
	bare := filepath.Join(h, "accounts", "claude", "bare")
	out.Reset()
	Tap(strings.NewReader(input(project)), &out, env(h, t.TempDir(), map[string]string{"CLAUDE_CONFIG_DIR": bare}))
	if out.String() != "" {
		t.Fatalf("output %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(project, "ran")); err == nil {
		t.Fatal("the tap ran a project-level status line with no user one")
	}
}

func TestTapWithoutRateLimitsOrStatusLine(t *testing.T) {
	h := home(t)
	userHome := t.TempDir()
	write(t, filepath.Join(userHome, ".claude", "settings.json"), `{"statusLine":{"command":"/x/af internal plan-tap"}}`)
	var out bytes.Buffer
	Tap(strings.NewReader(`not json`), &out, env(h, userHome, nil))
	if out.Len() != 0 {
		t.Fatalf("output %q", out.String())
	}
	if r, _ := LoadReading(DefaultAccount); r != nil {
		t.Fatal("a reading without rate limits was written")
	}
	Tap(strings.NewReader(`{"rate_limits":{}}`), &out, env(h, t.TempDir(), nil))
	if r, _ := LoadReading(DefaultAccount); r != nil || out.Len() != 0 {
		t.Fatal("empty rate limits wrote a reading")
	}
}

func TestTapSurvivesAFailingUserCommand(t *testing.T) {
	h := home(t)
	userHome := t.TempDir()
	write(t, filepath.Join(userHome, ".claude", "settings.json"), `{"statusLine":{"command":"echo partial; exit 3"}}`)
	var out bytes.Buffer
	Tap(strings.NewReader(input(t.TempDir())), &out, env(h, userHome, nil))
	if out.String() != "partial\n" {
		t.Fatalf("output %q", out.String())
	}
}

func TestAccountForConfigDir(t *testing.T) {
	h := "/af"
	for dir, want := range map[string]string{
		"":                          DefaultAccount,
		"/home/me/.claude":          DefaultAccount,
		"/af/accounts/claude/work":  "work",
		"/af/accounts/claude/work/": "work",
		"/af/accounts/codex/work":   DefaultAccount,
		// A registered account named "default" is its own identity, not the
		// ambient one (Codex P2 on #6299/#6300): the ambient key can never be a
		// valid account name.
		"/af/accounts/claude/default": "default",
	} {
		if got := AccountForConfigDir(h, dir); got != want {
			t.Errorf("%q: %q", dir, got)
		}
	}
}

func TestTheAmbientKeyCannotBeARegisteredAccountName(t *testing.T) {
	if agentaccountNameValid(DefaultAccount) {
		t.Fatalf("%q is a valid account name, so an account could share the ambient identity's readings and meter", DefaultAccount)
	}
	if !agentaccountNameValid("default") {
		t.Fatal("an account named default must stay legal")
	}
}

func TestTapSkipsOversizedInput(t *testing.T) {
	h := home(t)
	userHome := t.TempDir()
	write(t, filepath.Join(userHome, ".claude", "settings.json"), `{"statusLine":{"command":"echo ran"}}`)
	big := input(t.TempDir())
	big = big[:len(big)-1] + `,"pad":"` + strings.Repeat("x", maxStatusLineInput) + `"}`
	var out bytes.Buffer
	Tap(strings.NewReader(big), &out, env(h, userHome, nil))
	if out.Len() != 0 {
		t.Fatalf("the user's command ran on truncated input: %q", out.String())
	}
	if r, _ := LoadReading(DefaultAccount); r != nil {
		t.Fatal("a reading was written from truncated input")
	}
}

func TestTapKillsTheUsersWholeProcessGroupOnTimeout(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	h := home(t)
	userHome := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "pid")
	write(t, filepath.Join(userHome, ".claude", "settings.json"),
		`{"statusLine":{"command":"sh -c 'echo $$ > `+pidFile+`; exec sleep 30' & wait"}}`)
	prev := tapUserTimeout
	tapUserTimeout = 200 * time.Millisecond
	t.Cleanup(func() { tapUserTimeout = prev })
	var out bytes.Buffer
	Tap(strings.NewReader(input(t.TempDir())), &out, env(h, userHome, nil))
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	statPath := filepath.Join("/proc", strings.TrimSpace(string(raw)), "stat")
	// SIGKILL delivery is asynchronous, so the grandchild can still read as
	// running for a moment: poll until it is gone or a zombie.
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat, err := os.ReadFile(statPath)
		if err != nil {
			return
		}
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the user's grandchild outlived the timeout: %s", stat)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A tap that outlives the opt-in being turned off records nothing, and the
// user's own status line still renders.
func TestTapRecordsNothingWhenPlanUsageIsOff(t *testing.T) {
	h := home(t)
	userHome := t.TempDir()
	write(t, filepath.Join(userHome, ".claude", "settings.json"), `{"statusLine":{"command":"echo mine"}}`)
	e := env(h, userHome, nil)
	e.Record = false
	var out bytes.Buffer
	Tap(strings.NewReader(input(t.TempDir())), &out, e)
	if out.String() != "mine\n" {
		t.Fatalf("output %q", out.String())
	}
	if r, _ := LoadReading(DefaultAccount); r != nil {
		t.Fatalf("a reading was stored while plan usage is off: %+v", r)
	}
}

func agentaccountNameValid(name string) bool { return agentaccount.ValidateName(name) == nil }

// An account named "meter" is legal; its readings must not share a path with the
// meter state file (Codex P2 on #6299).
func TestAReadingNeverSharesAPathWithTheMeterState(t *testing.T) {
	home(t)
	reading, err := ReadingPath("meter")
	if err != nil {
		t.Fatal(err)
	}
	meter, err := MeterPath()
	if err != nil {
		t.Fatal(err)
	}
	if reading == meter {
		t.Fatalf("an account named meter writes its reading to the meter state file: %s", reading)
	}
	if err := WriteReading("meter", Reading{FiveHour: &Limit{UsedPercentage: 5, ResetsAt: 1}, ReadAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st := NewMeterState()
	st.RememberRunDirs("s1", []string{"/a"})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if r, err := LoadReading("meter"); err != nil || r == nil || r.FiveHour == nil {
		t.Fatalf("saving the meter state clobbered the account's reading: %+v %v", r, err)
	}
	if loaded, err := LoadMeter(); err != nil || len(loaded.RunDirs["s1"]) != 1 {
		t.Fatalf("writing a reading clobbered the meter state: %+v %v", loaded, err)
	}
}
