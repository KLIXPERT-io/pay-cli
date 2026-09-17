//go:build windows

package update

import (
	"os"

	"golang.org/x/sys/windows"
)

// detachSysProcAttr detaches the child from this console and process group so
// it survives the parent's exit (§15.1).
func detachSysProcAttr() *windows.SysProcAttr {
	return &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}

// oldSuffix is where the running .exe is parked during a swap.
const oldSuffix = ".old"

// swapBinary performs §15.1's Windows rename-self dance: os.Rename over a
// RUNNING .exe fails (§19.13), so the current image is moved aside first and
// removed on the next start.
func swapBinary(src, dest string) error {
	old := dest + oldSuffix
	// Best effort: a leftover from a previous update may still be locked.
	_ = os.Remove(old)

	if err := moveFileEx(dest, old); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	if err := moveFileEx(src, dest); err != nil {
		// Put the original back so the install is never left without a binary.
		_ = moveFileEx(old, dest)
		return err
	}
	return nil
}

// moveFileEx is MoveFileEx(from, to, MOVEFILE_REPLACE_EXISTING).
func moveFileEx(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_REPLACE_EXISTING)
}

// CleanupOldBinary removes the parked previous image, ignoring
// ERROR_SHARING_VIOLATION when it is still mapped. Call it at startup.
func CleanupOldBinary(dest string) {
	if dest == "" {
		return
	}
	_ = os.Remove(dest + oldSuffix)
}
