package sessionenv

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const straceOracleSkipReason = "strace arity oracle skipped: strace executable is not installed"

type installedStraceLongOption struct {
	name   string
	hasArg uint32
	flag   uint64
	value  uint32
}

type elfSectionImage struct {
	address uint64
	data    []byte
}

func TestStraceSeparateValueTableCoversInstalledBinary(t *testing.T) {
	stracePath := installedStracePath(t)
	options := readInstalledStraceLongOptions(t, stracePath)
	probe := filepath.Join(t.TempDir(), "missing", "child")
	missing := make([]string, 0)
	separateCount := 0
	for _, option := range options {
		if option.hasArg != 1 { // required_argument in getopt.h
			continue
		}
		separateCount++
		name := "--" + option.name
		output, _ := exec.Command(stracePath, name, probe).CombinedOutput()
		require.NotContains(t, string(output), "Cannot stat '"+probe+"'",
			"installed strace did not consume %s's required operand", name)
		if _, covered := straceLongOptionsWithSeparateValue[name]; !covered {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	t.Logf("strace arity oracle ran: derived %d long options and probed %d separate-value options from %s",
		len(options), separateCount, stracePath)
	require.Empty(t, missing,
		"installed strace has separate-value long options missing from the child-boundary table")
}

func TestStraceEquivalentAliasPrefixesMatchInstalledOracle(t *testing.T) {
	stracePath := installedStracePath(t)
	options := readInstalledStraceLongOptions(t, stracePath)
	probe := filepath.Join(t.TempDir(), "missing", "child")
	for _, test := range []struct {
		option string
		value  string
	}{
		{"--decode-pi", "comm"},
		{"--signa", "none"},
		{"--trace-f", "3"},
	} {
		t.Run(test.option, func(t *testing.T) {
			if _, accepted := installedStracePrefixMatch(options, test.option); !accepted {
				t.Skipf("installed strace does not accept alias prefix %s", test.option)
			}
			output, _ := exec.Command(stracePath, test.option, test.value, probe).CombinedOutput()
			require.Contains(t, string(output), probe,
				"strace did not reach the nonexistent child; output: %s", output)
			require.Contains(t, string(output), "Cannot stat",
				"strace rejected the shared alias prefix instead of executing its child; output: %s", output)
		})
	}
}

func TestStracePrefixResolverMatchesInstalledGetoptTable(t *testing.T) {
	stracePath := installedStracePath(t)
	options := readInstalledStraceLongOptions(t, stracePath)
	checked := 0
	seen := make(map[string]struct{})
	refused := make([]string, 0)
	for _, option := range options {
		fullName := "--" + option.name
		for length := 3; length <= len(fullName); length++ {
			prefix := fullName[:length]
			if _, alreadyChecked := seen[prefix]; alreadyChecked {
				continue
			}
			seen[prefix] = struct{}{}
			expected, accepted := installedStracePrefixMatch(options, prefix)
			if !accepted {
				continue
			}
			checked++
			canonical, result := classifyStraceLongOption(prefix)
			if result == straceOptionUnsafe {
				// A cross-version union can contain an inequivalent option that
				// makes a host-unique prefix undecidable (for example, strace 6.8's
				// --col after newer --color is added). Refusal is the documented
				// safe residual; equivalent aliases must instead resolve below.
				refused = append(refused, prefix)
				continue
			}
			if expected.name == "help" || expected.name == "version" {
				require.Equal(t, straceOptionStops, result,
					"installed strace accepts terminal prefix %s", prefix)
				continue
			}
			require.Equal(t, straceOptionContinue, result,
				"installed strace accepts prefix %s without ambiguity", prefix)
			if expected.hasArg == 1 {
				require.NotEmpty(t, canonical,
					"installed strace consumes a separate value for prefix %s", prefix)
			} else {
				require.Empty(t, canonical,
					"installed strace keeps the next word as the child for prefix %s", prefix)
			}
		}
	}
	sort.Strings(refused)
	t.Logf("strace prefix oracle checked %d accepted exact names and abbreviations; "+
		"refused %d because the cross-version table has inequivalent matches: %v",
		checked, len(refused), refused)
}

func installedStracePrefixMatch(
	options []installedStraceLongOption,
	prefix string,
) (installedStraceLongOption, bool) {
	for _, option := range options {
		if "--"+option.name == prefix {
			return option, true // getopt_long gives exact names precedence.
		}
	}
	var match installedStraceLongOption
	found := false
	for _, option := range options {
		if !strings.HasPrefix("--"+option.name, prefix) {
			continue
		}
		if !found {
			match = option
			found = true
			continue
		}
		if option.hasArg != match.hasArg || option.flag != match.flag || option.value != match.value {
			return installedStraceLongOption{}, false
		}
	}
	return match, found
}

func installedStracePath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("strace")
	if errors.Is(err, exec.ErrNotFound) {
		t.Skip(straceOracleSkipReason)
	}
	require.NoError(t, err, "locate strace for the arity oracle")
	return path
}

func readInstalledStraceLongOptions(t *testing.T, path string) []installedStraceLongOption {
	t.Helper()
	file, err := elf.Open(path)
	require.NoError(t, err, "open installed strace as ELF")
	t.Cleanup(func() { require.NoError(t, file.Close()) })

	pointerSize, entrySize := 0, 0
	switch file.Class {
	case elf.ELFCLASS32:
		pointerSize, entrySize = 4, 16
	case elf.ELFCLASS64:
		pointerSize, entrySize = 8, 32
	default:
		t.Fatalf("unsupported strace ELF class %v", file.Class)
	}
	images := loadELFSectionImages(t, file)
	best := make([]installedStraceLongOption, 0)
	for _, image := range images {
		for offset := 0; offset+entrySize <= len(image.data); offset += pointerSize {
			options, ok := readLongOptionSequence(
				image.data[offset:], images, file.ByteOrder, pointerSize, entrySize,
			)
			if ok && len(options) > len(best) && looksLikeStraceLongOptions(options) {
				best = options
			}
		}
	}
	require.GreaterOrEqual(t, len(best), 20,
		"derive strace's getopt_long table from the installed binary")
	return best
}

func loadELFSectionImages(t *testing.T, file *elf.File) []elfSectionImage {
	t.Helper()
	images := make([]elfSectionImage, 0, len(file.Sections))
	for _, section := range file.Sections {
		if section.Type == elf.SHT_NOBITS || section.Flags&elf.SHF_ALLOC == 0 ||
			section.Flags&elf.SHF_EXECINSTR != 0 {
			continue
		}
		data, err := section.Data()
		require.NoError(t, err, "read ELF section %s", section.Name)
		images = append(images, elfSectionImage{address: section.Addr, data: data})
	}
	return images
}

func readLongOptionSequence(
	data []byte,
	images []elfSectionImage,
	order binary.ByteOrder,
	pointerSize int,
	entrySize int,
) ([]installedStraceLongOption, bool) {
	options := make([]installedStraceLongOption, 0)
	for offset := 0; offset+entrySize <= len(data); offset += entrySize {
		entry := data[offset : offset+entrySize]
		nameAddress := readELFPointer(entry, 0, pointerSize, order)
		hasArg := order.Uint32(entry[pointerSize : pointerSize+4])
		flag := readELFPointer(entry, pointerSize*2, pointerSize, order)
		value := order.Uint32(entry[pointerSize*3 : pointerSize*3+4])
		if nameAddress == 0 {
			return options, len(options) > 0 && hasArg == 0 && flag == 0 && value == 0
		}
		name, ok := readELFLongOptionName(images, nameAddress)
		if !ok || hasArg > 2 || flag != 0 {
			return nil, false
		}
		options = append(options, installedStraceLongOption{
			name: name, hasArg: hasArg, flag: flag, value: value,
		})
	}
	return nil, false
}

func readELFPointer(data []byte, offset int, size int, order binary.ByteOrder) uint64 {
	if size == 4 {
		return uint64(order.Uint32(data[offset : offset+size]))
	}
	return order.Uint64(data[offset : offset+size])
}

func readELFLongOptionName(images []elfSectionImage, address uint64) (string, bool) {
	for _, image := range images {
		if address < image.address || address >= image.address+uint64(len(image.data)) {
			continue
		}
		data := image.data[address-image.address:]
		end := bytes.IndexByte(data, 0)
		if end <= 0 || end > 128 {
			return "", false
		}
		name := string(data[:end])
		if name[0] < 'a' || name[0] > 'z' || strings.IndexFunc(name[1:], func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-'
		}) >= 0 {
			return "", false
		}
		return name, true
	}
	return "", false
}

func looksLikeStraceLongOptions(options []installedStraceLongOption) bool {
	found := map[string]bool{}
	for _, option := range options {
		found[option.name] = true
	}
	return found["env"] && found["help"] && found["output"] && found["version"]
}
