//go:build unix

package update

import (
	"os"
	"syscall"
)

// detachSysProcAttr puts the child in its own session so it survives the
// parent's exit and the terminal's SIGHUP (§15.1).
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// swapBinary replaces dest with src. On unix a running executable can be
// renamed over, so this is one atomic rename.
func swapBinary(src, dest string) error {
	if err := os.Chmod(src, 0o755); err != nil {
		return err
	}
	return os.Rename(src, dest)
}

// CleanupOldBinary is a no-op on unix; it exists so callers do not need build
// tags for §19.13's Windows rename-self dance.
func CleanupOldBinary(dest string) {}
