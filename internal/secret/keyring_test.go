package secret

import (
	"context"
	"errors"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// fakeKeyring is an in-memory keychain. No unit test may touch dbus or
// wincred (§17.1).
type fakeKeyring struct {
	entries map[string]string
	err     error
	calls   int
	block   chan struct{}
}

func newFakeKeyring(mode KeyringMode, entries map[string]string) (*Keyring, *fakeKeyring) {
	f := &fakeKeyring{entries: entries}
	if f.entries == nil {
		f.entries = map[string]string{}
	}
	k := &Keyring{
		Mode:    mode,
		Timeout: time.Second,
		GetFunc: func(service, account string) (string, error) {
			f.calls++
			if f.block != nil {
				<-f.block
			}
			if f.err != nil {
				return "", f.err
			}
			v, ok := f.entries[service+"|"+account]
			if !ok {
				return "", keyring.ErrNotFound
			}
			return v, nil
		},
		SetFunc: func(service, account, secret string) error {
			f.calls++
			if f.err != nil {
				return f.err
			}
			f.entries[service+"|"+account] = secret
			return nil
		},
		DeleteFunc: func(service, account string) error {
			f.calls++
			if f.err != nil {
				return f.err
			}
			delete(f.entries, service+"|"+account)
			return nil
		},
	}
	return k, f
}

func TestKeyringOffNeverTouchesTheBackend(t *testing.T) {
	k, f := newFakeKeyring(KeyringOff, map[string]string{
		KeyringService + "|" + KeyringAccount("local"): "example-api-key",
	})
	value, ok, err := k.Get(context.Background(), "local")
	if err != nil || ok || value != "" {
		t.Fatalf("Get = %q,%v,%v; want an empty miss", value, ok, err)
	}
	if f.calls != 0 {
		t.Errorf("the backend was called %d times with PAY_KEYRING=off", f.calls)
	}
	if k.Enabled() {
		t.Error("Enabled() = true in off mode")
	}
}

func TestKeyringGetSetDelete(t *testing.T) {
	k, _ := newFakeKeyring(KeyringAuto, nil)
	ctx := context.Background()

	if _, ok, err := k.Get(ctx, "local"); err != nil || ok {
		t.Fatalf("a missing entry must be a clean miss: %v,%v", ok, err)
	}
	if err := k.Set(ctx, "local", "example-api-key"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, ok, err := k.Get(ctx, "local")
	if err != nil || !ok || value != "example-api-key" {
		t.Fatalf("Get = %q,%v,%v", value, ok, err)
	}
	if err := k.Delete(ctx, "local"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := k.Get(ctx, "local"); ok {
		t.Error("the entry survived Delete")
	}
	if err := k.Delete(ctx, "local"); err != nil {
		t.Errorf("deleting a missing entry must succeed: %v", err)
	}
}

func TestKeyringErrorsAreActionable(t *testing.T) {
	k, f := newFakeKeyring(KeyringAuto, nil)
	f.err = errors.New("dbus: no session bus")

	_, _, err := k.Get(context.Background(), "local")
	if !apierr.HasCode(err, apierr.CodeAuthHelperFailed) {
		t.Fatalf("err = %v, want auth_helper_failed", err)
	}
	if err := k.Set(context.Background(), "local", "x"); err == nil {
		t.Error("Set must surface the backend failure")
	}
}

func TestKeyringTimeout(t *testing.T) {
	k, f := newFakeKeyring(KeyringAuto, nil)
	f.block = make(chan struct{})
	k.Timeout = 10 * time.Millisecond
	defer close(f.block)

	// Without the deadline this call would block until the deferred close,
	// i.e. forever, and the test would fail by timing out.
	if _, _, err := k.Get(context.Background(), "local"); err == nil {
		t.Fatal("a blocked keychain must time out")
	}
}

func TestKeyringAccountName(t *testing.T) {
	if got := KeyringAccount("local"); got != "profile:local" {
		t.Errorf("KeyringAccount = %q, want profile:local", got)
	}
	if KeyringService != "pay-cli" {
		t.Errorf("KeyringService = %q, want pay-cli", KeyringService)
	}
	if k := NewKeyring(""); k.Mode != KeyringOff {
		t.Errorf("an unset mode must default to off, got %q", k.Mode)
	}
}
