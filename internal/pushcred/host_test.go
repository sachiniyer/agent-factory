package pushcred

import (
	"bytes"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestReadCredentialRequestTakesTheThreeFieldsGitSends(t *testing.T) {
	in := "capability[]=authtype\nprotocol=https\nhost=github.com\npath=owner/repo.git\nwwwauth[]=Basic realm=\"GitHub\"\n\nignored=after-blank\n"
	got, err := ReadCredentialRequest(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got != (CredentialRequest{Protocol: "https", Host: "github.com", Path: "owner/repo.git"}) {
		t.Fatalf("ReadCredentialRequest = %+v", got)
	}
}

func TestReadCredentialRequestRefusesMalformedInput(t *testing.T) {
	for name, in := range map[string]string{
		"repeated host": "protocol=https\nhost=github.com\nhost=evil.example\n\n",
		"no equals":     "protocol https\n\n",
		"oversize":      "path=" + strings.Repeat("a", maxRequestBytes+10) + "\n\n",
	} {
		if got, err := ReadCredentialRequest(strings.NewReader(in)); err == nil {
			t.Errorf("%s: ReadCredentialRequest = %+v, want a refusal", name, got)
		}
	}
}

func TestWriteCredentialNeverWritesASecondAttribute(t *testing.T) {
	var b bytes.Buffer
	if err := WriteCredential(&b, "x-access-token", "gho_abc"); err != nil {
		t.Fatal(err)
	}
	if b.String() != "username=x-access-token\npassword=gho_abc\n" {
		t.Fatalf("WriteCredential wrote %q", b.String())
	}
	for _, bad := range []string{"tok\nurl=https://evil.example", "", "a b"} {
		b.Reset()
		if err := WriteCredential(&b, "x-access-token", bad); err == nil || b.Len() != 0 {
			t.Errorf("WriteCredential(%q) wrote %q, err %v; want nothing and a refusal", bad, b.String(), err)
		}
	}
	b.Reset()
	if err := WriteQuit(&b); err != nil || b.String() != "quit=1\n" {
		t.Fatalf("WriteQuit = %q, %v", b.String(), err)
	}
}

// shellQuoteLikeTheProvisioner is session.shellQuote, restated so this package's
// test pins the exact format the provisioner writes.
func shellQuoteLikeTheProvisioner(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func TestParseCallbackEnvReadsWhatTheProvisionerWrites(t *testing.T) {
	url := "unix:///home/u/.af-sessions/x.y/cb's.sock"
	token := "sbx_tok'en$(id)`x`"
	data := "AF_DAEMON_URL=" + shellQuoteLikeTheProvisioner(url) + "\nAF_DAEMON_TOKEN=" + shellQuoteLikeTheProvisioner(token) + "\n"
	gotURL, gotToken, err := ParseCallbackEnv([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != url || gotToken != token {
		t.Fatalf("ParseCallbackEnv = %q, %q; want %q, %q", gotURL, gotToken, url, token)
	}
}

func TestParseCallbackEnvRefusesWhatItCannotRead(t *testing.T) {
	for name, data := range map[string]string{
		"unquoted":     "AF_DAEMON_URL=unix:///s\nAF_DAEMON_TOKEN='t'\n",
		"unterminated": "AF_DAEMON_URL='unix:///s\nAF_DAEMON_TOKEN='t'\n",
		"no token":     "AF_DAEMON_URL='unix:///s'\n",
		"empty":        "",
		"oversize":     "AF_DAEMON_URL='" + strings.Repeat("a", maxEnvBytes) + "'\n",
	} {
		if u, tok, err := ParseCallbackEnv([]byte(data)); err == nil {
			t.Errorf("%s: ParseCallbackEnv = %q, %q; want a refusal", name, u, tok)
		}
	}
}

func TestGHEnvSetsExactlyOneTokenForTheOriginsHostKind(t *testing.T) {
	pane := []string{"PATH=/usr/bin", "GH_TOKEN=host-own", "GITHUB_TOKEN=ci", "GH_ENTERPRISE_TOKEN=x", "GITHUB_ENTERPRISE_TOKEN=y", "GH_HOST=other.example", "HOME=/h"}
	got := GHEnv(pane, Origin{Host: "github.com", Path: "o/r"}, "gho_op")
	sort.Strings(got)
	want := []string{ShimTokenEnv + "=1", "GH_HOST=other.example", "GH_TOKEN=gho_op", "HOME=/h", "PATH=/usr/bin"}
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("public GHEnv = %q; want %q", got, want)
	}

	got = GHEnv(pane, Origin{Host: "ghe.example:8443", Path: "o/r", Enterprise: true}, "ghe_op")
	sort.Strings(got)
	want = []string{ShimTokenEnv + "=1", "GH_ENTERPRISE_TOKEN=ghe_op", "GH_HOST=ghe.example:8443", "HOME=/h", "PATH=/usr/bin"}
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("enterprise GHEnv = %q; want %q", got, want)
	}
}

func TestEnvironmentTokenIsTheHostsOwnAndNeverTheShims(t *testing.T) {
	vars := map[string]string{"GH_TOKEN": "gh-host", "GITHUB_TOKEN": "github-host", "GH_ENTERPRISE_TOKEN": "ghe-host"}
	get := func(name string) string { return vars[name] }
	if tok, ok := EnvironmentToken(get, "github.com", ""); !ok || tok != "gh-host" {
		t.Fatalf("public = %q, %v", tok, ok)
	}
	if tok, ok := EnvironmentToken(get, "ghe.example", "ghe.example"); !ok || tok != "ghe-host" {
		t.Fatalf("enterprise = %q, %v", tok, ok)
	}
	if _, ok := EnvironmentToken(get, "gitlab.com", ""); ok {
		t.Fatal("a host that is not GitHub has no GitHub token")
	}
	vars[ShimTokenEnv] = "1"
	if tok, ok := EnvironmentToken(get, "github.com", ""); ok {
		t.Fatalf("the shim's (operator's) token was offered as the host's: %q", tok)
	}
	delete(vars, ShimTokenEnv)
	vars["GH_TOKEN"] = "two\nlines"
	if _, ok := EnvironmentToken(get, "github.com", ""); ok {
		t.Fatal("an implausible token was offered")
	}
}

func TestWithoutShimTokenDropsOnlyAShimSuppliedToken(t *testing.T) {
	marked := []string{"PATH=/usr/bin", "GH_TOKEN=op", "GITHUB_TOKEN=op2", "GH_ENTERPRISE_TOKEN=op3", "GITHUB_ENTERPRISE_TOKEN=op4", "GH_HOST=ghe.example", ShimTokenEnv + "=1"}
	got := WithoutShimToken(marked)
	sort.Strings(got)
	if want := []string{"GH_HOST=ghe.example", "PATH=/usr/bin"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("marked environment = %v, want %v", got, want)
	}
	host := []string{"PATH=/usr/bin", "GH_TOKEN=mine", ShimTokenEnv + "="}
	if got := WithoutShimToken(host); !reflect.DeepEqual(got, host) {
		t.Fatalf("an unmarked environment (the host's own token) changed: %v", got)
	}
}
