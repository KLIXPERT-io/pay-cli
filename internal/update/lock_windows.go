//go:build windows

package update

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
)

// fileLock is a LockFileEx range lock held for the duration of an apply.
type fileLock struct{ f *os.File }

// acquireLock takes a non-blocking exclusive lock, returning ErrLocked when
// another `pay` process holds it.
func acquireLock(path string) (*fileLock, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, fsatomic.DirPerm); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped,
	)
	if err != nil {
		f.Close()
		if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_IO_PENDING {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	var overlapped windows.Overlapped
	err := windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &overlapped)
	if closeErr := l.f.Close(); err == nil {
		err = closeErr
	}
	l.f = nil
	return err
}
