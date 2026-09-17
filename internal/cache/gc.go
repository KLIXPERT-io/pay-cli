package cache

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

// §8.7 GC.
//
// On any write, with probability 1/50 or when the sentinel mtime is older than
// 24 h, sweep $CACHE/v1 and collect scope directories whose manifest.json ->
// meta.confirmed_at (the single authoritative copy, §8.2) is older than 30 days,
// plus foreign epoch directories. Never on the read path: a sweep on read would
// put a multi-second stat storm in front of `pay --help`.
const (
	// GCMaxAge is the 30-day collection threshold.
	GCMaxAge = 30 * 24 * time.Hour
	// GCStampMaxAge forces a sweep when the sentinel is this old.
	GCStampMaxAge = 24 * time.Hour
	// DefaultGCDivisor is §8.7's 1-in-50 write-path probability.
	DefaultGCDivisor = 50

	trashInfix = ".trash-"
)

// GCStats reports what a sweep collected. `pay doctor` prints it.
type GCStats struct {
	ScopesCollected int `json:"scopes_collected"`
	EpochsCollected int `json:"epochs_collected"`
	TrashRemoved    int `json:"trash_removed"`
	Errors          int `json:"errors"`
}

// MaybeGC runs the opportunistic sweep described in §8.7. It is called from the
// write path only, after the lock is released.
func (s *Store) MaybeGC(now time.Time) []output.Warning {
	if !s.Enabled() || s.DisableGC {
		return nil
	}
	if !s.gcDue(now) {
		return nil
	}
	s.stampGC(now)
	_, warns := s.GC(now)
	return warns
}

func (s *Store) gcDue(now time.Time) bool {
	stamp := filepath.Join(s.EpochDir(), fileGCStamp)
	if fi, err := os.Stat(stamp); err == nil {
		if now.Sub(fi.ModTime()) > GCStampMaxAge {
			return true
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		// First write into a fresh cache: stamp it now so the 24 h clock
		// starts, but do not pay for a sweep of a directory we just created.
		s.stampGC(now)
		return false
	}
	divisor := s.GCDivisor
	if divisor <= 0 {
		divisor = DefaultGCDivisor
	}
	if divisor == 1 {
		return true
	}
	// Probabilistic GC sampling; nothing security-sensitive depends on it.
	return rand.IntN(divisor) == 0 //nolint:gosec // G404: sampling, not a secret
}

func (s *Store) stampGC(now time.Time) {
	stamp := filepath.Join(s.EpochDir(), fileGCStamp)
	// The content is informational; the mtime is the signal. The mtime is set
	// explicitly from the injected clock so the sweep interval is testable and
	// cannot drift from the caller's notion of "now" (§3.1 keeps time.Now out
	// of this package entirely).
	if err := fsatomic.Write(stamp, []byte(now.UTC().Format(time.RFC3339)+"\n"), FilePerm); err != nil {
		return
	}
	_ = os.Chtimes(stamp, now, now)
}

// GC sweeps the cache root. It never returns an error: every failure is a
// skipped collection that the next sweep retries, and a cache problem never
// fails a command (§8.7).
//
// Nothing is unlinked in place. A scope directory is first renamed to
// <scope>.trash-<ulid> — atomic, and invisible to scope lookup, which only ever
// resolves an exact 16-hex name — and then removed best effort. On Windows,
// unlinking a file another process has open fails outright, so the rename is
// what makes GC safe there; on unix it additionally guarantees a mid-traversal
// reader gets a clean ENOENT, which §8.3 already defines as a miss.
func (s *Store) GC(now time.Time) (GCStats, []output.Warning) {
	var stats GCStats
	var warns []output.Warning
	if !s.Enabled() {
		return stats, nil
	}

	roots, err := os.ReadDir(s.root)
	if err != nil {
		if w := readWarning(s.root, err); w != nil {
			warns = append(warns, *w)
		}
		return stats, warns
	}

	for _, entry := range roots {
		name := entry.Name()
		path := filepath.Join(s.root, name)
		switch {
		case strings.Contains(name, trashInfix):
			// Left over from a previous sweep whose removal failed.
			if removeTrash(path) {
				stats.TrashRemoved++
			} else {
				stats.Errors++
			}
		case !entry.IsDir():
			// Nothing but epoch directories belongs at the root.
		case name == Epoch:
			s.sweepEpoch(now, path, &stats, &warns)
		case epochPattern.MatchString(name):
			// A foreign epoch. §8.2 says "older than 30 days"; §8.7 says "plus
			// any foreign epoch directory". The 30-day guard is honoured,
			// because an older *or newer* PayCLI may be using that directory
			// right now and collecting it immediately would make two installed
			// versions fight over the cache on every invocation.
			if olderThan(path, now, GCMaxAge) {
				if err := s.trash(path, now); err == nil {
					stats.EpochsCollected++
				} else {
					stats.Errors++
				}
			}
		}
	}
	return stats, warns
}

func (s *Store) sweepEpoch(now time.Time, epochDir string, stats *GCStats, warns *[]output.Warning) {
	entries, err := os.ReadDir(epochDir)
	if err != nil {
		if w := readWarning(epochDir, err); w != nil {
			*warns = append(*warns, *w)
		}
		return
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(epochDir, name)
		switch {
		case strings.Contains(name, trashInfix):
			if removeTrash(path) {
				stats.TrashRemoved++
			} else {
				stats.Errors++
			}
		case !e.IsDir():
			// auth-resolution.json and .gc-stamp deliberately survive every
			// sweep: §7.0(e) requires the resolved auth slug to outlive scope
			// GC so Stage -1's extra requests are paid once per credential.
		case ValidScopeKey(name):
			if !s.scopeExpired(Scope{Key: name}, path, now) {
				continue
			}
			if err := s.trash(path, now); err == nil {
				stats.ScopesCollected++
			} else {
				stats.Errors++
			}
		}
	}
}

// scopeExpired decides whether a scope directory is older than the 30-day
// threshold, reading meta.confirmed_at from manifest.json.
//
// A scope with no readable manifest falls back to the directory mtime: a
// directory that a crashed discovery left half-written must still be
// collectable, but a directory a writer created seconds ago must not be.
func (s *Store) scopeExpired(sc Scope, path string, now time.Time) bool {
	if m, ok, _ := s.ReadManifest(sc); ok {
		ref := m.Meta.ConfirmedAt
		if ref.IsZero() {
			ref = m.GeneratedAt
		}
		if ref.IsZero() {
			return olderThan(path, now, GCMaxAge)
		}
		return now.Sub(ref) > GCMaxAge
	}
	return olderThan(path, now, GCMaxAge)
}

func olderThan(path string, now time.Time, age time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) > age
}

// trash renames a directory out of the lookup namespace and then removes it.
func (s *Store) trash(dir string, now time.Time) error {
	target := dir + trashInfix + NewGeneration(now)
	if err := os.Rename(dir, target); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // another sweep won the race
		}
		return err
	}
	// Best effort: a removal that fails leaves a .trash-<ulid> directory the
	// next sweep retries. It can never be resolved as a scope.
	_ = os.RemoveAll(target)
	return nil
}

func removeTrash(path string) bool {
	err := os.RemoveAll(path)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}
