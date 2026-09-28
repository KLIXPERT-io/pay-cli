package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

// ---------------------------------------------------------------------------
// §7.10c — the block census, $CACHE/v1/<scope>/census.json.
//
// The census is not part of a discovery set: it is built lazily, per entity,
// long after the manifest, and one entity is refreshed without touching the
// others. It is therefore its own file with its own header, verified on read
// exactly like an Entry (§8.3): a scope mismatch or an unreadable body is a
// miss, never a command failure. `pay cache clear` and GC remove it with the
// scope directory it lives in.
//
// This package decodes only the header. internal/discovery owns the typed
// census, for the same reason it owns the typed manifest.
// ---------------------------------------------------------------------------

// CensusHeader is the part of census.json the cache verifies.
type CensusHeader struct {
	CensusVersion int       `json:"census_version"`
	Scope         string    `json:"scope"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// CensusPath returns $CACHE/v1/<scope>/census.json.
func (s *Store) CensusPath(sc Scope) string { return filepath.Join(s.ScopeDir(sc), fileCensus) }

// ReadCensus returns the raw census for a scope. A missing file is a plain
// miss; anything else that prevents reading it is a miss plus a
// cache_unreadable warning (§8.3).
func (s *Store) ReadCensus(sc Scope) ([]byte, bool, *output.Warning) {
	if !s.Enabled() || !sc.Valid() {
		return nil, false, nil
	}
	path := s.CensusPath(sc)
	data, err := fsatomic.ReadFile(path)
	if err != nil {
		return nil, false, readWarning(path, err)
	}
	var h CensusHeader
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, false, warning(WarnCacheUnreadable, path+": "+err.Error())
	}
	if h.Scope != sc.Key {
		return nil, false, warning(WarnCacheUnreadable, path+": scope mismatch")
	}
	return data, true, nil
}

// WriteCensus persists a census atomically under the scope lock. body must
// marshal to an object whose "scope" is this scope; a census for another scope
// is refused rather than written where the next reader would reject it.
//
// Like every cache write it never fails a command: it returns false plus a
// cache_write_failed warning (§8.7).
func (s *Store) WriteCensus(sc Scope, body any) (bool, *output.Warning) {
	return s.UpdateCensus(sc, func([]byte) (any, error) { return body, nil })
}

// UpdateCensus is WriteCensus as a read-modify-write under the scope lock:
// merge receives the census on disk as it is NOW (nil when there is none or it
// cannot be read for this scope) and returns the body to write. Agents run
// tools in parallel, and a census built from a copy read before another
// process wrote its own entities would otherwise drop them.
//
// A merge that returns (nil, nil) writes nothing and reports false.
func (s *Store) UpdateCensus(sc Scope, merge func(current []byte) (any, error)) (bool, *output.Warning) {
	if !s.Enabled() {
		return false, nil
	}
	if !sc.Valid() {
		return false, warning(WarnCacheWriteFailed, "refusing to write a census for a malformed scope key")
	}
	dir := s.ScopeDir(sc)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return false, warning(WarnCacheWriteFailed, dir+": "+err.Error())
	}
	lk, err := acquireLock(s.LockPath(sc), s.lockTimeout())
	if err != nil {
		return false, warning(WarnCacheWriteFailed, s.LockPath(sc)+": "+err.Error())
	}
	defer lk.release()
	current, ok, _ := s.ReadCensus(sc)
	if !ok {
		current = nil
	}
	body, err := merge(current)
	if err != nil {
		return false, warning(WarnCacheWriteFailed, s.CensusPath(sc)+": "+err.Error())
	}
	if body == nil {
		return false, nil
	}
	data, err := censusJSON(body)
	if err != nil {
		return false, warning(WarnCacheWriteFailed, s.CensusPath(sc)+": "+err.Error())
	}
	var h CensusHeader
	if err := json.Unmarshal(data, &h); err != nil || h.Scope != sc.Key {
		return false, warning(WarnCacheWriteFailed, s.CensusPath(sc)+": census scope does not match the target scope")
	}
	if err := fsatomic.Write(s.CensusPath(sc), data, FilePerm); err != nil {
		return false, warning(WarnCacheWriteFailed, s.CensusPath(sc)+": "+err.Error())
	}
	return true, nil
}
