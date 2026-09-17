package cache

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

// §8.2 on-disk layout.
//
//	$CACHE/v1/auth-resolution.json          §7.0(e) resolved auth-collection slug
//	$CACHE/v1/<scope>/manifest.json         the index (§7.8.1), meta+fingerprint folded in
//	$CACHE/v1/<scope>/fields/<slug>.json    one field shard per entity (§7.8.2)
//	$CACHE/v1/<scope>/fields/_global_<slug>.json
//	$CACHE/v1/<scope>/graphql/<Type>.json   per-type raw introspection
//	$CACHE/v1/<scope>/.lock
const (
	// Epoch is the cache-format epoch. A directory under any other vN is
	// garbage-collected rather than read (§8.2).
	Epoch = "v1"

	fileManifest       = "manifest.json"
	fileLockName       = ".lock"
	fileAuthResolution = "auth-resolution.json"
	fileGCStamp        = ".gc-stamp"
	dirFields          = "fields"
	dirGraphQL         = "graphql"

	// globalShardPrefix distinguishes a global's shard from a collection's,
	// so a global and a collection may share a slug without colliding.
	globalShardPrefix = "_global_"

	// FilePerm is the mode of every cache file; DirPerm of every directory
	// (§4.1: files 0600, directories 0700).
	FilePerm fs.FileMode = 0o600
	DirPerm              = fsatomic.DirPerm
)

// EntityKind distinguishes the two shard namespaces.
type EntityKind string

const (
	KindCollection EntityKind = "collection"
	KindGlobal     EntityKind = "global"
)

// slugPattern is deliberately strict: a shard name is turned into a filesystem
// path, so anything that could escape the scope directory is rejected outright
// rather than cleaned.
var (
	slugPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	typeNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	epochPattern    = regexp.MustCompile(`^v[0-9]+$`)
)

// ValidSlug reports whether slug is safe to use in a shard file name.
func ValidSlug(slug string) bool {
	return slugPattern.MatchString(slug) && !strings.Contains(slug, "..")
}

// ShardName returns the manifest-relative shard path for an entity, matching
// §7.8.1's fields_shard values ("fields/pages.json", "fields/_global_nav.json").
func ShardName(slug string, kind EntityKind) string {
	if kind == KindGlobal {
		return dirFields + "/" + globalShardPrefix + slug + ".json"
	}
	return dirFields + "/" + slug + ".json"
}

// ValidShardRef reports whether a manifest's fields_shard value is a relative
// path this package is willing to open.
func ValidShardRef(ref string) bool {
	if ref == "" || strings.ContainsAny(ref, `\:`) || strings.Contains(ref, "..") {
		return false
	}
	if !strings.HasPrefix(ref, dirFields+"/") {
		return false
	}
	base := strings.TrimPrefix(ref, dirFields+"/")
	if !strings.HasSuffix(base, ".json") {
		return false
	}
	name := strings.TrimSuffix(base, ".json")
	name = strings.TrimPrefix(name, globalShardPrefix)
	return ValidSlug(name)
}

// Root returns the cache root ($CACHE). It is exposed for `pay cache path`.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// EpochDir returns $CACHE/v1.
func (s *Store) EpochDir() string { return filepath.Join(s.Root(), Epoch) }

// ScopeDir returns $CACHE/v1/<scope>.
func (s *Store) ScopeDir(sc Scope) string { return filepath.Join(s.EpochDir(), sc.Key) }

// ManifestPath returns $CACHE/v1/<scope>/manifest.json.
func (s *Store) ManifestPath(sc Scope) string { return filepath.Join(s.ScopeDir(sc), fileManifest) }

// LockPath returns $CACHE/v1/<scope>/.lock.
func (s *Store) LockPath(sc Scope) string { return filepath.Join(s.ScopeDir(sc), fileLockName) }

// AuthResolutionPath returns $CACHE/v1/auth-resolution.json. It lives beside the
// scope directories, not inside one, so it survives scope GC (§7.0(e)).
func (s *Store) AuthResolutionPath() string { return filepath.Join(s.EpochDir(), fileAuthResolution) }

// shardPath resolves a manifest-relative shard reference to an absolute path.
func (s *Store) shardPath(sc Scope, ref string) (string, bool) {
	if !ValidShardRef(ref) {
		return "", false
	}
	return filepath.Join(s.ScopeDir(sc), filepath.FromSlash(ref)), true
}

// graphQLPath resolves $CACHE/v1/<scope>/graphql/<Type>.json.
func (s *Store) graphQLPath(sc Scope, typeName string) (string, bool) {
	if !typeNamePattern.MatchString(typeName) {
		return "", false
	}
	return filepath.Join(s.ScopeDir(sc), dirGraphQL, typeName+".json"), true
}

// AuthResolution is one §7.0(e) record: the auth-collection slug resolved for a
// (normURL, keyFP) pair. The pair itself is hashed, so the file never reveals
// which servers or credentials a user has.
type AuthResolution struct {
	AuthCollection string    `json:"auth_collection"`
	ResolvedAt     time.Time `json:"resolved_at"`
	CLIMajor       int       `json:"cli_major"`
}

