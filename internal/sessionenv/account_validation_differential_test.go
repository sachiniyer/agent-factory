package sessionenv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// accountValidationVerdictsGolden holds ValidateAccountEnvironmentCommand's
// verdict for every command in accountValidationCorpus. The #4966 corpus was
// recorded from master 8c33e1c533 BEFORE #4966 made the walk linear; the
// #4968 shapes appended after it were recorded from master 4a0c09273b BEFORE
// #4968 made the wrapper option runs linear, with every earlier verdict
// unchanged. The walk is a fail-closed security check, so a performance fix
// may not move any verdict in either direction; this file is the old
// implementation's answer sheet.
//
// Regenerate only when a change is MEANT to move verdicts, and say which ones
// in the PR: AF_UPDATE_GOLDENS=1 go test ./internal/sessionenv/ -run
// TestAccountValidationVerdictsMatchGolden
const accountValidationVerdictsGolden = "testdata/account_validation_verdicts.golden"

// accountValidationTokens is the corpus alphabet: the wrappers the walk peels
// or scans (modeled, unmodeled, and shells), env's option and assignment
// spellings, denied and allowed names, dynamic words, and trusted shell forms.
var accountValidationTokens = []string{
	"echo", "env", "/usr/bin/env", "./env", "strace", "nohup", "nice", "nice -n 5",
	"timeout 5", "setsid", "stdbuf -o0", "ionice -c 2", "ionice -c", "taskset 1",
	"taskset -p", "xargs", "exec", "command", "builtin", "unset", "export", "printf -v",
	"sh", "sh -c", "/bin/sh", "/bin/sh -i", "/bin/bash --noprofile --norc -i", "/bin/zsh -f -i",
	"-u", "-uCODEX_HOME", "--unset=CODEX_HOME", "-i", "-", "--", "-C", "/tmp", "-v", "-S",
	"-E", "--env", "-ECODEX_HOME=1", "--env=CODEX_HOME=x", "--process-slot-var=CODEX_HOME",
	"CODEX_HOME=/x", "FOO=1", "A=$x", "CODEX_HOME=$x", "ZDOTDIR=/z", "BASH_ENV=/e",
	"CODEX_HOME", "FOO", "a", "x", "codex", `"$y"`, "$(z)", "-p", "123", "-f", "--noprofile",
}

// accountValidationCoreTokens is the smaller alphabet enumerated at length 3.
var accountValidationCoreTokens = []string{
	"echo", "env", "strace", "nice -n", "xargs", "exec", "sh", "/bin/sh -i",
	"-u", "-i", "--", "-C", "-E", "CODEX_HOME=/x", "FOO=1", "A=$x", "CODEX_HOME",
	"a", `"$y"`, "-p",
}

var accountValidationSuffixes = []string{
	"", "", "", " <f", " | cat", "; /bin/sh -i", " && /bin/sh -i <f", "; unset CODEX_HOME",
}

