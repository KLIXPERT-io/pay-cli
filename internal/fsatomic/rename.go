package fsatomic

import (
	"os"
	"runtime"
	"time"
)

// renameRetries and renameBackoff bound the Windows retry loop below.
//
// This is defence in depth, not the primary fix: PayCLI's own readers use
// fsatomic.ReadFile, which opens with FILE_SHARE_DELETE so the rename is not
// denied in the first place. The retry only covers a handle opened by
// something outside PayCLI — a virus scanner, an editor, Explorer's preview.
// The budget is 12 attempts with linear backoff (~390ms total). That is sized
// for genuinely lockless contention — TestWriteConcurrent has 24 goroutines
// renaming onto one path — and costs the cache nothing, because the cache
// serialises its writers with a scope lock and so reaches the retry only when
// a handle is held by something outside PayCLI.
const (
	renameRetries = 12
	renameBackoff = 5 * time.Millisecond
)

// renameWithRetry is os.Rename plus a bounded retry on Windows.
//
// On POSIX, rename(2) over an open file is atomic and always succeeds: readers
// keep their existing inode. Windows has no such guarantee — MoveFileEx fails
// with ERROR_ACCESS_DENIED (or ERROR_SHARING_VIOLATION) when any other handle
// to the destination is open without FILE_SHARE_DELETE, which is exactly what
// a concurrent reader of a cache shard looks like. The failure is transient by
// construction, so a short bounded retry converts it into the POSIX behaviour
// the rest of PayCLI assumes.
//
// Everywhere except Windows this is a single os.Rename with no added latency.
func renameWithRetry(oldPath, newPath string) error {
	err := os.Rename(oldPath, newPath)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	for attempt := 1; attempt < renameRetries; attempt++ {
		time.Sleep(time.Duration(attempt) * renameBackoff)
		if err = os.Rename(oldPath, newPath); err == nil {
			return nil
		}
	}
	return err
}
