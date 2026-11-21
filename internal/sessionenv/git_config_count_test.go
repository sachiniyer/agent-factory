package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// parseGitConfigCount mirrors git's strtoul on GIT_CONFIG_COUNT: leading
// whitespace and one optional sign, then base-10 digits to the end of the
// string — any tail git's *endp check would call "bogus", any count above
// INT_MAX or strtoul-ERANGE git would call "too many entries", yields
// ok=false. Every case below was verified against git 2.43.
func TestParseGitConfigCount(t *testing.T) {
	cases := map[string]struct {
		want int
		ok   bool
	}{
		"":                     {0, true}, // strtoul("") is 0 with *endp at NUL
		"0":                    {0, true},
		"+0":                   {0, true},
		"-0":                   {0, true}, // strtoul("-0") is 0 — git reads a zero count
		" -0":                  {0, true},
		"1":                    {1, true},
		"+1":                   {1, true}, // git's strtoul accepts the leading '+'
		" 2":                   {2, true}, // strtoul skips leading whitespace
		"007":                  {7, true},
		"2147483647":           {2147483647, true}, // INT_MAX is a valid count to git
		"abc":                  {0, false},         // no conversion, *endp != 0
		" ":                    {0, false},
		"2 ":                   {0, false}, // trailing garbage
		"+ 1":                  {0, false}, // a sign must be followed by digits
		"+":                    {0, false},
		"12abc":                {0, false},
		"1e3":                  {0, false},
		"0x10":                 {0, false},
		"-1":                   {0, false}, // strtoul wraps to ULONG_MAX: "too many"
		"2147483648":           {0, false}, // INT_MAX+1: "too many"
		"99999999999999999999": {0, false}, // strtoul ERANGE: "too many"
	}
	for value, want := range cases {
		got, ok := parseGitConfigCount(value)
		require.Equal(t, want.ok, ok, "parseGitConfigCount(%q) ok", value)
		require.Equal(t, want.want, got, "parseGitConfigCount(%q) count", value)
	}
}

// gitConfigPairIndex requires the literal spelling git asks for:
// "GIT_CONFIG_KEY_00" is a different environment name, not index 0.
func TestGitConfigPairIndex(t *testing.T) {
	for name, want := range map[string]int{
		"GIT_CONFIG_KEY_0":          0,
		"GIT_CONFIG_KEY_7":          7,
		"GIT_CONFIG_VALUE_0":        0,
		"GIT_CONFIG_KEY_2147483647": 2147483647,
	} {
		got, ok := gitConfigPairIndex(name, "GIT_CONFIG_KEY_")
		if strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			got, ok = gitConfigPairIndex(name, "GIT_CONFIG_VALUE_")
		}
		require.True(t, ok, name)
		require.Equal(t, want, got, name)
	}
	for _, name := range []string{
		"GIT_CONFIG_KEY_", "GIT_CONFIG_KEY_00", "GIT_CONFIG_KEY_01",
		"GIT_CONFIG_KEY_+0", "GIT_CONFIG_KEY_-0", "GIT_CONFIG_KEY_ 0",
		"GIT_CONFIG_KEY_1x", "GIT_CONFIG_KEY_2147483648",
		"GIT_CONFIG_KEY_99999999999999999999", "GIT_CONFIG_KEY_A",
		"GIT_CONFIG_VALUE_X0",
	} {
		if _, ok := gitConfigPairIndex(name, "GIT_CONFIG_KEY_"); ok && !strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			t.Errorf("gitConfigPairIndex(%q) accepted a non-literal index", name)
		}
	}
}

