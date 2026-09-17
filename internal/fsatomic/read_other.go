//go:build !windows

package fsatomic

import "os"

// ReadFile is os.ReadFile. The Windows build adds FILE_SHARE_DELETE so a
// concurrent Write()'s rename is not denied; POSIX needs nothing extra.
func ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }
