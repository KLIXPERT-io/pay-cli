package secret

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "credentials.json")
}

// TestSaveDefaultsToTheFile is §5.2's core policy: file first.
func TestSaveDefaultsToTheFile(t *testing.T) {
	s := &Store{File: NewFileStore(tempPath(t)), Keyring: NewKeyring(KeyringOff)}
	res, err := s.SaveAPIKey(context.Background(), "local", "example-api-key", false, testNow)
	if err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	if res.Target != TargetFile || res.Warning != "" {
		t.Errorf("result = %+v, want the file target with no warning", res)
	}
	if res.Fingerprint != Fingerprint("example-api-key") {
		t.Errorf("fingerprint = %q", res.Fingerprint)
	}

	rec, secret, err := s.Lookup(context.Background(), "local")
	if err != nil || rec == nil || secret != "example-api-key" {
		t.Fatalf("Lookup = %+v,%q,%v", rec, secret, err)
	}
	if rec.Keyring {
		t.Error("the record must not claim the keyring")
	}
}

func TestSaveToKeyringStoresOnlyAPointer(t *testing.T) {
	kr, fake := newFakeKeyring(KeyringAuto, nil)
	s := &Store{File: NewFileStore(tempPath(t)), Keyring: kr}

	res, err := s.SaveAPIKey(context.Background(), "staging", "example-api-key", true, testNow)
	if err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	if res.Target != TargetKeyring {
		t.Fatalf("target = %q, want keyring", res.Target)
	}
	if fake.entries[KeyringService+"|"+KeyringAccount("staging")] != "example-api-key" {
		t.Error("the secret did not reach the keychain")
	}

	rec, _, err := s.File.Get("staging")
	if err != nil {
		t.Fatal(err)
	}
	if rec.APIKey != "" || rec.Token != "" {
		t.Error("a keyring-backed record must carry no secret")
	}
	if !rec.Keyring || rec.Fingerprint != Fingerprint("example-api-key") {
		t.Errorf("record = %+v", rec)
	}

	// Lookup follows the pointer.
	_, secret, err := s.Lookup(context.Background(), "staging")
	if err != nil || secret != "example-api-key" {
		t.Fatalf("Lookup = %q,%v", secret, err)
	}
}

// TestKeyringFailureIsNeverSilent is §5.2: an explicit --keyring or
// PAY_KEYRING=force must fail loudly, PAY_KEYRING=auto may fall back but must
// name the path it used.
func TestKeyringFailureIsNeverSilent(t *testing.T) {
	t.Run("explicit flag fails", func(t *testing.T) {
		kr, fake := newFakeKeyring(KeyringAuto, nil)
		fake.err = errors.New("no keychain here")
		s := &Store{File: NewFileStore(tempPath(t)), Keyring: kr}

		if _, err := s.SaveAPIKey(context.Background(), "local", "example-api-key", true, testNow); err == nil {
			t.Fatal("want an error when --keyring was requested and the keychain failed")
		}
		if _, ok, _ := s.File.Get("local"); ok {
			t.Error("nothing should have been written to the file")
		}
	})

	t.Run("force fails", func(t *testing.T) {
		kr, fake := newFakeKeyring(KeyringForce, nil)
		fake.err = errors.New("no keychain here")
		s := &Store{File: NewFileStore(tempPath(t)), Keyring: kr}
		if _, err := s.SaveAPIKey(context.Background(), "local", "example-api-key", false, testNow); err == nil {
			t.Fatal("PAY_KEYRING=force must fail loudly")
		}
	})

	t.Run("auto falls back with a warning", func(t *testing.T) {
		kr, fake := newFakeKeyring(KeyringAuto, nil)
		fake.err = errors.New("no keychain here")
		path := tempPath(t)
		s := &Store{File: NewFileStore(path), Keyring: kr}

		res, err := s.SaveAPIKey(context.Background(), "local", "example-api-key", false, testNow)
		if err != nil {
			t.Fatalf("auto must fall back, not fail: %v", err)
		}
		if res.Target != TargetFile {
			t.Errorf("target = %q", res.Target)
		}
		if !strings.Contains(res.Warning, path) {
			t.Errorf("warning = %q, want it to name %q", res.Warning, path)
		}
		if _, ok, _ := s.File.Get("local"); !ok {
			t.Error("the fallback did not write the file")
		}
	})
}

func TestSaveJWTKeepsOnlyTokenAndExpiry(t *testing.T) {
	s := &Store{File: NewFileStore(tempPath(t)), Keyring: NewKeyring(KeyringOff)}
	token := JWT{Token: "minted-token", Exp: testNow.Add(24 * time.Hour)}

	if _, err := s.SaveJWT(context.Background(), "cloud", token, "ops@example.com", "email", false, testNow); err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	rec, _, err := s.File.Get("cloud")
	if err != nil {
		t.Fatal(err)
	}
	if rec.AuthMode != ModeJWT || rec.Token != "minted-token" {
		t.Errorf("record = %+v", rec)
	}
	if rec.TokenExp == nil || !rec.TokenExp.Equal(token.Exp) {
		t.Errorf("token_exp = %v", rec.TokenExp)
	}
	if rec.LoginIdentifier != "ops@example.com" || rec.LoginField != "email" {
		t.Errorf("login identity = %q/%q", rec.LoginIdentifier, rec.LoginField)
	}
	if rec.APIKey != "" {
		t.Error("a jwt record must have no api key")
	}
}

func TestLogoutRemovesBothBackends(t *testing.T) {
	kr, fake := newFakeKeyring(KeyringAuto, nil)
	s := &Store{File: NewFileStore(tempPath(t)), Keyring: kr}
	if _, err := s.SaveAPIKey(context.Background(), "staging", "example-api-key", true, testNow); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Logout(context.Background(), "staging")
	if err != nil || !removed {
		t.Fatalf("Logout = %v,%v", removed, err)
	}
	if len(fake.entries) != 0 {
		t.Error("the keychain entry survived logout")
	}
	if _, ok, _ := s.File.Get("staging"); ok {
		t.Error("the file record survived logout")
	}
}

func TestStoreZeroValueIsSafe(t *testing.T) {
	var s *Store
	rec, secret, err := s.Lookup(context.Background(), "local")
	if rec != nil || secret != "" || err != nil {
		t.Errorf("nil store Lookup = %v,%q,%v", rec, secret, err)
	}
	if s.Path() != "" || s.KeyringMode() != KeyringOff {
		t.Error("nil store accessors must be safe")
	}
	if _, err := s.SaveAPIKey(context.Background(), "local", "k", false, testNow); err == nil {
		t.Error("saving through a nil store must error")
	}
}
