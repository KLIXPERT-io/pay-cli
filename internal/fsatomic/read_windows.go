//go:build windows

package fsatomic

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// ReadFile is os.ReadFile plus FILE_SHARE_DELETE on Windows.
//
// Write() commits with a rename over the destination. On POSIX that always
// succeeds — a concurrent reader keeps its own inode. On Windows, MoveFileEx
// fails with ERROR_ACCESS_DENIED / ERROR_SHARING_VIOLATION when any other
// handle to the destination is open without FILE_SHARE_DELETE, and Go's
// os.Open does not request it. A single concurrent reader of a cache shard is
// therefore enough to make an atomic write fail.
//
// Opening with FILE_SHARE_READ|WRITE|DELETE restores the POSIX contract: the
// rename proceeds, and this reader continues to see the bytes it opened.
func ReadFile(name string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(h), name)
	defer f.Close()
	return io.ReadAll(f)
}
