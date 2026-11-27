package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

// The forwarding half of the credential-mount classification (the tables
// live in agent_credentials_env.go): names whose readers chain them as
// precedence siblings are claimed AS GROUPS — forwarding any member
// through session_env_passthrough exempts the group from the pin or
// re-assert and reseats the unforwarded members to the operator's value,
// so an af-set or image-supplied sibling can never sit ahead of the
// operator's choice (#7529 R2, #7531, #7889).

// credentialMountProxyCasePairs pairs each proxy variable's upper- and
// lower-case spellings. Programs disagree on which spelling wins — and some
// treat a defined-empty sibling as an explicit "no proxy" that cancels the
// other spelling (measured: Python's getproxies and reqwest both let an empty
// lowercase twin override a forwarded uppercase value). Forwarding either
// spelling therefore exempts BOTH from the empty pin; pinning the twin of a
// forwarded name could silently cancel the operator's proxy (#7529 R2).
var credentialMountProxyCasePairs = sessionenv.ProxyEnvCasePairs

// credentialMountTrustBundleGroup is the precedence chain the CA-bundle
// spellings form across readers: Python Requests' environment merge is
// `REQUESTS_CA_BUNDLE or CURL_CA_BUNDLE or verify`, and curl prefers
// `CURL_CA_BUNDLE` over `SSL_CERT_FILE` for --cacert (curl --manual). Any
// af-set member — an empty pin or a platform-resolved path alike —
// outranks a lower member the operator forwarded for their private CA
// (#7889). Forwarding ANY member therefore claims the whole chain: the
// others are exempted from the pin and reseated to the operator's bundle
// — the proxy twins' claim semantics — so the operator's CA reaches every
// client of the chain. All three are bundle file paths, so the reseat is
// verbatim, no grammar translation.
var credentialMountTrustBundleGroup = []string{
	"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "SSL_CERT_FILE",
}

// credentialMountSelectorGroups pairs the program-selector names whose
// consumers consult them as a precedence ladder: git runs GIT_SSH_COMMAND
// before GIT_SSH (an askpass client reads GIT_ASKPASS before SSH_ASKPASS),
// and the editors claude and codex shell out to read VISUAL before EDITOR.
// Forwarding ANY member through session_env_passthrough exempts the WHOLE
// group from the re-assert — the operator's choice must win whichever name
// a tool reads. Measured on git 2.43 why per-name exemption is not enough:
// forwarding GIT_SSH=/op-ssh while GIT_SSH_COMMAND=ssh is re-asserted
// leaves the re-asserted name ahead in precedence, so the operator's
// program never runs; forwarding EDITOR=vim while VISUAL=/bin/false is
// re-asserted hides the operator's editor behind the sibling. The
// unforwarded member still cannot keep an image value — the twin
// forwarding in credentialMountSelectorForwarding reseats it to the
// operator's forwarded value in the SIBLING's own grammar, so every name
// in the group resolves to the same operator program (#7531).
var credentialMountSelectorGroups = [][2]string{
	{"GIT_SSH", "GIT_SSH_COMMAND"},
	{"GIT_ASKPASS", "SSH_ASKPASS"},
	{"EDITOR", "VISUAL"},
}

// credentialMountSelectorReseat re-expresses a forwarded member's value in
// the SIBLING's own grammar — a verbatim copy is only faithful between
// names a consumer reads the same way (#7531):
//   - GIT_SSH_COMMAND is a shell COMMAND LINE git parses and runs through
//     the shell, while GIT_SSH is a program path exec'd directly. A path
//     with whitespace, quotes, or shell metacharacters lands verbatim as
//     several words — measured on git 2.43, GIT_SSH='/op dir/op-ssh'
//     copied raw fails `/op: not found` and the operator's program never
//     runs. The sibling value is therefore shell-quoted (' -> '"'"') —
//     measured byte-identical to running the path under GIT_SSH alone.
//   - GIT_SSH is a program path: a command line cannot be expressed as
//     one. When the forwarded command is not already a single bare word
//     (shellquote.Quote leaves a bare path unchanged), no faithful
//     translation exists, so the sibling keeps af's own working default —
//     git's own `ssh` — which can never name a repository program and
//     sits behind the forwarded command in precedence anyway.
//   - The askpass and editor pairs are same-grammar (a program path or
//     command each consumer reads identically), so the sibling takes the
//     value verbatim.
func credentialMountSelectorReseat(sibling, value string) string {
	switch sibling {
	case "GIT_SSH_COMMAND":
		return shellquote.Quote(value)
	case "GIT_SSH":
		if shellquote.Quote(value) == value {
			return value
		}
		return credentialMountSharedProgramEntries["GIT_SSH"]
	}
	return value
}