// NeutralizeIncompleteGitConfigCount pins the count empty when a live count
// arrives without its required pairs — including a KEY that is defined but
// empty, which git fails with "empty config key" — and leaves complete or
// inert counts untouched.
func TestNeutralizeIncompleteGitConfigCount(t *testing.T) {
	t.Run("count N>0 with no pairs -> pinned empty", func(t *testing.T) {
		in := []string{"PATH=/bin", "GIT_CONFIG_COUNT=2", "HOME=/h"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok)
		v, present := envEntryValue(out, "GIT_CONFIG_COUNT")
		require.True(t, present)
		require.Empty(t, v, "the count must be pinned defined-empty")
		require.Contains(t, out, "PATH=/bin")
		require.Contains(t, out, "HOME=/h")
	})

	t.Run("count N>0 with only some pairs -> pinned empty", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok)
		v, _ := envEntryValue(out, "GIT_CONFIG_COUNT")
		require.Empty(t, v)
	})

	t.Run("count +1 with no pairs -> pinned empty (git's strtoul accepts the sign)", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=+1"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok, "a live +1 count with no pairs must be neutralized — git fails it with missing config key")
		v, _ := envEntryValue(out, "GIT_CONFIG_COUNT")
		require.Empty(t, v)
	})

	t.Run("count INT_MAX with no pairs -> pinned empty, bounded work", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=2147483647"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok)
		v, _ := envEntryValue(out, "GIT_CONFIG_COUNT")
		require.Empty(t, v)
	})

	t.Run("count git rejects above INT_MAX -> untouched for git's own diagnostic", func(t *testing.T) {
		for _, in := range [][]string{
			{"GIT_CONFIG_COUNT=2147483648"},
			{"GIT_CONFIG_COUNT=99999999999999999999"},
			{"GIT_CONFIG_COUNT=abc"},
			{"GIT_CONFIG_COUNT=12abc"},
			{"GIT_CONFIG_COUNT=-1"},
		} {
			out, ok := NeutralizeIncompleteGitConfigCount(in)
			require.False(t, ok, "%v: a count git itself rejects is left to git's own diagnostic, not iterated or neutralized", in)
			require.Equal(t, in, out)
		}
	})

	t.Run("defined-empty KEY counts as missing -> pinned empty", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0=v"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok, "git fails GIT_CONFIG_KEY_0= with 'empty config key' exactly as it fails an absent one")
		v, _ := envEntryValue(out, "GIT_CONFIG_COUNT")
		require.Empty(t, v)
	})

	t.Run("defined-empty VALUE is a legal empty value -> untouched", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0="}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok, "an empty GIT_CONFIG_VALUE_0 sets the key to empty — a working set af must not touch")
		require.Equal(t, in, out)
	})

	t.Run("count N>0 with all required pairs -> untouched", func(t *testing.T) {
		in := []string{
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=k0", "GIT_CONFIG_VALUE_0=v0",
			"GIT_CONFIG_KEY_1=k1", "GIT_CONFIG_VALUE_1=v1",
		}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok)
		require.Equal(t, in, out, "a complete forward must pass through unchanged")
	})

	t.Run("count 0 -> untouched", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=0"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok)
		require.Equal(t, in, out)
	})

	t.Run("count defined-empty -> untouched", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT="}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok)
		require.Equal(t, in, out)
	})

	t.Run("count absent -> untouched", func(t *testing.T) {
		in := []string{"PATH=/bin"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok)
		require.Equal(t, in, out)
	})

	t.Run("duplicated count entry is replaced exactly once", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=3", "GIT_CONFIG_COUNT=3"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok)
		seen := 0
		for _, e := range out {
			if e == "GIT_CONFIG_COUNT=" {
				seen++
			}
		}
		require.Equal(t, 1, seen, "the count must be blanked once, not duplicated")
	})
}

