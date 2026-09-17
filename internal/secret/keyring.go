package secret

import (
	"context"
	"errors"
	"time"

	keyring "github.com/zalando/go-keyring"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Keychain identifiers (§5.1 step 8).
const (
	KeyringService = "pay-cli"
	accountPrefix  = "profile:"
)

// KeyringTimeout bounds every keychain call. A dbus or wincred prompt that
// never returns must not hang a CLI an agent is driving (§5.1 step 8).
const KeyringTimeout = 2 * time.Second

// KeyringMode mirrors PAY_KEYRING (§5.2). It is declared here rather than
// imported from internal/config so that this package stays independent of the
// configuration layer; the values are identical strings.
type KeyringMode string

// Keyring modes.
const (
	KeyringAuto  KeyringMode = "auto"
	KeyringOff   KeyringMode = "off"
	KeyringForce KeyringMode = "force"
)

// KeyringAccount is the keychain account name for a profile.
func KeyringAccount(profile string) string { return accountPrefix + profile }

// Keyring is the opt-in OS keychain backend. The three function fields are
// injection points: unit tests never touch dbus or wincred (§17.1).
type Keyring struct {
	Mode    KeyringMode
	Timeout time.Duration

	GetFunc    func(service, account string) (string, error)
	SetFunc    func(service, account, secret string) error
	DeleteFunc func(service, account string) error
}

// NewKeyring builds a keychain bound to the real OS backend.
//
// Mode "off" short-circuits every call before the backend is touched at all,
// which is what PAY_KEYRING=off promises: no dbus, no wincred, no prompt.
func NewKeyring(mode KeyringMode) *Keyring {
	if mode == "" {
		mode = KeyringOff
	}
	return &Keyring{
		Mode:       mode,
		Timeout:    KeyringTimeout,
		GetFunc:    keyring.Get,
		SetFunc:    keyring.Set,
		DeleteFunc: keyring.Delete,
	}
}

// Enabled reports whether the keychain may be touched at all.
func (k *Keyring) Enabled() bool {
	return k != nil && k.Mode != KeyringOff && k.GetFunc != nil
}

// Get reads a profile's secret. The boolean is false for "no entry", which is
// not an error.
func (k *Keyring) Get(ctx context.Context, profile string) (string, bool, error) {
	if !k.Enabled() {
		return "", false, nil
	}
	value, err := run(ctx, k.timeout(), func() (string, error) {
		return k.GetFunc(KeyringService, KeyringAccount(profile))
	})
	switch {
	case err == nil:
		if value == "" {
			return "", false, nil
		}
		return value, true, nil
	case errors.Is(err, keyring.ErrNotFound):
		return "", false, nil
	default:
		return "", false, k.wrap(err, "read")
	}
}

// Set stores a profile's secret.
func (k *Keyring) Set(ctx context.Context, profile, value string) error {
	if !k.Enabled() {
		return k.wrap(keyring.ErrUnsupportedPlatform, "write")
	}
	_, err := run(ctx, k.timeout(), func() (string, error) {
		return "", k.SetFunc(KeyringService, KeyringAccount(profile), value)
	})
	if err != nil {
		return k.wrap(err, "write")
	}
	return nil
}

// Delete removes a profile's secret; a missing entry is not an error.
func (k *Keyring) Delete(ctx context.Context, profile string) error {
	if !k.Enabled() {
		return nil
	}
	_, err := run(ctx, k.timeout(), func() (string, error) {
		return "", k.DeleteFunc(KeyringService, KeyringAccount(profile))
	})
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return k.wrap(err, "delete")
	}
	return nil
}

func (k *Keyring) timeout() time.Duration {
	if k.Timeout > 0 {
		return k.Timeout
	}
	return KeyringTimeout
}

// wrap turns a backend failure into an actionable error. The keychain is
// never a silent fallback: the user must know where the secret went (§5.2).
func (k *Keyring) wrap(err error, op string) error {
	return apierr.Wrap(err, apierr.CodeAuthHelperFailed,
		"OS keychain %s failed for service %q", op, KeyringService).
		WithHint("PAY_KEYRING=off stores credentials in credentials.json (0600) instead")
}

// run executes fn with a deadline. The goroutine is abandoned, not killed, on
// timeout — the keychain libraries are blocking C/dbus calls — but the buffered
// channel means it can still finish and be garbage collected.
func run(ctx context.Context, timeout time.Duration, fn func() (string, error)) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		value string
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()

	select {
	case r := <-ch:
		return r.value, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
