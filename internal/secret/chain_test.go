package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// chainFixture builds an Input whose *every* step of §5.1 can answer, so that
// removing one step at a time proves the ordering.
type chainFixture struct {
	in    Input
	store *Store
	fake  *fakeKeyring
}

func newChainFixture(t *testing.T) *chainFixture {
	t.Helper()
	kr, fake := newFakeKeyring(KeyringAuto, map[string]string{
		KeyringService + "|" + KeyringAccount("local"): "from-keyring",
	})
	store := &Store{File: NewFileStore(filepath.Join(t.TempDir(), "credentials.json")), Keyring: kr}
	if err := store.File.Put("local", &Record{AuthMode: ModeAPIKey, APIKey: "from-file", Keyring: true}, testNow); err != nil {
		t.Fatal(err)
	}

	keyFile := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(keyFile, []byte("from-file-flag\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return &chainFixture{
		store: store,
		fake:  fake,
		in: Input{
			Profile: "local",
			BaseURL: "http://localhost:3900",
			Env: MapEnv(map[string]string{
				"PAY_API_KEY_LOCAL": "from-profile-env",
				"PAY_API_KEY":       "from-generic-env",
				"DUMMY_KEY":         "from-named-env",
			}),
			Flags: Flags{
				APIKey:      "from-flag",
				APIKeyStdin: true,
				APIKeyFile:  keyFile,
				Stdin:       strings.NewReader("from-stdin\nignored\n"),
			},
			APIKeyEnv: "DUMMY_KEY",
			Helper: &Helper{Command: "helper", Timeout: time.Second,
				Run: func(context.Context, string) ([]byte, []byte, error) {
					return []byte("from-helper\n"), nil, nil
				}},
			Store: store,
			Now:   testNow,
			Mode:  ModeAuto,
		},
	}
}

// TestChainOrder walks §5.1 from the top, disabling one step per case.
func TestChainOrder(t *testing.T) {
	type disable func(*Input, *chainFixture)

	steps := []struct {
		step       string
		disable    disable
		wantValue  string
		wantSource string
		wantMode   Mode
	}{
		{
			step:       "1 --api-key-stdin",
			disable:    func(*Input, *chainFixture) {},
			wantValue:  "from-stdin",
			wantSource: SourceStdin,
			wantMode:   ModeAPIKey,
		},
		{
			step:       "2 --api-key-file",
			disable:    func(in *Input, _ *chainFixture) { in.Flags.APIKeyStdin = false },
			wantValue:  "from-file-flag",
			wantSource: SourceKeyFile,
			wantMode:   ModeAPIKey,
		},
		{
			step:       "3 --api-key",
			disable:    func(in *Input, _ *chainFixture) { in.Flags.APIKeyFile = "" },
			wantValue:  "from-flag",
			wantSource: SourceFlag,
			wantMode:   ModeAPIKey,
		},
		{
			step:       "4 PAY_API_KEY_<PROFILE>",
			disable:    func(in *Input, _ *chainFixture) { in.Flags.APIKey = "" },
			wantValue:  "from-profile-env",
			wantSource: "env:PAY_API_KEY_LOCAL",
			wantMode:   ModeAPIKey,
		},
		{
			step: "5 PAY_API_KEY",
			disable: func(in *Input, _ *chainFixture) {
				in.Env = MapEnv(map[string]string{"PAY_API_KEY": "from-generic-env", "DUMMY_KEY": "from-named-env"})
			},
			wantValue:  "from-generic-env",
			wantSource: "env:PAY_API_KEY",
			wantMode:   ModeAPIKey,
		},
		{
			step: "6 api_key_env",
			disable: func(in *Input, _ *chainFixture) {
				in.Env = MapEnv(map[string]string{"DUMMY_KEY": "from-named-env"})
			},
			wantValue:  "from-named-env",
			wantSource: "env:DUMMY_KEY",
			wantMode:   ModeAPIKey,
		},
		{
			step: "8 the keychain, because the record opted in",
			disable: func(in *Input, _ *chainFixture) {
				in.Env = MapEnv(nil)
			},
			wantValue:  "from-keyring",
			wantSource: SourceKeyringName,
			wantMode:   ModeAPIKey,
		},
		{
			step: "7 credentials.json when the keychain is empty",
			disable: func(in *Input, f *chainFixture) {
				in.Env = MapEnv(nil)
				f.fake.entries = map[string]string{}
				if err := f.store.File.Put("local",
					&Record{AuthMode: ModeAPIKey, APIKey: "from-file"}, testNow); err != nil {
					t.Fatal(err)
				}
			},
			wantValue:  "from-file",
			wantSource: SourceCredFile,
			wantMode:   ModeAPIKey,
		},
		{
			step: "9 credential_helper",
			disable: func(in *Input, f *chainFixture) {
				in.Env = MapEnv(nil)
				f.fake.entries = map[string]string{}
				if _, err := f.store.File.Delete("local"); err != nil {
					t.Fatal(err)
				}
			},
			wantValue:  "from-helper",
			wantSource: SourceHelper,
			wantMode:   ModeAPIKey,
		},
		{
			step: "10 a stored, unexpired JWT",
			disable: func(in *Input, f *chainFixture) {
				in.Env = MapEnv(nil)
				in.Helper = nil
				f.fake.entries = map[string]string{}
				exp := testNow.Add(time.Hour)
				if err := f.store.File.Put("local",
					&Record{AuthMode: ModeJWT, Token: "stored-token", TokenExp: &exp,
						LoginIdentifier: "ops@example.com", LoginField: "email"}, testNow); err != nil {
					t.Fatal(err)
				}
			},
			wantValue:  "stored-token",
			wantSource: SourceStoredJWT,
			wantMode:   ModeJWT,
		},
		{
			step: "11 nothing resolves: anonymous, not an error",
			disable: func(in *Input, f *chainFixture) {
				in.Env = MapEnv(nil)
				in.Helper = nil
				f.fake.entries = map[string]string{}
				if _, err := f.store.File.Delete("local"); err != nil {
					t.Fatal(err)
				}
			},
			wantValue:  "",
			wantSource: SourceNone,
			wantMode:   ModeAnonymous,
		},
	}

	for _, tc := range steps {
		t.Run(tc.step, func(t *testing.T) {
			f := newChainFixture(t)
			in := f.in
			// Each case disables every step above the one it targets.
			switch tc.step {
			case "1 --api-key-stdin":
			case "2 --api-key-file":
				in.Flags.APIKeyStdin = false
			case "3 --api-key":
				in.Flags.APIKeyStdin = false
				in.Flags.APIKeyFile = ""
			default:
				in.Flags.APIKeyStdin = false
				in.Flags.APIKeyFile = ""
				in.Flags.APIKey = ""
			}
			tc.disable(&in, f)

			got, err := Resolve(context.Background(), in)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Value != tc.wantValue {
				t.Errorf("value = %q, want %q", got.Value, tc.wantValue)
			}
			if !strings.HasPrefix(got.Source, tc.wantSource) {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", got.Mode, tc.wantMode)
			}
			wantFP := Fingerprint(tc.wantValue)
			if tc.wantMode == ModeAnonymous {
				wantFP = Anon
			}
			if got.Fingerprint != wantFP {
				t.Errorf("fingerprint = %q, want %q", got.Fingerprint, wantFP)
			}
		})
	}
}

func TestAnonymousModeShortCircuits(t *testing.T) {
	f := newChainFixture(t)
	f.in.Mode = ModeAnonymous

	got, err := Resolve(context.Background(), f.in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Anonymous() || got.Value != "" || got.Fingerprint != Anon {
		t.Errorf("credential = %+v, want a pure anonymous result", got)
	}
	if f.fake.calls != 0 {
		t.Error("anonymous mode must not touch the keychain")
	}
}

func TestExplicitModesRestrictTheChain(t *testing.T) {
	// A stored JWT must not satisfy --auth-mode api-key.
	f := newChainFixture(t)
	f.in.Flags = Flags{}
	f.in.Env = MapEnv(nil)
	f.in.Helper = nil
	f.fake.entries = map[string]string{}
	exp := testNow.Add(time.Hour)
	if err := f.store.File.Put("local", &Record{AuthMode: ModeJWT, Token: "stored-token", TokenExp: &exp}, testNow); err != nil {
		t.Fatal(err)
	}
	f.in.Mode = ModeAPIKey

	_, err := Resolve(context.Background(), f.in)
	if !apierr.HasCode(err, apierr.CodeAuthMissing) {
		t.Fatalf("err = %v, want auth_missing", err)
	}
	if hint := apierr.From(err).Hint; !strings.Contains(hint, "pay auth login --profile local") ||
		!strings.Contains(hint, "http://localhost:3900") {
		t.Errorf("hint = %q, want the login command with the profile and base url", hint)
	}

	// An API key must not satisfy --auth-mode jwt.
	f = newChainFixture(t)
	f.in.Flags = Flags{APIKey: "from-flag"}
	f.in.Env = MapEnv(map[string]string{"PAY_API_KEY": "from-generic-env"})
	f.in.Helper = nil
	f.in.Mode = ModeJWT
	f.fake.entries = map[string]string{}
	if _, err := f.store.File.Delete("local"); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), f.in); !apierr.HasCode(err, apierr.CodeAuthMissing) {
		t.Errorf("err = %v, want auth_missing", err)
	}
}

