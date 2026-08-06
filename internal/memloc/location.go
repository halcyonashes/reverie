// Package memloc adds a second, repository-local memory store alongside the
// global one in ~/.engram.
//
// The global store is shared by every project on the machine. The local store
// lives inside a single repository, at <repoRoot>/.claude/engram/engram.db, and
// holds knowledge that should not leak beyond that repo.
//
// A repository opts in by the mere existence of the <repoRoot>/.claude/engram
// directory — there is no config to parse and no inheritance from ancestor
// directories. A repository that has not opted in behaves exactly as it did
// before this package existed.
package memloc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reverie/internal/store"
)

// Location names which physical store an operation targets.
type Location string

const (
	// Global targets ~/.engram (or ENGRAM_DATA_DIR).
	Global Location = "global"
	// Local targets <repoRoot>/.claude/engram.
	Local Location = "local"
	// Both targets local and global together. Reads only.
	Both Location = "both"
)

// ErrNoLocalStore is returned when an operation explicitly asks for the local
// store in a repository that has not opted in.
var ErrNoLocalStore = errors.New("no local memory store in this repository")

// ParamDescription is the shared description for the MCP `location` argument.
const ParamDescription = "Which memory store to use: local (this repo's .claude/engram) | global (~/.engram) | both. " +
	"Reads default to both. Pass local when the user says \"reverie local\" or \"recall local\", " +
	"global when they say \"reverie global\" or \"recall global\"."

// dirName is the local store directory, relative to the repository root.
var dirName = filepath.Join(".claude", "engram")

// configName is the optional per-repo settings file inside the local store dir.
const configName = "config.json"

// localConfig is the on-disk shape of <repoRoot>/.claude/engram/config.json.
type localConfig struct {
	// DefaultWrite selects where writes land when the caller does not say.
	// "global" (the default) or "local".
	DefaultWrite string `json:"default_write"`
}

// LocalDirFor returns the local store directory for a repository root.
// It returns "" when repoRoot is empty.
func LocalDirFor(repoRoot string) string {
	if strings.TrimSpace(repoRoot) == "" {
		return ""
	}
	return filepath.Join(repoRoot, dirName)
}

// HasLocal reports whether repoRoot has opted in to local memory.
func HasLocal(repoRoot string) bool {
	dir := LocalDirFor(repoRoot)
	if dir == "" {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// InitLocal creates the local store directory for repoRoot and opens it once so
// the schema is migrated. It is idempotent. Returns the directory created.
func InitLocal(repoRoot string) (string, error) {
	dir := LocalDirFor(repoRoot)
	if dir == "" {
		return "", errors.New("memloc: cannot initialize local memory without a repository root")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("memloc: resolve %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return "", fmt.Errorf("memloc: create %q: %w", abs, err)
	}
	s, _, err := openAt(abs)
	if err != nil {
		return "", err
	}
	defer s.Close()
	return abs, nil
}

// openAt opens (and migrates) a store rooted at an absolute directory, then
// raises its id sequence past store.LocalIDFence.
//
// Fencing on every open — not just at creation — is what makes id routing robust:
// a migration that rebuilds a table cannot silently drop local ids back into the
// global store's range.
func openAt(dir string) (*store.Store, string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, "", fmt.Errorf("memloc: resolve %q: %w", dir, err)
	}
	s, err := store.New(store.FallbackConfig(abs))
	if err != nil {
		return nil, "", fmt.Errorf("memloc: open local store at %q: %w", abs, err)
	}
	warning, err := s.ApplyLocalIDFence()
	if err != nil {
		_ = s.Close()
		return nil, "", fmt.Errorf("memloc: fence local store ids at %q: %w", abs, err)
	}
	return s, warning, nil
}

// Set is the pair of stores an MCP server operates on. Local is nil whenever
// the repository has not opted in, which is the common case.
type Set struct {
	Global *store.Store
	Local  *store.Store

	// LocalDir is the local store directory. Non-empty even when Local is nil,
	// so callers can tell the user where the store *would* live.
	LocalDir string

	// Warning is a non-fatal advisory: the repo opted in but the local store
	// could not be opened. Global-only operation continues.
	Warning string

	defaultWrite Location
}

// GlobalOnly wraps a single global store as a Set with no local store, so
// callers that predate repo-local memory behave exactly as before.
func GlobalOnly(global *store.Store) *Set {
	return &Set{Global: global, defaultWrite: Global}
}

// Open pairs an already-open global store with the repository-local store for
// repoRoot, if that repository has opted in.
//
// Opening the local store is deliberately tolerant: a corrupt or unreadable
// local database degrades to global-only operation with a Warning rather than
// taking down the whole MCP server.
//
// The opt-in check happens before store.New, because store.New would create
// the directory itself and thereby opt every repository in.
func Open(global *store.Store, repoRoot string) *Set {
	set := &Set{
		Global:       global,
		LocalDir:     LocalDirFor(repoRoot),
		defaultWrite: Global,
	}
	if !HasLocal(repoRoot) {
		return set
	}

	local, fenceWarning, err := openAt(set.LocalDir)
	if err != nil {
		set.Warning = fmt.Sprintf("local memory store at %s could not be opened (%v) — using global memory only", set.LocalDir, err)
		return set
	}
	set.Local = local
	set.Warning = fenceWarning
	set.defaultWrite = readDefaultWrite(set.LocalDir)
	return set
}

// OpenLocalOnly opens just the repository-local store, for CLI commands that
// inspect it without needing the global store. Local is nil when the repository
// has not opted in.
func OpenLocalOnly(repoRoot string) *Set {
	return Open(nil, repoRoot)
}

// WriteDefaultWrite persists the default write target into the local store's
// config file.
func WriteDefaultWrite(localDir string, loc Location) error {
	if loc != Local && loc != Global {
		return fmt.Errorf("memloc: default_write must be local or global, got %q", loc)
	}
	data, err := json.MarshalIndent(localConfig{DefaultWrite: string(loc)}, "", "  ")
	if err != nil {
		return fmt.Errorf("memloc: encode local config: %w", err)
	}
	path := filepath.Join(localDir, configName)
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		return fmt.Errorf("memloc: write %q: %w", path, err)
	}
	return nil
}

// readDefaultWrite reads default_write from the local config, falling back to
// Global. An unreadable or malformed config is not an error — writes simply
// keep their historical destination.
func readDefaultWrite(localDir string) Location {
	data, err := os.ReadFile(filepath.Join(localDir, configName))
	if err != nil {
		return Global
	}
	var cfg localConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Global
	}
	if strings.EqualFold(strings.TrimSpace(cfg.DefaultWrite), string(Local)) {
		return Local
	}
	return Global
}

