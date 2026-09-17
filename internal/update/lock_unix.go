//go:build unix

package update

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
)

// fileLock is an advisory flock held for the duration of an apply.
type fileLock struct{ f *os.File }

// acquireLock takes a non-blocking exclusive lock. A lock held by another
// `pay` process returns ErrLocked, which the caller reports as a skipped
// update rather than a failure: two concurrent swaps of the same file is the
// one outcome worth preventing.
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
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
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
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	if closeErr := l.f.Close(); err == nil {
		err = closeErr
	}
	l.f = nil
	return err
}