func TestPayJWTEnvForcesJWTMode(t *testing.T) {
	f := newChainFixture(t)
	f.in.Flags = Flags{}
	f.in.Helper = nil
	f.in.Env = MapEnv(map[string]string{"PAY_JWT_LOCAL": "env-token"})

	got, err := Resolve(context.Background(), f.in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Mode != ModeJWT || got.Value != "env-token" || got.Source != "env:PAY_JWT_LOCAL" {
		t.Errorf("credential = %+v", got)
	}

	// …but an API key env var still wins, because §5.1 puts it earlier.
	f.in.Env = MapEnv(map[string]string{"PAY_JWT": "env-token", "PAY_API_KEY": "env-key"})
	got, err = Resolve(context.Background(), f.in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Mode != ModeAPIKey || got.Value != "env-key" {
		t.Errorf("credential = %+v, want the api key", got)
	}
}

func TestExpiredStoredTokenIsTreatedAsAbsent(t *testing.T) {
	f := newChainFixture(t)
	f.in.Flags = Flags{}
	f.in.Env = MapEnv(nil)
	f.in.Helper = nil
	f.fake.entries = map[string]string{}
	exp := testNow.Add(30 * time.Second) // inside the 60s skew
	if err := f.store.File.Put("local", &Record{AuthMode: ModeJWT, Token: "stale", TokenExp: &exp}, testNow); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(context.Background(), f.in)
	if err != nil {
		t.Fatalf("auto mode must degrade to anonymous, not fail: %v", err)
	}
	if got.Mode != ModeAnonymous {
		t.Errorf("mode = %q, want anonymous", got.Mode)
	}
	if len(got.Warnings) == 0 {
		t.Error("an expired stored token must warn")
	}
	for _, w := range got.Warnings {
		if strings.Contains(w, "stale") {
			t.Fatal("the warning leaked the token")
		}
	}

	// In explicit jwt mode the same state is auth_invalid, not auth_missing.
	f.in.Mode = ModeJWT
	if _, err := Resolve(context.Background(), f.in); !apierr.HasCode(err, apierr.CodeAuthInvalid) {
		t.Errorf("err = %v, want auth_invalid", err)
	}
}

func TestAPIKeyFlagWarnsOnATTY(t *testing.T) {
	f := newChainFixture(t)
	f.in.Flags = Flags{APIKey: "from-flag", IsTTY: true}

	got, err := Resolve(context.Background(), f.in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "--api-key-stdin") {
		t.Errorf("warnings = %v", got.Warnings)
	}
	for _, w := range got.Warnings {
		if strings.Contains(w, "from-flag") {
			t.Fatal("the warning leaked the key")
		}
	}

	f.in.Flags.IsTTY = false
	got, _ = Resolve(context.Background(), f.in)
	if len(got.Warnings) != 0 {
		t.Errorf("no warning is due off a TTY: %v", got.Warnings)
	}
}

func TestAPIKeyFileRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(path, []byte("example-api-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{Profile: "local", Flags: Flags{APIKeyFile: path}, Env: MapEnv(nil), Now: testNow}

	_, err := Resolve(context.Background(), in)
	if !apierr.HasCode(err, apierr.CodeAuthInsecurePermissions) {
		t.Fatalf("err = %v, want auth_insecure_permissions", err)
	}
	if strings.Contains(err.Error(), "example-api-key") {
		t.Fatal("the error leaked the key")
	}
}

func TestAPIKeyFileMissing(t *testing.T) {
	in := Input{Profile: "local", Env: MapEnv(nil), Now: testNow,
		Flags: Flags{APIKeyFile: filepath.Join(t.TempDir(), "absent")}}
	if _, err := Resolve(context.Background(), in); !apierr.HasCode(err, apierr.CodeFileMissing) {
		t.Errorf("err = %v, want file_missing", err)
	}
}

func TestStdinIsReadOneLineOnly(t *testing.T) {
	body := strings.NewReader("example-api-key\n{\"title\":\"doc\"}\n")
	in := Input{Profile: "local", Env: MapEnv(nil), Now: testNow,
		Flags: Flags{APIKeyStdin: true, Stdin: body}}
	got, err := Resolve(context.Background(), in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Value != "example-api-key" {
		t.Errorf("value = %q", got.Value)
	}

	// No stdin attached at all is an error, not a hang.
	in.Flags.Stdin = nil
	if _, err := Resolve(context.Background(), in); !apierr.HasCode(err, apierr.CodeAuthMissing) {
		t.Errorf("err = %v, want auth_missing", err)
	}

	// Empty stdin is an error too: the flag was an explicit promise.
	in.Flags.Stdin = strings.NewReader("")
	if _, err := Resolve(context.Background(), in); !apierr.HasCode(err, apierr.CodeAuthMissing) {
		t.Errorf("err = %v, want auth_missing", err)
	}
}

func TestHelperFailureIsFatal(t *testing.T) {
	f := newChainFixture(t)
	f.in.Flags = Flags{}
	f.in.Env = MapEnv(nil)
	f.fake.entries = map[string]string{}
	if _, err := f.store.File.Delete("local"); err != nil {
		t.Fatal(err)
	}
	f.in.Helper = &Helper{Command: "op read …", Timeout: time.Second,
		Run: func(context.Context, string) ([]byte, []byte, error) {
			return nil, []byte("not signed in"), errors.New("exit status 1")
		}}

	if _, err := Resolve(context.Background(), f.in); !apierr.HasCode(err, apierr.CodeAuthHelperFailed) {
		t.Errorf("err = %v, want auth_helper_failed", err)
	}
}

func TestKeyringIsNotTouchedWithoutOptIn(t *testing.T) {
	kr, fake := newFakeKeyring(KeyringOff, map[string]string{
		KeyringService + "|" + KeyringAccount("local"): "from-keyring",
	})
	store := &Store{File: NewFileStore(filepath.Join(t.TempDir(), "credentials.json")), Keyring: kr}
	if err := store.File.Put("local", &Record{AuthMode: ModeAPIKey, APIKey: "from-file"}, testNow); err != nil {
		t.Fatal(err)
	}
	in := Input{Profile: "local", Env: MapEnv(nil), Store: store, Now: testNow, Mode: ModeAuto}

	got, err := Resolve(context.Background(), in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Value != "from-file" || got.Source != SourceCredFile {
		t.Errorf("credential = %+v", got)
	}
	if fake.calls != 0 {
		t.Errorf("the keychain was consulted %d times without an opt-in", fake.calls)
	}
}

func TestInsecureCredentialsFileIsFatalMidChain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	f := newChainFixture(t)
	f.in.Flags = Flags{}
	f.in.Env = MapEnv(nil)
	if err := os.Chmod(f.store.File.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), f.in); !apierr.HasCode(err, apierr.CodeAuthInsecurePermissions) {
		t.Errorf("err = %v, want auth_insecure_permissions", err)
	}
}

func TestNoStoreConfigured(t *testing.T) {
	in := Input{Profile: "local", Env: MapEnv(nil), Now: testNow, Mode: ModeAuto}
	got, err := Resolve(context.Background(), in)
	if err != nil {
		t.Fatalf("Resolve without a store must degrade to anonymous: %v", err)
	}
	if !got.Anonymous() {
		t.Errorf("credential = %+v", got)
	}
}