// HasLocalStore reports whether this Set has a usable local store.
func (s *Set) HasLocalStore() bool {
	return s != nil && s.Local != nil
}

// DefaultWrite returns where writes land when the caller does not specify.
// Always Global when there is no local store, and Global for a Set that was
// built as a literal rather than through Open — Set has exported fields, so an
// unset default must resolve to the safe, shared store rather than to nothing.
func (s *Set) DefaultWrite() Location {
	if !s.HasLocalStore() || s.defaultWrite != Local {
		return Global
	}
	return Local
}

// Store returns the store for a single-target location.
func (s *Set) Store(loc Location) (*store.Store, error) {
	switch loc {
	case Local:
		if !s.HasLocalStore() {
			return nil, ErrNoLocalStore
		}
		return s.Local, nil
	case Global:
		return s.Global, nil
	default:
		return nil, fmt.Errorf("memloc: %q is not a single-target location", loc)
	}
}

// Close closes the local store. The global store is owned by the caller.
func (s *Set) Close() error {
	if s.HasLocalStore() {
		return s.Local.Close()
	}
	return nil
}

// Normalize turns a raw `location` argument into a usable Location.
//
// An empty argument falls back to def, which callers set to Both for reads and
// to Set.DefaultWrite() for writes. An explicit "local" in a repository without
// a local store is an error rather than a silent fall back to global — sending a
// memory the user asked to keep local into the shared store is the worst
// failure mode available here.
func Normalize(raw string, hasLocal bool, def Location) (Location, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		// Without a local store there is nothing to target or merge, so any
		// default collapses to global. This keeps repositories that never opted
		// in on exactly their pre-fork path.
		if !hasLocal {
			return Global, nil
		}
		return def, nil
	}

	switch Location(v) {
	case Local:
		if !hasLocal {
			return "", ErrNoLocalStore
		}
		return Local, nil
	case Global:
		return Global, nil
	case Both:
		if !hasLocal {
			return Global, nil
		}
		return Both, nil
	default:
		return "", fmt.Errorf("memloc: unknown location %q — use local, global, or both", v)
	}
}

// Targets expands a location into the ordered stores to operate on. Local is
// listed first so that merged output leads with the repository's own memory.
func (s *Set) Targets(loc Location) ([]Target, error) {
	switch loc {
	case Both:
		targets := make([]Target, 0, 2)
		if s.HasLocalStore() {
			targets = append(targets, Target{Location: Local, Store: s.Local})
		}
		targets = append(targets, Target{Location: Global, Store: s.Global})
		return targets, nil
	case Local, Global:
		st, err := s.Store(loc)
		if err != nil {
			return nil, err
		}
		return []Target{{Location: loc, Store: st}}, nil
	default:
		return nil, fmt.Errorf("memloc: unknown location %q", loc)
	}
}

// Target is one store paired with the location that selected it.
type Target struct {
	Location Location
	Store    *store.Store
}
