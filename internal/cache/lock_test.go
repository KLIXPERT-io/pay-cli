package cache

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lock")

	first, err := acquireLock(path, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// The second acquisition must give up on its own, well inside this window.
	second := make(chan error, 1)
	go func() {
		_, err := acquireLock(path, 60*time.Millisecond)
		second <- err
	}()
	select {
	case err := <-second:
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("second acquire = %v, want ErrLockTimeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the lock timeout was not honoured")
	}

	first.release()
	third, err := acquireLock(path, time.Second)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	third.release()
}

// TestLockSerialisesWriters is the property §8.7 depends on: only one writer
// publishes a set at a time.
func TestLockSerialisesWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lock")

	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk, err := acquireLock(path, 10*time.Second)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer lk.release()
			n := inside.Add(1)
			for {
				old := maxInside.Load()
				if n <= old || maxInside.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
		}()
	}
	wg.Wait()
	if got := maxInside.Load(); got != 1 {
		t.Fatalf("%d holders were inside the lock at once", got)
	}
}

func TestLockTimeoutSkipsTheWriteAndWarns(t *testing.T) {
	s, sc := newTestStore(t)
	s.LockTimeout = 30 * time.Millisecond
	if err := os.MkdirAll(s.ScopeDir(sc), DirPerm); err != nil {
		t.Fatal(err)
	}
	held, err := acquireLock(s.LockPath(sc), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()

	gen := NewGeneration(testNow)
	ok, warns := s.WriteSet(sc, Set{Generation: gen, Manifest: manifestDoc(sc, gen)}, testNow)
	if ok {
		t.Fatal("WriteSet succeeded while the lock was held")
	}
	if len(warns) != 1 || warns[0].Code != WarnCacheWriteFailed {
		t.Fatalf("warnings = %+v, want one cache_write_failed", warns)
	}
	// A stale lock must never wedge the CLI: the command carries on, and the
	// read path still works.
	if _, hit, warn := s.ReadManifest(sc); hit || warn != nil {
		t.Fatalf("read after a skipped write: hit=%v warn=%+v", hit, warn)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	lk, err := acquireLock(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	lk.release()
	lk.release()
	var nilLock *fileLock
	nilLock.release()

	again, err := acquireLock(path, time.Second)
	if err != nil {
		t.Fatalf("acquire after a double release: %v", err)
	}
	again.release()
}
