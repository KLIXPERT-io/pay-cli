//go:build windows

package cache

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes a non-blocking exclusive LockFileEx on the first byte of
// .lock (§8.7). Windows byte-range locks are mandatory rather than advisory,
// which is exactly what makes the rename-then-remove GC dance necessary.
func lockFile(f *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped,
	)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION),
		errors.Is(err, windows.ERROR_IO_PENDING),
		errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return false, nil
	default:
		return false, err
	}
}

func unlockFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}
