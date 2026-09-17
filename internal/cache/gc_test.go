package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGCCollectsExpiredScopes(t *testing.T) {
	s, sc := newTestStore(t)
	s.DisableGC = false

	gen := NewGeneration(testNow)
	writeFixture(t, s, sc, gen, "pages")

	// Still inside the 30-day window.
	stats, warns := s.GC(testNow.Add(29 * 24 * time.Hour))
	if stats.ScopesCollected != 0 {
		t.Fatalf("a 29-day-old scope was collected: %+v %+v", stats, warns)
	}
	if _, ok, _ := s.ReadManifest(sc); !ok {
		t.Fatal("a live scope was removed")
	}

	// Past it.
	stats, _ = s.GC(testNow.Add(31 * 24 * time.Hour))
	if stats.ScopesCollected != 1 {
		t.Fatalf("stats = %+v, want one collected scope", stats)
	}
	if _, ok, _ := s.ReadManifest(sc); ok {
		t.Fatal("an expired scope survived GC")
	}
	// Nothing is left behind, not even the renamed tree.
	entries, err := os.ReadDir(s.EpochDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), trashInfix) {
			t.Fatalf("GC left %s behind", e.Name())
		}
	}
}

// TestGCUsesConfirmedAt is §8.2: meta.confirmed_at is the single authoritative
// timestamp, so a manifest generated long ago but confirmed recently survives.
func TestGCUsesConfirmedAt(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	doc := manifestDoc(sc, gen, "pages")
	old := testNow.Add(-90 * 24 * time.Hour)
	doc["generated_at"] = old.Format(time.RFC3339)
	doc["meta"].(map[string]any)["created_at"] = old.Format(time.RFC3339)
	data, _ := json.Marshal(doc)
	mustWriteFile(t, s.ManifestPath(sc), data)

	if stats, _ := s.GC(testNow.Add(time.Hour)); stats.ScopesCollected != 0 {
		t.Fatalf("a recently confirmed scope was collected: %+v", stats)
	}
}

func TestGCKeepsAuthResolutionAndStamp(t *testing.T) {
	s, sc := newTestStore(t)
	writeFixture(t, s, sc, NewGeneration(testNow), "pages")
	if ok, _ := s.PutAuthResolution(sc, "users", testNow); !ok {
		t.Fatal("PutAuthResolution failed")
	}
	s.stampGC(testNow)

	if stats, _ := s.GC(testNow.Add(31 * 24 * time.Hour)); stats.ScopesCollected != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if _, ok, _ := s.LookupAuthResolution(sc); !ok {
		t.Fatal("auth-resolution.json was collected; §7.0(e) requires it to survive")
	}
	if _, err := os.Stat(filepath.Join(s.EpochDir(), fileGCStamp)); err != nil {
		t.Fatalf("the GC stamp was collected: %v", err)
	}
}

func TestGCCollectsForeignEpochs(t *testing.T) {
	s, _ := newTestStore(t)
	old := filepath.Join(s.Root(), "v0")
	mustWriteFile(t, filepath.Join(old, "whatever.json"), []byte(`{}`))
	ancient := testNow.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(old, ancient, ancient); err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(s.Root(), "v2")
	mustWriteFile(t, filepath.Join(fresh, "whatever.json"), []byte(`{}`))
	if err := os.Chtimes(fresh, testNow, testNow); err != nil {
		t.Fatal(err)
	}

	stats, _ := s.GC(testNow)
	if stats.EpochsCollected != 1 {
		t.Fatalf("stats = %+v, want exactly the stale epoch", stats)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("the stale epoch survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a recent foreign epoch was collected: %v", err)
	}
}

func TestGCRemovesLeftoverTrash(t *testing.T) {
	s, _ := newTestStore(t)
	leftover := filepath.Join(s.EpochDir(), "0df77681bf2d6bfc"+trashInfix+NewGeneration(testNow))
	mustWriteFile(t, filepath.Join(leftover, "manifest.json"), []byte(`{}`))

	stats, _ := s.GC(testNow)
	if stats.TrashRemoved != 1 {
		t.Fatalf("stats = %+v, want one trash directory removed", stats)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("trash survived: %v", err)
	}
}

// TestTrashedScopeIsInvisibleImmediately is why §8.7 renames before removing.
func TestTrashedScopeIsInvisibleImmediately(t *testing.T) {
	s, sc := newTestStore(t)
	writeFixture(t, s, sc, NewGeneration(testNow), "pages")

	dir := s.ScopeDir(sc)
	target := dir + trashInfix + NewGeneration(testNow)
	if err := os.Rename(dir, target); err != nil {
		t.Fatal(err)
	}
	if _, ok, warn := s.ReadManifest(sc); ok || warn != nil {
		t.Fatalf("a renamed scope was still resolvable: ok=%v warn=%+v", ok, warn)
	}
	if ValidScopeKey(filepath.Base(target)) {
		t.Fatal("a trashed directory name still parses as a scope key")
	}
}

func TestMaybeGCIsProbabilistic(t *testing.T) {
	s, sc := newTestStore(t)
	s.DisableGC = false
	s.GCDivisor = 1 // always
	writeFixture(t, s, sc, NewGeneration(testNow), "pages")
	s.stampGC(testNow)

	// With divisor 1 the sweep always runs, so an expired scope goes away on
	// the very next write.
	other := mustScope(t, func() ScopeInput { in := base(); in.Credential = "another-key-entirely"; return in }())
	writeFixture(t, s, other, NewGeneration(testNow), "pages")
	if warns := s.MaybeGC(testNow.Add(31 * 24 * time.Hour)); len(warns) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	if _, ok, _ := s.ReadManifest(sc); ok {
		t.Fatal("MaybeGC did not sweep with divisor 1")
	}

	// DisableGC wins.
	s2, sc2 := newTestStore(t)
	s2.GCDivisor = 1
	writeFixture(t, s2, sc2, NewGeneration(testNow), "pages")
	s2.stampGC(testNow)
	s2.MaybeGC(testNow.Add(31 * 24 * time.Hour))
	if _, ok, _ := s2.ReadManifest(sc2); !ok {
		t.Fatal("DisableGC did not disable the sweep")
	}
}

func TestGCNeverRunsOnAFreshCache(t *testing.T) {
	s, _ := newTestStore(t)
	s.DisableGC = false
	s.GCDivisor = 1000000
	if s.gcDue(testNow) {
		t.Fatal("a fresh cache reported a sweep as due")
	}
	if _, err := os.Stat(filepath.Join(s.EpochDir(), fileGCStamp)); err != nil {
		t.Fatalf("the stamp was not laid down: %v", err)
	}
	// 25 hours later the stamp forces a sweep regardless of the divisor.
	if !s.gcDue(testNow.Add(25 * time.Hour)) {
		t.Fatal("a 25-hour-old stamp did not force a sweep")
	}
}

func TestGCOnMissingRootIsSilent(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "never-created"))
	stats, warns := s.GC(testNow)
	if stats != (GCStats{}) || len(warns) != 0 {
		t.Fatalf("stats=%+v warns=%+v, want a silent no-op", stats, warns)
	}
}
