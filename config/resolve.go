package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// inRepoHashFileName is the per-repo state file holding the sha256 of the
// last in-repo config content that was announced in the log. Observability
// only — it gates nothing.
const inRepoHashFileName = "inrepo-config-hash"

// legacyDeprecationLogged dedupes the legacy-location deprecation warning to
// once per repo per process.
var legacyDeprecationLogged sync.Map

// ResolvedConfig is effective configuration plus the provenance produced by
// the same manifest-driven pass. Every consumer of per-repo configuration
// (programs, remote hooks, post-worktree commands) must go through this file's
// ResolveConfig or ResolveConfigForInspection entry point rather than reading
// source files directly, so precedence and scoping stay uniform.
type ResolvedConfig struct {
	// Config carries the effective app-level fields. DefaultProgram and
	// ProgramOverrides may have been overridden/merged from the in-repo
	// file; the global-only fields (AutoYes, AutoUpdate, DaemonPollInterval,
	// BranchPrefix, DetachKeys) always come from the global config because
	// LoadInRepoConfig rejects them per-repo.
	Config

	// PostWorktreeCommands are the effective post-worktree hooks: the
	// in-repo value when its key is present (even empty), otherwise the
	// legacy ~/.agent-factory/repos/<id>/config.json value.
	PostWorktreeCommands []string `config:"post_worktree_commands"`
	// RemoteHooks is the effective remote hook backend config, with the
	// same in-repo-then-legacy resolution as PostWorktreeCommands. Command
	// values that were relative filesystem paths have been rewritten to
	// absolute paths under repoRoot (#834); consumers can exec them without
	// caring about the process cwd.
	RemoteHooks *RemoteHooks `config:"remote_hooks"`

	// Backend is the effective `backend` runtime selector (#1592 Phase 4 PR3):
	// one of local|docker|ssh|hook, empty meaning local. In-repo only — there
	// is no legacy per-repo location for it. Validated by the session package
	// when it resolves the runtime, not here.
	Backend string `config:"backend"`
	// Docker/SSH parameterize the docker/ssh runtimes; non-nil only when the
	// repo's in-repo config declares the corresponding section.
	Docker *DockerConfig `config:"docker"`
	SSH    *SSHConfig    `config:"ssh"`

	// ProjectRoot is empty for ResolveGlobalConfig and is the repository root
	// supplied to ResolveConfig. Presentation code may replace it with an
	// equivalent lexical spelling through RebaseProjectPathsForDisplay.
	ProjectRoot string `json:"-" toml:"-"`

	// Resolution is produced by the same manifest-driven pass that populated
	// the effective fields above. Renderers consume it directly; they never
	// reconstruct precedence from the finished Config.
	Resolution []ResolvedValue `json:"-" toml:"-"`
}

// ResolveGlobalConfig resolves the built-in and global layers only. It backs
// the historical global `af config get/list` contract while still returning
// presence-aware provenance for --explain.
func ResolveGlobalConfig() (*ResolvedConfig, error) {
	global, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	documents, err := globalResolutionDocuments(global)
	if err != nil {
		return nil, err
	}
	return materializeResolution(global, "", Manifest(), documents, false)
}

// ResolveConfig returns effective configuration and provenance for repoRoot
// (normally the main worktree root from RepoFromPath or CurrentRepo). It also
// records the existing per-repo load observation for command-bearing checked-in
// config. Inspection-only callers must use ResolveConfigForInspection so a read
// cannot create that durable state.
func ResolveConfig(repoRoot string) (*ResolvedConfig, error) {
	return resolveConfig(repoRoot, recordInRepoLoadObservation)
}

// ResolveConfigForInspection returns the same effective values and provenance
// as ResolveConfig without logging or persisting the per-repo load observation.
// It is the resolver for read surfaces such as `af config --project`. This is
// deliberately not called a generally write-free load: LoadConfig retains its
// documented first-run and legacy-format migration behavior.
func ResolveConfigForInspection(repoRoot string) (*ResolvedConfig, error) {
	return resolveConfig(repoRoot, suppressInRepoLoadObservation)
}

type inRepoLoadObservation uint8

const (
	suppressInRepoLoadObservation inRepoLoadObservation = iota
	recordInRepoLoadObservation
)