// accountValidationCorpus is deterministic: exhaustive short commands, seeded
// random mixes, nested env/strace/echo wrapper stacks with assignments and
// options spliced in, and long argument lists. Env stacks stay at depth <= 12
// and argument lists at <= 400 words so the pre-#4966 implementation that
// recorded the golden could finish.
func accountValidationCorpus() []string {
	var corpus []string
	for _, a := range accountValidationTokens {
		corpus = append(corpus, a)
		for _, b := range accountValidationTokens {
			corpus = append(corpus, a+" "+b)
		}
	}
	for _, a := range accountValidationCoreTokens {
		for _, b := range accountValidationCoreTokens {
			for _, c := range accountValidationCoreTokens {
				corpus = append(corpus, a+" "+b+" "+c)
			}
		}
	}

	rng := rand.New(rand.NewPCG(4966, 1))
	pick := func(tokens []string) string { return tokens[rng.IntN(len(tokens))] }
	for range 12000 {
		n := 4 + rng.IntN(11)
		words := make([]string, 0, n)
		envs := 0
		for len(words) < n {
			token := pick(accountValidationTokens)
			if token == "env" || token == "/usr/bin/env" || token == "./env" {
				if envs == 10 {
					continue
				}
				envs++
			}
			words = append(words, token)
		}
		corpus = append(corpus, strings.Join(words, " ")+pick(accountValidationSuffixes))
	}

	heads := []string{"", "echo ", "strace ", "nohup ", "nice -n 5 ", "strace -f "}
	splices := []string{
		"CODEX_HOME=/x", "FOO=1", "A=$x", "-u CODEX_HOME", "-u FOO", "-i", "--", "-v",
		"-C /tmp", "-u env", "-E CODEX_HOME=1", "sh -c 'unset CODEX_HOME'", "/bin/sh -i", `"$y"`,
		"strace", "echo", "nohup",
	}
	tails := []string{"x", "codex", "", "unset CODEX_HOME", "/bin/sh -i", `"$y"`}
	for depth := 1; depth <= 12; depth++ {
		for _, head := range heads {
			for _, tail := range tails {
				corpus = append(corpus, head+strings.Repeat("env ", depth)+tail)
			}
			for range 40 {
				words := make([]string, 0, 2*depth)
				for range depth {
					if rng.IntN(3) == 0 {
						words = append(words, pick(splices))
					}
					words = append(words, pick([]string{"env", "env", "env", "strace", "echo", "nohup"}))
					if rng.IntN(4) == 0 {
						words = append(words, pick(splices))
					}
				}
				corpus = append(corpus, head+strings.Join(words, " ")+" "+pick(tails)+pick(accountValidationSuffixes))
			}
		}
	}

	fillers := []string{"a", "-p", "123", "FOO=1", "x"}
	hazards := []string{
		"", "", "env CODEX_HOME=/x codex", `"$y"`, "/bin/sh -i", "sh -c x", "-E CODEX_HOME",
		"--env=CODEX_HOME=1", "env -u CODEX_HOME codex", "A=$x", "env",
	}
	for _, n := range []int{50, 100, 200, 400} {
		for _, head := range []string{"echo", "strace", "env", "nice -n 5 echo", "env -u FOO echo", "xargs"} {
			for _, filler := range fillers {
				for _, hazard := range hazards {
					words := make([]string, n)
					for i := range words {
						words[i] = filler
					}
					if hazard != "" {
						words[rng.IntN(n)] = hazard
					}
					corpus = append(corpus, head+" "+strings.Join(words, " "))
				}
			}
		}
	}
	return append(corpus, accountValidationWrapperOptionCorpus()...)
}

// accountValidationWrapperOptionTokens is the alphabet for #4968's shapes:
// the modeled wrappers' value-taking options and the words their operands and
// xargs's -I marker regions can hold.
var accountValidationWrapperOptionTokens = []string{
	"nice", "-n", "--adjustment", "-5", "timeout", "-k", "-s", "--signal", "5", "--foreground",
	"stdbuf", "-o", "-oL", "-i", "--output", "ionice", "-c", "-c2", "-t", "-p", `-c"$C"`,
	"--class", "taskset", "-a", "1", "--cpu-list", "setsid", "-f", "-w", "--help",
	"xargs", "-I", "{}", "-I{}", "-i", "-E", "-0", "--replace", "--replace=X", "X", "-P",
	"--max-args", "--process-slot-var", "env", "-u", "CODEX_HOME", "CODEX_HOME={}", "{}=1",
	"X=1", "FOO=1", "A=$x", "x", "codex", `"$y"`, "--", "sh", "-C", "/tmp",
}

