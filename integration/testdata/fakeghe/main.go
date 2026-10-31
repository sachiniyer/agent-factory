// Command fakeghe is the sandbox e2e's stand-in for a GitHub Enterprise host
// (#5571 W7.8): git smart HTTP that accepts a push ONLY with the one token the
// daemon was given, and a pull-request endpoint that records an authenticated
// create. It runs inside the mock ssh host's container, behind TLS whose CA the
// container trusts, so the session's git and gh reach it exactly as they would a
// real enterprise host.
//
// It lives under testdata so `./...` never builds or lints it as part of af; the
// e2e builds it by path.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"strings"
	"sync"
)

func main() {
	addr := flag.String("addr", ":8443", "listen address")
	cert := flag.String("cert", "", "TLS certificate file")
	key := flag.String("key", "", "TLS key file")
	root := flag.String("git-root", "/srv/git", "GIT_PROJECT_ROOT for git http-backend")
	backend := flag.String("http-backend", "/usr/libexec/git-core/git-http-backend", "git http-backend binary")
	record := flag.String("record", "/tmp/fakeghe.log", "where authenticated requests are recorded")
	flag.Parse()
	token := os.Getenv("FAKEGHE_TOKEN")
	if token == "" {
		log.Fatal("FAKEGHE_TOKEN is required")
	}

	var mu sync.Mutex
	note := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		f, err := os.OpenFile(*record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, format+"\n", args...)
	}

	authorized := func(r *http.Request) bool {
		h := r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(h, "Basic "):
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
			if err != nil {
				return false
			}
			user, pass, _ := strings.Cut(string(raw), ":")
			return user == "x-access-token" && pass == token
		case strings.HasPrefix(h, "token "):
			return strings.TrimPrefix(h, "token ") == token
		case strings.HasPrefix(h, "Bearer "):
			return strings.TrimPrefix(h, "Bearer ") == token
		}
		return false
	}

	git := &cgi.Handler{
		Path: *backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + *root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			note("api refused %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		note("api authorized %s %s %s", r.Method, r.URL.Path, strings.TrimSpace(string(body)))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "html_url": "https://" + r.Host + strings.TrimPrefix(r.URL.Path, "/api/v3/repos")})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		push := strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
		if push {
			if !authorized(r) {
				note("push challenged %s", r.URL.Path)
				w.Header().Set("WWW-Authenticate", `Basic realm="fakeghe"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			note("push authorized %s", r.URL.Path)
			// http-backend serves receive-pack only to an authenticated user.
			h := *git
			h.Env = append(append([]string(nil), git.Env...), "REMOTE_USER=x-access-token")
			h.ServeHTTP(w, r)
			return
		}
		git.ServeHTTP(w, r)
	})
	log.Fatal(http.ListenAndServeTLS(*addr, *cert, *key, mux))
}
