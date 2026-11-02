package pushcred

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func env(vals map[string]string) func(string) string {
	return func(name string) string { return vals[name] }
}

func TestTokenPrefersTheDaemonEnvironmentInTheCloneHelpersOrder(t *testing.T) {
	public := Origin{Host: "github.com", Path: "o/r"}
	enterprise := Origin{Host: "ghe.example", Path: "o/r", Enterprise: true}
	noGH := func(context.Context, string) ([]byte, error) {
		t.Fatal("gh ran although an environment token was set")
		return nil, nil
	}
	for _, tc := range []struct {
		name   string
		vars   map[string]string
		origin Origin
		want   string
	}{
		{"GH_TOKEN first", map[string]string{"GH_TOKEN": "a", "GITHUB_TOKEN": "b"}, public, "a"},
		{"GITHUB_TOKEN second", map[string]string{"GITHUB_TOKEN": "b", "GH_ENTERPRISE_TOKEN": "e"}, public, "b"},
		{"enterprise first", map[string]string{"GH_ENTERPRISE_TOKEN": "e", "GITHUB_ENTERPRISE_TOKEN": "f", "GH_TOKEN": "a"}, enterprise, "e"},
		{"enterprise second", map[string]string{"GITHUB_ENTERPRISE_TOKEN": "f", "GH_TOKEN": "a"}, enterprise, "f"},
	} {
		s := &Source{Getenv: env(tc.vars), RunGH: noGH}
		got, err := s.Token(context.Background(), tc.origin)
		if err != nil || got != tc.want {
			t.Errorf("%s: Token = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestTokenFallsBackToGHAndCachesWithinTheTTL(t *testing.T) {
	var runs atomic.Int32
	var gotHost string
	now := time.Unix(1000, 0)
	s := &Source{
		Getenv: env(nil),
		RunGH: func(_ context.Context, hostname string) ([]byte, error) {
			runs.Add(1)
			gotHost = hostname
			return []byte("gho_daemonbox\n"), nil
		},
		Now: func() time.Time { return now },
	}
	o := Origin{Host: "ghe.example:8443", Path: "o/r", Enterprise: true}
	for i := 0; i < 3; i++ {
		got, err := s.Token(context.Background(), o)
		if err != nil || got != "gho_daemonbox" {
			t.Fatalf("Token = %q, %v", got, err)
		}
	}
	if runs.Load() != 1 {
		t.Fatalf("gh ran %d times within one TTL; want 1", runs.Load())
	}
	if gotHost != "ghe.example" {
		t.Fatalf("gh was asked for %q; want the hostname without its port", gotHost)
	}
	now = now.Add(DefaultTTL + time.Second)
	if _, err := s.Token(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 2 {
		t.Fatalf("an expired token was reused (runs=%d)", runs.Load())
	}
}

func TestTokenRunsGHOncePerHostUnderConcurrency(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	s := &Source{
		Getenv: env(nil),
		RunGH: func(context.Context, string) ([]byte, error) {
			runs.Add(1)
			<-release
			return []byte("tok"), nil
		},
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := s.Token(context.Background(), Origin{Host: "github.com", Path: "o/r"}); err != nil || got != "tok" {
				t.Errorf("Token = %q, %v", got, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("gh ran %d times for one host; want 1", runs.Load())
	}
}

func TestTokenRefusesWhatCannotBeAToken(t *testing.T) {
	for name, out := range map[string]string{
		"empty":      "\n",
		"two lines":  "tok\npassword=other\n",
		"with space": "tok en",
		"too long":   strings.Repeat("a", maxTokenBytes+1),
	} {
		s := &Source{Getenv: env(nil), RunGH: func(context.Context, string) ([]byte, error) { return []byte(out), nil }}
		got, err := s.Token(context.Background(), Origin{Host: "github.com", Path: "o/r"})
		if err == nil || got != "" {
			t.Errorf("%s: Token = %q, %v; want a refusal", name, got, err)
		}
		if err != nil && strings.Contains(err.Error(), "password=other") {
			t.Errorf("%s: the error carries gh's output: %v", name, err)
		}
	}
	s := &Source{Getenv: env(map[string]string{"GH_TOKEN": "a\nb"})}
	if _, err := s.Token(context.Background(), Origin{Host: "github.com", Path: "o/r"}); err == nil {
		t.Error("a multi-line GH_TOKEN was returned")
	} else if strings.Contains(err.Error(), "a\nb") {
		t.Errorf("the error carries the variable's value: %v", err)
	}
}

func TestTokenReportsNoLoginAndBacksOff(t *testing.T) {
	var runs atomic.Int32
	now := time.Unix(1000, 0)
	s := &Source{
		Getenv: env(nil),
		RunGH:  func(context.Context, string) ([]byte, error) { runs.Add(1); return nil, ErrNoLogin },
		Now:    func() time.Time { return now },
	}
	o := Origin{Host: "github.com", Path: "o/r"}
	for i := 0; i < 3; i++ {
		if _, err := s.Token(context.Background(), o); !errors.Is(err, ErrNoLogin) {
			t.Fatalf("Token err = %v; want ErrNoLogin", err)
		}
	}
	if runs.Load() != 1 {
		t.Fatalf("a logged-out daemon box ran gh %d times inside the back-off; want 1", runs.Load())
	}
	now = now.Add(negativeTTL + time.Second)
	_, _ = s.Token(context.Background(), o)
	if runs.Load() != 2 {
		t.Fatalf("the back-off never expired (runs=%d)", runs.Load())
	}
}

// The real runner: gh is located by LookGH on the daemon's PATH, gets exactly the
// fixed argv, and a non-zero exit reads as "no login".
func TestRunRealGHUsesAFixedArgvAndAnAbsolutePathGH(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	cwdFile := filepath.Join(dir, "cwd")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\npwd > " + cwdFile + "\n" +
		"[ \"$4\" = logged-out.example ] && exit 1\necho gho_fromgh\n"
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, GHProgramName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := runRealGH(context.Background(), "github.com")
	if err != nil || strings.TrimSpace(string(out)) != "gho_fromgh" {
		t.Fatalf("runRealGH = %q, %v", out, err)
	}
	argv, _ := os.ReadFile(argvFile)
	if got := strings.Fields(string(argv)); strings.Join(got, " ") != "auth token --hostname github.com" {
		t.Fatalf("gh argv = %q", got)
	}
	cwd, _ := os.ReadFile(cwdFile)
	if strings.TrimSpace(string(cwd)) != "/" {
		t.Fatalf("gh ran in %q; want /", strings.TrimSpace(string(cwd)))
	}
	if _, err := runRealGH(context.Background(), "logged-out.example"); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("a non-zero gh exit = %v; want ErrNoLogin", err)
	}

	// No absolute gh on PATH (only a relative entry holding one): no login, and
	// the relative gh never ran.
	if err := os.Remove(argvFile); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", "bin")
	if _, err := runRealGH(context.Background(), "github.com"); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("runRealGH with only a relative PATH entry = %v; want ErrNoLogin", err)
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Fatal("the gh reachable only through a relative PATH entry was executed")
	}
}