// accountValidationWrapperOptionCorpus covers #4968: runs of value-taking
// options whose operands are themselves wrapper heads (`nice -n nice -n …`),
// nested xargs layers, and xargs -I marker regions across env words. Chains
// stay at depth <= 10 so the pre-#4968 walk that recorded the golden could
// finish.
func accountValidationWrapperOptionCorpus() []string {
	var corpus []string
	rng := rand.New(rand.NewPCG(4968, 1))
	pick := func(tokens []string) string { return tokens[rng.IntN(len(tokens))] }

	links := []string{
		"nice -n", "nice --adjustment", "timeout -k", "timeout -s", "timeout", "stdbuf -o", "stdbuf -i",
		"ionice -c", "ionice -n", "taskset", "taskset -c", "xargs -E", "xargs -I", "xargs -n",
		"xargs -a", "xargs --max-args", "xargs --process-slot-var", "xargs -P", "setsid -f",
	}
	tails := []string{
		"x", "codex", "env CODEX_HOME=/x codex", "env -u CODEX_HOME codex", "sh -c x", "unset CODEX_HOME",
		`"$y"`, "1 x", "1 env CODEX_HOME=/x codex", "-- x", "--help env CODEX_HOME=/x codex",
	}
	for _, link := range links {
		for depth := 1; depth <= 10; depth++ {
			for _, tail := range tails {
				corpus = append(corpus, strings.TrimSpace(strings.Repeat(link+" ", depth))+" "+tail)
			}
		}
	}
	for range 3000 {
		depth := 1 + rng.IntN(6)
		words := make([]string, 0, 2*depth+2)
		for range depth {
			words = append(words, pick(links))
		}
		corpus = append(corpus, strings.Join(words, " ")+" "+pick(tails)+pick(accountValidationSuffixes))
	}
	for range 6000 {
		n := 3 + rng.IntN(12)
		words := make([]string, 0, n+1)
		words = append(words, pick([]string{"nice", "timeout", "stdbuf", "ionice", "taskset", "xargs", "setsid", "xargs -I{}", "xargs -I X"}))
		for len(words) < n {
			words = append(words, pick(accountValidationWrapperOptionTokens))
		}
		corpus = append(corpus, strings.Join(words, " ")+pick(accountValidationSuffixes))
	}

	markers := []string{"xargs -I{}", "xargs -I X", "xargs -i", "xargs --replace", "xargs --replace=X", `xargs -I"$m"`, "xargs", "xargs -0"}
	envArgs := []string{"-u", "env", "FOO=1", "X=1", "{}=1", "CODEX_HOME={}", "{}", "X", "-i", "--", `"$y"`, "A=$x", "codex", "echo"}
	for _, head := range markers {
		for range 300 {
			words := []string{head}
			for range 1 + rng.IntN(4) {
				words = append(words, pick([]string{"env", "echo env", "nice env", "env env"}))
				for range rng.IntN(4) {
					words = append(words, pick(envArgs))
				}
			}
			corpus = append(corpus, strings.Join(words, " ")+pick([]string{"", " x", " codex", " {}"}))
		}
	}

	// Loop-state collisions: an option operand that is itself the wrapper
	// starts a nested layer in the loop's INITIAL state, at a position the
	// outer loop reaches later in the state its earlier options set. The
	// memo must key on that state; each block below has commands whose
	// verdict flips if it does not (ionice's proof scope, xargs's marker).
	for _, first := range []string{`-c"$C"`, `-n"$N"`, "-c2", "-t", `-p"$P"`, "--classdata=3", "-n4"} {
		for _, link := range []string{"-c", "-n", "--class"} {
			for _, next := range []string{
				"-p 1", "--help", "-t", "-c3", `-c"$C"`, `-n"$N"`, "-P 1", "-u 1", "--version", "--class=idle", "--", "",
			} {
				for _, tail := range []string{"x", "env CODEX_HOME=/x codex", "1 x"} {
					corpus = append(corpus, strings.Join(strings.Fields("ionice "+first+" "+link+" ionice "+next+" "+tail), " "))
				}
			}
		}
	}
	xargsModes := []string{"-I A", "-IA", "--replace=A", "-i", "--replace", "-I{}", `-I"$m"`, "-0", "-n 1", ""}
	for _, outer := range xargsModes {
		for _, link := range []string{"-E", "-n", "-a", "--max-args", "-P", "-d"} {
			for _, inner := range xargsModes {
				for _, command := range []string{
					"env A=1 codex", "env {}=1 codex", "env -u A codex", "env X codex", "env -- A codex",
					"env A", "echo env {}=1", "env",
				} {
					corpus = append(corpus, strings.Join(strings.Fields("xargs "+outer+" "+link+" xargs "+inner+" "+command), " "))
				}
			}
		}
	}

	for _, n := range []int{50, 200} {
		for _, link := range []string{"nice -n", "timeout -k", "stdbuf -o", "ionice -c", "xargs -E", "xargs -I", "xargs -n 1 xargs"} {
			for _, tail := range []string{"x", "env CODEX_HOME=/x codex", "1 env -u CODEX_HOME codex"} {
				corpus = append(corpus, strings.Repeat(link+" ", n)+tail)
			}
		}
		for _, command := range []string{
			"xargs a" + strings.Repeat(" env", n) + " x",
			strings.Repeat("xargs env ", n/10) + "x",
			"xargs -I{} echo env" + strings.Repeat(" -u env", n) + " x",
			"xargs -I{} echo env" + strings.Repeat(" -u env", n) + " {}",
			"xargs -I{} echo" + strings.Repeat(" env", n) + " x",
		} {
			corpus = append(corpus, command)
		}
	}
	return corpus
}

