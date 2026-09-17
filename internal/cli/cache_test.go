package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
)

// ---------------------------------------------------------------------------
// Shared harness for the operational commands.
//
// Every case below runs the real command tree in-process over byte buffers, a
// pinned clock and a throwaway $PAY_HOME. Nothing here touches the network:
// the commands under test (cache, skills, audit, update-self without a server)
// are exactly the ones that must work on a machine with no Payload instance at
// all.
// ---------------------------------------------------------------------------

// opsClock is the pinned wall clock for every test in this group.
var opsClock = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

type opsResult struct {
	Stdout   string
	Stderr   string
	Code     int
	Envelope map[string]any
}

// data returns envelope.data as a map, failing the test when it is not one.
func (r opsResult) data(t *testing.T) map[string]any {
	t.Helper()
	d, ok := r.Envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope.data is %T, want an object.\nstdout: %s", r.Envelope["data"], r.Stdout)
	}
	return d
}

// errorCode returns envelope.error.code, or "" when the command succeeded.
func (r opsResult) errorCode(t *testing.T) string {
	t.Helper()
	e, ok := r.Envelope["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := e["code"].(string)
	return code
}

func opsRun(t *testing.T, home string, args ...string) opsResult {
	t.Helper()
	return opsRunEnv(t, home, nil, args...)
}

func opsRunEnv(t *testing.T, home string, extraEnv []string, args ...string) opsResult {
	t.Helper()
	var out, errBuf bytes.Buffer
	env := append([]string{
		"PAY_HOME=" + home,
		"HOME=" + home,
		"USERPROFILE=" + home,
		"PAY_NO_UPDATE=1",
	}, extraEnv...)

	code := Run(context.Background(), App{
		Args:      args,
		Env:       env,
		Stdin:     strings.NewReader(""),
		Stdout:    &out,
		Stderr:    &errBuf,
		Now:       func() time.Time { return opsClock },
		WorkDir:   home,
		NoSignals: true,
	})

	res := opsResult{Stdout: out.String(), Stderr: errBuf.String(), Code: code}
	if strings.TrimSpace(res.Stdout) != "" {
		dec := json.NewDecoder(strings.NewReader(res.Stdout))
		dec.UseNumber()
		if err := dec.Decode(&res.Envelope); err != nil {
			// Not every --output mode is JSON; the caller decides whether that
			// matters.
			res.Envelope = nil
		}
	}
	return res
}

// ---------------------------------------------------------------------------

func TestCachePathIsAlwaysAnswerable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "cache", "path")

	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	if ok, _ := res.Envelope["ok"].(bool); !ok {
		t.Fatalf("ok = false\n%s", res.Stdout)
	}
	data := res.data(t)
	path, _ := data["path"].(string)
	if !strings.HasPrefix(path, home) {
		t.Fatalf("path = %q, want it under %s", path, home)
	}
	if kind, _ := res.Envelope["data_kind"].(string); kind != "op_result" {
		t.Fatalf("data_kind = %q, want op_result", kind)
	}
}

func TestCachePathRawPrintsBarePath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "cache", "path", "--output", "raw")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	got := strings.TrimSpace(res.Stdout)
	if !strings.HasPrefix(got, home) || strings.Contains(got, "{") {
		t.Fatalf("--output raw printed %q, want a bare path", got)
	}
}

func TestCacheLsOnAColdCache(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "cache", "ls")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	data := res.data(t)
	count, _ := data["count"].(json.Number)
	if count.String() != "0" {
		t.Fatalf("count = %s, want 0", count)
	}
	// An empty list must marshal as [], never null: an agent branching on
	// `.data.scopes | length` must not have to nil-check first.
	if _, ok := data["scopes"].([]any); !ok {
		t.Fatalf("scopes = %#v, want []", data["scopes"])
	}
}

func TestCacheInfoReportsTheTTLLadder(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "cache", "info")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	data := res.data(t)
	rows, ok := data["ttl"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("ttl = %#v, want a non-empty table", data["ttl"])
	}
	if epoch, _ := data["epoch"].(string); epoch != cache.Epoch {
		t.Fatalf("epoch = %q, want %q", epoch, cache.Epoch)
	}
}

func TestCacheTTLTableCoversEveryClass(t *testing.T) {
	t.Parallel()
	rows := cacheTTLTable()
	byName := map[string]cacheTTLRow{}
	for _, r := range rows {
		byName[r.Class] = r
	}
	for _, c := range cache.Classes() {
		if _, ok := cache.TTLFor(c); !ok {
			continue
		}
		if _, ok := byName[string(c)]; !ok {
			t.Errorf("cacheTTLTable is missing class %s", c)
		}
	}
	// Sorted, so the output is stable between runs.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Class > rows[i].Class {
			t.Fatalf("cacheTTLTable is not sorted: %s before %s", rows[i-1].Class, rows[i].Class)
		}
	}
}

func TestCacheShowRejectsAnUnknownScope(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "cache", "show", "not a scope key!!")
	if res.Code == 0 {
		t.Fatalf("exit = 0, want a failure\nstdout: %s", res.Stdout)
	}
	if code := res.errorCode(t); code != "invalid_args" {
		t.Fatalf("error.code = %q, want invalid_args\n%s", code, res.Stdout)
	}
}

func TestCacheClearRejectsAllPlusScope(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "cache", "clear", "--all", "--scope", "abc")
	if code := res.errorCode(t); code != "invalid_args" {
		t.Fatalf("error.code = %q, want invalid_args\n%s", code, res.Stdout)
	}
	if res.Code != 5 {
		t.Fatalf("exit = %d, want 5", res.Code)
	}
}

func TestCacheClearAllOnAColdCacheSucceeds(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "cache", "clear", "--all")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	data := res.data(t)
	if _, ok := data["cleared"].([]any); !ok {
		t.Fatalf("cleared = %#v, want []", data["cleared"])
	}
}

func TestCacheWarmWithoutABaseURLIsConfigMissing(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "cache", "warm")
	if code := res.errorCode(t); code != "config_missing" {
		t.Fatalf("error.code = %q, want config_missing\n%s", code, res.Stdout)
	}
	if res.Code != 9 {
		t.Fatalf("exit = %d, want 9", res.Code)
	}
}
