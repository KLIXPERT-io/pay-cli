package secret

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

var testNow = time.Date(2026, 9, 16, 17, 0, 0, 0, time.UTC)

func tempStore(t *testing.T) *FileStore {
	t.Helper()
	return NewFileStore(filepath.Join(t.TempDir(), "credentials.json"))
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := tempStore(t)

	if _, err := s.Load(); err != nil {
		t.Fatalf("Load on a missing file must succeed: %v", err)
	}
	if rec, ok, err := s.Get("local"); err != nil || ok || rec != nil {
		t.Fatalf("Get on a missing file = %v,%v,%v", rec, ok, err)
	}

	if err := s.Put("local", &Record{AuthMode: ModeAPIKey, APIKey: "example-api-key"}, testNow); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec, ok, err := s.Get("local")
	if err != nil || !ok {
		t.Fatalf("Get: %v,%v", ok, err)
	}
	if rec.APIKey != "example-api-key" {
		t.Errorf("api key did not round-trip: %q", rec.APIKey)
	}
	if rec.Fingerprint != Fingerprint("example-api-key") {
		t.Errorf("fingerprint = %q", rec.Fingerprint)
	}
	if rec.UpdatedAt == nil || !rec.UpdatedAt.Equal(testNow) {
		t.Errorf("updated_at = %v, want %v", rec.UpdatedAt, testNow)
	}

	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != FilePerm {
			t.Errorf("mode = %04o, want 0600", perm)
		}
	}

	// A second profile must not disturb the first.
	exp := testNow.Add(24 * time.Hour)
	if err := s.Put("cloud", &Record{AuthMode: ModeJWT, Token: "tok", TokenExp: &exp,
		LoginIdentifier: "ops@example.com", LoginField: "email"}, testNow); err != nil {
		t.Fatalf("Put: %v", err)
	}
	names, err := s.Profiles()
	if err != nil || len(names) != 2 || names[0] != "cloud" || names[1] != "local" {
		t.Fatalf("Profiles() = %v, %v", names, err)
	}

	cloud, _, _ := s.Get("cloud")
	if got := cloud.JWT(); got.Token != "tok" || !got.Exp.Equal(exp) {
		t.Errorf("JWT() = %+v", got)
	}

	// No password field exists, and none is ever written (§5.0).
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "password") {
		t.Fatal("a password reached credentials.json")
	}

	if removed, err := s.Delete("local"); err != nil || !removed {
		t.Fatalf("Delete = %v,%v", removed, err)
	}
	if removed, err := s.Delete("local"); err != nil || removed {
		t.Fatalf("a second Delete = %v,%v", removed, err)
	}

	if renamed, err := s.Rename("cloud", "prod"); err != nil || !renamed {
		t.Fatalf("Rename = %v,%v", renamed, err)
	}
	if _, ok, _ := s.Get("prod"); !ok {
		t.Error("the renamed profile is missing")
	}
}

// TestInsecurePermissionsAreRefused is §5.1 step 7.
func TestInsecurePermissionsAreRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	s := tempStore(t)
	if err := s.Put("local", &Record{AuthMode: ModeAPIKey, APIKey: "example-api-key"}, testNow); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := s.Load()
	if !apierr.HasCode(err, apierr.CodeAuthInsecurePermissions) {
		t.Fatalf("err = %v, want auth_insecure_permissions", err)
	}
	if hint := apierr.From(err).Hint; !strings.Contains(hint, "pay auth fix-perms") {
		t.Errorf("hint = %q, want the fix-perms command", hint)
	}
	if strings.Contains(err.Error(), "example-api-key") {
		t.Fatal("the error leaked the key")
	}

	if err := s.FixPerms(); err != nil {
		t.Fatalf("FixPerms: %v", err)
	}
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load after FixPerms: %v", err)
	}
}

func TestCorruptCredentialsFile(t *testing.T) {
	s := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load()
	if !apierr.HasCode(err, apierr.CodeAuthInvalid) {
		t.Fatalf("err = %v, want auth_invalid", err)
	}
}

func TestCheckPermOnMissingFile(t *testing.T) {
	if err := CheckPerm(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("a missing file must not be an error: %v", err)
	}
}

func TestFixPermsOnMissingFile(t *testing.T) {
	if err := tempStore(t).FixPerms(); err != nil {
		t.Errorf("FixPerms on a missing file: %v", err)
	}
}
