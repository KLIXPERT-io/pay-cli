package cache

import (
	"os"
	"strings"
	"testing"
)

func TestCensusRoundTripAndVerification(t *testing.T) {
	sc, err := NewScope(base())
	if err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir())

	if _, ok, w := s.ReadCensus(sc); ok || w != nil {
		t.Fatalf("a cold cache must be a plain miss, got ok=%v warning=%v", ok, w)
	}
	body := map[string]any{"census_version": 1, "scope": sc.Key, "collections": map[string]any{}}
	if ok, w := s.WriteCensus(sc, body); !ok || w != nil {
		t.Fatalf("write: ok=%v warning=%v", ok, w)
	}
	raw, ok, w := s.ReadCensus(sc)
	if !ok || w != nil || !strings.Contains(string(raw), sc.Key) {
		t.Fatalf("read: ok=%v warning=%v raw=%s", ok, w, raw)
	}
	info, err := os.Stat(s.CensusPath(sc))
	if err != nil || info.Mode().Perm() != FilePerm {
		t.Errorf("census file mode = %v (%v), want %v", info.Mode().Perm(), err, FilePerm)
	}

	// A census for another scope is refused on write and rejected on read.
	if ok, w := s.WriteCensus(sc, map[string]any{"scope": "ffffffffffffffff"}); ok || w == nil || w.Code != WarnCacheWriteFailed {
		t.Errorf("foreign-scope write: ok=%v warning=%v", ok, w)
	}
	if err := os.WriteFile(s.CensusPath(sc), []byte(`{"scope":"ffffffffffffffff"}`), FilePerm); err != nil {
		t.Fatal(err)
	}
	if _, ok, w := s.ReadCensus(sc); ok || w == nil || w.Code != WarnCacheUnreadable {
		t.Errorf("foreign-scope read: ok=%v warning=%v", ok, w)
	}
	if err := os.WriteFile(s.CensusPath(sc), []byte(`{not json`), FilePerm); err != nil {
		t.Fatal(err)
	}
	if _, ok, w := s.ReadCensus(sc); ok || w == nil {
		t.Errorf("corrupt census: ok=%v warning=%v", ok, w)
	}

	// --no-cache: a disabled store never touches disk.
	off := New("")
	if ok, w := off.WriteCensus(sc, body); ok || w != nil {
		t.Errorf("disabled store wrote: %v %v", ok, w)
	}
	if _, ok, _ := off.ReadCensus(sc); ok {
		t.Error("disabled store read")
	}
}

func TestUpdateCensusReadsUnderTheLock(t *testing.T) {
	sc, err := NewScope(base())
	if err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir())
	var seen []byte
	ok, w := s.UpdateCensus(sc, func(cur []byte) (any, error) {
		seen = cur
		return map[string]any{"census_version": 1, "scope": sc.Key, "collections": map[string]any{"a": 1}}, nil
	})
	if !ok || w != nil || seen != nil {
		t.Fatalf("first write: ok=%v w=%v seen=%s", ok, w, seen)
	}
	ok, _ = s.UpdateCensus(sc, func(cur []byte) (any, error) {
		seen = cur
		return nil, nil // nothing to change
	})
	if ok || !strings.Contains(string(seen), `"a"`) {
		t.Errorf("merge did not see the census on disk, or a nil body wrote: ok=%v seen=%s", ok, seen)
	}
}