// resolveConfig is the one value/provenance path for runtime and inspection
// callers. The typed observation mode is the only behavior difference.
func resolveConfig(repoRoot string, observation inRepoLoadObservation) (*ResolvedConfig, error) {
	if observation != suppressInRepoLoadObservation && observation != recordInRepoLoadObservation {
		return nil, fmt.Errorf("invalid in-repo load observation mode %d", observation)
	}
	global, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	documents, err := globalResolutionDocuments(global)
	if err != nil {
		return nil, err
	}

	repoID := RepoIDFromRoot(repoRoot)
	legacy, err := LoadRepoConfig(repoID)
	if err != nil {
		return nil, err
	}

	inRepo, raw, err := LoadInRepoConfig(repoRoot)
	if err != nil {
		return nil, err
	}
	if inRepo != nil && observation == recordInRepoLoadObservation {
		logInRepoConfigLoaded(repoID, repoRoot, inRepo, raw)
	}

	documents = append(documents, sourceDocument{
		layer:    SourceLegacyRepo,
		metadata: legacy.source,
		schemas:  []any{legacy},
	})
	if inRepo == nil {
		inRepo = &InRepoConfig{source: sourceMetadata{
			path:   InRepoTomlConfigPath(repoRoot),
			format: FormatTOML,
		}}
	}
	documents = append(documents, sourceDocument{
		layer:    SourceRepoShared,
		metadata: inRepo.source,
		schemas:  []any{inRepo},
	})

	res, err := materializeResolution(global, repoRoot, AllManifest(), documents, true)
	if err != nil {
		return nil, err
	}

	// Rewrite relative hook command paths to absolute against repoRoot
	// (#834). This is the single chokepoint for the rewrite: every exec of a
	// hook command — launch/list/attach/delete/terminal, startup import,
	// restore liveness, preview — receives its RemoteHooks from ResolveConfig, so
	// resolving here covers them all. repoRoot is the main worktree root, so
	// sessions in linked worktrees resolve hooks against the repository whose
	// config file was loaded, never against a worktree path. The rewrite
	// applies to the legacy-location value too, so both sources behave
	// identically.
	if res.RemoteHooks != nil {
		before := res.RemoteHooks
		res.RemoteHooks = res.RemoteHooks.resolveCommandPaths(repoRoot)
		if !jsonEquivalent(before, res.RemoteHooks) {
			annotateResolutionWinner(res, "remote_hooks", "relative command paths resolved against the project root")
		}
	}
	refreshResolutionValues(res)

	warnLegacyRepoConfig(repoID, repoRoot, legacy, inRepo)
	return res, nil
}

func globalResolutionDocuments(global *Config) ([]sourceDocument, error) {
	if global == nil || global.source.builtIn == nil {
		return nil, fmt.Errorf("loaded global config is missing its built-in source snapshot")
	}
	metadata := global.source
	if metadata.path == "" {
		path, err := globalConfigTomlPath()
		if err != nil {
			return nil, err
		}
		metadata.path = path
		metadata.format = FormatTOML
	}
	return []sourceDocument{
		{
			layer:   SourceBuiltIn,
			schemas: []any{global.source.builtIn, defaultInRepoConfig()},
		},
		{
			layer:          SourceGlobal,
			metadata:       metadata,
			schemas:        []any{global},
			valueSemantics: sourceValueSnapshot,
		},
	}, nil
}

func materializeResolution(global *Config, projectRoot string, entries []ManifestEntry, documents []sourceDocument, requireAllSources bool) (*ResolvedConfig, error) {
	computed, err := resolveManifest(entries, documents, requireAllSources)
	if err != nil {
		return nil, err
	}
	res := &ResolvedConfig{Config: *global, ProjectRoot: projectRoot}
	res.Resolution = make([]ResolvedValue, 0, len(computed))
	for _, value := range computed {
		if err := setResolvedConfigValue(res, value.resolved.Key, value.value); err != nil {
			return nil, err
		}
		value.resolved.Value = clonedInterface(value.value)
		res.Resolution = append(res.Resolution, value.resolved)
	}
	refreshResolutionValues(res)
	return res, nil
}

func setResolvedConfigValue(res *ResolvedConfig, key string, value reflect.Value) error {
	if field, ok := taggedFieldByKey(reflect.ValueOf(&res.Config), key); ok {
		return assignResolvedField(key, field, value)
	}
	if field, ok := taggedFieldByKey(reflect.ValueOf(res), key); ok {
		return assignResolvedField(key, field, value)
	}
	return fmt.Errorf("resolved manifest key %q has no destination field", key)
}

func assignResolvedField(key string, field, value reflect.Value) error {
	if !field.CanSet() {
		return fmt.Errorf("resolved manifest key %q destination is not settable", key)
	}
	if !value.IsValid() {
		field.Set(reflect.Zero(field.Type()))
		return nil
	}
	if !value.Type().AssignableTo(field.Type()) {
		return fmt.Errorf("resolved manifest key %q has type %s, destination wants %s", key, value.Type(), field.Type())
	}
	field.Set(value)
	return nil
}

func refreshResolutionValues(res *ResolvedConfig) {
	for i := range res.Resolution {
		resolved := &res.Resolution[i]
		value, ok := effectiveResolvedValue(res, resolved.Key)
		if ok {
			if resolved.Key == "keys" && !jsonEquivalent(resolved.Value, value) {
				annotateResolvedValueSources(resolved, "effective key bindings normalize each configured value to a list")
			}
			resolved.Value = value
		}
	}
}

