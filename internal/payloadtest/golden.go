package payloadtest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// UpdateEnv is the environment variable that rewrites golden files instead of
// comparing against them (`make golden`).
const UpdateEnv = "UPDATE_GOLDEN"

// Result is one whole-command run: the two streams and the exit status.
type Result struct {
	Stdout []byte
	Stderr []byte
	Exit   int
}

// Updating reports whether golden files are being rewritten.
//
// os.LookupEnv rather than os.Getenv: it distinguishes "set to empty" from
// "unset", and it keeps this package clear of the identifier §3.1's arch-lint
// reserves for internal/cli/app.go. This package is imported only by _test.go
// files, so it is test tooling rather than program code either way.
func Updating() bool {
	v, ok := os.LookupEnv(UpdateEnv)
	return ok && v != "" && v != "0" && v != "false"
}

// normalisers erase the few values that cannot be deterministic even with an
// injected clock: the per-invocation request id, the httptest port, the build
// stamp and any absolute temp path.
var normalisers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`"request_id"\s*:\s*"[^"]*"`), `"request_id": "<id>"`},
	{regexp.MustCompile(`"cli_version"\s*:\s*"[^"]*"`), `"cli_version": "<version>"`},
	{regexp.MustCompile(`"duration_ms"\s*:\s*\d+`), `"duration_ms": 0`},
	{regexp.MustCompile(`"generation"\s*:\s*"[0-9A-Za-z]{26}"`), `"generation": "<generation>"`},
	{regexp.MustCompile(`127\.0\.0\.1:\d+`), `127.0.0.1:<port>`},
	{regexp.MustCompile(`http://127\.0\.0\.1:<port>`), `http://127.0.0.1:<port>`},
}

// Normalize applies the golden normalisers to one stream.
func Normalize(b []byte) []byte {
	out := b
	for _, n := range normalisers {
		out = n.re.ReplaceAll(out, []byte(n.with))
	}
	return out
}

// Golden compares a whole-command result against
// testdata/golden/<name>.{out,err,exit}, or rewrites them under UPDATE_GOLDEN=1.
//
// Because cli.Run takes injected IO and returns the exit code (§17.3), this runs
// in-process: no subprocess, so coverage works and Windows is free.
func Golden(t testing.TB, name string, res Result) {
	t.Helper()
	dir := GoldenDir(t)
	if Updating() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("payloadtest: %v", err)
		}
		write(t, filepath.Join(dir, name+".out"), Normalize(res.Stdout))
		write(t, filepath.Join(dir, name+".err"), Normalize(res.Stderr))
		write(t, filepath.Join(dir, name+".exit"), []byte(strconv.Itoa(res.Exit)+"\n"))
		return
	}
	compare(t, filepath.Join(dir, name+".out"), Normalize(res.Stdout))
	compare(t, filepath.Join(dir, name+".err"), Normalize(res.Stderr))
	compare(t, filepath.Join(dir, name+".exit"), []byte(strconv.Itoa(res.Exit)+"\n"))
}

func write(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("payloadtest: %v", err)
	}
}

func compare(t testing.TB, path string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("payloadtest: %s does not exist.\nRe-run with %s=1 to create it. Got:\n%s",
				path, UpdateEnv, got)
		}
		t.Fatalf("payloadtest: %v", err)
	}
	if bytes.Equal(want, got) {
		return
	}
	t.Errorf("golden mismatch: %s\n%s", path, diff(string(want), string(got)))
}

// diff is a line-oriented first-difference report. It is deliberately tiny: the
// point is to name the line, not to reimplement a diff algorithm.
func diff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	var b strings.Builder
	n := len(wl)
	if len(gl) > n {
		n = len(gl)
	}
	shown := 0
	for i := 0; i < n && shown < 12; i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w == g {
			continue
		}
		fmt.Fprintf(&b, "  line %d:\n    want: %q\n    got:  %q\n", i+1, w, g)
		shown++
	}
	if shown == 0 {
		fmt.Fprintf(&b, "  (streams differ only in trailing bytes: want %d bytes, got %d)", len(want), len(got))
	}
	fmt.Fprintf(&b, "\nRe-run with %s=1 to accept the new output.", UpdateEnv)
	return b.String()
}
