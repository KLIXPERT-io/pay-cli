//go:build !windows

package cache

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes a non-blocking advisory exclusive flock (§8.7). The retry loop
// and the 5 s budget live in store.go so both platforms share them.
//
// flock locks are held per open file description, so two goroutines in one
// process that each opened .lock exclude each other exactly as two processes do.
func lockFile(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK), errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EACCES):
		return false, nil
	case errors.Is(err, unix.EINTR):
		return false, nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOLCK), errors.Is(err, unix.ENOSYS):
		// A filesystem without flock (some network mounts, some containers).
		// Refusing to write there would mean re-paying discovery forever; the
		// in-process lock in store.go still serialises this process, and the
		// per-file atomic rename still keeps readers consistent.
		return true, nil
	default:
		return false, err
	}
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