// AuthResolutionKey hashes (normURL, keyFP) the way §7.0(e) specifies. It is
// independent of graphql_path and of the extra-header set: the auth collection
// a credential belongs to cannot change because a bypass header was added.
func AuthResolutionKey(sc Scope) string {
	return hash16(sc.NormURL + "\x00" + sc.KeyFingerprint)
}

// LookupAuthResolution returns the cached auth-collection slug for this scope.
// Like every other read path it degrades to a miss (§8.3).
func (s *Store) LookupAuthResolution(sc Scope) (AuthResolution, bool, *output.Warning) {
	if !s.Enabled() {
		return AuthResolution{}, false, nil
	}
	path := s.AuthResolutionPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return AuthResolution{}, false, readWarning(path, err)
	}
	var all map[string]AuthResolution
	if err := json.Unmarshal(data, &all); err != nil {
		return AuthResolution{}, false, warning(WarnCacheUnreadable, path+": "+err.Error())
	}
	rec, ok := all[AuthResolutionKey(sc)]
	if !ok || rec.AuthCollection == "" {
		return AuthResolution{}, false, nil
	}
	if rec.CLIMajor != cliMajor() {
		// A different CLI major may have resolved it under different rules.
		return AuthResolution{}, false, nil
	}
	return rec, true, nil
}

// PutAuthResolution records the resolved slug. The whole map is rewritten
// atomically; a concurrent writer can lose an unrelated entry, which costs one
// extra Stage -1 bootstrap and never a wrong answer.
func (s *Store) PutAuthResolution(sc Scope, slug string, now time.Time) (bool, *output.Warning) {
	if !s.Enabled() || slug == "" {
		return false, nil
	}
	path := s.AuthResolutionPath()
	all := map[string]AuthResolution{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &all) // a corrupt file is replaced, not fatal
	}
	all[AuthResolutionKey(sc)] = AuthResolution{
		AuthCollection: slug,
		ResolvedAt:     now.UTC().Truncate(time.Second),
		CLIMajor:       cliMajor(),
	}
	data, err := canonicalJSON(all)
	if err != nil {
		return false, warning(WarnCacheWriteFailed, path+": "+err.Error())
	}
	if err := fsatomic.Write(path, data, FilePerm); err != nil {
		return false, warning(WarnCacheWriteFailed, path+": "+err.Error())
	}
	return true, nil
}

// ScopeInfo is one row of `pay cache ls` (§9). It carries no secret material:
// only fingerprints and header *names* (§8.1).
type ScopeInfo struct {
	Scope       string     `json:"scope"`
	Path        string     `json:"path"`
	Profiles    []string   `json:"profiles"`
	BaseURL     string     `json:"base_url"`
	APIPath     string     `json:"api_path"`
	GraphQLPath string     `json:"graphql_path"`
	HeaderNames []string   `json:"header_names"`
	KeyFP       string     `json:"key_fingerprint"`
	Generation  string     `json:"generation"`
	CreatedAt   *time.Time `json:"created_at"`
	ConfirmedAt *time.Time `json:"confirmed_at"`
	Collections int        `json:"collections"`
	Globals     int        `json:"globals"`
	Shards      int        `json:"shards"`
	Bytes       int64      `json:"bytes"`
	Readable    bool       `json:"readable"`
}

// Scopes lists every scope directory under the current epoch, newest first.
// Unreadable scopes are reported with Readable=false rather than skipped, so
// `pay cache ls` can explain a permissions problem instead of hiding it.
func (s *Store) Scopes() []ScopeInfo {
	if !s.Enabled() {
		return nil
	}
	entries, err := os.ReadDir(s.EpochDir())
	if err != nil {
		return nil
	}
	out := make([]ScopeInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !ValidScopeKey(e.Name()) {
			continue
		}
		sc := Scope{Key: e.Name()}
		info := ScopeInfo{
			Scope: e.Name(),
			Path:  s.ScopeDir(sc),
			Bytes: dirSize(s.ScopeDir(sc)),
		}
		if m, ok, _ := s.ReadManifest(sc); ok {
			info.Readable = true
			info.Profiles = m.Meta.Profiles
			info.BaseURL = m.Meta.BaseURL
			info.APIPath = m.Meta.APIPath
			info.GraphQLPath = m.Meta.GraphQLPath
			info.HeaderNames = m.Meta.HeaderNames
			info.KeyFP = m.Meta.KeyFingerprint
			info.Generation = m.Generation
			info.Collections = len(m.Collections)
			info.Globals = len(m.Globals)
			if !m.Meta.CreatedAt.IsZero() {
				t := m.Meta.CreatedAt
				info.CreatedAt = &t
			}
			if !m.Meta.ConfirmedAt.IsZero() {
				t := m.Meta.ConfirmedAt
				info.ConfirmedAt = &t
			}
		}
		if shards, err := os.ReadDir(filepath.Join(s.ScopeDir(sc), dirFields)); err == nil {
			info.Shards = len(shards)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := timeOrZero(out[i].ConfirmedAt), timeOrZero(out[j].ConfirmedAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort accounting
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
