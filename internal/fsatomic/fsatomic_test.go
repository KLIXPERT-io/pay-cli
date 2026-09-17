package fsatomic

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestWrite(t *testing.T) {
	tests := []struct {
		name    string
		pre     []byte
		data    []byte
		perm    fs.FileMode
		subdir  string
		wantErr bool
	}{
		{name: "create new", data: []byte("hello"), perm: 0o600},
		{name: "overwrite", pre: []byte("old contents that are longer"), data: []byte("new"), perm: 0o644},
		{name: "empty payload", data: []byte{}, perm: 0o600},
		{name: "creates parent dirs", data: []byte("x"), perm: 0o600, subdir: "a/b/c"},
		{name: "binary payload", data: []byte{0x00, 0xff, 0x10, 0x00}, perm: 0o600},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.subdir, "file.json")
			if tc.pre != nil {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, tc.pre, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := Write(path, tc.data, tc.perm)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Write() error = %v, wantErr %v", err, tc.wantErr)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(got, tc.data) {
				t.Errorf("contents = %q, want %q", got, tc.data)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && st.Mode().Perm() != tc.perm {
				t.Errorf("mode = %v, want %v", st.Mode().Perm(), tc.perm)
			}
			assertNoTempLeft(t, filepath.Dir(path))
		})
	}
}

func TestWriteEmptyPath(t *testing.T) {
	if err := Write("", []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error for an empty path")
	}
}

func TestWriteFrom(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stream.bin")
	if err := WriteFrom(path, strings.NewReader("streamed"), 0o600); err != nil {
		t.Fatalf("WriteFrom: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "streamed" {
		t.Errorf("contents = %q", got)
	}
}

// TestWriteLeavesNoTempOnFailure checks the deferred cleanup path: a failing
// writer must not leave a partial .tmp file behind.
func TestWriteLeavesNoTempOnFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f")
	boom := errors.New("boom")
	err := write(path, func(w io.Writer) error {
		if _, wErr := w.Write([]byte("partial")); wErr != nil {
			return wErr
		}
		return boom
	}, 0o600)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("target file should not exist")
	}
	assertNoTempLeft(t, root)
}

// TestWriteConcurrent proves the temp-name scheme tolerates concurrent writers
// to the same target: every observer sees a complete document.
func TestWriteConcurrent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "concurrent.json")
	payloads := [][]byte{
		bytes.Repeat([]byte("a"), 4096),
		bytes.Repeat([]byte("b"), 4096),
		bytes.Repeat([]byte("c"), 4096),
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Write(path, payloads[i%len(payloads)], 0o600); err != nil {
				t.Errorf("Write: %v", err)
			}
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, p := range payloads {
		if bytes.Equal(got, p) {
			ok = true
		}
	}
	if !ok {
		t.Errorf("file is a torn write: len=%d", len(got))
	}
	assertNoTempLeft(t, root)
}

func TestCreateTempNaming(t *testing.T) {
	root := t.TempDir()
	f, err := createTemp(root, "config.toml", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	name := filepath.Base(f.Name())
	if !strings.HasPrefix(name, "config.toml.") || !strings.HasSuffix(name, ".tmp") {
		t.Errorf("temp name = %q, want config.toml.<pid>.<rand6>.tmp", name)
	}
	parts := strings.Split(strings.TrimSuffix(name, ".tmp"), ".")
	rand := parts[len(parts)-1]
	if len(rand) != 6 {
		t.Errorf("random component = %q, want 6 hex chars", rand)
	}
	if filepath.Dir(f.Name()) != root {
		t.Errorf("temp file must live in the target directory, got %q", f.Name())
	}
}

func assertNoTempLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}
