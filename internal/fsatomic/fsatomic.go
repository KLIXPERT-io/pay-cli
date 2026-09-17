// Package fsatomic implements the single durable-write primitive required by
// §3.1: every durable write in PayCLI goes through Write.
//
// The sequence is: create a temp file in the *same* directory as the target
// (so the rename cannot cross a filesystem boundary) named
// "<base>.<pid>.<rand6>.tmp" -> write -> fsync the file -> chmod to the
// requested mode -> close -> rename over the target -> fsync the directory.
//
// The explicit chmod matters: O_CREATE applies the process umask, so a file
// created with 0600 under umask 077 is fine but under an exotic umask could be
// created 0400 and later reads would fail. Chmod after the write pins the mode
// exactly, which is what §5.2's 0600 credentials.json depends on.
package fsatomic

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// DirPerm is the mode used for parent directories created by Write.
// 0700 because PayCLI's state directories hold credentials (§5.2).
const DirPerm fs.FileMode = 0o700

// Write atomically replaces path with data, leaving the file mode at perm.
//
// The parent directory is created with DirPerm when it does not exist, so
// callers never need a separate MkdirAll step. A reader of path always observes
// either the previous contents or the complete new contents, never a prefix.
func Write(path string, data []byte, perm fs.FileMode) error {
	return write(path, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}, perm)
}

// WriteFrom is Write for a streaming source (used by downloads, §13).
func WriteFrom(path string, r io.Reader, perm fs.FileMode) error {
	return write(path, func(w io.Writer) error {
		_, err := io.Copy(w, r)
		return err
	}, perm)
}

func write(path string, fill func(io.Writer) error, perm fs.FileMode) (err error) {
	if path == "" {
		return errors.New("fsatomic: empty path")
	}
	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, DirPerm); mkErr != nil {
		return fmt.Errorf("fsatomic: create %s: %w", dir, mkErr)
	}

	tmp, err := createTemp(dir, filepath.Base(path), perm)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err = fill(tmp); err != nil {
		return fmt.Errorf("fsatomic: write %s: %w", tmpName, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("fsatomic: sync %s: %w", tmpName, err)
	}
	// Pin the mode regardless of umask.
	if err = tmp.Chmod(perm); err != nil && !ignorableChmodErr(err) {
		return fmt.Errorf("fsatomic: chmod %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("fsatomic: close %s: %w", tmpName, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("fsatomic: rename %s -> %s: %w", tmpName, path, err)
	}
	committed = true
	syncDir(dir)
	return nil
}

// createTemp opens a fresh "<base>.<pid>.<rand6>.tmp" in dir. O_EXCL means two
// concurrent PayCLI processes can never share a temp file even in the
// astronomically unlikely event that pid and rand6 collide.
func createTemp(dir, base string, perm fs.FileMode) (*os.File, error) {
	pid := os.Getpid()
	for attempt := 0; attempt < 10; attempt++ {
		suffix, err := rand6()
		if err != nil {
			return nil, fmt.Errorf("fsatomic: random suffix: %w", err)
		}
		name := filepath.Join(dir, fmt.Sprintf("%s.%d.%s.tmp", base, pid, suffix))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("fsatomic: create temp in %s: %w", dir, err)
		}
	}
	return nil, fmt.Errorf("fsatomic: could not create a temp file in %s after 10 attempts", dir)
}

func rand6() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// syncDir best-effort fsyncs the directory so the rename itself is durable.
// Opening a directory for sync is not supported on every platform (notably
// Windows), and a failure there does not invalidate the already-renamed file,
// so the error is deliberately dropped rather than surfaced to the caller.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	_ = d.Sync()
}

// ignorableChmodErr reports whether a Chmod failure is a platform limitation
// rather than a real problem (Plan 9 and some Windows filesystems).
func ignorableChmodErr(err error) bool {
	return errors.Is(err, fs.ErrInvalid) || errors.Is(err, errors.ErrUnsupported)
}
