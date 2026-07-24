package config

// ConfigSource identifies one layer that may participate in resolving a
// configuration value. The order of these constants is the canonical
// low-to-high precedence order; an entry's Precedence contains only the layers
// that actually participate for that key.
type ConfigSource uint8

const (
	// SourceInvalid is the zero value. It is never a real source: keeping zero
	// invalid makes an omitted precedence choice fail the manifest tests.
	SourceInvalid ConfigSource = iota
	// SourceBuiltIn is the value compiled into af.
	SourceBuiltIn
	// SourceGlobal is the user's machine-wide config under AGENT_FACTORY_HOME.
	SourceGlobal
	// SourceLegacyRepo is the deprecated machine-local repos/<path-hash> source.
	// It remains a read-only compatibility candidate for the two keys that used
	// to live there, but is not an allowed destination in ManifestEntry.Sources.
	SourceLegacyRepo
	// SourceRepoShared is <repo-root>/.agent-factory/config.{toml,json}.
	SourceRepoShared
	// SourceProjectPersonal is the approved future machine-local project layer.
	// Stage one defines the vocabulary but no entry admits it until that schema
	// and its read/write paths exist.
	SourceProjectPersonal
	// SourceInvocation is an explicit value supplied by the operation being
	// resolved. There is no universal CLI/environment layer today, so no entry
	// includes it until a real caller supplies one.
	SourceInvocation

	configSourceCount
)

func (s ConfigSource) String() string {
	switch s {
	case SourceBuiltIn:
		return "built-in"
	case SourceGlobal:
		return "global"
	case SourceLegacyRepo:
		return "legacy repo"
	case SourceRepoShared:
		return "repo-shared"
	case SourceProjectPersonal:
		return "personal project"
	case SourceInvocation:
		return "invocation"
	default:
		return "invalid"
	}
}

// SourceSet is the set of supported configuration locations in which a key
// may be declared. Built-in values and read-only compatibility candidates can
// appear in Precedence, but are deliberately absent from Sources: this field
// describes real user-facing configuration surfaces, not every resolver input.
type SourceSet uint16

// Has reports whether source is present in the set. Invalid and out-of-range
// sources are always absent.
func (s SourceSet) Has(source ConfigSource) bool {
	if source <= SourceInvalid || source >= configSourceCount {
		return false
	}
	return s&(SourceSet(1)<<source) != 0
}

const (
	sourceGlobalOnly SourceSet = SourceSet(1) << SourceGlobal
	sourceRepoOnly   SourceSet = SourceSet(1) << SourceRepoShared
	sourceGlobalRepo SourceSet = sourceGlobalOnly | sourceRepoOnly

	// sourcePersonalOnly is the machine-local per-project layer on its own. It is
	// never used alone (a key that admits personal always also admits global),
	// but naming the bit keeps the composed sets below readable.
	sourcePersonalOnly SourceSet = SourceSet(1) << SourceProjectPersonal
	// sourceGlobalPersonal admits a key globally and as a per-project personal
	// override, with no checked-in in-repo layer (a pure preference key such as
	// branch_prefix that a repository has no business dictating).
	sourceGlobalPersonal SourceSet = sourceGlobalOnly | sourcePersonalOnly
	// sourceGlobalRepoPersonal admits a key globally, in-repo (shared), and as a
	// per-project personal override — the full preference chain for keys such as
	// default_program and program_overrides.
	sourceGlobalRepoPersonal SourceSet = sourceGlobalRepo | sourcePersonalOnly
)

// MergePolicy says how successively higher-precedence present values combine.
// Its zero value is invalid so every new manifest entry must choose explicitly.
type MergePolicy uint8

const (
	MergeInvalid MergePolicy = iota
	// MergeReplace means the higher source replaces the lower value as a unit.
	MergeReplace
	// MergeMapByKey means map entries resolve independently by key.
	MergeMapByKey
	// MergeTableByField means fields of a structured value resolve independently.
	MergeTableByField
	// MergeListReplace means a present higher list replaces the lower list; it
	// never appends implicitly.
	MergeListReplace
)

func (m MergePolicy) String() string {
	switch m {
	case MergeReplace:
		return "replace"
	case MergeMapByKey:
		return "map-by-key"
	case MergeTableByField:
		return "table-by-field"
	case MergeListReplace:
		return "list-replace"
	default:
		return "invalid"
	}
}

// ConfigFormat identifies an on-disk config encoding.
type ConfigFormat uint8

const (
	// FormatInvalid is the zero value, never a real format.
	FormatInvalid ConfigFormat = iota
	FormatTOML
	FormatJSON

	configFormatCount
)

func (f ConfigFormat) String() string {
	switch f {
	case FormatTOML:
		return "TOML"
	case FormatJSON:
		return "JSON"
	default:
		return "invalid"
	}
}

// FormatSet is the set of on-disk encodings in which an entry is recognized
// across its supported Sources. Its zero value is invalid.
type FormatSet uint8

// Has reports whether format is present in the set. Invalid and out-of-range
// formats are always absent.
func (s FormatSet) Has(format ConfigFormat) bool {
	if format <= FormatInvalid || format >= configFormatCount {
		return false
	}
	return s&(FormatSet(1)<<format) != 0
}

const (
	formatTOMLOnly FormatSet = FormatSet(1) << FormatTOML
	formatTOMLJSON FormatSet = formatTOMLOnly | FormatSet(1)<<FormatJSON
)

// Shared precedence slices are immutable package data. Every public manifest
// accessor deep-copies them before returning entries to a caller.
var (
	precedenceGlobal     = []ConfigSource{SourceBuiltIn, SourceGlobal}
	precedenceGlobalRepo = []ConfigSource{SourceBuiltIn, SourceGlobal, SourceRepoShared}
	precedenceRepo       = []ConfigSource{SourceBuiltIn, SourceRepoShared}
	precedenceLegacyRepo = []ConfigSource{SourceBuiltIn, SourceLegacyRepo, SourceRepoShared}
	// precedenceGlobalPersonal and precedenceGlobalRepoPersonal place the
	// personal-project layer directly ABOVE the checked-in in-repo value: the
	// shared file is the team default, and the whole point of a machine-local
	// per-project override is to be able to beat it on this machine. The resolver
	// sorts documents by ConfigSource ordinal, so SourceProjectPersonal (which is
	// defined after SourceRepoShared) lands in exactly this position regardless of
	// append order; these slices only have to name the layers.
	precedenceGlobalPersonal     = []ConfigSource{SourceBuiltIn, SourceGlobal, SourceProjectPersonal}
	precedenceGlobalRepoPersonal = []ConfigSource{SourceBuiltIn, SourceGlobal, SourceRepoShared, SourceProjectPersonal}
)
