package pushcred

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrNoLogin means the daemon box holds no GitHub login for the origin's host:
// none of the token variables is set and gh is either not installed or not
// logged in there. It is the operator's to fix on the daemon box (`gh auth
// login`), and the only failure a sandbox is told about by name.
var ErrNoLogin = errors.New("the daemon box has no GitHub login for this host")

const (
	// DefaultTTL bounds how long a token read from the daemon box is reused. A
	// re-login or rotation there reaches every session within it, and a sandbox
	// asking repeatedly cannot make the daemon run gh more than once per window.
	DefaultTTL = time.Minute
	// negativeTTL is how long a "no login" answer is reused, so a sandbox that
	// keeps asking while the daemon box is logged out does not make it spawn gh on
	// every request either.
	negativeTTL = 5 * time.Second
	// ghTimeout bounds one `gh auth token` run.
	ghTimeout = 10 * time.Second
	// maxTokenBytes bounds what is read from gh. Real tokens are well under 300
	// bytes; the bound only stops a misbehaving gh from growing the daemon.
	maxTokenBytes = 4096
)

// Source reads the daemon box's GitHub token for an origin. The zero value is
// ready to use and reads the process environment and the gh on the daemon's PATH.
type Source struct {
	// Getenv reads the daemon's environment; nil means os.Getenv.
	Getenv func(string) string
	// RunGH runs `gh auth token --hostname <host>` and returns its stdout; nil
	// means the real gh, located with LookGH. A test replaces it.
	RunGH func(ctx context.Context, hostname string) ([]byte, error)
	// TTL overrides DefaultTTL when positive.
	TTL time.Duration
	// Now overrides time.Now for tests.
	Now func() time.Time

	mu       sync.Mutex
	cache    map[string]cachedToken
	inflight map[string]*tokenCall
}

type cachedToken struct {
	token   string
	err     error
	expires time.Time
}

type tokenCall struct {
	done  chan struct{}
	token string
	err   error
}

// publicTokenNames and enterpriseTokenNames are the daemon-environment variables
// read before gh, in precedence order — the same names, in the same order, as the
// value-free clone helper (session.githubEnvironmentCredential) expands.
var (
	publicTokenNames     = []string{"GH_TOKEN", "GITHUB_TOKEN"}
	enterpriseTokenNames = []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
)

// Token returns the daemon box's GitHub token for o. It never writes the token
// anywhere: the value lives in this process's memory for at most the TTL. An
// error never carries the token or gh's output.
func (s *Source) Token(ctx context.Context, o Origin) (string, error) {
	if o.Host == "" {
		return "", ErrNoLogin
	}
	names := publicTokenNames
	if o.Enterprise {
		names = enterpriseTokenNames
	}
	for _, name := range names {
		if v := strings.TrimSpace(s.getenv(name)); v != "" {
			if !plausibleToken(v) {
				return "", fmt.Errorf("the daemon's %s is not a usable token (empty, multi-line or over %d bytes)", name, maxTokenBytes)
			}
			return v, nil
		}
	}
	return s.fromGH(ctx, o.Hostname())
}

func (s *Source) getenv(name string) string {
	if s.Getenv != nil {
		return s.Getenv(name)
	}
	return os.Getenv(name)
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultTTL
}

// fromGH reads the token from gh, one run per host at a time, cached.
func (s *Source) fromGH(ctx context.Context, hostname string) (string, error) {
	s.mu.Lock()
	if c, ok := s.cache[hostname]; ok && s.now().Before(c.expires) {
		s.mu.Unlock()
		return c.token, c.err
	}
	if call, ok := s.inflight[hostname]; ok {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.token, call.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	call := &tokenCall{done: make(chan struct{})}
	if s.inflight == nil {
		s.inflight = map[string]*tokenCall{}
	}
	s.inflight[hostname] = call
	s.mu.Unlock()

	// The run is not bound to the first caller's context: a caller that gives up
	// must not fail every other caller waiting on the same run. ghTimeout bounds it.
	runCtx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	token, err := s.runGH(runCtx, hostname)
	cancel()

	s.mu.Lock()
	expires := s.now().Add(s.ttl())
	if errors.Is(err, ErrNoLogin) {
		expires = s.now().Add(negativeTTL)
	}
	if err == nil || errors.Is(err, ErrNoLogin) {
		if s.cache == nil {
			s.cache = map[string]cachedToken{}
		}
		s.cache[hostname] = cachedToken{token: token, err: err, expires: expires}
	}
	delete(s.inflight, hostname)
	call.token, call.err = token, err
	close(call.done)
	s.mu.Unlock()
	return token, err
}

func (s *Source) runGH(ctx context.Context, hostname string) (string, error) {
	run := s.RunGH
	if run == nil {
		run = runRealGH
	}
	out, err := run(ctx, hostname)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", ErrNoLogin
	}
	if !plausibleToken(token) {
		return "", errors.New("gh auth token printed something that is not a single token")
	}
	return token, nil
}

// runRealGH runs the daemon box's gh, located on the daemon's PATH by LookGH
// (absolute entries only), with a fixed argv whose one variable is a hostname the
// daemon derived from its own record of the origin. It runs from "/" so no
// working directory's repository can influence it, with prompts and the update
// notifier off.
func runRealGH(ctx context.Context, hostname string) ([]byte, error) {
	exe, err := LookGH(os.Getenv("PATH"))
	if err != nil {
		return nil, ErrNoLogin
	}
	cmd := exec.CommandContext(ctx, exe, "auth", "token", "--hostname", hostname)
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: maxTokenBytes + 1}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// gh exits non-zero when it holds no token for the host.
			return nil, ErrNoLogin
		}
		return nil, fmt.Errorf("running gh auth token on the daemon box failed: %w", err)
	}
	return stdout.Bytes(), nil
}

// plausibleToken reports whether v can be a bearer token: one line of printable,
// space-free ASCII within the size bound. It does not judge the token's validity
// — only GitHub can — just that it cannot smuggle a second credential-protocol
// line or a header.
func plausibleToken(v string) bool {
	if v == "" || len(v) > maxTokenBytes {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// limitedWriter keeps at most n bytes and silently drops the rest; a token that
// long is refused by plausibleToken.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	keep := p
	if len(keep) > l.n {
		keep = keep[:l.n]
	}
	if _, err := l.w.Write(keep); err != nil {
		return 0, err
	}
	l.n -= len(keep)
	return len(p), nil
}