// GitConfigCountForwardMissingPairs reports the bounded missing-pair
// report for the docker account path: capped names in index order, exact
// total, and a defined-but-empty KEY counted as missing.
func TestGitConfigCountForwardMissingPairs(t *testing.T) {
	count := func(v string, pairs ...string) []string {
		out := append([]string{"PATH=/bin"}, "GIT_CONFIG_COUNT="+v)
		return append(out, pairs...)
	}

	t.Run("count forwarded live without pairs -> all 2N missing in index order", func(t *testing.T) {
		names, total := GitConfigCountForwardMissingPairs(true,
			[]string{"GIT_CONFIG_COUNT"}, count("2"))
		require.Equal(t, []string{
			"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
			"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1",
		}, names)
		require.Equal(t, int64(4), total)
	})

	t.Run("some pairs forwarded -> names only the missing", func(t *testing.T) {
		names, total := GitConfigCountForwardMissingPairs(true,
			[]string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"},
			count("2", "GIT_CONFIG_KEY_0=k0", "GIT_CONFIG_VALUE_0=v0"))
		require.Equal(t, []string{"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"}, names)
		require.Equal(t, int64(2), total)
	})

	t.Run("complete forward -> complete", func(t *testing.T) {
		names, total := GitConfigCountForwardMissingPairs(true,
			[]string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"},
			count("1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v"))
		require.Empty(t, names)
		require.Zero(t, total)
	})

	t.Run("forwarded KEY unset or empty in source counts as missing", func(t *testing.T) {
		// Forwarded-but-unset under gemini blanking emits GIT_CONFIG_KEY_0=
		// and a forwarded empty value delivers it — git fails both with
		// "empty config key".
		for _, source := range [][]string{
			count("1"),
			count("1", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0=v"),
		} {
			names, total := GitConfigCountForwardMissingPairs(true,
				[]string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"}, source)
			require.Equal(t, []string{"GIT_CONFIG_KEY_0"}, names, "source=%v", source)
			require.Equal(t, int64(1), total)
		}
	})

	t.Run("forwarded VALUE unset is missing only where no blank delivers it", func(t *testing.T) {
		passthrough := []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"}
		source := count("1", "GIT_CONFIG_KEY_0=k")
		// gemini's loader blanking emits GIT_CONFIG_VALUE_0= — a legal
		// empty value, so the forward is complete.
		names, total := GitConfigCountForwardMissingPairs(true, passthrough, source)
		require.Empty(t, names)
		require.Zero(t, total)
		// For an agent that emits no pair blank the name stays absent —
		// "missing config value" — so the forward is incomplete.
		names, total = GitConfigCountForwardMissingPairs(false, passthrough, source)
		require.Equal(t, []string{"GIT_CONFIG_VALUE_0"}, names)
		require.Equal(t, int64(1), total)
	})

	t.Run("count not forwarded -> complete (not the operator's forward)", func(t *testing.T) {
		names, total := GitConfigCountForwardMissingPairs(true, nil, count("5"))
		require.Nil(t, names)
		require.Zero(t, total)
	})

	t.Run("count forwarded but absent or inert in source -> complete", func(t *testing.T) {
		for _, source := range [][]string{
			{"PATH=/bin"},
			count("0"),
			count(""),
			count("abc"),        // git itself rejects: not a count to iterate
			count("2147483648"), // above INT_MAX: "too many entries"
		} {
			names, total := GitConfigCountForwardMissingPairs(true,
				[]string{"GIT_CONFIG_COUNT"}, source)
			require.Nil(t, names, "source=%v", source)
			require.Zero(t, total, "source=%v", source)
		}
	})

	t.Run("passthrough names are trimmed before membership is checked", func(t *testing.T) {
		names, total := GitConfigCountForwardMissingPairs(true,
			[]string{" GIT_CONFIG_COUNT "}, count("1"))
		require.Equal(t, []string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"}, names)
		require.Equal(t, int64(2), total)
	})

	t.Run("enormous count -> capped names, exact total, bounded work", func(t *testing.T) {
		names, total := GitConfigCountForwardMissingPairs(true,
			[]string{"GIT_CONFIG_COUNT"}, count("2147483647"))
		require.Len(t, names, missingPairReportCap,
			"the report names the first missing indices rather than all 2N")
		require.Equal(t, "GIT_CONFIG_KEY_0", names[0])
		require.Equal(t, "GIT_CONFIG_VALUE_0", names[1])
		require.Equal(t, 2*int64(2147483647), total)
	})
}

// missingGitConfigCountPairsInEnv is the exec-shim diagnostic: the bounded
// report over the final environment.
func TestMissingGitConfigCountPairsInEnv(t *testing.T) {
	names, total := missingGitConfigCountPairsInEnv(
		[]string{"GIT_CONFIG_COUNT=5"})
	require.Equal(t, missingPairReportCap, len(names),
		"the diagnostic lists the first missing indices rather than all 2N")
	require.Equal(t, "GIT_CONFIG_KEY_0", names[0])
	require.Equal(t, "GIT_CONFIG_VALUE_0", names[1])
	require.Equal(t, int64(10), total)

	names, total = missingGitConfigCountPairsInEnv(
		[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0=v"})
	require.Equal(t, []string{"GIT_CONFIG_KEY_0"}, names,
		"a defined-empty KEY counts as missing — git fails it with 'empty config key'")
	require.Equal(t, int64(1), total)

	names, total = missingGitConfigCountPairsInEnv(
		[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0="})
	require.Empty(t, names)
	require.Zero(t, total, "a defined-empty VALUE is a legal empty value")

	names, total = missingGitConfigCountPairsInEnv(
		[]string{"GIT_CONFIG_COUNT=99999999999999999999"})
	require.Nil(t, names)
	require.Zero(t, total, "a count git itself rejects is not iterated")
}

// FormatGitConfigMissingPairs renders the capped names plus the exact
// remainder so a huge count still fits a log line.
func TestFormatGitConfigMissingPairs(t *testing.T) {
	require.Equal(t, "GIT_CONFIG_KEY_0, GIT_CONFIG_VALUE_0",
		FormatGitConfigMissingPairs(
			[]string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"}, 2))
	require.Equal(t, "GIT_CONFIG_KEY_0, … and 3 more",
		FormatGitConfigMissingPairs([]string{"GIT_CONFIG_KEY_0"}, 4))
}