// credentialMountTwinForwarding normalizes a name pair (proxy case
// spellings, or a program-selector precedence group) so the unforwarded
// spelling cannot keep an image-supplied value: every spelling is in the
// inner filter's always-admitted set, so exempting a twin from the pin or
// re-assert without reseating it would leave the image's value reachable
// by clients that prefer that spelling (Python's getproxies prefers a set
// lowercase name over an uppercase one; git consults GIT_SSH_COMMAND
// before GIT_SSH). For each pair the operator claimed through
// session_env_passthrough, the first forwarded-and-host-set spelling
// supplies the value V; every other spelling of the pair is emitted
// `-e NAME` and the docker client environment synthesizes `NAME=V`, so
// both spellings resolve to the operator's value. A pair member that is
// itself forwarded AND set keeps its own approved value. When no
// forwarded spelling is set on the host the pair contributes nothing —
// the "claimed a name they do not set" semantics already lets the image's
// values stand for names the operator forwarded empty.
//
// names is the extra `-e NAME` set for docker run; synth is `NAME=V`
// entries appended to the docker client's filtered environment, which is
// where a bare `-e NAME` resolves its value — values therefore never
// appear in argv.
func credentialMountTwinForwarding(pairs [][2]string, passthrough []string, environ []string) (names, synth []string) {
	forwarded := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		forwarded[name] = struct{}{}
	}
	host := make(map[string]string, len(environ))
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok {
			host[name] = value
		}
	}
	for _, pair := range pairs {
		var value string
		claimed := false
		for _, s := range pair {
			if _, ok := forwarded[s]; !ok {
				continue
			}
			if v, set := host[s]; set {
				value, claimed = v, true
				break
			}
		}
		if !claimed {
			continue
		}
		for _, s := range pair {
			_, fwd := forwarded[s]
			_, set := host[s]
			if fwd && set {
				continue
			}
			names = append(names, s)
			synth = append(synth, s+"="+value)
		}
	}
	return names, synth
}

// credentialMountSelectorForwarding is credentialMountTwinForwarding for
// the selector groups with each sibling's synthesized value re-expressed
// in the sibling's own grammar (credentialMountSelectorReseat) — a path
// copied verbatim into a command-line slot would split on the operator's
// whitespace and run nothing.
func credentialMountSelectorForwarding(passthrough []string, environ []string) (names, synth []string) {
	names, synth = credentialMountTwinForwarding(credentialMountSelectorGroups, passthrough, environ)
	for i, entry := range synth {
		name, value, _ := strings.Cut(entry, "=")
		synth[i] = name + "=" + credentialMountSelectorReseat(name, value)
	}
	return names, synth
}

// credentialMountChainForwarding is credentialMountTwinForwarding over a
// precedence chain of more than two members: the first forwarded-and-
// host-set member supplies the value every other member is reseated to,
// so no lower member keeps an image-supplied or af-set value a reader
// could chain through (#7889's CA-bundle chain).
func credentialMountChainForwarding(chain []string, passthrough, environ []string) (names, synth []string) {
	forwarded := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		forwarded[name] = struct{}{}
	}
	host := make(map[string]string, len(environ))
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok {
			host[name] = value
		}
	}
	var value string
	claimed := false
	for _, s := range chain {
		if _, ok := forwarded[s]; !ok {
			continue
		}
		if v, set := host[s]; set {
			value, claimed = v, true
			break
		}
	}
	if !claimed {
		return nil, nil
	}
	for _, s := range chain {
		_, fwd := forwarded[s]
		_, set := host[s]
		if fwd && set {
			continue
		}
		names = append(names, s)
		synth = append(synth, s+"="+value)
	}
	return names, synth
}

// credentialMountGroupForwarding applies the twin forwarding to every
// pair table — proxy case spellings, the CA-bundle precedence chain, and
// selector precedence groups share the same claim semantics (#7531).
func credentialMountGroupForwarding(passthrough []string, environ []string) (names, synth []string) {
	names, synth = credentialMountTwinForwarding(credentialMountProxyCasePairs, passthrough, environ)
	n, s := credentialMountSelectorForwarding(passthrough, environ)
	names, synth = append(names, n...), append(synth, s...)
	n, s = credentialMountChainForwarding(credentialMountTrustBundleGroup, passthrough, environ)
	return append(names, n...), append(synth, s...)
}
