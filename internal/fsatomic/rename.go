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
// Deliberately short (5 attempts, ~75ms): the cache holds a file lock across
// the write, so a long retry here starves other writers and converts an
// ERROR_ACCESS_DENIED into a lock-acquisition timeout.
const (
	renameRetries = 5
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
