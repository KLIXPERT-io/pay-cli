package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

func TestResolveDownloadName(t *testing.T) {
	doc := payload.Doc{
		"id":       7,
		"filename": "hero.png",
		"sizes": map[string]any{
			"thumbnail": map[string]any{"filename": "hero-300x300.png"},
			"card":      map[string]any{"filename": nil},
		},
	}

	tests := []struct {
		name     string
		doc      payload.Doc
		filename string
		size     string
		want     string
		wantCode apierr.Code
	}{
		{name: "explicit filename", filename: "other.png", want: "other.png"},
		{name: "from the document", doc: doc, want: "hero.png"},
		{name: "a generated size", doc: doc, size: "thumbnail", want: "hero-300x300.png"},
		{name: "unknown size", doc: doc, size: "huge", wantCode: apierr.CodeInvalidOption},
		{name: "size that was never generated", doc: doc, size: "card", wantCode: apierr.CodeFeatureUnavailable},
		{name: "size without a document", size: "thumbnail", wantCode: apierr.CodeInvalidArgs},
		{name: "document with no filename", doc: payload.Doc{"id": 7}, wantCode: apierr.CodeFileMissing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDownloadName(tc.doc, tc.filename, tc.size)
			if tc.wantCode != "" {
				if err == nil {
					t.Fatalf("expected %s, got %q", tc.wantCode, got)
				}
				if apierr.CodeOf(err) != tc.wantCode {
					t.Fatalf("code = %s, want %s", apierr.CodeOf(err), tc.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("name = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDownloadSink(t *testing.T) {
	d := &Deps{RT: &Runtime{App: App{Stdout: &strings.Builder{}}}}

	sink, err := openDownloadSink(d, "-", true)
	if err != nil || sink == nil || sink.Writer == nil {
		t.Fatalf("stdout sink: sink=%v err=%v", sink, err)
	}

	dir := t.TempDir()
	sink, err = openDownloadSink(d, filepath.Join(dir, "out.png"), false)
	if err != nil {
		t.Fatal(err)
	}
	if sink.Writer == nil {
		t.Fatal("file sink")
	}
	if _, err := io.WriteString(sink, "bytes"); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "out.png")); err != nil || string(got) != "bytes" {
		t.Fatalf("committed file = %q, %v", got, err)
	}

	if _, err := openDownloadSink(d, dir, false); err == nil {
		t.Fatal("a directory destination must be rejected")
	}
}

// TestDownloadSinkAbortLeavesTheDestinationAlone is finding 13's
// truncate-before-the-request half: the old sink os.Create()d the destination
// before the HTTP call, so a 403/500 destroyed a file that was already there.
func TestDownloadSinkAbortLeavesTheDestinationAlone(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(dest, []byte("IMPORTANT USER DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink, err := openDownloadSink(&Deps{}, dest, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(sink, "partial attacker bytes")
	sink.abort()

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "IMPORTANT USER DATA" {
		t.Fatalf("an aborted download changed the destination: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("a temp file was left behind: %v", entries)
	}
}

// TestSafeLocalName is the path-traversal guard for finding 13: the filename in
// the envelope is whatever the *server* says, so it may never become a local
// path on its own.
func TestSafeLocalName(t *testing.T) {
	ok := []string{"hero.png", "hero-300x300.png", ".bashrc", "a b.png", "..hidden"}
	for _, name := range ok {
		got, err := safeLocalName(name)
		if err != nil || got != name {
			t.Errorf("safeLocalName(%q) = %q, %v; want it accepted unchanged", name, got, err)
		}
	}
	attacks := []string{
		"../../../.bashrc",
		"..%2F..%2Fnotes.txt/../evil",
		"/etc/passwd",
		"/tmp/payrev-victim/notes.txt",
		"..",
		".",
		"",
		"sub/dir.png",
		`..\..\Users\me\.ssh\authorized_keys`,
		`C:\Windows\System32\drivers\etc\hosts`,
		"evil\x00.png",
	}
	for _, name := range attacks {
		if got, err := safeLocalName(name); err == nil {
			t.Errorf("safeLocalName(%q) = %q, want a refusal", name, got)
		} else if apierr.CodeOf(err) != apierr.CodeInvalidArgs {
			t.Errorf("safeLocalName(%q) code = %s, want invalid_args", name, apierr.CodeOf(err))
		}
	}
}

func TestDownloadHelpExplainsThe500(t *testing.T) {
	cmd := newDownloadCmd(nil)
	if !strings.Contains(cmd.Long, "HTTP 500 rather than 404") {
		t.Fatal("download help must explain that a missing file answers 500")
	}
	for _, name := range []string{"filename", "out", "size"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("pay download is missing --%s", name)
		}
	}
}

// downloadServer stubs the two routes `pay download` uses: the document read
// that supplies the filename, and the raw-bytes route. filename is what a
// hostile (or merely compromised) Payload returns for doc media/4.
func downloadServer(t *testing.T, filename string, fileStatus int, fileBody string) *payloadtest.Server {
	t.Helper()
	return payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/media/4", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			_, _ = io.WriteString(w, `{"id":4,"filename":`+quoteJSON(filename)+
				`,"mimeType":"text/plain","sizes":{"thumbnail":{"filename":"../../thumb-escape.png"}}}`)
		}),
		payloadtest.WithHandler("GET", "/api/media/file/"+filename, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("X-Powered-By", "Payload")
			w.WriteHeader(fileStatus)
			_, _ = io.WriteString(w, fileBody)
		}))
}

func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// downloadDirs returns a home (cache + config) and a work directory inside it,
// plus a victim file one level ABOVE the work directory — the file a
// "../notes.txt" filename is aiming at.
func downloadDirs(t *testing.T) (home, work, victim string) {
	t.Helper()
	home = t.TempDir()
	work = filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	victim = filepath.Join(home, "notes.txt")
	if err := os.WriteFile(victim, []byte("IMPORTANT USER DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, work, victim
}

// TestDownloadRefusesAServerControlledPath is finding 13 end to end: a server
// that answers with filename "../notes.txt" must not be able to create,
// truncate or overwrite anything outside the working directory.
func TestDownloadRefusesAServerControlledPath(t *testing.T) {
	for _, args := range [][]string{
		{"download", "media", "4"},
		{"download", "media", "--filename", "../notes.txt"},
		{"download", "media", "4", "--force"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			home, work, victim := downloadDirs(t)
			srv := downloadServer(t, "../notes.txt", 200, "ssh-rsa AAAAB3NzaC1ATTACKERKEY attacker@evil")
			seedDiscovery(t, home, srv.URL)
			res := cliRun(t, invocation{
				Home: home, WorkDir: work, Args: args,
				Env: []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"},
			})
			if res.Code == 0 {
				t.Fatalf("exit = 0; the traversal was accepted\n%s", res.Stdout)
			}
			if got := res.code(t); got != "invalid_args" {
				t.Errorf("code = %q, want invalid_args\n%s", got, res.Stdout)
			}
			got, err := os.ReadFile(victim)
			if err != nil || string(got) != "IMPORTANT USER DATA" {
				t.Fatalf("the victim file was written: %q (%v)", got, err)
			}
			if n := srv.Count("GET", "/api/media/file"); n != 0 {
				t.Errorf("the bytes were fetched anyway (%d requests)", n)
			}
			if entries, _ := os.ReadDir(work); len(entries) != 0 {
				t.Errorf("the working directory is not empty: %v", entries)
			}
		})
	}
}

// TestDownloadRefusesAServerControlledSizePath covers the --size branch, which
// reads sizes.<name>.filename from the very same document.
func TestDownloadRefusesAServerControlledSizePath(t *testing.T) {
	home, work, _ := downloadDirs(t)
	srv := downloadServer(t, "hero.png", 200, "bytes")
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home, WorkDir: work,
		Args: []string{"download", "media", "4", "--size", "thumbnail"},
		Env:  []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if res.Code == 0 {
		t.Fatalf("exit = 0; a traversing size filename was accepted\n%s", res.Stdout)
	}
	if got := res.code(t); got != "invalid_args" {
		t.Errorf("code = %q, want invalid_args\n%s", got, res.Stdout)
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("the working directory is not empty: %v", entries)
	}
}

// TestDownloadDoesNotTruncateOnAFailedRequest: the destination used to be
// os.Create()d before the request, so a 500 (which is what this route answers
// for a missing file) emptied a file the user already had.
func TestDownloadDoesNotTruncateOnAFailedRequest(t *testing.T) {
	home, work, _ := downloadDirs(t)
	ghost := filepath.Join(work, "ghost.png")
	if err := os.WriteFile(ghost, []byte("IMPORTANT LOCAL DATA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := downloadServer(t, "ghost.png", 500, `{"errors":[{"message":"Something went wrong."}]}`)
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home, WorkDir: work,
		Args: []string{"download", "media", "--filename", "ghost.png", "-o", ghost},
		Env:  []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if res.Code == 0 {
		t.Fatalf("exit = 0 on a 500\n%s", res.Stdout)
	}
	got, err := os.ReadFile(ghost)
	if err != nil || string(got) != "IMPORTANT LOCAL DATA\n" {
		t.Fatalf("a failed download changed the file: %q (%v)", got, err)
	}
}

// TestDownloadWillNotClobberWithoutForce: filepath.Base alone still lets a
// server pick "Makefile" or ".env" out of the working directory.
func TestDownloadWillNotClobberWithoutForce(t *testing.T) {
	home, work, _ := downloadDirs(t)
	makefile := filepath.Join(work, "Makefile")
	if err := os.WriteFile(makefile, []byte("all:\n\techo mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := downloadServer(t, "Makefile", 200, "all:\n\trm -rf /\n")
	seedDiscovery(t, home, srv.URL)
	env := []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"}

	res := cliRun(t, invocation{Home: home, WorkDir: work, Env: env,
		Args: []string{"download", "media", "4"}})
	if res.Code == 0 {
		t.Fatalf("exit = 0; an existing file was silently overwritten\n%s", res.Stdout)
	}
	if got, _ := os.ReadFile(makefile); string(got) != "all:\n\techo mine\n" {
		t.Fatalf("the existing file was replaced: %q", got)
	}
	if hint := strings.Join([]string{res.Stdout, res.Stderr}, ""); !strings.Contains(hint, "--force") {
		t.Errorf("the refusal must name --force:\n%s", hint)
	}

	// --force is the explicit opt-in, and then the write goes through.
	res = cliRun(t, invocation{Home: home, WorkDir: work, Env: env,
		Args: []string{"download", "media", "4", "--force"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d with --force\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if got, _ := os.ReadFile(makefile); string(got) != "all:\n\trm -rf /\n" {
		t.Fatalf("--force did not write: %q", got)
	}
}

// TestDownloadWritesTheFileAndReportsWhereItWent is the happy path, so the
// hardening above cannot pass by breaking downloads outright.
func TestDownloadWritesTheFileAndReportsWhereItWent(t *testing.T) {
	home, work, _ := downloadDirs(t)
	srv := downloadServer(t, "hero.png", 200, "PNGDATA")
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home, WorkDir: work,
		Args: []string{"download", "media", "4"},
		Env:  []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	got, err := os.ReadFile(filepath.Join(work, "hero.png"))
	if err != nil || string(got) != "PNGDATA" {
		t.Fatalf("downloaded file = %q (%v)", got, err)
	}
	data := res.data(t)
	if data["filename"] != "hero.png" {
		t.Errorf("data.filename = %v", data["filename"])
	}
	if data["path"] != filepath.Join(work, "hero.png") {
		t.Errorf("data.path = %v, want %s", data["path"], filepath.Join(work, "hero.png"))
	}
}

// TestDownloadToStdoutKeepsTheStreamClean is the `-o -` corruption bug.
//
// `pay download media 4 -o - > f.png` wrote the file's bytes to stdout and
// then appended the op_result envelope to the SAME stream, so the redirect
// captured a hybrid that is neither the download nor valid JSON (a 463-byte
// PNG came out 1570 bytes long). download.go's own comment claimed the
// envelope went to the error stream; it did not, because --errors-to only
// moves FAILURES and this envelope is a success.
func TestDownloadToStdoutKeepsTheStreamClean(t *testing.T) {
	const body = "\x89PNG\r\n\x1a\n-not-json-at-all-"
	srv := downloadServer(t, "hero.png", 200, body)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home, WorkDir: home,
		Args: []string{"download", "media", "4", "-o", "-"},
		Env:  []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d: %s", res.Code, res.Stderr)
	}
	if res.Stdout != body {
		t.Errorf("stdout is not exactly the downloaded bytes.\n got %d bytes: %q\nwant %d bytes: %q",
			len(res.Stdout), res.Stdout, len(body), body)
	}
	if strings.Contains(res.Stdout, `"data_kind"`) {
		t.Error("the envelope was appended to the byte stream on stdout")
	}
	// The envelope is not lost — it moved to stderr, where it stays parseable.
	var env map[string]any
	if err := json.Unmarshal([]byte(res.Stderr), &env); err != nil {
		t.Fatalf("stderr is not the envelope: %v\n%s", err, res.Stderr)
	}
	if env["ok"] != true || env["command"] != "download" {
		t.Errorf("stderr envelope = %v", env)
	}
}
