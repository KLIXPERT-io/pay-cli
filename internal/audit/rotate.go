package audit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
)

// rotatedName is the name of generation n (1 = most recent rotation).
func rotatedName(path string, n int) string {
	return path + "." + strconv.Itoa(n)
}

// rotateIfNeeded rotates when appending incoming bytes would push the log past
// MaxBytes (§12.7: 10 MB, 3 generations).
//
// A single record larger than MaxBytes is still written — rotating forever
// would never make room for it — but it lands in a freshly rotated file, so the
// previous history survives.
func (l *Logger) rotateIfNeeded(incoming int64) error {
	info, err := os.Stat(l.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Size()+incoming <= l.maxBytes {
		return nil
	}
	return l.rotate()
}

// rotate shifts audit.log → .1 → .2 → .3 and drops what falls off the end.
// Every step is an os.Rename, so a crash mid-rotation leaves complete files.
func (l *Logger) rotate() error {
	oldest := rotatedName(l.path, l.generations)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("audit: removing %s: %w", oldest, err)
	}
	for n := l.generations - 1; n >= 1; n-- {
		from, to := rotatedName(l.path, n), rotatedName(l.path, n+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("audit: rotating %s: %w", from, err)
		}
	}
	if err := os.Rename(l.path, rotatedName(l.path, 1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("audit: rotating %s: %w", l.path, err)
	}
	return nil
}

// Generations lists the rotated files that currently exist, newest first, for
// `pay doctor`.
func (l *Logger) Generations() []string {
	var out []string
	for n := 1; n <= l.generations; n++ {
		name := rotatedName(l.path, n)
		if _, err := os.Stat(name); err == nil {
			out = append(out, name)
		}
	}
	return out
}
