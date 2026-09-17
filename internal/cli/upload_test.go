package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", payload.DefaultMaxUploadSize, false},
		{"100MB", 100 * 1000 * 1000, false},
		{"10MiB", 10 << 20, false},
		{"2g", 2 << 30, false},
		{"104857600", 104857600, false},
		{"1 KiB", 1 << 10, false},
		{"big", 0, true},
		{"12 photos", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %d", got)
				}
				if apierr.ExitCode(err) != apierr.ExitValidation {
					t.Fatalf("exit = %d, want 5", apierr.ExitCode(err))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestCheckRemoteAllowed covers §13's --allow-remote gate.
func TestCheckRemoteAllowed(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		allowed bool
		wantErr bool
	}{
		{"https public host", "https://example.com/logo.svg", false, false},
		{"plain http needs the flag", "http://example.com/logo.svg", false, true},
		{"plain http with the flag", "http://example.com/logo.svg", true, false},
		{"loopback needs the flag", "https://127.0.0.1/logo.svg", false, true},
		{"localhost needs the flag", "https://localhost:3900/logo.svg", false, true},
		{"private range needs the flag", "https://10.0.0.5/logo.svg", false, true},
		{"private range with the flag", "https://10.0.0.5/logo.svg", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRemoteAllowed(tc.url, tc.allowed)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "--allow-remote") &&
				!strings.Contains(apierr.From(err).Hint, "--allow-remote") {
				t.Fatalf("the error must name --allow-remote: %v", err)
			}
		})
	}
}

func TestUploadPreflightRejectsNonUploadCollection(t *testing.T) {
	// §13 preflight: flags.upload == false is a local refusal…
	if err := payload.CheckUploadCollection("pages", boolp(false)); err == nil {
		t.Fatal("expected not_upload_collection")
	} else if apierr.CodeOf(err) != apierr.CodeNotUploadCollection {
		t.Fatalf("code = %s", apierr.CodeOf(err))
	}
	// …and an unknown flag is not, per the tri-state rule.
	if err := payload.CheckUploadCollection("pages", nil); err != nil {
		t.Fatalf("unknown upload support must not reject: %v", err)
	}
}

func TestOpenUploadSource(t *testing.T) {
	d := &Deps{RT: &Runtime{App: App{Stdin: strings.NewReader("bytes")}}}

	t.Run("stdin", func(t *testing.T) {
		f := &uploadFlags{filename: "shot.png", contentType: "image/png"}
		body, name, ctype, closer, err := openUploadSource(t.Context(), d, f, "-")
		if err != nil {
			t.Fatal(err)
		}
		if closer != nil {
			t.Fatal("stdin must not be closed by the command")
		}
		if name != "shot.png" || ctype != "image/png" || body == nil {
			t.Fatalf("name=%q ctype=%q", name, ctype)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		f := &uploadFlags{}
		_, _, _, _, err := openUploadSource(t.Context(), d, f, "/nonexistent/pay-cli/hero.png")
		if err == nil {
			t.Fatal("expected file_missing")
		}
		if apierr.CodeOf(err) != apierr.CodeFileMissing {
			t.Fatalf("code = %s", apierr.CodeOf(err))
		}
	})

	t.Run("local file", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/hero.png"
		if err := writeTempFile(path, "png-bytes"); err != nil {
			t.Fatal(err)
		}
		f := &uploadFlags{}
		body, name, _, closer, err := openUploadSource(t.Context(), d, f, path)
		if err != nil {
			t.Fatal(err)
		}
		defer closer.Close()
		if name != "hero.png" || body == nil {
			t.Fatalf("name = %q", name)
		}
	})
}

func TestUploadHelpNamesTheFootguns(t *testing.T) {
	cmd := newUploadCmd(nil)
	for _, phrase := range []string{
		`part must be named literally "file"`,
		"auto-renamed SERVER-SIDE",
		"mints a NEW",
		"--alt → --data/--data-file → --set → --set-json",
	} {
		if !strings.Contains(cmd.Long, phrase) {
			t.Fatalf("upload help is missing %q", phrase)
		}
	}
}

func writeTempFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