// accountValidationVerdict encodes one command's outcome as a single byte:
// which ValidateAccountEnvironmentCommand refusal fired (or none), plus
// commandFeedsProvenShell's own answer, which Validate short-circuits past
// whenever the mutation walk refuses first.
func accountValidationVerdict(command string) byte {
	class := 0
	if err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()); err != nil {
		switch message := err.Error(); {
		case strings.Contains(message, "is not a bash `set -o`"):
			class = 2
		case strings.Contains(message, "shell arithmetic"):
			class = 3
		case strings.Contains(message, "sets an identity or shell-startup variable"):
			class = 1
		case strings.Contains(message, "gives an interactive shell input"):
			class = 4
		default:
			class = 5
		}
	}
	feeds := 0
	if commandFeedsProvenShell(command) {
		feeds = 1
	}
	return byte('A' + class*2 + feeds)
}

func accountValidationCorpusDigest(corpus []string) string {
	sum := sha256.New()
	for _, command := range corpus {
		sum.Write([]byte(command))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// TestAccountValidationVerdictsMatchGolden is the differential oracle for
// #4966: every verdict the linear walk returns must equal the one the
// exponential/quadratic walk returned for the same command.
func TestAccountValidationVerdictsMatchGolden(t *testing.T) {
	corpus := accountValidationCorpus()
	verdicts := make([]byte, len(corpus))
	for i, command := range corpus {
		verdicts[i] = accountValidationVerdict(command)
	}
	digest := accountValidationCorpusDigest(corpus)

	if os.Getenv("AF_UPDATE_GOLDENS") != "" {
		var out strings.Builder
		fmt.Fprintf(&out, "corpus-sha256 %s\ncommands %d\n", digest, len(corpus))
		for start := 0; start < len(verdicts); start += 100 {
			out.Write(verdicts[start:min(start+100, len(verdicts))])
			out.WriteByte('\n')
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(accountValidationVerdictsGolden), 0o755))
		require.NoError(t, os.WriteFile(accountValidationVerdictsGolden, []byte(out.String()), 0o644))
		return
	}

	raw, err := os.ReadFile(accountValidationVerdictsGolden)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	require.Equal(t, "corpus-sha256 "+digest, lines[0],
		"the generated corpus changed; the golden verdicts no longer describe it")
	require.Equal(t, fmt.Sprintf("commands %d", len(corpus)), lines[1])
	want := strings.Join(lines[2:], "")
	require.Len(t, want, len(corpus))

	mismatches := 0
	for i, command := range corpus {
		if verdicts[i] != want[i] {
			mismatches++
			if mismatches <= 20 {
				t.Errorf("verdict changed for %q: golden %c, now %c", command, want[i], verdicts[i])
			}
		}
	}
	require.Zero(t, mismatches, "%d of %d verdicts differ from the golden recorded before the walk changed", mismatches, len(corpus))
}
