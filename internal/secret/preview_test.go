package secret

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestPreviewSecretEnvNames(t *testing.T) {
	got := PreviewSecretEnvNames("klixpert-stage")
	want := []string{"PAY_PREVIEW_SECRET_KLIXPERT_STAGE", "PAY_PREVIEW_SECRET"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if got := PreviewSecretEnvNames(""); len(got) != 1 || got[0] != EnvPreviewSecret {
		t.Fatalf("no profile = %v", got)
	}
}

// F9's chain, first hit wins, and every step names a location, never a value.
func TestResolvePreviewSecretOrder(t *testing.T) {
	path := tempPath(t)
	store := &Store{File: NewFileStore(path), Keyring: NewKeyring(KeyringOff)}
	if err := store.File.SetPreviewSecret("stage", "stored-preview-secret", testNow); err != nil {
		t.Fatalf("SetPreviewSecret: %v", err)
	}
	cases := []struct {
		name       string
		env        map[string]string
		secretEnv  string
		wantValue  string
		wantSource string
	}{
		{"per-profile env beats everything",
			map[string]string{"PAY_PREVIEW_SECRET_STAGE": "p1-profile", "PAY_PREVIEW_SECRET": "p2-general", "PREVIEW_SECRET": "p3-app"},
			"PREVIEW_SECRET", "p1-profile", "env:PAY_PREVIEW_SECRET_STAGE"},
		{"general env beats preview_secret_env",
			map[string]string{"PAY_PREVIEW_SECRET": "p2-general", "PREVIEW_SECRET": "p3-app"},
			"PREVIEW_SECRET", "p2-general", "env:PAY_PREVIEW_SECRET"},
		{"preview_secret_env names the app's own variable",
			map[string]string{"PREVIEW_SECRET": "p3-app"}, "PREVIEW_SECRET", "p3-app", "env:PREVIEW_SECRET"},
		{"set-but-empty counts as unset",
			map[string]string{"PAY_PREVIEW_SECRET": "  ", "PREVIEW_SECRET": ""}, "PREVIEW_SECRET",
			"stored-preview-secret", SourcePreviewCredFile},
		{"the credentials store is last", nil, "", "stored-preview-secret", SourcePreviewCredFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolvePreviewSecret(PreviewInput{Profile: "stage", Env: MapEnv(tc.env), SecretEnv: tc.secretEnv, Store: store})
			if err != nil || got == nil {
				t.Fatalf("Resolve = %+v, %v", got, err)
			}
			if got.Value != tc.wantValue || got.Source != tc.wantSource {
				t.Fatalf("got %q from %q, want %q from %q", got.Value, got.Source, tc.wantValue, tc.wantSource)
			}
			if got.Fingerprint != Fingerprint(tc.wantValue) || strings.Contains(got.Source, got.Value) {
				t.Fatalf("fingerprint/source wrong: %+v", got)
			}
		})
	}

	none, err := ResolvePreviewSecret(PreviewInput{Profile: "other", Env: MapEnv(nil), Store: store})
	if err != nil || none != nil {
		t.Fatalf("nothing configured must be (nil, nil), got %+v, %v", none, err)
	}
}

// A preview secret is a separate credential: re-logging in keeps it, and a
// record that holds only a preview secret authenticates nothing.
func TestPreviewSecretSurvivesLoginAndDoesNotAuthenticate(t *testing.T) {
	ctx := context.Background()
	s := &Store{File: NewFileStore(tempPath(t)), Keyring: NewKeyring(KeyringOff)}
	if err := s.File.SetPreviewSecret("local", "stored-preview-secret", testNow); err != nil {
		t.Fatalf("SetPreviewSecret: %v", err)
	}
	// A preview-only record must not satisfy the API-key chain.
	cred, err := Resolve(ctx, Input{Profile: "local", Env: MapEnv(nil), Mode: ModeAuto, Store: s, Now: testNow})
	if err != nil || !cred.Anonymous() {
		t.Fatalf("a preview-only record resolved an API credential: %+v, %v", cred, err)
	}
	rec, _, _ := s.File.Get("local")
	if rec.HasAPICredential() {
		t.Fatal("HasAPICredential on a preview-only record")
	}

	if _, err := s.SaveAPIKey(ctx, "local", "example-api-key", false, testNow); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	rec, _, _ = s.File.Get("local")
	if rec.APIKey != "example-api-key" || rec.PreviewSecret != "stored-preview-secret" {
		t.Fatalf("login dropped the preview secret or the key: %+v", rec)
	}

	removed, err := s.File.ClearPreviewSecret("local")
	if err != nil || !removed {
		t.Fatalf("Clear = %v, %v", removed, err)
	}
	rec, ok, _ := s.File.Get("local")
	if !ok || rec.APIKey != "example-api-key" || rec.PreviewSecret != "" {
		t.Fatalf("clear must keep the API key: %+v", rec)
	}
	if removed, _ := s.File.ClearPreviewSecret("local"); removed {
		t.Fatal("a second clear reported a removal")
	}
}

func TestClearingTheOnlySecretRemovesTheRecord(t *testing.T) {
	path := tempPath(t)
	fs := NewFileStore(path)
	if err := fs.SetPreviewSecret("p", "stored-preview-secret", testNow); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != FilePerm {
		t.Fatalf("credentials.json mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if _, err := fs.ClearPreviewSecret("p"); err != nil {
		t.Fatal(err)
	}
	if names, _ := fs.Profiles(); len(names) != 0 {
		t.Fatalf("an empty record was left behind: %v", names)
	}
	if err := fs.SetPreviewSecret("p", "", testNow); err == nil {
		t.Fatal("an empty secret must be refused")
	}
}
