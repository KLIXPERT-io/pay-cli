package config

import (
	"path"
	"path/filepath"
	"strings"
)

// PathsFor takes goos as a parameter so the §4.1 path matrix is a table test
// runnable on any host. filepath.Join and filepath.Clean, however, follow the
// *host* separator, not the requested goos — so simulating linux on a Windows
// runner produced `\home\u\.config\pay` and the table failed in CI.
//
// joinFor and cleanFor make PathsFor genuinely pure with respect to goos:
// Windows rules for goos == "windows", POSIX rules otherwise. When goos is the
// host's own GOOS — the only case that occurs in production — these are exactly
// filepath.Join and filepath.Clean.

func joinFor(goos string, elems ...string) string {
	if goos == "windows" {
		return filepath.Join(elems...)
	}
	// Drop leading empties the way filepath.Join does, then join POSIX-style.
	kept := elems[:0]
	for _, e := range elems {
		if e != "" {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return path.Clean(strings.Join(kept, "/"))
}

func cleanFor(goos, p string) string {
	if p == "" {
		return ""
	}
	if goos == "windows" {
		return filepath.Clean(p)
	}
	return path.Clean(p)
}

// joinUnder is join() with an explicit goos: empty base yields empty result so
// the caller's `set` skips it.
func joinUnder(goos, base string, elems ...string) string {
	if base == "" {
		return ""
	}
	return joinFor(goos, append([]string{base}, elems...)...)
}