func annotateResolvedValueSources(value *ResolvedValue, note string) {
	layers := make(map[string]bool)
	if value.Winner != nil {
		layers[value.Winner.Layer] = true
	}
	for _, origin := range value.Origins {
		layers[origin.Layer] = true
	}
	for i := range value.Candidates {
		candidate := &value.Candidates[i]
		if layers[candidate.Layer] && candidate.Present && candidate.Allowed {
			candidate.Reason += "; " + note
		}
	}
}

func annotateResolutionWinner(res *ResolvedConfig, key, note string) {
	for i := range res.Resolution {
		value := &res.Resolution[i]
		if value.Key != key || value.Winner == nil {
			continue
		}
		for j := range value.Candidates {
			candidate := &value.Candidates[j]
			if candidate.Layer == value.Winner.Layer && candidate.Result == "winner" {
				candidate.Reason += "; " + note
				return
			}
		}
	}
}

func effectiveResolvedValue(res *ResolvedConfig, key string) (any, bool) {
	if res == nil {
		return nil, false
	}
	if key == "keys" {
		return cloneExportedValue(reflect.ValueOf(res.KeymapOverrides())).Interface(), true
	}
	if field, ok := taggedFieldByKey(reflect.ValueOf(&res.Config), key); ok {
		return clonedInterface(field), true
	}
	if field, ok := taggedFieldByKey(reflect.ValueOf(res), key); ok {
		return clonedInterface(field), true
	}
	return nil, false
}

// ResolvedValue returns one key's effective value and provenance.
func (r *ResolvedConfig) ResolvedValue(key string) (ResolvedValue, bool) {
	if r == nil {
		return ResolvedValue{}, false
	}
	for _, value := range r.Resolution {
		if value.Key == key {
			return value, true
		}
	}
	return ResolvedValue{}, false
}

// logInRepoConfigLoaded emits one INFO line the first time a command-bearing
// in-repo config is seen for a repo, and again whenever its content hash
// changes. The last-announced hash is tracked in the per-repo state dir
// purely to decide when to re-log; it gates nothing.
func logInRepoConfigLoaded(repoID, repoRoot string, inRepo *InRepoConfig, raw []byte) {
	fields := inRepo.CommandBearingFields()
	if len(fields) == 0 {
		return
	}
	hash := InRepoConfigHash(raw)
	dir, err := repoStateDir(repoID)
	if err != nil {
		log.WarningLog.Printf("failed to resolve state dir for repo %s: %v", repoID, err)
		return
	}
	hashPath := filepath.Join(dir, inRepoHashFileName)
	if prev, err := os.ReadFile(hashPath); err == nil && strings.TrimSpace(string(prev)) == hash {
		return
	}
	log.InfoLog.Printf("loaded in-repo config for %s: %s (sha256 %s)", repoRoot, strings.Join(fields, ", "), hash[:8])
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.WarningLog.Printf("failed to create repo state dir %s: %v", dir, err)
		return
	}
	if err := AtomicWriteFile(hashPath, []byte(hash+"\n"), 0644); err != nil {
		log.WarningLog.Printf("failed to record in-repo config hash for %s: %v", repoRoot, err)
	}
}

// warnLegacyRepoConfig logs (once per repo per process) when fields from the
// deprecated legacy location are still in effect — i.e. present there and not
// shadowed by the in-repo file. The legacy location keeps working for one
// release so existing setups don't break mid-upgrade (#800).
func warnLegacyRepoConfig(repoID, repoRoot string, legacy *RepoConfig, inRepo *InRepoConfig) {
	var fields []string
	if len(legacy.PostWorktreeCommands) > 0 && !inRepo.IsSet("post_worktree_commands") {
		fields = append(fields, "post_worktree_commands")
	}
	if legacy.RemoteHooks != nil && !inRepo.IsSet("remote_hooks") {
		fields = append(fields, "remote_hooks")
	}
	if len(fields) == 0 {
		return
	}
	if _, loaded := legacyDeprecationLogged.LoadOrStore(repoID, true); loaded {
		return
	}
	// Derive the legacy path from the same resolver the read uses
	// (repoConfigPath -> repoStateDir -> GetConfigDir) so the warning names
	// the real file even when AGENT_FACTORY_HOME relocates the config dir
	// (#890). If resolution fails, still warn but omit the now-unknown source
	// path rather than printing a wrong one.
	_, legacyPath, err := repoConfigPath(repoID)
	if err != nil {
		log.WarningLog.Printf("deprecated: %s is still read from the legacy per-repo config location; move it to %s — the legacy location stops working in a future release",
			strings.Join(fields, ", "), InRepoConfigPath(repoRoot))
		return
	}
	log.WarningLog.Printf("deprecated: %s is still read from %s; move it to %s — the legacy location stops working in a future release",
		strings.Join(fields, ", "), prettyHomePath(legacyPath), InRepoConfigPath(repoRoot))
}
